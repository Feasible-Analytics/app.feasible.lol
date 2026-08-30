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
)

// TestOpenCreatesTheDirectory covers the case that actually happens on a first
// run: the data directory does not exist yet. Failing there would make a fresh
// install look broken.
func TestOpenCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "control.db")

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
	path := filepath.Join(t.TempDir(), "control.db")

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
	source := filepath.Join(dir, "control.db")

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
	source := filepath.Join(dir, "control.db")

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
