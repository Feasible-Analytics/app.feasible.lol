//
// jobs.go
// The background job queue: claim by queue, dispatch by worker kind.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package jobs runs the work that must not happen inside a request: importing a
// year of history, preparing a full-site export, refreshing a Google token. It
// is a table in control.db and a loop, not a hosted queue, because the whole
// pitch of this product is one binary and one data directory.
//
// One rule in here is worth more than the rest of the package put together: a
// job is identified by its **kind**, never by its queue plus the shape of its
// arguments. An incumbent's error reporter decided a crash was a failed import
// because the job was on the import queue and its arguments contained an import
// id. A different worker sharing that queue crashed, was read as a failed
// import, and the cleanup it triggered purged fifteen completed imports while
// the interface went on showing them as completed for thirteen days. Every
// lookup in this package therefore filters on kind, and a test asserts that a
// foreign job carrying an import id in its arguments is not reported as one.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Queue names. They are constants so a typo cannot create a queue nothing is
// draining, which looks exactly like a job that never runs.
const (
	QueueImports = "imports"
	QueueExports = "exports"
)

// Kinds are the worker types. This is the value everything in this package
// matches on, and the one that must never be inferred from a queue name.
const (
	KindCSVImport           = "csv_import"
	KindGA4Import           = "ga4_import"
	KindSearchConsoleImport = "search_console_import"
	KindSiteExport          = "site_export"
)

// States a row can be in. They mirror the CHECK constraint in the control
// schema, which is what stops a typo from writing a state nothing claims.
const (
	StateAvailable = "available"
	StateExecuting = "executing"
	StateCompleted = "completed"
	StateDiscarded = "discarded"
)

// DefaultMaxAttempts is how many times a job is retried before it is discarded.
// It is low for this kind of work on purpose: an import that fails four times
// is failing for a reason a retry will not fix, and the customer is better
// served by a readable failure than by a job that keeps almost-working.
const DefaultMaxAttempts = 4

// PollInterval is how long an idle runner waits before looking again. Two
// seconds keeps a freshly-enqueued import feeling immediate without turning an
// idle install into a database write every tick.
const PollInterval = 2 * time.Second

// StaleClaim is how long a job may sit in `executing` before it is assumed
// abandoned. A process that is killed — a deploy, an out-of-memory kill, a
// laptop closing — leaves its claimed jobs in that state with nothing to
// release them, and without this an import would sit at "running" for ever
// while the customer waits for a progress bar that is never going to move.
const StaleClaim = 15 * time.Minute

// Job is one row of the queue.
type Job struct {
	ID          int64
	Queue       string
	Kind        string
	Args        json.RawMessage
	State       string
	Attempt     int
	MaxAttempts int
	ScheduledAt int64
	LastError   string
}

// Worker runs one kind of job. It takes the raw arguments rather than a decoded
// struct so that the queue never has to know what any particular job carries,
// and returns an error whose message is shown to the customer — so it is
// written for them.
type Worker interface {
	Run(ctx context.Context, job Job) error
}

// WorkerFunc adapts a function to Worker.
type WorkerFunc func(ctx context.Context, job Job) error

// Run calls the function.
func (f WorkerFunc) Run(ctx context.Context, job Job) error { return f(ctx, job) }

// Permanent wraps an error that must not be retried. A malformed CSV is not
// going to parse on the fourth attempt, and retrying it three more times only
// delays the moment the customer is told what is wrong with their file.
type Permanent struct{ Err error }

// Error renders the wrapped message unchanged, because it is the sentence the
// customer reads.
func (p *Permanent) Error() string { return p.Err.Error() }

// Unwrap exposes the cause to errors.Is and errors.As.
func (p *Permanent) Unwrap() error { return p.Err }

// PermanentError marks an error as not worth retrying.
func PermanentError(err error) error { return &Permanent{Err: err} }

// isPermanent reports whether an error asked not to be retried.
func isPermanent(err error) bool {
	var permanent *Permanent

	return errors.As(err, &permanent)
}

// Client is the queue's write side: everything that puts work into it or reads
// its state back out. It holds control.db, which is where the jobs table lives
// because a job can be about any account and there is only one of these.
type Client struct {
	db *sql.DB

	// Now is injectable so a test can drive retry backoff and scheduling
	// without waiting for wall-clock time to pass.
	Now func() time.Time
}

// NewClient builds a client over control.db.
func NewClient(db *sql.DB) *Client {
	return &Client{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the client's clock.
func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}

	return c.Now().UTC()
}

// Enqueue adds a job. The unique key is optional and only holds while the job
// is live: an import that runs twice doubles a customer's numbers and no later
// check can tell which half was the duplicate, so the caller passes a key and
// the partial index in the schema enforces it.
func (c *Client) Enqueue(ctx context.Context, queue, kind string, args any, uniqueKey string) (int64, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return 0, fmt.Errorf("jobs: encode %s arguments: %w", kind, err)
	}

	var key any
	if uniqueKey != "" {
		key = uniqueKey
	}

	result, err := c.db.ExecContext(ctx, `
		INSERT INTO jobs (queue, kind, args, state, max_attempts, scheduled_at, unique_key)
		VALUES (?, ?, ?, 'available', ?, ?, ?)`,
		queue, kind, string(encoded), DefaultMaxAttempts, c.now().Unix(), key)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %s: %w", kind, err)
	}

	return result.LastInsertId()
}

// Claim takes the oldest available job on a queue whose time has come. The
// UPDATE ... WHERE state = 'available' is the whole of the locking: SQLite
// serialises writers, so exactly one runner can move a row out of available
// even when several are polling.
func (c *Client) Claim(ctx context.Context, queue string) (*Job, bool, error) {
	now := c.now().Unix()

	var job Job
	var args string
	var lastError sql.NullString

	err := c.db.QueryRowContext(ctx, `
		SELECT id, queue, kind, args, state, attempt, max_attempts, scheduled_at, last_error
		FROM jobs
		WHERE state = 'available' AND queue = ? AND scheduled_at <= ?
		ORDER BY scheduled_at, id
		LIMIT 1`, queue, now).
		Scan(&job.ID, &job.Queue, &job.Kind, &args, &job.State, &job.Attempt,
			&job.MaxAttempts, &job.ScheduledAt, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("jobs: claim: %w", err)
	}

	result, err := c.db.ExecContext(ctx, `
		UPDATE jobs SET state = 'executing', attempt = attempt + 1, attempted_at = ?
		WHERE id = ? AND state = 'available'`, now, job.ID)
	if err != nil {
		return nil, false, fmt.Errorf("jobs: claim: %w", err)
	}

	// Another runner got there first. Answering "nothing to do" rather than
	// retrying keeps the loop simple and costs one poll interval at most.
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil, false, nil
	}

	job.Args = json.RawMessage(args)
	job.State = StateExecuting
	job.Attempt++
	job.LastError = lastError.String

	return &job, true, nil
}

// Complete marks a job done.
func (c *Client) Complete(ctx context.Context, id int64) error {
	_, err := c.db.ExecContext(ctx,
		"UPDATE jobs SET state = 'completed', completed_at = ?, last_error = NULL WHERE id = ?",
		c.now().Unix(), id)
	if err != nil {
		return fmt.Errorf("jobs: complete %d: %w", id, err)
	}

	return nil
}

// Fail records an attempt that did not work. It reschedules with exponential
// backoff until the attempts run out, then discards — and either way the
// message is kept, because a discarded job with no reason attached is the thing
// this whole package exists to avoid.
func (c *Client) Fail(ctx context.Context, job *Job, cause error) error {
	message := cause.Error()

	if job.Attempt >= job.MaxAttempts || isPermanent(cause) {
		_, err := c.db.ExecContext(ctx,
			"UPDATE jobs SET state = 'discarded', completed_at = ?, last_error = ? WHERE id = ?",
			c.now().Unix(), message, job.ID)
		if err != nil {
			return fmt.Errorf("jobs: discard %d: %w", job.ID, err)
		}

		return nil
	}

	// Backoff in whole seconds: 2, 4, 8, 16. Long enough that a database that
	// is momentarily busy has recovered, short enough that a customer watching
	// a progress bar does not conclude the import is stuck.
	delay := time.Duration(math.Pow(2, float64(job.Attempt))) * time.Second

	_, err := c.db.ExecContext(ctx,
		"UPDATE jobs SET state = 'available', scheduled_at = ?, last_error = ? WHERE id = ?",
		c.now().Add(delay).Unix(), message, job.ID)
	if err != nil {
		return fmt.Errorf("jobs: reschedule %d: %w", job.ID, err)
	}

	return nil
}

// ReleaseStale puts abandoned claims back on the queue. It is the only thing
// standing between a process being killed mid-job and that job being lost: the
// state lives in the database, so nothing else in the system will ever notice
// that whoever claimed it is gone.
//
// The attempt count is left alone, so an abandoned job still runs out of
// attempts eventually rather than retrying for ever.
func (c *Client) ReleaseStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := c.now().Add(-olderThan).Unix()

	result, err := c.db.ExecContext(ctx, `
		UPDATE jobs SET state = 'available', scheduled_at = ?
		WHERE state = 'executing' AND attempted_at IS NOT NULL AND attempted_at < ?`,
		c.now().Unix(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("jobs: release stale claims: %w", err)
	}

	released, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("jobs: release stale claims: %w", err)
	}

	return released, nil
}

// Get reads one job back.
func (c *Client) Get(ctx context.Context, id int64) (*Job, error) {
	var job Job
	var args string
	var lastError sql.NullString

	err := c.db.QueryRowContext(ctx, `
		SELECT id, queue, kind, args, state, attempt, max_attempts, scheduled_at, last_error
		FROM jobs WHERE id = ?`, id).
		Scan(&job.ID, &job.Queue, &job.Kind, &args, &job.State, &job.Attempt,
			&job.MaxAttempts, &job.ScheduledAt, &lastError)
	if err != nil {
		return nil, fmt.Errorf("jobs: read %d: %w", id, err)
	}

	job.Args = json.RawMessage(args)
	job.LastError = lastError.String

	return &job, nil
}

// FailedOfKind lists the jobs of one worker type that were discarded.
//
// The signature is the point. Asking "which jobs on the imports queue failed
// and happen to carry an import id" is how an unrelated worker's crash gets
// attributed to an import, and how a cleanup built on that answer deletes
// fifteen imports that had finished perfectly. A caller here has to name the
// worker type it means, and cannot express the other question at all.
func (c *Client) FailedOfKind(ctx context.Context, kind string) ([]Job, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, queue, kind, args, state, attempt, max_attempts, scheduled_at, last_error
		FROM jobs
		WHERE kind = ? AND state = 'discarded'
		ORDER BY id`, kind)
	if err != nil {
		return nil, fmt.Errorf("jobs: read failed %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var found []Job

	for rows.Next() {
		var job Job
		var args string
		var lastError sql.NullString

		if err := rows.Scan(&job.ID, &job.Queue, &job.Kind, &args, &job.State, &job.Attempt,
			&job.MaxAttempts, &job.ScheduledAt, &lastError); err != nil {
			return nil, fmt.Errorf("jobs: read failed %s: %w", kind, err)
		}

		job.Args = json.RawMessage(args)
		job.LastError = lastError.String
		found = append(found, job)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: read failed %s: %w", kind, err)
	}

	return found, nil
}

// Runner drains one or more queues, dispatching each job to the worker
// registered for its kind. A kind with no worker is discarded with a message
// saying so rather than retried forever: nothing in this process is ever going
// to grow the ability to run it.
type Runner struct {
	client *Client

	// Interval is how long an idle pass waits before polling again.
	Interval time.Duration

	// OnError is called for failures the runner itself cannot act on — a
	// database that will not answer a claim. Job failures are recorded on the
	// row instead, because that is where the customer can see them.
	OnError func(error)

	mu      sync.RWMutex
	workers map[string]Worker
	queues  []string
}

// NewRunner builds a runner over a client.
func NewRunner(client *Client) *Runner {
	return &Runner{client: client, workers: map[string]Worker{}, Interval: PollInterval}
}

// Register attaches a worker to a kind and makes sure its queue is drained.
// Registration is by kind and the queue is derived from it, which is what keeps
// "which worker ran this" a fact rather than an inference.
func (r *Runner) Register(queue, kind string, worker Worker) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.workers[kind] = worker

	for _, existing := range r.queues {
		if existing == queue {
			return
		}
	}

	r.queues = append(r.queues, queue)
}

// workerFor looks up a kind's worker.
func (r *Runner) workerFor(kind string) (Worker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	worker, ok := r.workers[kind]

	return worker, ok
}

// queueNames copies the registered queues so the loop does not hold the lock
// while it works.
func (r *Runner) queueNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.queues...)
}

// Run drains every registered queue until the context is cancelled. Each queue
// runs at concurrency one, which suits a system with a single SQLite writer per
// account and makes batch work deliberately serial.
func (r *Runner) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = PollInterval
	}

	// Anything left claimed by a process that is no longer running is put back
	// before this one starts, which is what makes a restart the fix for a job
	// that is stuck rather than the cause of one.
	if _, err := r.client.ReleaseStale(ctx, StaleClaim); err != nil && r.OnError != nil {
		r.OnError(err)
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	sweep := time.Now()

	for {
		// A long-lived process has to keep looking: a sibling replica can die
		// at any point, and its claims are only ever released from here.
		if time.Since(sweep) > StaleClaim {
			sweep = time.Now()

			if _, err := r.client.ReleaseStale(ctx, StaleClaim); err != nil && r.OnError != nil {
				r.OnError(err)
			}
		}

		worked, err := r.Once(ctx)
		if err != nil && r.OnError != nil {
			r.OnError(err)
		}

		// A pass that did work looks again immediately: a queue with a backlog
		// should drain at the speed of the work, not at the speed of the timer.
		wait := interval
		if worked {
			wait = 0
		}

		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// Once makes one pass over every queue and reports whether it ran anything.
// It is exported so a test can drive the runner deterministically instead of
// sleeping and hoping.
func (r *Runner) Once(ctx context.Context) (bool, error) {
	worked := false

	for _, queue := range r.queueNames() {
		job, ok, err := r.client.Claim(ctx, queue)
		if err != nil {
			return worked, err
		}
		if !ok {
			continue
		}

		worked = true
		r.dispatch(ctx, job)
	}

	return worked, nil
}

// dispatch runs one claimed job and records the outcome on its row.
func (r *Runner) dispatch(ctx context.Context, job *Job) {
	worker, ok := r.workerFor(job.Kind)
	if !ok {
		// Not an error to report upwards: this process simply does not run this
		// kind. Discarding with the reason attached is more honest than
		// retrying something that will never succeed here.
		err := r.client.Fail(ctx, job, PermanentError(fmt.Errorf("no worker is registered for %q in this process", job.Kind)))
		if err != nil && r.OnError != nil {
			r.OnError(err)
		}

		return
	}

	if err := worker.Run(ctx, *job); err != nil {
		if failErr := r.client.Fail(ctx, job, err); failErr != nil && r.OnError != nil {
			r.OnError(failErr)
		}

		return
	}

	if err := r.client.Complete(ctx, job.ID); err != nil && r.OnError != nil {
		r.OnError(err)
	}
}
