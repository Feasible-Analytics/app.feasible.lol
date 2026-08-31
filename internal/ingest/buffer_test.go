//
// buffer_test.go
// Tests for batching by size and by time, and for putting a failed batch back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recording is a Transport that remembers what it was handed and can be made to
// fail, which is the only way to exercise the requeue path deterministically.
type recording struct {
	mu sync.Mutex

	batches [][]Event
	shards  []int

	// failNext makes the next send fail, and commitOnFail decides how much of
	// the batch it claims to have committed — a partial commit is exactly what
	// a lost acknowledgement mid-batch looks like.
	failNext     bool
	commitOnFail int
}

// Send records the batch and either accepts it or fails as configured.
func (r *recording) Send(_ context.Context, shard int, batch []Event) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.batches = append(r.batches, append([]Event(nil), batch...))
	r.shards = append(r.shards, shard)

	if r.failNext {
		r.failNext = false

		var committed []uuid.UUID
		for i := 0; i < r.commitOnFail && i < len(batch); i++ {
			committed = append(committed, batch[i].UUID)
		}

		return committed, errors.New("disk full")
	}

	committed := make([]uuid.UUID, 0, len(batch))
	for _, event := range batch {
		committed = append(committed, event.UUID)
	}

	return committed, nil
}

// count reports how many batches have been sent.
func (r *recording) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.batches)
}

// bufferEvent builds a derived event on a given shard.
func bufferEvent(shard int) Event {
	return Event{UUID: uuid.New(), Shard: shard, AccountID: 1, SiteID: 1, Name: EventPageview}
}

// TestFlushGroupsByShard checks the buffer hands each shard its own batch. The
// shard is a property of the event, and a transport that had to work it out
// would have to know the routing table.
func TestFlushGroupsByShard(t *testing.T) {
	transport := &recording{}
	buffer := NewBuffer(transport, 100, time.Hour)

	ctx := context.Background()
	for _, shard := range []int{0, 1, 0, 1, 2} {
		buffer.Add(ctx, bufferEvent(shard))
	}

	if err := buffer.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if transport.count() != 3 {
		t.Fatalf("sent %d batches, want one per shard", transport.count())
	}

	total := 0
	for _, batch := range transport.batches {
		total += len(batch)
		for _, event := range batch {
			if event.Shard != batch[0].Shard {
				t.Fatal("a batch mixed events from two shards")
			}
		}
	}
	if total != 5 {
		t.Fatalf("delivered %d events, want 5", total)
	}
}

// TestFlushOnSize checks the size trigger. The flush runs on its own goroutine
// so the request that filled the buffer is not the one that waits for the disk.
func TestFlushOnSize(t *testing.T) {
	transport := &recording{}
	buffer := NewBuffer(transport, 3, time.Hour)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		buffer.Add(ctx, bufferEvent(0))
	}

	deadline := time.Now().Add(2 * time.Second)
	for transport.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if transport.count() != 1 {
		t.Fatalf("sent %d batches, want 1 once the buffer filled", transport.count())
	}
	if buffer.Len() != 0 {
		t.Fatalf("%d events left in the buffer after a full flush", buffer.Len())
	}
}

// TestFlushOnInterval checks the time trigger, which is what stops a quiet site
// waiting for a batch that will never fill.
func TestFlushOnInterval(t *testing.T) {
	transport := &recording{}
	buffer := NewBuffer(transport, 1000, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go buffer.Run(ctx)

	buffer.Add(ctx, bufferEvent(0))

	deadline := time.Now().Add(2 * time.Second)
	for transport.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if transport.count() == 0 {
		t.Fatal("the interval never flushed a partly-full buffer")
	}
}

// TestFinalFlushOnShutdown is what makes a deploy graceful. Without it every
// event from the last half second is lost on every restart.
func TestFinalFlushOnShutdown(t *testing.T) {
	transport := &recording{}
	buffer := NewBuffer(transport, 1000, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		buffer.Run(ctx)
		close(done)
	}()

	buffer.Add(ctx, bufferEvent(0))
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the buffer did not stop")
	}

	if transport.count() != 1 {
		t.Fatalf("sent %d batches on shutdown, want 1", transport.count())
	}
	if buffer.Len() != 0 {
		t.Fatalf("%d events were lost on shutdown", buffer.Len())
	}
}

// TestUncommittedEventsAreRequeued checks a partial failure retries exactly what
// did not land. Returning the committed ids rather than a bare error is what
// makes that possible without writing the successful half twice.
func TestUncommittedEventsAreRequeued(t *testing.T) {
	transport := &recording{failNext: true, commitOnFail: 2}
	buffer := NewBuffer(transport, 100, time.Hour)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		buffer.Add(ctx, bufferEvent(0))
	}

	if err := buffer.Flush(ctx); err == nil {
		t.Fatal("the flush reported success after the transport failed")
	}

	if buffer.Len() != 3 {
		t.Fatalf("%d events were requeued, want the 3 that did not commit", buffer.Len())
	}

	// The retry delivers only the uncommitted three.
	if err := buffer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(transport.batches[1]); got != 3 {
		t.Fatalf("the retry sent %d events, want 3", got)
	}
	if buffer.Len() != 0 {
		t.Fatalf("%d events left after a successful retry", buffer.Len())
	}
}

// TestFlushErrorIsReported checks a stuck buffer becomes visible. Store and
// forward hides failure by design — the client already has its 202 — so this
// callback is the only place anybody finds out.
func TestFlushErrorIsReported(t *testing.T) {
	transport := &recording{failNext: true}
	buffer := NewBuffer(transport, 100, time.Hour)

	var reported error
	buffer.OnError = func(err error) { reported = err }

	ctx := context.Background()
	buffer.Add(ctx, bufferEvent(0))

	if err := buffer.Flush(ctx); err == nil {
		t.Fatal("the flush reported success")
	}
	if reported == nil {
		t.Fatal("the failure was not reported to OnError")
	}
}

// TestEmptyFlushIsFree checks the timer does not send empty batches, which on a
// quiet site would be a transaction a second for nothing.
func TestEmptyFlushIsFree(t *testing.T) {
	transport := &recording{}
	buffer := NewBuffer(transport, 100, time.Hour)

	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.count() != 0 {
		t.Fatal("an empty buffer still sent a batch")
	}
}
