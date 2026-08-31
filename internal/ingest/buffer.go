//
// buffer.go
// The write buffer: batch by size or by 500ms, whichever comes first.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/metrics"
)

// DefaultBufferSize is how many events accumulate before a flush is forced. A
// few hundred is where the per-transaction overhead stops dominating without
// the buffer holding enough events that losing it would matter.
const DefaultBufferSize = 250

// DefaultFlushInterval is the longest an event waits before being written. Half
// a second is short enough that a dashboard looks live and long enough that a
// quiet site does not pay a transaction per pageview.
const DefaultFlushInterval = 500 * time.Millisecond

// FlushTimeout bounds one flush the buffer starts on its own behalf. Without a
// bound a wedged shard would park a goroutine and its batch for the life of the
// process; with one the batch comes back to the front of the buffer and is
// retried by the next flush.
const FlushTimeout = 30 * time.Second

// Buffer holds derived events until there are enough of them to be worth a
// transaction. Every event goes through it, even in single-process mode where
// "forward" is a function call, because exercising the seam from day one is the
// entire point of having it.
type Buffer struct {
	transport Transport

	size     int
	interval time.Duration

	// OnError is called when a flush fails. Store-and-forward hides failure by
	// design — the client already has its 202 — so this callback is the only
	// place a stuck buffer becomes visible to anyone.
	OnError func(error)

	mu      sync.Mutex
	pending []Event

	// flushing serialises flushes without holding the append lock across the
	// write. An append during a flush lands in the next batch rather than
	// blocking the request that made it.
	flushing sync.Mutex

	// scheduled means a size-triggered flush is already on its way. Without it
	// every append past the size threshold starts another goroutine that can
	// only queue behind the first, so one slow shard turns a burst of traffic
	// into a pile of goroutines. The ticker picks up anything this skips.
	scheduled atomic.Bool
}

// NewBuffer builds a buffer over a transport. Zero or negative bounds fall back
// to the defaults so a caller can ask for "the normal one".
func NewBuffer(transport Transport, size int, interval time.Duration) *Buffer {
	if size <= 0 {
		size = DefaultBufferSize
	}
	if interval <= 0 {
		interval = DefaultFlushInterval
	}

	return &Buffer{transport: transport, size: size, interval: interval}
}

// Add appends an event and flushes if the batch is full. It never blocks on the
// write itself: the visitor's page is waiting on this call, and a slow disk
// must not become a slow page.
//
// It deliberately takes no context. The only context a caller could pass is the
// request's, and the flush outlives the request by design — a write that
// inherited it would be cancelled the moment the 202 was sent, which is a
// buffer that never writes anything except on its ticker.
func (b *Buffer) Add(event Event) {
	b.mu.Lock()
	b.pending = append(b.pending, event)
	full := len(b.pending) >= b.size
	b.mu.Unlock()

	if !full || !b.scheduled.CompareAndSwap(false, true) {
		return
	}

	// The flush runs on its own goroutine so the request returns immediately.
	// Losing the buffered events in a crash is the trade store-and-forward
	// exists to make: the alternative is a visitor waiting on an fsync.
	go func() {
		defer b.scheduled.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
		defer cancel()

		// flush already reports through OnError, so the error is swallowed here
		// rather than reported twice.
		_ = b.flush(ctx)
	}()
}

// Flush writes everything buffered right now. It is exported so shutdown and
// the tests can drain the buffer synchronously, which is the difference between
// a graceful stop and a stop that loses half a second of traffic.
func (b *Buffer) Flush(ctx context.Context) error {
	return b.flush(ctx)
}

// flush takes the pending events and hands them to the transport, grouped by
// shard. Grouping happens here rather than in the transport because the shard
// is a property of the event, and a transport that had to work it out would
// have to know the routing table.
func (b *Buffer) flush(ctx context.Context) error {
	b.flushing.Lock()
	defer b.flushing.Unlock()

	b.mu.Lock()
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	// A flush is the one place where "we said 202" becomes "it is on disk", so
	// it is timed and counted here rather than inside a transport that only one
	// deployment shape uses.
	started := time.Now()

	metrics.FlushBatchSize.Observe(float64(len(batch)))

	defer func() { metrics.FlushDuration.Observe(time.Since(started).Seconds()) }()

	byShard := map[int][]Event{}
	for _, event := range batch {
		byShard[event.Shard] = append(byShard[event.Shard], event)
	}

	var firstErr error

	for shard, events := range byShard {
		committed, err := b.transport.Send(ctx, shard, events)
		if err == nil {
			metrics.Flushes.WithLabelValues(metrics.OutcomeOK).Inc()
			metrics.EventsWritten.Add(float64(len(events)))
			continue
		}

		// Whatever did commit still counts as written: a partial failure that
		// reported nothing would make accepted-minus-written look like data
		// loss when it is a retry in progress.
		metrics.Flushes.WithLabelValues(metrics.OutcomeError).Inc()
		metrics.EventsWritten.Add(float64(len(committed)))

		if firstErr == nil {
			firstErr = err
		}
		if b.OnError != nil {
			b.OnError(err)
		}

		// Whatever did not commit goes back on the front of the buffer. The
		// committed ids come back from the transport precisely so that a partial
		// failure can be retried without writing the successful half twice.
		b.requeue(events, committed)
	}

	return firstErr
}

// requeue puts the uncommitted part of a failed batch back. It is what turns a
// transient write failure into a delayed write rather than a lost event.
func (b *Buffer) requeue(events []Event, committed []uuid.UUID) {
	done := make(map[uuid.UUID]struct{}, len(committed))
	for _, id := range committed {
		done[id] = struct{}{}
	}

	var retry []Event
	for _, event := range events {
		if _, ok := done[event.UUID]; ok {
			continue
		}
		retry = append(retry, event)
	}

	if len(retry) == 0 {
		return
	}

	b.mu.Lock()
	b.pending = append(retry, b.pending...)
	b.mu.Unlock()
}

// Len reports how many events are waiting. A buffer that only grows is a
// transport that has stopped accepting, and that is worth seeing on the health
// panel long before it is worth seeing in a memory graph.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.pending)
}

// Run flushes on a ticker until the context is cancelled, then flushes one last
// time. The final flush is what makes shutdown graceful: without it every event
// from the last half second is lost on every deploy.
func (b *Buffer) Run(ctx context.Context) {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The parent context is already cancelled, so the final flush needs
			// one of its own or the write would be cancelled before it started.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if err := b.flush(final); err != nil && b.OnError != nil {
				b.OnError(err)
			}
			cancel()

			return

		case <-ticker.C:
			if err := b.flush(ctx); err != nil && b.OnError != nil {
				b.OnError(err)
			}
		}
	}
}
