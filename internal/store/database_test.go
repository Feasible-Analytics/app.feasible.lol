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
	"fmt"
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

// TestEachHandleWaitsItsOwnTimeForALock pins how long the writer and the
// readers wait for a lock.
func TestEachHandleWaitsItsOwnTimeForALock(t *testing.T) {
	db := newDatabase(t)

	if got, want := pragma(t, db.Writer(), "busy_timeout"), fmt.Sprint(WriteBusyTimeout.Milliseconds()); got != want {
		t.Errorf("writer busy_timeout is %s ms, want %s", got, want)
	}
	if got, want := pragma(t, db.Reader(), "busy_timeout"), fmt.Sprint(ReadBusyTimeout.Milliseconds()); got != want {
		t.Errorf("reader busy_timeout is %s ms, want %s", got, want)
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
	shared := []string{
		"file:/tmp/x.db?",
		"journal_mode(WAL)", "synchronous(FULL)", "secure_delete(1)",
		"foreign_keys(1)", "cache_size(-64000)", "mmap_size(268435456)",
		"temp_store(MEMORY)", "wal_autocheckpoint(1000)",
	}

	for name, dsn := range map[string]string{
		"read":  DSN("/tmp/x.db", "_pragma=query_only(1)"),
		"write": WriteDSN("/tmp/x.db"),
	} {
		for _, want := range shared {
			if !strings.Contains(dsn, want) {
				t.Errorf("the %s DSN is missing %s: %s", name, want, dsn)
			}
		}

		// The driver does not keep the order of two busy_timeout pragmas.
		if got := strings.Count(dsn, "busy_timeout("); got != 1 {
			t.Errorf("the %s DSN sets busy_timeout %d times, want once: %s", name, got, dsn)
		}
	}

	if read := DSN("/tmp/x.db", "_pragma=query_only(1)"); !strings.Contains(read, "busy_timeout(5000)") ||
		!strings.Contains(read, "query_only(1)") || strings.Contains(read, TxLockImmediate) {
		t.Errorf("the read DSN should wait 5s, be query-only and take no write lock at BEGIN: %s", read)
	}
	if write := WriteDSN("/tmp/x.db"); !strings.Contains(write, "busy_timeout(20000)") ||
		!strings.Contains(write, TxLockImmediate) {
		t.Errorf("the write DSN should wait 20s and take the write lock at BEGIN: %s", write)
	}
}
