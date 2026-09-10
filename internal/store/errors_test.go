//
// errors_test.go
// The driver errors callers branch on, produced for real rather than faked.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// TestIsUniqueViolationRecognisesADuplicate makes SQLite refuse a real
// duplicate rather than constructing the driver error by hand, which is the
// only way the test still fails if the driver changes its codes.
func TestIsUniqueViolationRecognisesADuplicate(t *testing.T) {
	ctx := context.Background()

	db, err := Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "CREATE TABLE names (name TEXT NOT NULL UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO names (name) VALUES ('one')"); err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO names (name) VALUES ('one')")
	if !IsUniqueViolation(err) {
		t.Fatalf("a duplicate insert was not read as a unique violation: %v", err)
	}
	if IsBusy(err) {
		t.Fatal("a unique violation was read as a locked database")
	}
}

// TestIsBusyRecognisesALockedDatabase holds the write lock on one handle and
// gives a second handle no patience at all, which is exactly the failure the
// recurring scheduler meets in production.
func TestIsBusyRecognisesALockedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "system.db")

	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	if _, err := holder.ExecContext(ctx, "CREATE TABLE counters (n INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	// One pinned connection with no patience at all, so the blocked BEGIN below
	// fails at once rather than sitting out the five-second default.
	impatient, err := second.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close() //nolint:errcheck // returning a pinned connection to the pool cannot fail usefully

	if _, err := impatient.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}

	held, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback() //nolint:errcheck // rollback after commit is harmless

	if _, err := held.ExecContext(ctx, "INSERT INTO counters (n) VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	_, err = impatient.BeginTx(ctx, nil)
	if err == nil {
		t.Fatal("the second transaction began while the write lock was held")
	}
	if !IsBusy(err) {
		t.Fatalf("a held write lock was not read as a busy database: %v", err)
	}
	if IsUniqueViolation(err) {
		t.Fatal("a busy database was read as a unique violation")
	}
}

// TestIsBusyIgnoresEverythingElse keeps the helper from turning any old failure
// into "try again", which would have the scheduler retry a broken query forever.
func TestIsBusyIgnoresEverythingElse(t *testing.T) {
	if IsBusy(nil) {
		t.Fatal("nil was read as a busy database")
	}
	if IsBusy(fmt.Errorf("jobs: begin periodic: %w", errors.New("database is locked"))) {
		t.Fatal("a plain error whose text says locked was read as a busy database")
	}
}
