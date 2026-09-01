//
// indexes.go
// Dropping the fact-table indexes for the load and rebuilding them afterwards.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// factTables are the two tables a seed run writes millions of rows into. Their
// dimension tables are tiny and their indexes cost nothing to maintain, so only
// these two are worth suspending.
var factTables = []string{"events", "sessions"}

// keptColumn names the column whose index has to survive the load. When an
// out-of-order event bridges two visits the fold merges them, and the repair
// updates every event row of the absorbed session — which without an index on
// session_id is a full scan of the fact table, once per merge.
const keptColumn = "session_id"

// suspendedIndex is one index and the statement that recreates it.
type suspendedIndex struct {
	name string
	sql  string
}

// suspendIndexes drops the secondary indexes on the fact tables and returns the
// function that puts them back.
//
// Five b-trees per event row, each insert landing in a different place in each
// of them, is most of the cost of a bulk load. Building them once at the end
// from sorted data is several times faster, and it is the difference between a
// million pageviews in a minute and a million pageviews in four.
//
// The definitions are read out of the schema rather than written here, so the
// rebuild cannot drift from the migration that created them. The one thing this
// does change is packing: an index built in one pass is denser than one grown a
// row at a time, so a seeded database is a slightly optimistic measurement of a
// database that has been written to for months.
func suspendIndexes(ctx context.Context, db *sql.DB) ([]suspendedIndex, error) {
	var suspended []suspendedIndex

	for _, table := range factTables {
		rows, err := db.QueryContext(ctx, `
			SELECT name, sql FROM sqlite_master
			WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL`, table)
		if err != nil {
			return nil, fmt.Errorf("seed: read indexes: %w", err)
		}

		for rows.Next() {
			var index suspendedIndex

			if scanErr := rows.Scan(&index.name, &index.sql); scanErr != nil {
				readErr := fmt.Errorf("seed: read indexes: %w", scanErr)
				if closeErr := rows.Close(); closeErr != nil {
					readErr = errors.Join(readErr, fmt.Errorf("seed: close index rows: %w", closeErr))
				}
				return nil, readErr
			}

			// Matched on the definition rather than the name, so renaming an
			// index in a migration makes this keep working rather than make the
			// seed mysteriously slow.
			if strings.Contains(index.sql, keptColumn) {
				continue
			}

			suspended = append(suspended, index)
		}

		if rowErr := rows.Err(); rowErr != nil {
			readErr := fmt.Errorf("seed: read indexes: %w", rowErr)
			if closeErr := rows.Close(); closeErr != nil {
				readErr = errors.Join(readErr, fmt.Errorf("seed: close index rows: %w", closeErr))
			}
			return nil, readErr
		}

		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("seed: close index rows: %w", err)
		}
	}

	for _, index := range suspended {
		// The name comes from sqlite_master rather than from a caller, which is
		// what makes interpolating it here safe — an identifier cannot be a
		// bind parameter.
		if _, err := db.ExecContext(ctx, "DROP INDEX IF EXISTS "+quoteIdentifier(index.name)); err != nil {
			return nil, fmt.Errorf("seed: drop index %s: %w", index.name, err)
		}
	}

	return suspended, nil
}

// restoreIndexes rebuilds what suspendIndexes dropped. It runs whether the run
// succeeded or failed: a database left without its indexes would still answer
// every query, slowly and silently, which is the worst of both outcomes.
func restoreIndexes(ctx context.Context, db *sql.DB, suspended []suspendedIndex) error {
	for _, index := range suspended {
		if _, err := db.ExecContext(ctx, index.sql); err != nil {
			return fmt.Errorf("seed: rebuild index %s: %w", index.name, err)
		}
	}

	return nil
}

// quoteIdentifier wraps an index name for SQL. SQL escapes an embedded quote by
// doubling it, which is not what Go's %q would produce.
func quoteIdentifier(name string) string {
	quoted := make([]byte, 0, len(name)+2)
	quoted = append(quoted, '"')

	for i := 0; i < len(name); i++ {
		if name[i] == '"' {
			quoted = append(quoted, '"')
		}
		quoted = append(quoted, name[i])
	}

	return string(append(quoted, '"'))
}
