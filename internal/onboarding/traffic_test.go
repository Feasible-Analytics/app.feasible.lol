//
// traffic_test.go
// The has-data check against a real account database.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// TestHasEventsCountsOnlyLiveSessions proves an empty account and one holding
// only imported history both read as having no data, and one real session
// flips the answer.
func TestHasEventsCountsOnlyLiveSessions(t *testing.T) {
	manager := accounts.NewManager(t.TempDir())
	defer manager.CloseAll() //nolint:errcheck // the test has no useful cleanup recovery

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	traffic := AccountTraffic{Accounts: manager}

	check := func(want bool, when string) {
		t.Helper()

		got, err := traffic.HasEvents(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("HasEvents %s = %v, want %v", when, got, want)
		}
	}

	check(false, "on an empty account")

	if _, err := account.Writer().Exec(`
		INSERT INTO sessions (site_id, user_id, started_at, last_seen_at, is_imported) VALUES (1, 1, 100, 100, 1)
	`); err != nil {
		t.Fatal(err)
	}

	check(false, "with only imported history")

	if _, err := account.Writer().Exec(`
		INSERT INTO sessions (site_id, user_id, started_at, last_seen_at) VALUES (1, 2, 200, 200)
	`); err != nil {
		t.Fatal(err)
	}

	check(true, "after a real session")
}
