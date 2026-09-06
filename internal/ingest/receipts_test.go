//
// receipts_test.go
// What the prune removes, what it must never remove, and what it costs.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
)

// prunerOver builds a pruner and an open account, on a fixed clock.
func prunerOver(t *testing.T, now time.Time, owners ...int64) (*ReceiptPruner, *accounts.Manager) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { checkClose(t, "pruner account manager", manager.CloseAll) })

	for _, owner := range owners {
		if _, err := manager.Open(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
	}

	return &ReceiptPruner{
		Accounts: manager,
		Now:      func() time.Time { return now },
		Owners:   func(context.Context) ([]int64, error) { return owners, nil },
	}, manager
}

// receipt writes one row straight into the dedupe table at a chosen age.
func receipt(t *testing.T, manager *accounts.Manager, owner int64, at time.Time) uuid.UUID {
	t.Helper()

	account, err := manager.Open(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	id := uuid.New()

	if _, err := account.Writer().ExecContext(context.Background(),
		"INSERT INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)", id[:], at.Unix()); err != nil {
		t.Fatal(err)
	}

	return id
}

// receipts counts what one account still holds.
func receipts(t *testing.T, manager *accounts.Manager, owner int64) int {
	t.Helper()

	account, err := manager.Open(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}

	var count int
	if err := account.Reader().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM recent_event_ids").Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count
}

// TestAReplayFromInsideTheWindowIsStillADuplicate is the property the whole
// table exists for, driven through the writer rather than asserted on a row.
//
// The tracker gives up on a stashed event after seven days, so anything younger
// than that can still arrive twice and the second one must be recognised.
func TestAReplayFromInsideTheWindowIsStillADuplicate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1)

	// The writer stamps the receipt with its own clock, so both run on the same
	// one: a receipt dated by wall time against a cutoff dated by a fixture is
	// a test that passes or fails by the hour it is run at.
	accepted := now.Add(-8 * 24 * time.Hour)
	writer := NewWriter(manager)
	writer.Now = func() time.Time { return accepted }

	// Eight days ago: a day past the tracker's own limit, and well inside the
	// server's.
	first := writerEvent(1, EventPageview, accepted.Unix(), "/replayed")
	if _, err := writer.Write(ctx, []Event{first}); err != nil {
		t.Fatal(err)
	}

	if _, err := pruner.Run(ctx, jobs.Job{}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// The same event again, which is what a browser replaying its outbox sends.
	if _, err := writer.Write(ctx, []Event{first}); err != nil {
		t.Fatal(err)
	}

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	var pageviews int
	if err := account.Reader().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM events WHERE site_id = 1").Scan(&pageviews); err != nil {
		t.Fatal(err)
	}

	if pageviews != 1 {
		t.Fatalf("the replay wrote %d events, want 1 — its receipt was pruned inside the window", pageviews)
	}
}

// TestNothingIsPrunedBeforeTheUndatableEntriesExpire is the one path to a
// doubled pageview.
//
// An entry stashed by a tracker cached before the stamp existed has no date, so
// it is dated from the day the stamp shipped and stays replayable for a week
// after that — however old it really is, and however old its receipt is.
func TestNothingIsPrunedBeforeTheUndatableEntriesExpire(t *testing.T) {
	before := PruneNotBefore.Add(-time.Hour)

	pruner, manager := prunerOver(t, before, 1)
	receipt(t, manager, 1, before.Add(-365*24*time.Hour))

	outcome, err := pruner.Run(context.Background(), jobs.Job{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if outcome.Handled != 0 {
		t.Errorf("the prune removed %d receipts before undatable entries expired", outcome.Handled)
	}

	if got := receipts(t, manager, 1); got != 1 {
		t.Errorf("%d receipts remain, want the year-old one still there", got)
	}

	if err := outcome.Validate(); err != nil {
		t.Errorf("the prune waited and did not say so: %v", err)
	}
}

// TestReceiptsOlderThanTheWindowAreRemoved is the other half: the table has to
// actually stop growing, or the index this job needs is the only thing that
// changed.
func TestReceiptsOlderThanTheWindowAreRemoved(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1)

	receipt(t, manager, 1, now.Add(-31*24*time.Hour))
	receipt(t, manager, 1, now.Add(-90*24*time.Hour))
	receipt(t, manager, 1, now.Add(-time.Hour))

	outcome, err := pruner.Run(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if outcome.Handled != 2 {
		t.Errorf("the prune reported %d removed, want 2", outcome.Handled)
	}

	if got := receipts(t, manager, 1); got != 1 {
		t.Errorf("%d receipts remain, want the one inside the window", got)
	}
}

// TestAQuietPruneSaysWhyItDidNothing is the rule the whole job package enforces:
// a run that reports success while doing nothing is indistinguishable from one
// that is working, which is how a prune that stopped running goes unnoticed
// until the table is large.
func TestAQuietPruneSaysWhyItDidNothing(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1)
	receipt(t, manager, 1, now.Add(-time.Hour))

	outcome, err := pruner.Run(context.Background(), jobs.Job{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if err := outcome.Validate(); err != nil {
		t.Errorf("the prune reported nothing and said nothing: %v", err)
	}
}

// TestOneAccountFailingDoesNotStopTheRest keeps a single unreadable database
// from leaving every other account's table growing.
func TestOneAccountFailingDoesNotStopTheRest(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1, 2)
	receipt(t, manager, 2, now.Add(-31*24*time.Hour))

	if err := manager.Block(1); err != nil {
		t.Fatal(err)
	}

	outcome, err := pruner.Run(context.Background(), jobs.Job{})
	if err == nil {
		t.Fatal("a failed account was reported as a clean run")
	}

	if outcome.Handled != 1 {
		t.Errorf("the reachable account removed %d receipts, want 1", outcome.Handled)
	}
}

// TestThePruneClearsABacklogOverSeveralBatches covers the size the job exists
// for. One transaction over a large table is the write lock held long enough to
// stop ingestion, which is the thing being fixed rather than traded for.
func TestThePruneClearsABacklogOverSeveralBatches(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1)

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := account.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	aged := now.Add(-31 * 24 * time.Hour).Unix()

	for range PruneBatch + 100 {
		id := uuid.New()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)", id[:], aged); err != nil {
			t.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	outcome, err := pruner.Run(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if outcome.Handled != PruneBatch+100 {
		t.Errorf("the prune removed %d, want %d", outcome.Handled, PruneBatch+100)
	}

	if got := receipts(t, manager, 1); got != 0 {
		t.Errorf("%d receipts remain after the backlog cleared", got)
	}
}

// TestThePruneReadsAnIndexRatherThanTheTable is why 0015 exists. Without the
// index every pass is a full scan of the one table that grows without limit,
// which is the cost this job was added to avoid.
func TestThePruneReadsAnIndexRatherThanTheTable(t *testing.T) {
	ctx := context.Background()

	// The whole detail line, not just the index name: the index leads with
	// received_at, so a plan that used it and then read all of it would still
	// name it, and reading all of it is the scan this test exists to forbid.
	const want = "SEARCH recent_event_ids USING COVERING INDEX recent_event_ids_received (received_at<?)"

	plan := queryPlan(t, ctx, planDatabase(t),
		"SELECT event_uuid FROM recent_event_ids WHERE received_at < ? LIMIT ?")

	if !strings.Contains(plan, want) {
		t.Fatalf("the prune scans the whole table:\n%s\nwant it to contain\n%s", plan, want)
	}
}

// TestTheReceiptTableStaysShallowWhenItIsPruned is the structural half of the
// issue's cost argument.
//
// The table is WITHOUT ROWID over a random uuid, so it is its own index and its
// depth is what a random insert walks. A wall-clock comparison cannot show this
// in a test: SQLite holds the whole tree in its page cache at any size a test
// will build, so the random disk read the issue is about never happens. The
// depth does show it, and the depth is what the prune keeps down.
func TestTheReceiptTableStaysShallowWhenItIsPruned(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1)

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Well past the window, which is what an unpruned table accumulates.
	aged := now.Add(-90 * 24 * time.Hour).Unix()

	tx, err := account.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	for range 50_000 {
		id := uuid.New()
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)", id[:], aged); err != nil {
			t.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	grown := pages(t, account, "recent_event_ids")

	if _, err := pruner.Run(ctx, jobs.Job{}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := account.Writer().ExecContext(ctx, "VACUUM"); err != nil {
		t.Fatal(err)
	}

	if after := pages(t, account, "recent_event_ids"); after >= grown/10 {
		t.Errorf("the table holds %d pages after the prune against %d before, so it is not shrinking", after, grown)
	}
}

// pages counts the b-tree pages one table occupies.
func pages(t *testing.T, account *accounts.Account, table string) int {
	t.Helper()

	var count int
	if err := account.Reader().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM dbstat WHERE name = ?", table).Scan(&count); err != nil {
		t.Skipf("dbstat is not available in this build: %v", err)
	}

	return count
}
