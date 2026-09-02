//
// store_test.go
// Tests for opening, versioning and snapshotting SQLite databases.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenCreatesTheDirectory covers the case that actually happens on a first
// run: the data directory does not exist yet. Failing there would make a fresh
// install look broken.
func TestOpenCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "system.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
}

// TestSchemaVersionRoundTrip checks the per-database migration level survives a
// close and reopen. Every database in the system carries its own, and a version
// that did not persist would make a resumable migration impossible.
func TestSchemaVersionRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "system.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	version, err := SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("a new database should start at 0, got %d", version)
	}

	if err := SetSchemaVersion(ctx, db, 7); err != nil {
		t.Fatal(err)
	}
	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	version, err = SchemaVersion(ctx, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if version != 7 {
		t.Fatalf("schema version did not persist: got %d", version)
	}
}

// TestBackup exercises the real VACUUM INTO path, including that the snapshot
// carries the data and the schema version with it — a backup that lost the
// version would restore into a database nothing could migrate.
func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "system.db")

	db, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "CREATE TABLE sites (domain TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO sites (domain) VALUES ('example.com')"); err != nil {
		t.Fatal(err)
	}
	if err := SetSchemaVersion(ctx, db, 3); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dest := filepath.Join(dir, "backups", "control-copy.db")
	if err := Backup(ctx, source, dest); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()

	var domain string
	if err := restored.QueryRowContext(ctx, "SELECT domain FROM sites").Scan(&domain); err != nil {
		t.Fatal(err)
	}
	if domain != "example.com" {
		t.Fatalf("snapshot lost data: got %q", domain)
	}

	version, err := SchemaVersion(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("snapshot lost the schema version: got %d", version)
	}
}

// TestBackupRefusesToOverwrite pins the rule that a backup never replaces an
// existing file. Silently overwriting yesterday's good copy with today's broken
// one is worse than having no backup at all.
func TestBackupRefusesToOverwrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "system.db")

	db, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	dest := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(dest, []byte("do not clobber me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Backup(ctx, source, dest); err == nil {
		t.Fatal("expected an error when the destination already exists")
	}

	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "do not clobber me" {
		t.Fatal("the existing file was modified")
	}
}

// TestBackupMissingSource checks the error path a cron job will hit first, when
// it is pointed at a data directory that has not been created yet.
func TestBackupMissingSource(t *testing.T) {
	dir := t.TempDir()

	err := Backup(context.Background(), filepath.Join(dir, "nope.db"), filepath.Join(dir, "out.db"))
	if err == nil {
		t.Fatal("expected an error for a missing source database")
	}
}

// TestQuoteLiteral guards the escaping used for the VACUUM INTO path, which
// cannot use a bind parameter.
func TestQuoteLiteral(t *testing.T) {
	if got := quoteLiteral("/tmp/o'brien.db"); got != "'/tmp/o''brien.db'" {
		t.Fatalf("got %s", got)
	}
}

// TestOpenTransactionsWaitForTheWriterAtBegin is the reason every handle takes
// the write lock at BEGIN. Two pooled connections each start a transaction,
// read, and then write: with deferred locking the second one reads a snapshot
// that the first then invalidates, and its write fails with SQLITE_BUSY at
// once, with no busy_timeout to save it. With immediate locking the second
// BEGIN waits for the first to commit and the read-then-write just works.
func TestOpenTransactionsWaitForTheWriterAtBegin(t *testing.T) {
	ctx := context.Background()

	db, err := Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "CREATE TABLE counters (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO counters (id, n) VALUES (1, 0)"); err != nil {
		t.Fatal(err)
	}

	first, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecContext(ctx, "UPDATE counters SET n = n + 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	committed := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		committed <- first.Commit()
	}()

	second, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("second BEGIN: %v", err)
	}
	defer second.Rollback() //nolint:errcheck // rollback after commit is harmless

	var n int
	if err := second.QueryRowContext(ctx, "SELECT n FROM counters WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ExecContext(ctx, "UPDATE counters SET n = ? WHERE id = 1", n+1); err != nil {
		t.Fatalf("read-then-write in the second transaction failed: %v", err)
	}
	if err := second.Commit(); err != nil {
		t.Fatalf("commit second: %v", err)
	}

	if err := <-committed; err != nil {
		t.Fatalf("commit first: %v", err)
	}

	if err := db.QueryRowContext(ctx, "SELECT n FROM counters WHERE id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("counter = %d, want both increments to land", n)
	}
}

// TestIsUniqueViolationReadsTheDriverCode checks the helper recognises a real
// duplicate and nothing else, since every caller turns its answer into a
// customer-facing "already exists" rather than an internal error.
func TestIsUniqueViolationReadsTheDriverCode(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "unique.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE things (name TEXT NOT NULL UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO things (name) VALUES ('one')`); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`INSERT INTO things (name) VALUES ('one')`)
	if !IsUniqueViolation(err) {
		t.Fatalf("a duplicate insert was not recognised: %v", err)
	}

	_, err = db.Exec(`INSERT INTO nowhere (name) VALUES ('one')`)
	if IsUniqueViolation(err) {
		t.Fatalf("a missing table was mistaken for a duplicate: %v", err)
	}

	if IsUniqueViolation(nil) {
		t.Fatal("nil was reported as a duplicate")
	}
}
