//
// database_test.go
// Tests for the writer/reader split and the pragmas that come with it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newDatabase opens a database inside a directory that does not exist yet,
// which is the state a first run and a new account both start from.
func newDatabase(t *testing.T) *Database {
	t.Helper()

	db, err := OpenDatabase(filepath.Join(t.TempDir(), "accounts", "000001", "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	return db
}

// pragma reads one pragma back from a handle. Pragmas are per connection, so
// asking the handle is the only way to know what a query on it would actually
// get.
func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()

	var value string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return value
}

// TestOpenDatabaseCreatesTheFile covers the first-use path. A missing directory
// has to be created rather than reported, or an account's first event fails on
// something nobody has had a chance to create yet.
func TestOpenDatabaseCreatesTheFile(t *testing.T) {
	db := newDatabase(t)

	if _, err := os.Stat(db.Path()); err != nil {
		t.Fatalf("the database file was not created: %v", err)
	}
}

// TestPragmasAreAppliedToBothHandles is the check that a pragma cannot be set
// on one handle and forgotten on the other. WAL is what lets a dashboard read
// run during an ingest write, and foreign keys are off by default in SQLite.
func TestPragmasAreAppliedToBothHandles(t *testing.T) {
	db := newDatabase(t)

	for name, handle := range map[string]*sql.DB{"writer": db.Writer(), "reader": db.Reader()} {
		if mode := pragma(t, handle, "journal_mode"); mode != "wal" {
			t.Errorf("%s journal_mode is %q, want wal", name, mode)
		}
		if keys := pragma(t, handle, "foreign_keys"); keys != "1" {
			t.Errorf("%s foreign_keys is %q, want 1", name, keys)
		}
		if sync := pragma(t, handle, "synchronous"); sync != "2" {
			t.Errorf("%s synchronous is %q, want 2 (FULL)", name, sync)
		}
		if secure := pragma(t, handle, "secure_delete"); secure != "1" {
			t.Errorf("%s secure_delete is %q, want 1", name, secure)
		}
		if cache := pragma(t, handle, "cache_size"); cache != "-64000" {
			t.Errorf("%s cache_size is %q, want -64000", name, cache)
		}
	}
}

// TestWriterIsASingleConnection pins the decision the whole write path is built
// on. SQLite allows one writer, so a second connection would buy nothing but
// SQLITE_BUSY errors under load.
func TestWriterIsASingleConnection(t *testing.T) {
	db := newDatabase(t)

	if max := db.Writer().Stats().MaxOpenConnections; max != 1 {
		t.Fatalf("the writer allows %d connections, want 1", max)
	}

	if max := db.Reader().Stats().MaxOpenConnections; max != ReaderPoolSize {
		t.Fatalf("the reader pool allows %d connections, want %d", max, ReaderPoolSize)
	}
}

// TestReaderIsQueryOnly checks the read pool cannot write. A query bug that
// wrote would take the write lock away from ingestion; query_only turns it into
// an error at the point of the mistake.
func TestReaderIsQueryOnly(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := db.Writer().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Reader().ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err == nil {
		t.Fatal("the read pool accepted a write")
	}
}

// TestReaderSeesCommittedWrites is the property WAL is chosen for: a reader
// opened separately from the writer sees a committed row without either of them
// blocking the other.
func TestReaderSeesCommittedWrites(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := db.Writer().ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx, "INSERT INTO t (id) VALUES (7)"); err != nil {
		t.Fatal(err)
	}

	var id int
	if err := db.Reader().QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != 7 {
		t.Fatalf("the reader saw %d", id)
	}
}

// TestCloseClosesBothHandles makes sure neither handle is leaked. A connection
// left open to a database we believe is closed leaves the WAL uncheckpointed
// until the process exits.
func TestCloseClosesBothHandles(t *testing.T) {
	db, err := OpenDatabase(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := db.Writer().PingContext(context.Background()); err == nil {
		t.Fatal("the writer is still open")
	}
	if err := db.Reader().PingContext(context.Background()); err == nil {
		t.Fatal("the reader is still open")
	}
}

// TestDSNCarriesEveryPragma guards the one place a pragma can be added, since
// a pragma that is missing does not fail — it just quietly changes how the
// database behaves under load.
func TestDSNCarriesEveryPragma(t *testing.T) {
	dsn := DSN("/tmp/x.db", "_txlock=immediate")

	for _, want := range []string{
		"file:/tmp/x.db?",
		"journal_mode(WAL)", "synchronous(FULL)", "secure_delete(1)", "busy_timeout(5000)",
		"foreign_keys(1)", "cache_size(-64000)", "mmap_size(268435456)",
		"temp_store(MEMORY)", "wal_autocheckpoint(1000)", "_txlock=immediate",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("the DSN is missing %s: %s", want, dsn)
		}
	}
}
