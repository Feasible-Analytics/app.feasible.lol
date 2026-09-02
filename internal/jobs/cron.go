//
// cron.go
// A periodic tick that every process may enqueue and exactly one will run.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// Cron turns "every hour" into rows in the jobs table.
//
// It is a producer for the one queue, not a scheduler of its own: it enqueues
// through the same Client everything else does, and the Runner drains what it
// writes. A second queue with its own claiming and its own retries would mean
// two answers to "is anything stuck", and the readiness probe can only report
// on one of them.
//
// It deliberately knows nothing about local time. Everything that has to happen
// at a customer's local hour is decided inside the job itself, by asking each
// site whether its own clock has crossed a boundary — see the reports package.
// Cron's only job is to make sure that question gets asked once an hour.
//
// Every process in a deployment runs its own Cron and they all try to enqueue
// the same tick. The unique key makes that safe: they race, one insert wins,
// the rest are no-ops, and no leader election is needed for something whose
// whole content is "it is now the top of the hour".
type Cron struct {
	Client  *Client
	Log     *logger.Logger
	Entries []CronEntry

	// Interval is how often Cron looks at its entries. It has to be shorter
	// than the shortest entry period or a tick can be missed entirely.
	Interval time.Duration

	mu          sync.Mutex
	lastRun     time.Time
	lastCreated int
	lastErr     error
}

// CronEntry is one recurring job.
type CronEntry struct {
	Queue string
	Kind  string

	// Every is the period. The tick is keyed by the period bucket the clock
	// falls in, so an entry never fires twice in one bucket however many
	// processes are running and however often Cron looks.
	Every time.Duration

	// Lookback bounds outage recovery. Zero creates only the current bucket;
	// a positive value fills durable buckets after the last observed slot, but
	// never reaches farther back than this duration.
	Lookback time.Duration
}

// PeriodicArgs preserves the tick that created a recurring job. A retry after
// a crash must evaluate the original scheduling bucket, especially for a local
// midnight report that is no longer "due now" fifteen minutes later.
type PeriodicArgs struct {
	ScheduledAt int64 `json:"scheduled_at"`
}

// NewCron builds a cron over the queue's client.
func NewCron(client *Client, log *logger.Logger) *Cron {
	return &Cron{Client: client, Log: log, Interval: time.Minute}
}

// Add registers a recurring job.
func (c *Cron) Add(queue, kind string, every time.Duration) {
	c.Entries = append(c.Entries, CronEntry{Queue: queue, Kind: kind, Every: every})
}

// AddCatchUp registers a recurring job whose missed durable buckets are
// recreated after an outage, bounded by lookback.
func (c *Cron) AddCatchUp(queue, kind string, every, lookback time.Duration) {
	c.Entries = append(c.Entries, CronEntry{Queue: queue, Kind: kind, Every: every, Lookback: lookback})
}

// Queues lists the queues Cron writes to, so the process that runs it can be
// checked against the queues it also drains. A tick enqueued onto a queue
// nothing claims is work that silently never happens.
func (c *Cron) Queues() []string {
	seen := map[string]bool{}
	queues := []string{}

	for _, entry := range c.Entries {
		if seen[entry.Queue] {
			continue
		}

		seen[entry.Queue] = true
		queues = append(queues, entry.Queue)
	}

	return queues
}

// EnqueueDue adds a job for every entry whose current period has no job yet,
// and reports how many were actually created. The count is returned rather than
// discarded so that a Cron which has silently stopped creating work is visible
// in a log line instead of only in the absence of email.
func (c *Cron) EnqueueDue(ctx context.Context, now time.Time) (created int, runErr error) {
	for _, entry := range c.Entries {
		if entry.Every <= 0 {
			continue
		}

		current := now.UTC().Truncate(entry.Every)
		start := current

		if entry.Lookback > 0 {
			last, found, err := c.Client.LatestPeriodicBucket(ctx, entry.Queue, entry.Kind)
			if err != nil {
				return created, err
			}
			if found {
				start = time.Unix(last, 0).UTC().Add(entry.Every)
				oldest := current.Add(-entry.Lookback).Truncate(entry.Every)
				if start.Before(oldest) {
					start = oldest
				}
			}
		}

		for bucketTime := start; !bucketTime.After(current); bucketTime = bucketTime.Add(entry.Every) {
			bucket := bucketTime.Unix()
			_, made, err := c.Client.EnqueuePeriodic(ctx, entry.Queue, entry.Kind,
				PeriodicArgs{ScheduledAt: bucket}, bucket)
			if err != nil {
				return created, err
			}

			if made {
				created++
			}
		}
	}

	return created, nil
}

// Health reports whether Cron has run recently and whether its latest enqueue
// pass succeeded. The created count is intentionally not treated as health:
// zero is truthful and normal when another process already owns the buckets.
func (c *Cron) Health(now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lastRun.IsZero() {
		return errors.New("recurring scheduler has not run")
	}
	if c.lastErr != nil {
		return fmt.Errorf("recurring scheduler failed: %w", c.lastErr)
	}
	interval := c.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	if now.UTC().Sub(c.lastRun) > 3*interval {
		return fmt.Errorf("recurring scheduler last ran at %s", c.lastRun.Format(time.RFC3339))
	}

	return nil
}

// recordRun stores the latest observable scheduler outcome.
func (c *Cron) recordRun(at time.Time, created int, err error) {
	c.mu.Lock()
	c.lastRun = at.UTC()
	c.lastCreated = created
	c.lastErr = err
	c.mu.Unlock()

	if c.Log == nil {
		return
	}
	if err != nil {
		c.Log.Error("the recurring jobs could not be enqueued", "created_jobs", created, "error", err)
		return
	}
	c.Log.Info("recurring jobs enqueued", "created_jobs", created)
}

// Run enqueues due entries on a ticker until the context is cancelled. It runs
// once immediately so that a process which has just started does not wait a
// whole interval before the first tick — which, after a deploy at five past the
// hour, would otherwise cost an hour of alerts.
func (c *Cron) Run(ctx context.Context, now func() time.Time) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	interval := c.Interval
	if interval <= 0 {
		interval = time.Minute
	}

	runAt := now()
	created, err := c.EnqueueDue(ctx, runAt)
	c.recordRun(runAt, created, err)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runAt := now()
			created, err := c.EnqueueDue(ctx, runAt)
			c.recordRun(runAt, created, err)
		}
	}
}
