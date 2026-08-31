//
// traffic_test.go
// The sparkline, the onboarding poll, and resetting one site's statistics.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// newTestTraffic builds a traffic reader over an account manager rooted in a
// temporary directory.
func newTestTraffic(t *testing.T) (*Traffic, *accounts.Manager) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())

	t.Cleanup(func() { manager.CloseAll() })

	return NewTraffic(manager), manager
}

// insertSession writes one visit into an account database, which is enough for
// everything on the sites list and the onboarding poll.
func insertSession(t *testing.T, manager *accounts.Manager, accountID, siteID, startedAt int64) {
	t.Helper()

	account, err := manager.Open(context.Background(), accountID)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}

	_, err = account.Writer().Exec(`
		INSERT INTO sessions (site_id, user_id, started_at, last_seen_at) VALUES (?, ?, ?, ?)
	`, siteID, 1, startedAt, startedAt)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// TestSparklineBucketsBySiteDay checks the chart is bucketed in the site's own
// timezone. A sparkline whose days do not line up with the dashboard's days is
// a chart that quietly disagrees with the page it links to.
func TestSparklineBucketsBySiteDay(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	site := &Site{ID: 7, AccountID: 1, Domain: "example.com", Timezone: "Etc/UTC"}

	insertSession(t, manager, 1, site.ID, now.Unix())
	insertSession(t, manager, 1, site.ID, now.Unix())
	insertSession(t, manager, 1, site.ID, now.AddDate(0, 0, -1).Unix())

	// A visit for another site in the same account must not leak into this
	// site's chart — one account database holds every site a team owns.
	insertSession(t, manager, 1, 99, now.Unix())

	if err := traffic.Sparklines(ctx, 1, []*Site{site}, now); err != nil {
		t.Fatalf("sparklines: %v", err)
	}

	if len(site.Sparkline) != SparklineDays {
		t.Fatalf("want %d days, got %d", SparklineDays, len(site.Sparkline))
	}

	if site.Visitors != 3 {
		t.Errorf("want 3 visits, got %d", site.Visitors)
	}

	// The series is oldest first, so today is the last entry.
	if last := site.Sparkline[len(site.Sparkline)-1]; last != 2 {
		t.Errorf("today should hold 2 visits, got %d", last)
	}

	if yesterday := site.Sparkline[len(site.Sparkline)-2]; yesterday != 1 {
		t.Errorf("yesterday should hold 1 visit, got %d", yesterday)
	}
}

// TestFirstEventAtIsTheOnboardingPoll checks the query the waiting screen runs:
// zero until traffic arrives, then the moment it did.
func TestFirstEventAtIsTheOnboardingPoll(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	at, err := traffic.FirstEventAt(ctx, 1, 7)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}

	if at != 0 {
		t.Errorf("a site with no traffic should report zero, got %d", at)
	}

	when := time.Now().Add(-time.Hour).Unix()
	insertSession(t, manager, 1, 7, when)

	at, err = traffic.FirstEventAt(ctx, 1, 7)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}

	if at != when {
		t.Errorf("want %d, got %d", when, at)
	}
}

// TestResetStatsOnlyTouchesOneSite is the important guard. An account database
// holds every site a team owns, and a reset scoped by anything but the site id
// would take the customer's other sites with it — with no undo.
func TestResetStatsOnlyTouchesOneSite(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	now := time.Now()

	insertSession(t, manager, 1, 7, now.Unix())
	insertSession(t, manager, 1, 8, now.Unix())

	if err := traffic.ResetStats(ctx, 1, 7); err != nil {
		t.Fatalf("reset stats: %v", err)
	}

	gone, err := traffic.FirstEventAt(ctx, 1, 7)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}

	if gone != 0 {
		t.Error("the reset site should have no traffic left")
	}

	kept, err := traffic.FirstEventAt(ctx, 1, 8)
	if err != nil {
		t.Fatalf("first event: %v", err)
	}

	if kept == 0 {
		t.Error("the other site in the same account must be untouched")
	}
}
