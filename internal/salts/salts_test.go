//
// salts_test.go
// Tests for daily rotation at 00:00 UTC, the two-salt window and the 48-hour delete.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package salts

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newSystemDB builds a migrated system database. The rotation only makes
// sense against the real salts table and its unique day index, so these tests
// run the actual schema rather than a hand-written stand-in.
func newSystemDB(t testing.TB) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	return db
}

// testKey is a fixed encryption key, so a test never depends on a file being
// generated somewhere.
var testKey = bytes.Repeat([]byte{0x2a}, KeySize)

// newStore builds a store whose clock the test controls. Every interesting
// property of this package is about what happens at a particular instant, and a
// test that had to wait for real midnight would never be written.
func newStore(t testing.TB, db *sql.DB, now *time.Time) *Store {
	t.Helper()

	store, err := NewStore(db, testKey)
	if err != nil {
		t.Fatal(err)
	}
	store.SetClock(func() time.Time { return *now })

	return store
}

// at builds a UTC instant. Everything in this package is UTC by construction,
// so the helper takes no location and there is nowhere to pass one.
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// TestRotatesAtUTCMidnight is the trap this whole package exists to avoid. A
// timezone-local rotation would give two accounts different visitor identities
// for the same person on the same day, and no later job could reconcile them.
func TestRotatesAtUTCMidnight(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 30, 23, 59)
	store := newStore(t, db, &now)

	before, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// One minute before midnight and one minute after are different days, so
	// the salt has to have changed.
	now = at(2026, time.August, 31, 0, 1)

	after, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(before.Current, after.Current) {
		t.Fatal("the salt did not rotate across 00:00 UTC")
	}
	if !bytes.Equal(after.Previous, before.Current) {
		t.Fatal("yesterday's salt is not the previous salt after rotation")
	}
	if after.Day != before.Day+1 {
		t.Fatalf("day went from %d to %d, want consecutive", before.Day, after.Day)
	}
}

// TestRotationIsExactNotEventual checks the boundary itself. The background
// refresh runs every ninety seconds, so without an on-demand refresh there
// would be a window after midnight where events hashed with yesterday's salt.
func TestRotatesExactlyAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := time.Date(2026, time.August, 30, 23, 59, 59, 0, time.UTC)
	store := newStore(t, db, &now)

	before, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The very first second of the new day.
	now = time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC)

	after, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(before.Current, after.Current) {
		t.Fatal("the first second of the new UTC day still used yesterday's salt")
	}
}

// TestSalidStaysStableWithinADay checks the other side: repeated calls inside
// one UTC day must return the same salt, or every request would fingerprint the
// same visitor differently.
func TestSaltStaysStableWithinADay(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 30, 0, 1)
	store := newStore(t, db, &now)

	first, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, hour := range []int{6, 12, 18, 23} {
		now = at(2026, time.August, 30, hour, 30)

		again, err := store.Pair(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again.Current, first.Current) {
			t.Fatalf("the salt changed at %02d:30 within the same UTC day", hour)
		}
	}
}

// TestOnlyCurrentAndPreviousAreUsable checks the hashing window never widens;
// the third stored generation is tomorrow's unused rollover material.
func TestOnlyCurrentAndPreviousAreUsable(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 28, 12, 0)
	store := newStore(t, db, &now)

	for _, day := range []int{28, 29, 30} {
		now = at(2026, time.August, day, 12, 0)
		if _, err := store.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}

	pair, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(pair.Current) != Size || len(pair.Previous) != Size {
		t.Fatalf("expected two live salts, got %d and %d bytes", len(pair.Current), len(pair.Previous))
	}
	if bytes.Equal(pair.Current, pair.Previous) {
		t.Fatal("the two live salts are the same value")
	}
	if len(pair.Next) != Size {
		t.Fatal("the next UTC day was not pre-provisioned")
	}
}

// TestRowsPastRetentionAreDeleted is the deletion the privacy claim rests on.
// After 48 hours a fingerprint must be unreconstructable by anyone, including
// us, and that is only true if the row is actually gone.
func TestRowsPastRetentionAreDeleted(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 27, 12, 0)
	store := newStore(t, db, &now)

	for _, day := range []int{27, 28, 29, 30} {
		now = at(2026, time.August, day, 12, 0)
		if _, err := store.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM salts").Scan(&count); err != nil {
		t.Fatal(err)
	}

	// Four days were created; at noon on the 30th only the 29th and 30th are
	// inside 48 hours, and the row for the 28th is exactly at the boundary.
	if count > 3 {
		t.Fatalf("salts table holds %d rows after four days, want at most 3", count)
	}

	var oldest int64
	if err := db.QueryRowContext(ctx, "SELECT MIN(created_at) FROM salts").Scan(&oldest); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-Retention).Unix()
	if oldest < cutoff {
		t.Fatalf("oldest salt is at %d, which is before the %d cutoff", oldest, cutoff)
	}
}

// TestOneSaltPerDayUnderConcurrency covers the midnight race. Every process
// offers random current and next-day material, while SQLite makes one authority
// value win for each day.
func TestOneSaltPerDayUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 30, 0, 0)

	// Four independent stores over one database is the multi-process case: each
	// has its own cache and its own idea of when to refresh.
	var pairs []Pair
	for i := 0; i < 4; i++ {
		store := newStore(t, db, &now)

		pair, err := store.Refresh(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, pair)
	}

	for i := 1; i < len(pairs); i++ {
		if !bytes.Equal(pairs[i].Current, pairs[0].Current) {
			t.Fatal("two processes ended up with different salts for the same UTC day")
		}
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM salts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("salts table holds %d rows, want current and pre-provisioned next", count)
	}
}

// TestPermanentKeyCannotRegenerateDailySalt proves salt rows are random
// authority material, not values reproducible from the at-rest encryption key.
func TestPermanentKeyCannotRegenerateDailySalt(t *testing.T) {
	ctx := context.Background()
	now := at(2026, time.August, 31, 12, 0)
	first := newStore(t, newSystemDB(t), &now)
	second := newStore(t, newSystemDB(t), &now)

	firstPair, err := first.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer firstPair.Erase()
	secondPair, err := second.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer secondPair.Erase()

	if bytes.Equal(firstPair.Current, secondPair.Current) || bytes.Equal(firstPair.Next, secondPair.Next) {
		t.Fatal("independent authorities regenerated matching random salt material")
	}
}

// TestPruneFailureErasesTheUsableCache drives the privacy failure boundary.
// Once SQLite refuses deletion, the same-day fast path must stay unavailable.
func TestPruneFailureErasesTheUsableCache(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)
	now := at(2026, time.August, 28, 0, 0)
	store := newStore(t, db, &now)

	initial, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	initial.Erase()
	retiredCurrent := store.cached.Current
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER refuse_expired_salt_delete
		BEFORE DELETE ON salts
		BEGIN
			SELECT RAISE(FAIL, 'fixture refuses salt erasure');
		END`); err != nil {
		t.Fatal(err)
	}

	now = at(2026, time.August, 30, 0, 0)
	if _, err := store.Refresh(ctx); err == nil {
		t.Fatal("refresh succeeded after SQLite refused expired salt erasure")
	}
	if !bytes.Equal(retiredCurrent, make([]byte, Size)) {
		t.Fatal("prune failure released usable cache bytes without overwriting them")
	}
	if _, err := store.Pair(ctx); err == nil {
		t.Fatal("same-day fast path remained usable after prune failure")
	}
}

// TestRefreshOverwritesRetiredCacheSlices observes the backing arrays that held
// yesterday's pair and verifies rollover erases them before release.
func TestRefreshOverwritesRetiredCacheSlices(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)
	now := at(2026, time.August, 30, 12, 0)
	store := newStore(t, db, &now)
	pair, err := store.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pair.Erase()

	oldCurrent := store.cached.Current
	oldNext := store.cached.Next
	now = at(2026, time.August, 31, 0, 0)
	nextPair, err := store.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nextPair.Erase()

	if !bytes.Equal(oldCurrent, make([]byte, Size)) || !bytes.Equal(oldNext, make([]byte, Size)) {
		t.Fatal("retired cache backing arrays were released without being overwritten")
	}
}

// TestStoredSaltIsEncrypted checks the row is unreadable on its own. The salts
// table is as sensitive as raw IP logs, and a plaintext one would make the
// encryption claim false while looking identical from the outside.
func TestStoredSaltIsEncrypted(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 30, 12, 0)
	store := newStore(t, db, &now)

	pair, err := store.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := db.QueryRowContext(ctx, "SELECT salt FROM salts").Scan(&stored); err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(stored, pair.Current) {
		t.Fatal("the salt is stored in the clear")
	}
	if bytes.Contains(stored, pair.Current) {
		t.Fatal("the plaintext salt appears inside the stored value")
	}
}

// TestWrongKeyIsAnExplicitFailure checks a changed key is reported rather than
// silently rotating every visitor's identity, which is what a store that shrugged
// and made a new salt would do.
func TestWrongKeyIsAnExplicitFailure(t *testing.T) {
	ctx := context.Background()
	db := newSystemDB(t)

	now := at(2026, time.August, 30, 12, 0)
	if _, err := newStore(t, db, &now).Pair(ctx); err != nil {
		t.Fatal(err)
	}

	other, err := NewStore(db, bytes.Repeat([]byte{0x99}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	other.SetClock(func() time.Time { return now })

	if _, err := other.Refresh(ctx); err == nil {
		t.Fatal("a store with the wrong key read the salts without complaint")
	}
}

// TestDayIsPureUTCArithmetic checks the rotation has no timezone in it at all.
// The same instant expressed in two zones must land on the same day number.
func TestDayIsPureUTCArithmetic(t *testing.T) {
	instant := at(2026, time.August, 30, 23, 30)

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no timezone database on this machine")
	}

	if Day(instant) != Day(instant.In(tokyo)) {
		t.Fatal("the day number changed when the same instant was expressed in another zone")
	}

	// And the boundary is where UTC says it is, not where any local calendar
	// does: 08:30 in Tokyo on the 31st is still the 30th in UTC.
	local := time.Date(2026, time.August, 31, 8, 30, 0, 0, tokyo)
	if Day(local) != Day(instant) {
		t.Fatal("a local morning after local midnight rotated the salt early")
	}
}

// TestSetRandomMakesTheSaltReproducible covers the one hook that exists for the
// seed generator. A generated dataset has to hash to the same visitor ids every
// time it is built, and it cannot while the salt underneath it is fresh random
// on every run.
func TestSetRandomMakesTheSaltReproducible(t *testing.T) {
	now := at(2026, time.August, 30, 9, 0)

	first := saltFromFixedSource(t, &now)
	second := saltFromFixedSource(t, &now)

	if !bytes.Equal(first, second) {
		t.Fatalf("two stores on one source produced different salts: %x and %x", first, second)
	}

	// The default source is the real one, so an ordinary store must not be
	// reproducible: a predictable salt is a reversible fingerprint.
	store := newStore(t, newSystemDB(t), &now)

	pair, err := store.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(pair.Current, first) {
		t.Fatal("a store using the default source produced the seeded salt")
	}
}

// saltFromFixedSource builds a store over a fixed byte source and returns the
// salt it creates for the day.
func saltFromFixedSource(t *testing.T, now *time.Time) []byte {
	t.Helper()

	store := newStore(t, newSystemDB(t), now)
	store.SetRandom(bytes.NewReader(bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 64)))

	pair, err := store.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	return pair.Current
}
