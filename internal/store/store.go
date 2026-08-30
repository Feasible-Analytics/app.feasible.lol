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

// Open opens (and creates, if missing) a SQLite database with the pragmas this
// project needs everywhere. WAL keeps a dashboard read from blocking an ingest
// write, and the busy timeout turns the lock contention that would otherwise
// surface as a hard "database is locked" error into a short wait.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)

	db, err := sql.Open(DriverName, dsn)
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
// carries one — control.db and one per account — and a header field needs no
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
