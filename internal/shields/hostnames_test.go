//
// hostnames_test.go
// Tests for the durable rejected-hostname settings view.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// TestListRejectedAggregatesRecentDaysInStableOrder proves the settings view
// reads committed account evidence and never exposes older retained rows.
func TestListRejectedAggregatesRecentDaysInStableOrder(t *testing.T) {
	ctx := context.Background()
	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.CloseAll() })
	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	day := now.Unix() / rejectedDaySeconds
	for _, row := range []struct {
		hostname string
		day      int64
		events   int
	}{
		{"preview.example.net", day, 2},
		{"preview.example.net", day - 1, 3},
		{"checkout.example.net", day, 4},
		{"old.example.net", day - 2, 99},
	} {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO hostname_rejections (site_id, hostname, day, events)
			VALUES (1, ?, ?, ?)`, row.hostname, row.day, row.events); err != nil {
			t.Fatal(err)
		}
	}

	view := NewRejections(manager)
	view.Now = func() time.Time { return now }
	got, err := view.ListRejected(ctx, 1, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d rejected hostnames, want 2: %+v", len(got), got)
	}
	if got[0].Hostname != "preview.example.net" || got[0].Events != 5 {
		t.Fatalf("first rejection = %+v, want preview.example.net with 5", got[0])
	}
	if got[1].Hostname != "checkout.example.net" || got[1].Events != 4 {
		t.Fatalf("second rejection = %+v, want checkout.example.net with 4", got[1])
	}
}
