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
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// recording is a Transport that remembers what it was handed and can be made to
// fail, which is the only way to exercise the requeue path deterministically.
type recording struct {
	mu sync.Mutex

	batches [][]Event
	shards  []int

	// rejectCanceled makes Send model the real writer's immediate refusal when
	// a queued flush reaches it with an already-expired context.
	rejectCanceled bool

	// failNext makes the next send fail, and commitOnFail decides how much of
	// the batch it claims to have committed — a partial commit is exactly what
	// a lost acknowledgement mid-batch looks like.
	failNext     bool
	commitOnFail int
}

// Send records the batch and either accepts it or fails as configured.
func (r *recording) Send(ctx context.Context, shard int, batch []Event) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.batches = append(r.batches, append([]Event(nil), batch...))
	r.shards = append(r.shards, shard)

	if r.rejectCanceled && ctx.Err() != nil {
		return nil, ctx.Err()
	}

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
		buffer.Add(bufferEvent(shard))
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

// TestBufferedStaleRouteDrainsAfterDeletion covers the split-ingest case where
// an accepted event was buffered before the account disappeared from routing.
func TestBufferedStaleRouteDrainsAfterDeletion(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)
	writer := NewWriter(manager)
	buffer := NewBuffer(NewDirect(writer), 100, time.Hour)

	if _, err := manager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	buffer.Add(bufferEvent(0))
	if err := accounts.NewManager(dataDir).Block(1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(accounts.Dir(dataDir, 1)); err != nil {
		t.Fatal(err)
	}

	if err := buffer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("deleted account left %d events retrying", buffer.Len())
	}
	if _, err := os.Stat(accounts.Dir(dataDir, 1)); !os.IsNotExist(err) {
		t.Fatalf("buffered event recreated the deleted directory: %v", err)
	}
}

// TestFlushOnSize checks the size trigger. The flush runs on its own goroutine
// so the request that filled the buffer is not the one that waits for the disk.
func TestFlushOnSize(t *testing.T) {
	transport := &recording{}
	buffer := NewBuffer(transport, 3, time.Hour)

	for i := 0; i < 3; i++ {
		buffer.Add(bufferEvent(0))
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

	buffer.Add(bufferEvent(0))

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

	buffer.Add(bufferEvent(0))
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
		buffer.Add(bufferEvent(0))
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

// TestFlushErrorIsReported checks a stuck buffer becomes visible to operators
// as well as to any durable request waiter.
func TestFlushErrorIsReported(t *testing.T) {
	transport := &recording{failNext: true}
	buffer := NewBuffer(transport, 100, time.Hour)

	var reported error
	buffer.OnError = func(err error) { reported = err }

	ctx := context.Background()
	buffer.Add(bufferEvent(0))

	if err := buffer.Flush(ctx); err == nil {
		t.Fatal("the flush reported success")
	}
	if reported == nil {
		t.Fatal("the failure was not reported to OnError")
	}
}

// TestAddAndWaitReportsFailureBeforeAcknowledgement proves a request cannot
// receive its durable success until the transport has committed the event. A
// failed waiter is not retained internally because the browser owns the retry.
func TestAddAndWaitReportsFailureBeforeAcknowledgement(t *testing.T) {
	transport := &recording{failNext: true}
	buffer := NewBuffer(transport, 100, time.Millisecond)
	event := bufferEvent(0)

	if err := buffer.AddAndWait(context.Background(), event); err == nil {
		t.Fatal("durable add reported success after the transport failed")
	}
	if got := buffer.Len(); got != 0 {
		t.Fatalf("failed durable request left %d internally owned retries", got)
	}
	if err := buffer.AddAndWait(context.Background(), event); err != nil {
		t.Fatalf("caller retry did not commit: %v", err)
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("transport received %d attempts, want 2", got)
	}
}

// TestExpiredQueuedFlushDoesNotConsumeLaterBatch proves a flush whose context
// expires while waiting for serialization cannot detach and fail work that
// arrived after it queued. A later live flush remains responsible for the
// event and its durable waiter.
func TestExpiredQueuedFlushDoesNotConsumeLaterBatch(t *testing.T) {
	transport := &recording{rejectCanceled: true}
	buffer := NewBuffer(transport, 100, time.Hour)

	buffer.flushing.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(queued)
		finished <- buffer.Flush(ctx)
	}()

	<-queued
	cancel()

	event := bufferEvent(0)
	waiter := make(chan error, 1)
	buffer.add(event, waiter)
	buffer.flushing.Unlock()

	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("expired flush returned %v, want context cancellation", err)
	}
	if got := transport.count(); got != 0 {
		t.Fatalf("expired flush sent %d batches, want none", got)
	}
	if got := buffer.Len(); got != 1 {
		t.Fatalf("expired flush left %d events pending, want 1", got)
	}
	select {
	case err := <-waiter:
		t.Fatalf("expired flush released the later waiter with %v", err)
	default:
	}

	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("live flush failed the preserved waiter: %v", err)
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
