//
// auth_test.go
// The shared test fixtures, and the store's own small surface.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newTestStore builds a store over a migrated control database in a temporary
// directory. Every test gets its own file, so nothing leaks between them and a
// failure names a path somebody can open.
func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("open control database: %v", err)
	}

	t.Cleanup(func() { checkClose(t, "control database", db.Close) })

	if _, err := migrate.Run(context.Background(), db, migrate.Control()); err != nil {
		t.Fatalf("migrate control database: %v", err)
	}
	return NewStore(db), db
}

// checkClose runs one test cleanup and reports a failure against the test that
// owns the resource instead of silently discarding it.
func checkClose(t testing.TB, name string, close func() error) {
	t.Helper()
	if err := close(); err != nil {
		t.Errorf("close %s: %v", name, err)
	}
}

// closeResponseBody closes an HTTP response in a test and attributes a cleanup
// failure to the request assertion that consumed it.
func closeResponseBody(t testing.TB, response *http.Response) {
	t.Helper()
	checkClose(t, "response body", response.Body.Close)
}

// writeTestResponse writes a fixture response and fails its owning test if the
// server connection cannot accept the complete body.
func writeTestResponse(t testing.TB, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write fixture response: %v", err)
	}
}

// newTestSealer builds a sealer over a fixed key, so a test can assert on what
// encryption at rest actually produces rather than only that it did not error.
func newTestSealer(t *testing.T) *Sealer {
	t.Helper()

	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}

	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatalf("build sealer: %v", err)
	}

	return sealer
}

// TestStoreClockIsInjectable checks the seam every expiry test in this package
// depends on. Without it, testing a fourteen-day window means waiting fourteen
// days.
func TestStoreClockIsInjectable(t *testing.T) {
	s, _ := newTestStore(t)

	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return fixed })

	if !s.Now().Equal(fixed) {
		t.Errorf("want %v, got %v", fixed, s.Now())
	}
}

// TestExistsHandlesNoRows checks that "no such row" is an answer rather than an
// error, since forgetting that branch is what turns a missing row into a 500.
func TestExistsHandlesNoRows(t *testing.T) {
	_, db := newTestStore(t)
	ctx := context.Background()

	found, err := exists(ctx, db, "SELECT 1 FROM users WHERE id = ?", 999)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}

	if found {
		t.Error("no user should have been found")
	}
}
