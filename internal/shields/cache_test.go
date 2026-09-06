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
	"errors"
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

// newRouting builds a system database holding one team and one site, which is
// what the shield cache walks to find the accounts it has to read.
func newRouting(t *testing.T) *sites.Cache {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()

	if _, err := migrate.Run(ctx, db, migrate.System()); err != nil {
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

	writer := ingest.NewWriter(manager)
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

// TestDefaultHostnamePolicyStopsCopycatWithoutConfiguredRules proves the site
// domain itself is always the base allow-list in the live cache.
func TestDefaultHostnamePolicyStopsCopycatWithoutConfiguredRules(t *testing.T) {
	ctx := context.Background()
	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.CloseAll() })
	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	cache := New(newRouting(t), manager)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	writer := ingest.NewWriter(manager)
	writer.Now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	writer.Shield = cache

	event := shieldEvent("/pricing", writer.Now())
	event.Hostname = "copycat.test"
	if _, err := writer.Write(ctx, []ingest.Event{event}); err != nil {
		t.Fatal(err)
	}

	var facts, rejections int64
	if err := account.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err := account.Reader().QueryRowContext(ctx, "SELECT SUM(events) FROM hostname_rejections").Scan(&rejections); err != nil {
		t.Fatal(err)
	}
	if facts != 0 || rejections != 1 {
		t.Fatalf("copycat produced %d facts and %d rejections, want 0 and 1", facts, rejections)
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

// countingOpener wraps a real manager and counts how many times a refresh
// reached for an account database. It also refuses one id on demand, which is
// how the one-bad-account case is driven.
type countingOpener struct {
	inner   *accounts.Manager
	opens   int
	refuses map[int64]bool

	// before runs on the first open of a pass, which is how a test lands a save
	// in the window between a refresh reading the snapshot and publishing one.
	before func()
}

// AcquireForScan counts the call and hands back a real lease, unless this id is one
// the test wants to fail.
func (c *countingOpener) AcquireForScan(ctx context.Context, id int64) (*accounts.Lease, error) {
	c.opens++

	if c.before != nil {
		c.before()
	}

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

// TestOneUnreadableAccountDoesNotStopTheOthers bounds the blast radius of one
// busy database to the account that owns it, rather than every customer on the
// shard holding a stale snapshot.
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

// TestAFullPassLeavesNothingToDo is the bookkeeping half of the saving.
//
// A full pass that recorded nothing about what it read would make the very next
// incremental pass believe every stamped account had moved, and re-open all of
// them fifteen seconds later — twice an hour, for ever.
func TestAFullPassLeavesNothingToDo(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()

	// Stamped, so the markers are not all zero. Zero is the value a broken
	// full pass records, and it would match.
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

// TestASaveDuringARefreshSurvivesIt is the other half. A refresh reads the
// snapshot, spends seconds on I/O, and writes a new one; a save landing in
// between used to be thrown away, and with carry-forward it would stay thrown
// away until the marker brought it back.
func TestASaveDuringARefreshSurvivesIt(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()

	if err := f.cache.RefreshChanged(ctx); err != nil {
		t.Fatal(err)
	}

	// The save happens while the pass is between reading and publishing, which
	// is exactly the window a real one would land in.
	f.opener.before = func() {
		f.opener.before = nil

		f.cache.Set(1, []Rule{{ID: 99, SiteID: 1, Kind: KindPage, Value: "/saved*"}})
	}

	if err := f.cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if allowed(f.cache, 1, "/saved/thing") {
		t.Error("the rule the settings page pushed was thrown away by the pass that was running")
	}
}
