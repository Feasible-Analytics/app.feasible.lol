//
// recorder.go
// Counting billable events on the write path without touching control.db.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import (
	"context"
	"sync"
	"time"
)

// FlushInterval is how often accumulated counts are written to control.db.
// Thirty seconds bounds how much is lost if a shard is killed uncleanly, and
// keeps the busiest path in the system away from the file the dashboard, the
// job queue and billing all contend for.
const FlushInterval = 30 * time.Second

// Recorder accumulates billable counts in memory and flushes them in batches.
//
// The rule it exists to keep is that nothing on the ingest hot path may touch
// control.db. A per-event UPDATE against the shared control database would put
// the busiest write in the product behind the one lock every other subsystem
// is already queued on, and at a thousand events a second it would be the
// bottleneck for all of them.
//
// Losing a flush is acceptable and losing a customer's data is not, which is
// why this counts after the account's transaction has committed: an event that
// was never stored must never be billed.
type Recorder struct {
	store *Store

	// Now decides which calendar month a count lands in, and is injectable so a
	// test can cross a month boundary without waiting for one.
	Now func() time.Time

	// OnError reports a failed flush. A silently dropped flush is a customer
	// under-billed and, worse, a limit warning that never fires.
	OnError func(error)

	mu      sync.Mutex
	pending map[key]Counts
}

// key is one account in one calendar month. The period is part of the key
// rather than resolved at flush time, so events that arrive either side of a
// month boundary land in the month they happened in rather than the month the
// flush ran in.
type key struct {
	teamID int64
	period string
}

// NewRecorder builds a recorder over the store.
func NewRecorder(store *Store) *Recorder {
	return &Recorder{store: store, pending: map[key]Counts{}}
}

// Record adds one account's committed events to the in-memory totals. It is
// called from the shard writer after the transaction commits, takes a mutex and
// touches no I/O, so its cost on the write path is a map lookup.
func (r *Recorder) Record(teamID int64, counts Counts) {
	if teamID < 1 || (counts.Pageviews == 0 && counts.CustomEvents == 0) {
		return
	}

	k := key{teamID: teamID, period: Period(r.now())}

	r.mu.Lock()
	entry := r.pending[k]
	entry.Add(counts)
	r.pending[k] = entry
	r.mu.Unlock()
}

// now returns the recorder's clock.
func (r *Recorder) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}

	return r.Now().UTC()
}

// Flush writes everything accumulated so far. The pending map is swapped out
// under the lock and written outside it, so a slow control database cannot
// block the ingest path that is still calling Record.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	batch := r.pending
	r.pending = map[key]Counts{}
	r.mu.Unlock()

	var firstErr error

	for k, counts := range batch {
		if err := r.store.Add(ctx, k.teamID, k.period, counts); err != nil {
			// The counts are put back so the next flush retries them. Dropping
			// them would under-bill the customer and, more importantly, would
			// hide an account climbing towards its limit.
			r.mu.Lock()
			entry := r.pending[k]
			entry.Add(counts)
			r.pending[k] = entry
			r.mu.Unlock()

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// Run flushes on a ticker until the context is cancelled, then flushes once
// more on the way out so a clean shutdown does not lose the last interval.
func (r *Recorder) Run(ctx context.Context) {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The parent context is already cancelled, so the final flush needs
			// its own or every statement in it would fail immediately.
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := r.Flush(final); err != nil && r.OnError != nil {
				r.OnError(err)
			}
			cancel()

			return

		case <-ticker.C:
			if err := r.Flush(ctx); err != nil && r.OnError != nil {
				r.OnError(err)
			}
		}
	}
}

// Pending reports how many account-months are waiting to be written. A number
// that only grows means every flush is failing, which is what a health panel
// should show rather than leaving it to be discovered on an invoice.
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.pending)
}
