//
// legacy_test.go
// Carrying the retired system-database hostname list onto shield rules.
//
// Created: 2026-11-18
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newLegacySystem builds a migrated system database with one site,
// example.com, owned by account 1.
func newLegacySystem(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	exec(t, db, "INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', 0, 0)")
	exec(t, db, "INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (7, 1, 'example.com', 0, 0)")

	return db
}

// allowLegacy inserts one row into the retired table for site 7.
func allowLegacy(t *testing.T, db *sql.DB, hostname string, createdAt int64) {
	t.Helper()

	exec(t, db, fmt.Sprintf(
		"INSERT INTO site_allowed_hostnames (site_id, hostname, created_at) VALUES (7, '%s', %d)", hostname, createdAt))
}

// TestReadLegacyHostnamesJoinsEachRowToItsSite checks the carry knows where
// each row has to go: the site's storage account and its registered domain.
func TestReadLegacyHostnamesJoinsEachRowToItsSite(t *testing.T) {
	system := newLegacySystem(t)
	allowLegacy(t, system, "staging.other.example", 1_790_000_000)

	rows, err := ReadLegacyHostnames(context.Background(), system)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("read %d rows, want 1", len(rows))
	}

	row := rows[0]
	if row.SiteID != 7 || row.AccountID != 1 || row.Domain != "example.com" ||
		row.Hostname != "staging.other.example" || row.CreatedAt != 1_790_000_000 {
		t.Fatalf("read %+v", row)
	}
}

// TestReadLegacyHostnamesWithoutTheTable is an install that never had the
// table, or has dropped it. There is nothing to carry and nothing is wrong.
func TestReadLegacyHostnamesWithoutTheTable(t *testing.T) {
	system := newLegacySystem(t)
	exec(t, system, "DROP TABLE site_allowed_hostnames")

	rows, err := ReadLegacyHostnames(context.Background(), system)
	if err != nil || rows != nil {
		t.Fatalf("read %v, %v; want nothing and no error", rows, err)
	}
}

// TestCarryLegacyHostnamesKeepsEveryChoice checks what lands in shield_rules:
// every hostname somebody allowed, normalised, with its original date, and
// nothing for the site's own domain or subdomains, which count without a rule.
func TestCarryLegacyHostnamesKeepsEveryChoice(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)

	rows := []LegacyHostname{
		{ID: 1, SiteID: 7, AccountID: 1, Domain: "example.com", Hostname: "example.com", CreatedAt: 100},
		{ID: 2, SiteID: 7, AccountID: 1, Domain: "example.com", Hostname: "Staging.Other.example", CreatedAt: 100},
		{ID: 3, SiteID: 7, AccountID: 1, Domain: "example.com", Hostname: "blog.example.com", CreatedAt: 200},
		{ID: 4, SiteID: 7, AccountID: 1, Domain: "example.com", Hostname: "checkout.example.net", CreatedAt: 300},
	}

	added, err := CarryLegacyHostnames(ctx, account.Writer(), rows, "Allowed from the health panel")
	if err != nil {
		t.Fatal(err)
	}

	if added != 2 {
		t.Fatalf("added %d rules, want 2", added)
	}

	rules, err := List(ctx, account.Reader(), 7)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]int64{"staging.other.example": 100, "checkout.example.net": 300}
	if len(rules) != len(want) {
		t.Fatalf("stored %+v", rules)
	}

	for _, rule := range rules {
		if rule.Kind != KindHostname || want[rule.Value] != rule.CreatedAt || rule.Note != "Allowed from the health panel" {
			t.Errorf("stored %+v", rule)
		}
	}

	// A second run, after an interruption, adds nothing.
	again, err := CarryLegacyHostnames(ctx, account.Writer(), rows, "Allowed from the health panel")
	if err != nil {
		t.Fatal(err)
	}

	if again != 0 {
		t.Fatalf("the second run added %d rules", again)
	}
}

// TestCarryLegacyHostnamesIgnoresTheCap checks a site past the thirty-rule cap
// still gets every hostname it allowed. The cap did not exist where those
// choices were made, and dropping one would block traffic somebody asked for.
func TestCarryLegacyHostnamesIgnoresTheCap(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	for i := 0; i < MaxRulesPerKind; i++ {
		if _, err := Add(ctx, account.Writer(), 7, KindHostname, fmt.Sprintf("host-%d.example.net", i), "", now); err != nil {
			t.Fatal(err)
		}
	}

	added, err := CarryLegacyHostnames(ctx, account.Writer(), []LegacyHostname{
		{ID: 1, SiteID: 7, AccountID: 1, Domain: "example.com", Hostname: "staging.other.example", CreatedAt: 100},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	if added != 1 {
		t.Fatalf("added %d rules past the cap, want 1", added)
	}
}

// TestForgetLegacyHostnamesDeletesOnlyTheNamedRows checks a row whose account
// could not be reached is still there for the next run.
func TestForgetLegacyHostnamesDeletesOnlyTheNamedRows(t *testing.T) {
	ctx := context.Background()
	system := newLegacySystem(t)
	allowLegacy(t, system, "staging.other.example", 100)
	allowLegacy(t, system, "checkout.example.net", 100)

	rows, err := ReadLegacyHostnames(ctx, system)
	if err != nil {
		t.Fatal(err)
	}

	if err := ForgetLegacyHostnames(ctx, system, rows[:1]); err != nil {
		t.Fatal(err)
	}

	left, err := ReadLegacyHostnames(ctx, system)
	if err != nil {
		t.Fatal(err)
	}

	if len(left) != 1 || left[0].Hostname != "checkout.example.net" {
		t.Fatalf("left %+v, want only the row that was not named", left)
	}
}
