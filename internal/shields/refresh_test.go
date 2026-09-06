//
// refresh_test.go
// That a quiet fifteen seconds opens nothing.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// countingOpener wraps a real manager and counts how many times a refresh
// reached for an account database. It also refuses one id on demand, which is
// how the one-bad-account case is driven.
type countingOpener struct {
	inner   *accounts.Manager
	opens   int
	refuses map[int64]bool
}

// Acquire counts the call and hands back a real lease, unless this id is one
// the test wants to fail.
func (c *countingOpener) Acquire(ctx context.Context, id int64) (*accounts.Lease, error) {
	c.opens++

	if c.refuses[id] {
		return nil, errors.New("this account is busy")
	}

	return c.inner.Acquire(ctx, id)
}

// refreshFixture is a control database with two accounts, one site each, and a
// shield rule on both.
type refreshFixture struct {
	control *sql.DB
	sites   *sites.Cache
	opener  *countingOpener
	cache   *Cache
	now     time.Time
}

// newRefreshFixture builds and seeds it.
func newRefreshFixture(t *testing.T) *refreshFixture {
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

	exec(t, control, "INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'One', 0, 0)")
	exec(t, control, "INSERT INTO teams (id, name, created_at, updated_at) VALUES (2, 'Two', 0, 0)")
	exec(t, control, "INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'one.example', 0, 0)")
	exec(t, control, "INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (2, 2, 'two.example', 0, 0)")

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

		if _, err := Add(ctx, account.Writer(), id, KindPage, "/admin*", "", now); err != nil {
			t.Fatal(err)
		}
	}

	opener := &countingOpener{inner: manager, refuses: map[int64]bool{}}

	return &refreshFixture{
		control: control,
		sites:   routing,
		opener:  opener,
		cache:   New(routing, opener),
		now:     now,
	}
}

// TestAQuietPassOpensNothing is the acceptance criterion. Two loops opening
// every account on the box four times a minute is a quarter of a core spent
// re-reading rules nobody changed, and it is what stops a bounded handle cache
// from ever settling.
func TestAQuietPassOpensNothing(t *testing.T) {
	f := newRefreshFixture(t)
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

	// And the rules survived being carried forward rather than rebuilt.
	if allowed(f.cache, 1, "/admin/users") {
		t.Error("the rule stopped applying after the quiet passes")
	}
}

// TestASavedRuleIsLiveWithinOneInterval keeps the behaviour the refresh exists
// for: a rule saved in the dashboard applies everywhere on the next pass.
func TestASavedRuleIsLiveWithinOneInterval(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if allowed(f.cache, 2, "/checkout") == false {
		t.Fatal("the site was already blocking /checkout")
	}

	// Written the way another process would write it: to the account database,
	// then a stamp on the control database.
	account, err := f.opener.inner.Open(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Add(ctx, account.Writer(), 2, KindPage, "/checkout*", "", f.now); err != nil {
		t.Fatal(err)
	}

	if err := f.sites.StampRules(ctx, 2, f.now); err != nil {
		t.Fatal(err)
	}

	before := f.opener.opens

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if allowed(f.cache, 2, "/checkout") {
		t.Error("the saved rule is not live after one pass")
	}

	// Only the account that changed was opened.
	if opened := f.opener.opens - before; opened != 1 {
		t.Errorf("the pass opened %d accounts, want only the one that changed", opened)
	}

	// And the untouched account kept its rule.
	if allowed(f.cache, 1, "/admin/users") {
		t.Error("the other account lost its rule")
	}
}

// TestOneUnreadableAccountDoesNotStopTheOthers is the blast-radius fix. The old
// refresh returned on the first account it could not open, so one busy database
// left every customer on the shard holding a stale snapshot.
func TestOneUnreadableAccountDoesNotStopTheOthers(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	// Both accounts change, but one cannot be opened.
	for _, id := range []int64{1, 2} {
		account, err := f.opener.inner.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := Add(ctx, account.Writer(), id, KindPage, "/new*", "", f.now); err != nil {
			t.Fatal(err)
		}

		if err := f.sites.StampRules(ctx, id, f.now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	f.opener.refuses[1] = true

	err := f.cache.RefreshChanged(ctx)
	if err == nil {
		t.Fatal("a refusal was not reported")
	}

	// The account that could be read did update.
	if allowed(f.cache, 2, "/new/thing") {
		t.Error("the readable account did not pick up its new rule")
	}

	// The one that could not keeps what it had, rather than lapsing.
	if allowed(f.cache, 1, "/admin/users") {
		t.Error("the unreadable account lost the rule it already had")
	}

	// And it is retried on the next pass rather than treated as up to date.
	f.opener.refuses[1] = false
	before := f.opener.opens

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if opened := f.opener.opens - before; opened != 1 {
		t.Errorf("the retry opened %d accounts, want the one that failed", opened)
	}

	if allowed(f.cache, 1, "/new/thing") {
		t.Error("the retried account did not pick up its new rule")
	}
}

// TestTheFullPassRebuildsWhateverTheMarkerSays is the backstop. A rule written
// by something that does not stamp — an older process mid-upgrade, a hand edit
// — is invisible to the incremental pass, and this is what bounds how long it
// stays invisible.
func TestTheFullPassRebuildsWhateverTheMarkerSays(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	account, err := f.opener.inner.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// No stamp: this is the case the marker cannot see.
	if _, err := Add(ctx, account.Writer(), 1, KindPage, "/secret*", "", f.now); err != nil {
		t.Fatal(err)
	}

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	if allowed(f.cache, 1, "/secret/x") == false {
		t.Fatal("the incremental pass saw an unstamped change, so this test proves nothing")
	}

	if err := f.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if allowed(f.cache, 1, "/secret/x") {
		t.Error("the full pass did not pick up the unstamped rule")
	}
}

// allowed reports whether a path would reach disk for one site, which is what a
// page rule decides. The hostname is the site's own, so the default hostname
// policy is not what answers.
func allowed(cache *Cache, siteID int64, path string) bool {
	hostname := "one.example"
	if siteID == 2 {
		hostname = "two.example"
	}

	ok, _ := cache.Allowed(siteID, hostname, path, "")

	return ok
}
