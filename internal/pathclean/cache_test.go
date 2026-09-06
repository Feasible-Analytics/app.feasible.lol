//
// cache_test.go
// That a quiet fifteen seconds opens nothing.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package pathclean

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// countingOpener wraps a real manager and counts the account databases a
// refresh reached for, which is the number this refresh schedule exists to keep
// at zero when nothing changed.
type countingOpener struct {
	inner   *accounts.Manager
	opens   int
	refuses map[int64]bool

	// before runs on every open, which is how a test lands a save in the window
	// between a refresh reading the snapshot and publishing one.
	before func()
}

// Acquire counts the call and hands back a real lease unless the test wants
// this id to fail.
func (c *countingOpener) Acquire(ctx context.Context, id int64) (*accounts.Lease, error) {
	c.opens++

	if c.before != nil {
		c.before()
	}

	if c.refuses[id] {
		return nil, errors.New("this account is busy")
	}

	return c.inner.Acquire(ctx, id)
}

// cacheFixture is a control database with two accounts, one site each, and a
// path rule on both.
type cacheFixture struct {
	sites  *sites.Cache
	opener *countingOpener
	cache  *Cache
	now    time.Time
}

// newCacheFixture builds and seeds it.
func newCacheFixture(t *testing.T) *cacheFixture {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(ctx, control, migrate.System()); err != nil {
		t.Fatal(err)
	}

	for _, statement := range []string{
		"INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'One', 0, 0)",
		"INSERT INTO teams (id, name, created_at, updated_at) VALUES (2, 'Two', 0, 0)",
		"INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'one.example', 0, 0)",
		"INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (2, 2, 'two.example', 0, 0)",
	} {
		if _, err := control.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	routing := sites.New(control)
	if err := routing.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	now := time.Unix(1_800_000_000, 0)

	for _, id := range []int64{1, 2} {
		account, err := manager.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := Replace(ctx, account.Writer(), id, []Rule{
			{Position: 1, Pattern: "^/p/(\\d+)$", Replacement: "/p/:id", Label: "products", Enabled: true},
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	opener := &countingOpener{inner: manager, refuses: map[int64]bool{}}

	return &cacheFixture{sites: routing, opener: opener, cache: New(routing, opener), now: now}
}

// TestAQuietPassOpensNothing is the acceptance criterion: fifteen seconds with
// no rule edits must cost one query against system.db and no account opens.
func TestAQuietPassOpensNothing(t *testing.T) {
	f := newCacheFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	first := f.opener.opens
	if first != 2 {
		t.Fatalf("the first pass opened %d accounts, want both", first)
	}

	for range 10 {
		if err := f.cache.RefreshChanged(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if f.opener.opens != first {
		t.Errorf("ten quiet passes opened %d accounts, want none", f.opener.opens-first)
	}

	if got := f.cache.Clean(1, "/p/42"); got != "/p/:id" {
		t.Errorf("the rule stopped applying after the quiet passes: %q", got)
	}
}

// TestASavedRuleIsLiveWithinOneInterval keeps the behaviour the refresh exists
// for.
func TestASavedRuleIsLiveWithinOneInterval(t *testing.T) {
	f := newCacheFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	account, err := f.opener.inner.Open(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := Replace(ctx, account.Writer(), 2, []Rule{
		{Position: 1, Pattern: "^/u/(\\d+)$", Replacement: "/u/:id", Label: "users", Enabled: true},
	}, f.now); err != nil {
		t.Fatal(err)
	}

	if err := f.sites.StampRules(ctx, 2, f.now); err != nil {
		t.Fatal(err)
	}

	before := f.opener.opens

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if got := f.cache.Clean(2, "/u/7"); got != "/u/:id" {
		t.Errorf("the saved rule is not live after one pass: %q", got)
	}

	if opened := f.opener.opens - before; opened != 1 {
		t.Errorf("the pass opened %d accounts, want only the one that changed", opened)
	}

	if got := f.cache.Clean(1, "/p/42"); got != "/p/:id" {
		t.Errorf("the other account lost its rule: %q", got)
	}
}

// TestOneUnreadableAccountDoesNotStopTheOthers is the blast-radius fix.
func TestOneUnreadableAccountDoesNotStopTheOthers(t *testing.T) {
	f := newCacheFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{1, 2} {
		account, err := f.opener.inner.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := Replace(ctx, account.Writer(), id, []Rule{
			{Position: 1, Pattern: "^/new/(\\d+)$", Replacement: "/new/:id", Label: "new", Enabled: true},
		}, f.now); err != nil {
			t.Fatal(err)
		}

		if err := f.sites.StampRules(ctx, id, f.now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	f.opener.refuses[1] = true

	if err := f.cache.RefreshChanged(ctx); err == nil {
		t.Fatal("a refusal was not reported")
	}

	if got := f.cache.Clean(2, "/new/7"); got != "/new/:id" {
		t.Errorf("the readable account did not update: %q", got)
	}

	if got := f.cache.Clean(1, "/p/42"); got != "/p/:id" {
		t.Errorf("the unreadable account lost the rule it had: %q", got)
	}

	f.opener.refuses[1] = false
	before := f.opener.opens

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if opened := f.opener.opens - before; opened != 1 {
		t.Errorf("the retry opened %d accounts, want the one that failed", opened)
	}

	if got := f.cache.Clean(1, "/new/7"); got != "/new/:id" {
		t.Errorf("the retried account did not update: %q", got)
	}
}

// TestTheFullPassRebuildsWhateverTheMarkerSays is the backstop against a rule
// saved by something that did not stamp one.
func TestTheFullPassRebuildsWhateverTheMarkerSays(t *testing.T) {
	f := newCacheFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	account, err := f.opener.inner.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if err := Replace(ctx, account.Writer(), 1, []Rule{
		{Position: 1, Pattern: "^/x/(\\d+)$", Replacement: "/x/:id", Label: "x", Enabled: true},
	}, f.now); err != nil {
		t.Fatal(err)
	}

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if got := f.cache.Clean(1, "/x/7"); got != "/x/7" {
		t.Fatalf("the incremental pass saw an unstamped change, so this test proves nothing: %q", got)
	}

	if err := f.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if got := f.cache.Clean(1, "/x/7"); got != "/x/:id" {
		t.Errorf("the full pass did not pick up the unstamped rule: %q", got)
	}
}

// TestAFullPassLeavesNothingToDo is the bookkeeping half of the saving: a full
// pass that recorded nothing about what it read would make the next incremental
// pass re-open every stamped account.
func TestAFullPassLeavesNothingToDo(t *testing.T) {
	f := newCacheFixture(t)
	ctx := context.Background()

	for _, id := range []int64{1, 2} {
		if err := f.sites.StampRules(ctx, id, f.now); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	before := f.opener.opens

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if opened := f.opener.opens - before; opened != 0 {
		t.Errorf("the pass after a full pass opened %d accounts, want none", opened)
	}
}

// TestASaveDuringARefreshSurvivesIt keeps a rule the settings page pushed from
// being thrown away by a pass that was already running.
func TestASaveDuringARefreshSurvivesIt(t *testing.T) {
	f := newCacheFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	saved, err := Compile([]Rule{
		{Position: 1, Pattern: "^/saved/(\\d+)$", Replacement: "/saved/:id", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	f.opener.before = func() {
		f.opener.before = nil

		f.cache.Set(1, saved)
	}

	if err := f.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if got := f.cache.Clean(1, "/saved/7"); got != "/saved/:id" {
		t.Errorf("the rule the settings page pushed was thrown away by the pass that was running: %q", got)
	}
}
