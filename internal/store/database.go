//
// database.go
// A single SQLite file behind one writer connection and a pool of readers.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReaderPoolSize caps the concurrent read connections per database. Readers do
// not block each other under WAL, so the limit is about memory rather than
// correctness: SQLite's page cache is per connection, so every extra reader is
// another 64 MB ceiling on a busy account.
const ReaderPoolSize = 4

// readerIdleTimeout releases connections an account has stopped using. A
// dashboard is looked at for a minute and then left alone for a day, and
// holding four warm connections per account for that day is how a box with a
// few hundred accounts runs out of both memory and file descriptors.
const readerIdleTimeout = 5 * time.Minute

// Database is one SQLite file with its two handles. SQLite allows exactly one
// writer at a time, so the choice is between a pool that discovers that the
// hard way under load and a pool of one that queues in Go. We queue in Go: a
// second writer connection buys nothing but SQLITE_BUSY errors, while readers
// under WAL genuinely do run concurrently and want a real pool.
type Database struct {
	path   string
	writer *sql.DB
	reader *sql.DB
}

// OpenDatabase opens a database file for serving, creating the file and its
// directory if they do not exist yet. The writer is opened first on purpose:
// it is what creates the file and puts it into WAL mode, and a reader attached
// to a database that is not yet in WAL would take the rollback journal's much
// coarser locks.
func OpenDatabase(path string) (*Database, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	writer, err := sql.Open(DriverName, DSN(path, TxLockImmediate))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)

	if err := writer.Ping(); err != nil {
		writer.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// query_only rejects a write at the SQL layer, which is the point: a query
	// bug that tries to write cannot corrupt anything or steal the write lock.
	// The file itself is still opened read-write, because a genuinely read-only
	// file handle cannot maintain the shared-memory index a WAL database needs.
	reader, err := sql.Open(DriverName, DSN(path, "_pragma=query_only(1)"))
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("open %s for reading: %w", path, err)
	}

	reader.SetMaxOpenConns(ReaderPoolSize)
	reader.SetMaxIdleConns(1)
	reader.SetConnMaxIdleTime(readerIdleTimeout)

	if err := reader.Ping(); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("open %s for reading: %w", path, err)
	}

	return &Database{path: path, writer: writer, reader: reader}, nil
}

// Path reports the file this database was opened from. Errors and log lines
// name the file rather than the account, because when something is wrong with
// one account's data the next step is always to open that file.
func (d *Database) Path() string {
	return d.path
}

// Writer returns the single-connection handle. Everything that inserts an
// event, updates a session or applies a migration goes through it, so that
// write serialisation is a queue in Go rather than a race in SQLite.
func (d *Database) Writer() *sql.DB {
	return d.writer
}

// Reader returns the pooled read-only handle. Dashboard and API queries belong
// here so a slow report cannot hold up ingestion.
func (d *Database) Reader() *sql.DB {
	return d.reader
}

// Close releases both handles. Both are closed even if the first fails,
// because leaking a connection to a database we believe is closed leaves the
// WAL checkpointed only when the process exits.
func (d *Database) Close() error {
	readerErr := d.reader.Close()

	if err := d.writer.Close(); err != nil {
		return fmt.Errorf("close %s: %w", d.path, err)
	}

	if readerErr != nil {
		return fmt.Errorf("close %s: %w", d.path, readerErr)
	}

	return nil
}
