//
// prune_test.go
// That the prune on every write batch touches only the rows it deletes.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// pruneDatabase is one migrated account database.
func pruneDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "account.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.Account()); err != nil {
		t.Fatal(err)
	}

	return db
}

// TestThePruneSeeksRatherThanScans is what stops the cost of writing a batch
// growing with the site's own traffic.
//
// The read path's index starts (site_id, user_id, …), so a query about a site
// and a time can only seek to the site and then read every row it has. A plan
// assertion is the only thing that catches that coming back: the results are
// identical either way, and the difference is invisible until one customer gets
// popular.
func TestThePruneSeeksRatherThanScans(t *testing.T) {
	db := pruneDatabase(t)
	ctx := context.Background()

	for name, query := range map[string]struct {
		sql  string
		want string
	}{
		"reading expired orphans": {
			sql:  "SELECT payload FROM ingest_orphan_engagements WHERE site_id = ? AND timestamp < ?",
			want: "ingest_orphan_engagements_expiry",
		},
		"deleting expired orphans": {
			sql:  "DELETE FROM ingest_orphan_engagements WHERE site_id = ? AND timestamp < ?",
			want: "ingest_orphan_engagements_expiry",
		},
		"deleting expired session state": {
			sql:  "DELETE FROM ingest_session_state WHERE site_id = ? AND last_seen_at < ?",
			want: "ingest_session_state_expiry",
		},
	} {
		plan := queryPlan(t, ctx, db, query.sql)

		if !strings.Contains(plan, query.want) {
			t.Errorf("%s does not use %s:\n%s", name, query.want, plan)
		}

		// A seek, not a scan of everything the site has.
		if !strings.Contains(plan, "SEARCH") || strings.Contains(plan, "SCAN") {
			t.Errorf("%s scans rather than seeking:\n%s", name, plan)
		}
	}
}

// TestTheVisitorLookupKeepsItsOwnIndex is the other half. The read path asks
// about one visitor, and the new indexes must not have made SQLite prefer one
// of them for that.
func TestTheVisitorLookupKeepsItsOwnIndex(t *testing.T) {
	db := pruneDatabase(t)
	ctx := context.Background()

	for name, query := range map[string]struct {
		sql  string
		want string
	}{
		"loading a visitor's session state": {
			sql: `SELECT payload FROM ingest_session_state
			      WHERE site_id = ? AND user_id = ? AND last_seen_at >= ?`,
			want: "ingest_session_state_visitor",
		},
		"adopting a visitor's parked pings": {
			sql: `SELECT payload FROM ingest_orphan_engagements
			      WHERE site_id = ? AND user_id = ? AND timestamp >= ?`,
			want: "ingest_orphan_engagements_visitor",
		},
	} {
		if plan := queryPlan(t, ctx, db, query.sql); !strings.Contains(plan, query.want) {
			t.Errorf("%s no longer uses %s:\n%s", name, query.want, plan)
		}
	}
}

// queryPlan asks SQLite how it would run one statement. The placeholders are
// filled with zeroes: the plan depends on the shape of the query, not on the
// values.
func queryPlan(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()

	args := make([]any, strings.Count(query, "?"))
	for i := range args {
		args[i] = 0
	}

	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}

	defer func() { _ = rows.Close() }()

	var plan strings.Builder

	for rows.Next() {
		var id, parent, notused int
		var detail string

		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}

		fmt.Fprintln(&plan, detail)
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return plan.String()
}

// TestThePruneCostDoesNotGrowWithTheSitesHistory is the property the indexes
// buy, measured rather than inferred from a plan.
//
// A site with a hundred times more live sessions and the same one expired row
// has to cost about the same. Without the index the second case reads a hundred
// times more index entries, so the threshold is loose enough to survive a busy
// machine and still an order of magnitude below the regression.
func TestThePruneCostDoesNotGrowWithTheSitesHistory(t *testing.T) {
	ctx := context.Background()

	taken := map[int]time.Duration{}

	for _, live := range []int{200, 20_000} {
		db := pruneDatabase(t)
		seedSessions(t, ctx, db, live)

		// Warm, so the first run's page faults are not the measurement.
		for range 3 {
			if _, err := db.ExecContext(ctx,
				"DELETE FROM ingest_session_state WHERE site_id = ? AND last_seen_at < ?", 1, 0); err != nil {
				t.Fatal(err)
			}
		}

		started := time.Now()

		for range 200 {
			if _, err := db.ExecContext(ctx,
				"DELETE FROM ingest_session_state WHERE site_id = ? AND last_seen_at < ?", 1, 0); err != nil {
				t.Fatal(err)
			}
		}

		taken[live] = time.Since(started)
	}

	// A hundredfold more history. Ten times the cost is already far past
	// anything an index seek explains, and far below the hundredfold a scan
	// produces.
	if taken[20_000] > taken[200]*10 {
		t.Errorf("pruning took %v at 200 sessions and %v at 20,000 — the cost is following the history",
			taken[200], taken[20_000])
	}
}

// seedSessions fills one site with live session state.
func seedSessions(t *testing.T, ctx context.Context, db *sql.DB, rows int) {
	t.Helper()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO ingest_session_state (site_id, user_id, started_at, last_seen_at, payload)
		VALUES (1, ?, ?, ?, x'00')`)
	if err != nil {
		t.Fatal(err)
	}

	defer insert.Close() //nolint:errcheck // the statement dies with the transaction

	// Every row is in the future relative to the cutoff below, so the delete
	// removes nothing and what is measured is only what it had to look at.
	for i := range rows {
		if _, err := insert.ExecContext(ctx, i, 1_000_000, 1_000_000); err != nil {
			t.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
