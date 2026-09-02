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

// TestFailedJobsAreFoundByWorkerType is the regression test for a real
// incident. An incumbent's error reporter decided a crash was a failed import
// because the job sat on the import queue and its arguments contained an
// import id; a different worker sharing that queue crashed, was read as a
// failed import, and the cleanup built on that answer purged fifteen imports
// that had finished perfectly — while the interface went on calling them
// completed for thirteen days.
//
// The fix is that nothing here can ask the question that way. A caller has to
// name the worker type, and this asserts that a foreign job carrying an import
// id in its arguments is not reported as one.
func TestFailedJobsAreFoundByWorkerType(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	// A genuine import that failed.
	importID, err := client.Enqueue(ctx, QueueImports, KindCSVImport, map[string]any{"import_id": 7}, "")
	if err != nil {
		t.Fatal(err)
	}

	// A completely different worker, on the same queue, whose arguments happen
	// to carry an import id. This is the job that was misread.
	strangerID, err := client.Enqueue(ctx, QueueImports, "rollup_rebuild", map[string]any{"import_id": 7, "site_id": 1}, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{importID, strangerID} {
		job, err := client.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		job.Attempt = job.MaxAttempts

		if err := client.Fail(ctx, job, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
	}

	failed, err := client.FailedOfKind(ctx, KindCSVImport)
	if err != nil {
		t.Fatal(err)
	}

	if len(failed) != 1 {
		t.Fatalf("%d failed imports reported, want exactly the one that was an import", len(failed))
	}

	if failed[0].ID != importID {
		t.Fatalf("the failed import reported was job %d, want %d — a foreign worker on the same queue was misread as an import",
			failed[0].ID, importID)
	}

	if failed[0].LastError == "" {
		t.Fatal("a discarded job carries no reason, which is the failure this queue exists to make visible")
	}
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

	if _, err := client.Enqueue(ctx, QueueImports, KindCSVImport, map[string]any{}, ""); err != nil {
		t.Fatal(err)
	}

	unhandled, err := client.Enqueue(ctx, QueueImports, "rollup_rebuild", map[string]any{}, "")
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

	if _, err := client.Enqueue(ctx, QueueImports, KindCSVImport, map[string]any{}, "import-4"); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Enqueue(ctx, QueueImports, KindCSVImport, map[string]any{}, "import-4"); err == nil {
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

	id, err := client.Enqueue(ctx, QueueImports, KindCSVImport, map[string]any{}, "")
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

	id, err := client.Enqueue(ctx, QueueExports, KindSiteExport, map[string]any{}, "")
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

// TestEnqueueRefusesADuplicateAndEnqueueUniqueDoesNot pins the difference
// between the two, which is the difference between work a person asked for and
// a tick every replica tries to create.
//
// An import enqueued twice doubles a customer's numbers, so Enqueue has to say
// no out loud. An hourly tick is expected to lose that race on every replica
// but one, and reporting each loss as an error would put a failure in the log
// every minute on every box.
func TestEnqueueRefusesADuplicateAndEnqueueUniqueDoesNot(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	if _, err := client.Enqueue(ctx, QueueImports, KindCSVImport, struct{}{}, "import:7"); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Enqueue(ctx, QueueImports, KindCSVImport, struct{}{}, "import:7"); err == nil {
		t.Fatal("a duplicate import was accepted silently")
	}

	if _, created, err := client.EnqueueUnique(ctx, "notifications", "reports.schedule", struct{}{}, "cron:1"); err != nil || !created {
		t.Fatalf("the first tick was created=%v err=%v", created, err)
	}

	_, created, err := client.EnqueueUnique(ctx, "notifications", "reports.schedule", struct{}{}, "cron:1")
	if err != nil {
		t.Fatalf("a losing tick was reported as an error: %v", err)
	}

	if created {
		t.Fatal("the same tick was created twice in one period")
	}
}

// TestEnqueueUniqueNeedsAKey refuses the call that would otherwise enqueue an
// unbounded number of identical rows, one per look.
func TestEnqueueUniqueNeedsAKey(t *testing.T) {
	client := NewClient(newSystem(t))

	if _, _, err := client.EnqueueUnique(context.Background(), "notifications", "reports.schedule", struct{}{}, ""); err == nil {
		t.Fatal("a unique enqueue with no key was accepted")
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

	id, err := client.Enqueue(ctx, QueueImports, KindCSVImport, struct{}{}, "")
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
