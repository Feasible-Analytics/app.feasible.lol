//
// jobs_test.go
// The queue, and the misattribution that once purged fifteen finished imports.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newSystem opens a migrated system database.
func newSystem(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	return db
}

// TestRunnerDispatchesByKind checks the other half of the same rule: a job runs
// on the worker registered for its kind, and a kind nothing handles is
// discarded with a message rather than retried until the attempts run out.
func TestRunnerDispatchesByKind(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))
	runner := NewRunner(client)

	ran := map[string]int{}

	runner.Register(QueueImports, KindCSVImport, WorkerFunc(func(_ context.Context, job Job) error {
		ran[job.Kind]++
		return nil
	}))

	if _, err := client.EnqueueOwned(ctx, 0, QueueImports, KindCSVImport, map[string]any{}, ""); err != nil {
		t.Fatal(err)
	}

	unhandled, err := client.EnqueueOwned(ctx, 0, QueueImports, "rollup_rebuild", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		if _, err := runner.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if ran[KindCSVImport] != 1 {
		t.Fatalf("the import worker ran %d times, want 1", ran[KindCSVImport])
	}

	stranger, err := client.Get(ctx, unhandled)
	if err != nil {
		t.Fatal(err)
	}

	if stranger.State != StateDiscarded {
		t.Fatalf("a job with no worker is in state %q, want discarded — retrying it would never succeed here", stranger.State)
	}
}

// TestUniqueKeyStopsDoubleEnqueue covers the constraint that matters most for
// an import: running one twice doubles a customer's numbers, and no later check
// can tell which half was the duplicate.
func TestUniqueKeyStopsDoubleEnqueue(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	if _, err := client.EnqueueOwned(ctx, 0, QueueImports, KindCSVImport, map[string]any{}, "import-4"); err != nil {
		t.Fatal(err)
	}

	if _, err := client.EnqueueOwned(ctx, 0, QueueImports, KindCSVImport, map[string]any{}, "import-4"); err == nil {
		t.Fatal("the same import was enqueued twice")
	}
}

// TestFailBacksOffThenDiscards checks that a transient failure is retried and a
// permanent one is not. A malformed CSV will not parse on the fourth attempt,
// and retrying it only delays telling the customer what is wrong with it.
func TestFailBacksOffThenDiscards(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	at := time.Unix(1_800_000_000, 0)
	client.Now = func() time.Time { return at }

	id, err := client.EnqueueOwned(ctx, 0, QueueImports, KindCSVImport, map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	job.Attempt = 1

	if err := client.Fail(ctx, job, errors.New("the disk was busy")); err != nil {
		t.Fatal(err)
	}

	retried, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if retried.State != StateAvailable {
		t.Fatalf("a first failure left the job in %q, want available", retried.State)
	}

	if retried.ScheduledAt <= at.Unix() {
		t.Fatal("a retry was scheduled with no backoff")
	}

	if err := client.Fail(ctx, job, PermanentError(errors.New("line 4: not a date"))); err != nil {
		t.Fatal(err)
	}

	discarded, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if discarded.State != StateDiscarded {
		t.Fatalf("a permanent failure left the job in %q, want discarded", discarded.State)
	}

	if discarded.LastError != "line 4: not a date" {
		t.Fatalf("the stored reason is %q, want the sentence written for the customer", discarded.LastError)
	}
}

// TestStaleClaimsAreReleased covers a process being killed mid-job. The claim
// lives in the database, so nothing else in the system would ever notice that
// whoever took it is gone, and an import would sit at "running" for ever while
// the customer watched a progress bar that was never going to move.
func TestStaleClaimsAreReleased(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	at := time.Unix(1_800_000_000, 0)
	client.Now = func() time.Time { return at }

	id, err := client.EnqueueOwned(ctx, 0, QueueExports, KindSiteExport, map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := client.Claim(ctx, QueueExports); err != nil {
		t.Fatal(err)
	}

	// A claim younger than the threshold belongs to a process that is probably
	// still working, and stealing it would run the job a second time.
	released, err := client.ReleaseStale(ctx, StaleClaim)
	if err != nil {
		t.Fatal(err)
	}

	if released != 0 {
		t.Fatalf("%d fresh claims were released", released)
	}

	client.Now = func() time.Time { return at.Add(StaleClaim + time.Minute) }

	released, err = client.ReleaseStale(ctx, StaleClaim)
	if err != nil {
		t.Fatal(err)
	}

	if released != 1 {
		t.Fatalf("%d abandoned claims were released, want 1", released)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if job.State != StateAvailable {
		t.Fatalf("an abandoned job is in state %q, want available", job.State)
	}
}

// TestAHeartbeatKeepsALongJobClaimed covers an import that outlives the stale
// window. Two app replicas both run the worker, and without a heartbeat the
// second would decide the first's still-running import was abandoned and start
// it again in parallel, doubling the customer's numbers.
func TestAHeartbeatKeepsALongJobClaimed(t *testing.T) {
	ctx := context.Background()
	db := newSystem(t)
	client := NewClient(db)

	var clock atomic.Int64
	start := time.Unix(1_800_000_000, 0)
	clock.Store(start.Unix())
	client.Now = func() time.Time { return time.Unix(clock.Load(), 0) }

	id, err := client.EnqueueOwned(ctx, 0, QueueImports, KindCSVImport, struct{}{}, "")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(client)
	runner.Heartbeat = 5 * time.Millisecond

	var released int64
	var releaseErr error
	runner.Register(QueueImports, KindCSVImport, WorkerFunc(func(ctx context.Context, job Job) error {
		// The job has now been running for longer than the stale window, and
		// the heartbeat must have moved its claim forward with the clock.
		later := start.Add(StaleClaim + time.Minute)
		clock.Store(later.Unix())

		deadline := time.Now().Add(5 * time.Second)
		for {
			var attemptedAt int64
			if err := db.QueryRowContext(ctx, "SELECT attempted_at FROM jobs WHERE id = ?", job.ID).Scan(&attemptedAt); err != nil {
				return err
			}
			if attemptedAt == later.Unix() {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("no heartbeat landed within five seconds")
			}
			time.Sleep(time.Millisecond)
		}

		released, releaseErr = client.ReleaseStale(ctx, StaleClaim)

		return nil
	}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if released != 0 {
		t.Fatalf("%d running jobs were released as stale while their worker was still going", released)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != StateCompleted {
		t.Fatalf("job state = %q, want completed: %s", job.State, job.LastError)
	}
}

// TestAPanickingWorkerFailsTheJobRatherThanTheProcess covers the deployment
// this product is built for. In the single-process shape the runner shares a
// process with the endpoint that accepts events, so a worker that panics on one
// malformed row would take ingestion down with it — and leave the row claimed,
// so the next boot picks up the same job and panics again.
func TestAPanickingWorkerFailsTheJobRatherThanTheProcess(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	id, err := client.EnqueueOwned(ctx, 0, QueueImports, KindCSVImport, struct{}{}, "")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(client)
	runner.Register(QueueImports, KindCSVImport, WorkerFunc(func(context.Context, Job) error {
		panic("a row the parser did not expect")
	}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if job.State == StateCompleted {
		t.Fatal("a job that panicked was recorded as completed")
	}

	if !strings.Contains(job.LastError, "panicked") {
		t.Fatalf("the row does not say the worker panicked: %q", job.LastError)
	}
}
