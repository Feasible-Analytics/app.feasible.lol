//
// store.go
// Opening SQLite databases, reading their schema version, and snapshotting them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package store holds the small amount of SQLite plumbing every other package
// needs: how a database is opened, where its schema version lives, and how a
// consistent copy is taken. The driver is modernc.org/sqlite, which is pure Go —
// running on any CPU with no cgo and no Docker is the project's differentiator,
// so the driver choice belongs behind one door rather than at every call site.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DriverName is the database/sql name registered by modernc.org/sqlite. It is
// exported so that a future driver swap is a one-constant change rather than a
// find-and-replace across the tree.
const DriverName = "sqlite"

// Pragmas are applied to every connection this package opens, and none of them
// are optional. The driver runs each one at connection time, which is the only
// place they can be set reliably: SQLite scopes most of these per connection,
// so a pragma executed once on a pooled handle would apply to whichever
// connection happened to serve that call and to no other.
//
//	journal_mode      WAL, so a dashboard read never blocks an ingest write
//	synchronous       FULL, fsync each commit before it is acknowledged;
//	                  otherwise a successful 202 can outrun its fact rows
//	secure_delete     overwrite deleted secret material rather than leaving it
//	                  recoverable in free database pages
//	busy_timeout      5s of waiting instead of an immediate "database is locked"
//	foreign_keys      on, because SQLite defaults it off and silently keeps
//	                  orphans otherwise
//	cache_size        -64000 = 64 MB of page cache
//	mmap_size         256 MB mapped, which is address space rather than resident
//	                  memory and turns most reads into page-cache hits
//	temp_store        MEMORY, so a GROUP BY spilling to a temp table stays in RAM
//	wal_autocheckpoint 1000 pages, keeping the WAL from growing without bound
const Pragmas = "_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(FULL)" +
	"&_pragma=secure_delete(1)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=cache_size(-64000)" +
	"&_pragma=mmap_size(268435456)" +
	"&_pragma=temp_store(MEMORY)" +
	"&_pragma=wal_autocheckpoint(1000)"

// DSN builds the connection string for a database file. Every caller goes
// through it so that a pragma can never be applied to one handle and forgotten
// on another — a difference that would not fail, it would just quietly perform
// or behave differently depending on which handle a query landed on.
func DSN(path string, extra ...string) string {
	dsn := "file:" + path + "?" + Pragmas

	for _, param := range extra {
		dsn += "&" + param
	}

	return dsn
}

// TxLockImmediate makes every transaction take SQLite's write lock at BEGIN
// rather than at its first write. A deferred transaction that reads and then
// writes has to upgrade its lock mid-flight, and SQLite cannot retry that: it
// returns SQLITE_BUSY at once and busy_timeout never applies. Taking the lock
// up front turns that immediate failure into a wait, and lets a read-then-write
// transaction be written without any locking ceremony of its own. Read-only
// transactions are unaffected, because the driver honours TxOptions.ReadOnly
// with a plain BEGIN.
const TxLockImmediate = "_txlock=immediate"

// Open opens (and creates, if missing) a SQLite database with the pragmas this
// project needs everywhere. It is the general-purpose handle used by
// maintenance commands and for system.db; the account serving path wants
// OpenDatabase instead, which separates the single writer from the reader pool.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	db, err := sql.Open(DriverName, DSN(path, TxLockImmediate))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// sql.Open is lazy, so without this a bad path or an unreadable file is not
	// reported until some unrelated query fails much later.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return db, nil
}

// SchemaVersion reads the database's own migration level. It lives in SQLite's
// user_version header rather than a table because every database in the system
// carries one — system.db and one per account — and a header field needs no
// bootstrap migration to exist and survives VACUUM.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int

	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}

	return version, nil
}

// SetSchemaVersion stamps the migration level. PRAGMA takes no bind parameters,
// so the value is formatted in; it is an int, which is why that is safe here and
// would not be anywhere else.
func SetSchemaVersion(ctx context.Context, db *sql.DB, version int) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}

	return nil
}

// Backup writes a consistent snapshot using VACUUM INTO. Copying the file with
// cp is not safe while the process is writing, and VACUUM INTO takes a proper
// read transaction and compacts as it goes — which is why the backup command
// exists at all rather than telling people to use the filesystem.
func Backup(ctx context.Context, sourcePath, destPath string) error {
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("backup source %s: %w", sourcePath, err)
	}

	// SQLite refuses to overwrite an existing target, and it is right to: a
	// backup that silently replaced yesterday's good copy with today's broken
	// one would be worse than no backup.
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("backup destination %s already exists", destPath)
	}

	if dir := filepath.Dir(destPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	db, err := Open(sourcePath)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "VACUUM INTO "+quoteLiteral(destPath)); err != nil {
		return fmt.Errorf("vacuum into %s: %w", destPath, err)
	}

	return nil
}

// quoteLiteral escapes a string for inclusion in SQL. VACUUM INTO does not
// accept a bind parameter in every SQLite build, so the path has to be inlined,
// and a path is attacker-influenced often enough to be worth doing properly.
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
