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
// bound a wedged account write would park a goroutine and its batch for the
// life of the process; with one the batch can be settled by a later attempt.
const FlushTimeout = 30 * time.Second

// Buffer holds derived events until there are enough to justify a transaction.
// Public request waiters are released only after the direct account write.
type Buffer struct {
	transport Transport

	size     int
	interval time.Duration

	// OnError reports a failed flush to operations in addition to the 503 sent
	// to any durable request waiter.
	OnError func(error)

	mu      sync.Mutex
	pending []Event
	waiters map[uuid.UUID][]chan error

	// flushing serialises flushes without holding the append lock across the
	// write. An append during a flush lands in the next batch rather than
	// blocking the request that made it.
	flushing sync.Mutex

	// scheduled means a size-triggered flush is already on its way. Without it
	// every append past the size threshold starts another goroutine that can
	// only queue behind the first, so one slow account turns a burst of traffic
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

	return &Buffer{
		transport: transport,
		size:      size,
		interval:  interval,
		waiters:   map[uuid.UUID][]chan error{},
	}
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
	b.add(event, nil)
}

// AddAndWait appends an event and waits until the transport has confirmed the
// event was written. Every public serving mode uses this path because a 202 is
// a durability promise: a process crash after the response must find the event
// in SQLite, not only in this process's pending slice.
//
// Waiting retains batching. Concurrent requests collect behind the same flush,
// and a quiet request starts one after the normal interval so it cannot wait
// forever when a Buffer is exercised without its Run loop.
func (b *Buffer) AddAndWait(ctx context.Context, event Event) error {
	done := make(chan error, 1)
	b.add(event, done)

	timer := time.NewTimer(b.interval)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FlushTimeout)
		_ = b.flush(flushCtx)
		cancel()

		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// add records one event and an optional caller waiting for its write outcome.
func (b *Buffer) add(event Event, waiter chan error) {
	b.mu.Lock()
	b.pending = append(b.pending, event)
	if waiter != nil {
		b.waiters[event.UUID] = append(b.waiters[event.UUID], waiter)
	}
	full := len(b.pending) >= b.size
	b.mu.Unlock()

	if !full || !b.scheduled.CompareAndSwap(false, true) {
		return
	}

	// The flush runs on its own goroutine so asynchronous internal callers do
	// not block. Public handlers use AddAndWait and do not acknowledge until
	// this flush has crossed its durable commit boundary.
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
			b.notify(events, nil, nil)
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

		// Asynchronous internal callers retain their uncommitted work. Durable
		// public callers receive the error and keep the authoritative browser
		// copy, so requeue deliberately excludes events with request waiters.
		b.requeue(events, committed)
		b.notify(events, committed, err)
	}

	return firstErr
}

// notify releases callers waiting for this flush. A partial write reports
// success only for UUIDs the transport named; every other durable caller
// receives the write error.
func (b *Buffer) notify(events []Event, committed []uuid.UUID, sendErr error) {
	done := make(map[uuid.UUID]struct{}, len(committed))
	for _, id := range committed {
		done[id] = struct{}{}
	}

	type result struct {
		waiter chan error
		err    error
	}

	var results []result

	b.mu.Lock()
	for _, event := range events {
		err := sendErr
		if sendErr == nil {
			err = nil
		} else if _, ok := done[event.UUID]; ok {
			err = nil
		}

		for _, waiter := range b.waiters[event.UUID] {
			results = append(results, result{waiter: waiter, err: err})
		}
		delete(b.waiters, event.UUID)
	}
	b.mu.Unlock()

	for _, result := range results {
		result.waiter <- result.err
	}
}

// requeue puts uncommitted asynchronous events back while leaving durable
// request retries to the browser copy that shares the same permanent UUID.
func (b *Buffer) requeue(events []Event, committed []uuid.UUID) {
	done := make(map[uuid.UUID]struct{}, len(committed))
	for _, id := range committed {
		done[id] = struct{}{}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var retry []Event
	for _, event := range events {
		if _, ok := done[event.UUID]; ok {
			continue
		}
		if len(b.waiters[event.UUID]) > 0 {
			continue
		}
		retry = append(retry, event)
	}

	if len(retry) == 0 {
		return
	}

	b.pending = append(retry, b.pending...)
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
