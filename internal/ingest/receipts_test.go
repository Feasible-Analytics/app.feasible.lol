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
// table exists for. The tracker gives up on a stashed event after seven days,
// so anything younger than that can still arrive and must still be recognised.
func TestAReplayFromInsideTheWindowIsStillADuplicate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	pruner, manager := prunerOver(t, now, 1)

	// A day past the tracker's own limit, and well inside the server's.
	kept := receipt(t, manager, 1, now.Add(-8*24*time.Hour))

	if _, err := pruner.Run(ctx, jobs.Job{}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	var found int
	if err := account.Reader().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM recent_event_ids WHERE event_uuid = ?", kept[:]).Scan(&found); err != nil {
		t.Fatal(err)
	}

	if found != 1 {
		t.Fatal("a receipt inside the retention window was pruned, so its replay would be counted twice")
	}
}

// TestReceiptsOlderThanTheWindowAreRemoved is the other half: the table has to
// actually stop growing, or the index this job needs is the only thing that
// changed.
func TestReceiptsOlderThanTheWindowAreRemoved(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

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
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

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
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

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
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

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
	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { checkClose(t, "query plan account manager", manager.CloseAll) })

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := account.Reader().QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT event_uuid FROM recent_event_ids WHERE received_at < ? LIMIT ?`, 0, PruneBatch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	plan := ""

	for rows.Next() {
		var id, parent, unused int
		var detail string

		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}

		plan += detail + "\n"
	}

	if !strings.Contains(plan, "recent_event_ids_received") {
		t.Fatalf("the prune scans the whole table:\n%s", plan)
	}
}

// TestTheInsertPathDoesNotDegradeWithTableSize is the measurement behind the
// issue. The table is WITHOUT ROWID over a random uuid, so every insert lands
// somewhere random in a tree that only grows; a table the prune keeps bounded
// costs what a small one costs.
func TestTheInsertPathDoesNotDegradeWithTableSize(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}

	ctx := context.Background()
	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { checkClose(t, "insert timing account manager", manager.CloseAll) })

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	fill := func(rows int) {
		tx, err := account.Writer().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		for range rows {
			id := uuid.New()
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)", id[:], 0); err != nil {
				t.Fatal(err)
			}
		}

		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	// One insert at a time, which is what the write path does inside its fact
	// transaction.
	measure := func() time.Duration {
		began := time.Now()

		for range 200 {
			id := uuid.New()
			if _, err := account.Writer().ExecContext(ctx,
				"INSERT INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)", id[:], 0); err != nil {
				t.Fatal(err)
			}
		}

		return time.Since(began)
	}

	fill(1000)
	small := measure()

	fill(80_000)
	large := measure()

	// A generous bound: this is guarding against an order of magnitude, not
	// against the noise of a laptop. Without the prune the table grows without
	// limit and this ratio is what climbs.
	if large > small*8 {
		t.Errorf("inserting into a large table took %v against %v for a small one", large, small)
	}
}
