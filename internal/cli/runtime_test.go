//
// runtime_test.go
// Tests for the wiring both long-running commands share.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestJobCountsSplitsTheQueueByState checks the metrics endpoint's view of the
// background queue, against the real schema.
//
// It runs the statement rather than asserting on its text because the point of
// failure is the statement itself: a query that no longer matches the table
// would report an empty queue forever, which reads as a healthy one.
func TestJobCountsSplitsTheQueueByState(t *testing.T) {
	dir := migratedDataDir(t)

	db, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := []struct {
		state string
		count int
	}{
		{"available", 3},
		{"executing", 1},
		{"completed", 5},
	}

	for _, row := range rows {
		for i := 0; i < row.count; i++ {
			if _, err := db.Exec(
				"INSERT INTO jobs (queue, kind, args, state, scheduled_at) VALUES ('default', 'test', '{}', ?, 0)",
				row.state,
			); err != nil {
				t.Fatal(err)
			}
		}
	}

	counts, err := jobCounts(db)(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if counts.Available != 3 {
		t.Errorf("available = %d, want 3", counts.Available)
	}

	// Completed jobs must not be counted: they are history, and a depth that
	// only ever grows is a metric nobody can alert on.
	if counts.Executing != 1 {
		t.Errorf("executing = %d, want 1", counts.Executing)
	}
}
