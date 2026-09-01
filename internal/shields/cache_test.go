//
// cache_test.go
// The snapshot in place: an event that a rule blocks never reaches disk.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"database/sql"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newRouting builds a control database holding one team and one site, which is
// what the shield cache walks to find the accounts it has to read.
func newRouting(t *testing.T) *sites.Cache {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()

	if _, err := migrate.Run(ctx, db, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	exec(t, db, "INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', 0, 0)")
	exec(t, db, "INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', 0, 0)")

	cache := sites.New(db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	return cache
}

// exec runs one statement or fails the test.
func exec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

// TestShardShieldStopsAnEventReachingDisk is the end-to-end half of the
// feature. A rule that is stored, compiled and consulted but does not actually
// stop a row being written is a setting that lies to the customer, so this
// drives the real writer rather than the ruleset.
func TestShardShieldStopsAnEventReachingDisk(t *testing.T) {
	ctx := context.Background()

	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_800_000_000, 0)

	if _, err := Add(ctx, account.Writer(), 1, KindPage, "/admin*", "staging", now); err != nil {
		t.Fatal(err)
	}

	cache := New(newRouting(t), manager)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	counters := ingest.NewCounters()

	writer := ingest.NewWriter(manager, ingest.NewSessionCache())
	writer.Now = func() time.Time { return now }
	writer.Shield = cache
	writer.Counters = counters

	if _, err := writer.Write(ctx, []ingest.Event{
		shieldEvent("/admin/users", now),
		shieldEvent("/pricing", now),
	}); err != nil {
		t.Fatal(err)
	}

	var written int64
	if err := account.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&written); err != nil {
		t.Fatal(err)
	}

	if written != 1 {
		t.Fatalf("%d events were written, want only the one no rule blocked", written)
	}

	// A blocked event has to be visible as a drop. "My numbers went down after
	// I added a rule" is a question the customer has to be able to answer, and
	// a silent drop is the one thing this product refuses to do.
	var dropped bool
	for _, count := range counters.Snapshot().Dropped {
		if count.Reason == ingest.ReasonShieldPage && count.Count > 0 {
			dropped = true
		}
	}

	if !dropped {
		t.Fatal("a shielded event was thrown away without being counted")
	}
}

// TestIPShieldIsAnsweredFromTheSnapshot covers the other evaluator: the ingest
// tier's, which runs where the raw address still exists.
func TestIPShieldIsAnsweredFromTheSnapshot(t *testing.T) {
	ctx := context.Background()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Add(ctx, account.Writer(), 1, KindIP, "203.0.113.14", "the office", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}

	cache := New(newRouting(t), manager)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if !cache.Blocked(1, netip.MustParseAddr("203.0.113.14")) {
		t.Error("a blocked address was allowed")
	}

	if cache.Blocked(1, netip.MustParseAddr("203.0.113.15")) {
		t.Error("an address that is not on the list was blocked")
	}

	// A site with no rules costs nothing and blocks nothing.
	if cache.Blocked(99, netip.MustParseAddr("203.0.113.14")) {
		t.Error("another site's rule was applied")
	}
}

// shieldEvent builds an event on the shielded site.
func shieldEvent(path string, now time.Time) ingest.Event {
	return ingest.Event{
		UUID:      uuid.New(),
		AccountID: 1,
		SiteID:    1,
		Timestamp: now.Unix(),
		DerivedAt: now.UnixNano(),
		Name:      ingest.EventPageview,
		UserID:    4242,
		Hostname:  "example.com",
		Pathname:  path,
		Country:   "US",
	}
}
