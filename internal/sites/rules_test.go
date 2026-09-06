//
// rules_test.go
// That a rule change is recorded where a refresh can see it.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sites

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newRuleCache is a cache over a migrated control database with two teams.
func newRuleCache(t *testing.T) *Cache {
	t.Helper()

	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(ctx, db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{1, 2} {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Team', 0, 0)", id); err != nil {
			t.Fatal(err)
		}
	}

	return New(db)
}

// TestAStampIsReadBackAndMovesOnEveryChange is what the incremental refresh
// compares against.
func TestAStampIsReadBackAndMovesOnEveryChange(t *testing.T) {
	cache := newRuleCache(t)
	ctx := context.Background()

	versions, err := cache.RuleVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 0 {
		t.Fatalf("a fresh install has %d markers", len(versions))
	}

	at := time.Unix(1_800_000_000, 0)

	if err := cache.StampRules(ctx, 1, at); err != nil {
		t.Fatal(err)
	}

	versions, err = cache.RuleVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 1 || versions[1] != at.UTC().UnixNano() {
		t.Fatalf("the marker read back as %v", versions)
	}

	// Nanoseconds: two edits inside one second are two edits, and a comparison
	// at second resolution would miss the later one for an hour.
	later := at.Add(time.Millisecond)

	if err := cache.StampRules(ctx, 1, later); err != nil {
		t.Fatal(err)
	}

	versions, err = cache.RuleVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if versions[1] != later.UTC().UnixNano() {
		t.Errorf("a second edit in the same second did not move the marker: %v", versions)
	}

	// One account's edit says nothing about another's.
	if _, stamped := versions[2]; stamped {
		t.Error("stamping one account stamped another")
	}
}

// TestACacheWithNoControlDatabaseRefusesRatherThanAnsweringEmpty keeps a
// misconfigured cache from reading as "nothing has ever changed", which is what
// a healthy install also looks like and would silently drop every rule change
// to the hourly pass.
func TestACacheWithNoControlDatabaseRefusesRatherThanAnsweringEmpty(t *testing.T) {
	if _, err := NewEmpty().RuleVersions(context.Background()); err == nil {
		t.Error("a cache with no control database answered as though nothing had changed")
	}

	var none *Cache

	if _, err := none.RuleVersions(context.Background()); err == nil {
		t.Error("a nil cache answered as though nothing had changed")
	}

	// Stamping without one is not an error: a self-hoster running one process
	// has nobody to tell, and its own snapshot is already up to date.
	if err := none.StampRules(context.Background(), 1, time.Now()); err != nil {
		t.Errorf("a nil cache refused to stamp: %v", err)
	}
}

// TestADeletedAccountTakesItsMarkerWithIt keeps a row keyed by a team that no
// longer exists from outliving the customer.
func TestADeletedAccountTakesItsMarkerWithIt(t *testing.T) {
	cache := newRuleCache(t)
	ctx := context.Background()

	if err := cache.StampRules(ctx, 1, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.db.ExecContext(ctx, "DELETE FROM teams WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	versions, err := cache.RuleVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, left := versions[1]; left {
		t.Error("the deleted account's marker is still there")
	}
}
