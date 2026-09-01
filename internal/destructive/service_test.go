//
// service_test.go
// Regression and race coverage for durable destructive site workflows.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package destructive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// destructiveFixture is one control database and its immutable analytics
// account, shared by the workflow and transfer store under test.
type destructiveFixture struct {
	db       *sql.DB
	manager  *accounts.Manager
	service  *Service
	team     *teams.Store
	now      time.Time
	account  *accounts.Account
	actorID  int64
	siteID   int64
	ownerID  int64
	targetID int64
}

// newDestructiveFixture creates two teams the same owner can transfer between.
func newDestructiveFixture(t *testing.T) *destructiveFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	controlPath := filepath.Join(dir, "control.db")
	db, err := store.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := migrate.Run(ctx, db, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	for _, statement := range []string{
		`INSERT INTO users (id, email, created_at, updated_at) VALUES (1, 'owner@example.test', 1, 1)`,
		`INSERT INTO teams (id, name, created_at, updated_at) VALUES (10, 'Owner', 1, 1), (20, 'Target', 1, 1)`,
		`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (10, 1, 'owner', 1), (20, 1, 'owner', 1)`,
		`INSERT INTO sites (id, account_id, owner_team_id, domain, created_at, updated_at) VALUES (30, 10, 10, 'example.test', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})
	account, err := manager.Open(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at)
		VALUES (1, 30, 99, 1, 1)
	`); err != nil {
		t.Fatal(err)
	}

	fixture := &destructiveFixture{
		db: db, manager: manager, now: now, account: account,
		actorID: 1, siteID: 30, ownerID: 10, targetID: 20,
	}
	fixture.service = &Service{
		DB: db, Accounts: manager, Lease: time.Second,
		Now: func() time.Time { return fixture.now },
	}

	// The transfer uses a separately opened SQLite pool, matching independent
	// application processes instead of relying on one sql.DB's connection pool.
	transferDB, err := store.Open(controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transferDB.Close() })
	fixture.team = teams.NewStore(transferDB)
	fixture.team.Now = func() time.Time { return fixture.now }

	return fixture
}

// TestSiteDeletionIsDurableAndBlocksTransfer simulates a crash after analytics
// erasure, proves a concurrent transfer is refused by the tombstone, and then
// proves an expired worker retry finishes without orphaning analytics.
func TestSiteDeletionIsDurableAndBlocksTransfer(t *testing.T) {
	f := newDestructiveFixture(t)
	ctx := context.Background()

	operation, err := f.service.claimSite(ctx, f.ownerID, f.siteID, KindSiteDelete)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.eraseSite(ctx, operation); err != nil {
		t.Fatal(err)
	}

	if err := f.team.TransferSite(ctx, f.actorID, f.siteID, f.targetID); !errors.Is(err, teams.ErrOperationInProgress) {
		t.Fatalf("transfer during deletion = %v, want operation-in-progress", err)
	}

	// The first worker dies here. Once its lease expires, the normal API-level
	// operation reclaims the tombstone and repeats the idempotent erasure.
	f.now = f.now.Add(2 * time.Second)
	if err := f.service.DeleteSite(ctx, f.ownerID, f.siteID); err != nil {
		t.Fatalf("retry delete: %v", err)
	}

	var sessions, sites, tombstones int
	if err := f.account.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE site_id = ?`, f.siteID).
		Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites WHERE id = ?`, f.siteID).Scan(&sites); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM destructive_operations WHERE resource_type = 'site' AND resource_id = ?
	`, f.siteID).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || sites != 0 || tombstones != 0 {
		t.Fatalf("after retry sessions/sites/tombstones = %d/%d/%d, want 0/0/0", sessions, sites, tombstones)
	}
}

// TestResetPreservesConfigurationAndDeleteErasesEverything makes the schema
// itself the coverage oracle. Reset follows the exhaustive analytics/config
// policy while deletion removes every direct or inherited site-scoped row.
func TestResetPreservesConfigurationAndDeleteErasesEverything(t *testing.T) {
	for _, operation := range []string{"reset", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newDestructiveFixture(t)
			ctx := context.Background()
			paths := seedEverySiteScopedRow(t, f)

			assertEveryScopedTableSeeded(t, f.account.Writer(), f.siteID, nil)
			assertEveryScopedTableSeeded(t, f.db, f.siteID, map[string]bool{"sites": true})

			var err error
			if operation == "reset" {
				err = f.service.ResetSite(ctx, f.ownerID, f.siteID)
			} else {
				err = f.service.DeleteSite(ctx, f.ownerID, f.siteID)
			}
			if err != nil {
				t.Fatalf("%s site: %v", operation, err)
			}

			if operation == "reset" {
				assertResetDisposition(t, f.account.Writer(), f.siteID, nil, accountResetDisposition)
				assertResetDisposition(t, f.db, f.siteID, map[string]bool{"sites": true}, controlResetDisposition)
				if _, err := os.Stat(paths[0]); err != nil {
					t.Fatalf("retained import file disappeared during reset: %v", err)
				}
				if _, err := os.Stat(paths[1]); !os.IsNotExist(err) {
					t.Fatalf("generated export survived reset: %v", err)
				}
			} else {
				assertNoScopedRows(t, f.account.Writer(), f.siteID, nil)
				assertNoScopedRows(t, f.db, f.siteID, map[string]bool{"sites": true})
				for _, path := range paths {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("site file %s survived delete: %v", path, err)
					}
				}
			}

			var sites int
			if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites WHERE id = ?`, f.siteID).Scan(&sites); err != nil {
				t.Fatal(err)
			}
			wantSites := 1
			if operation == "delete" {
				wantSites = 0
			}
			if sites != wantSites {
				t.Fatalf("site rows after %s = %d, want %d", operation, sites, wantSites)
			}

			// Topology migration 0009 made dedupe receipts account-global, so a
			// site reset or deletion cannot safely attribute and erase them.
			var receipts int
			if err := f.account.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM recent_event_ids`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if receipts != 1 {
				t.Fatalf("account-global receipts after %s = %d, want 1", operation, receipts)
			}

			if operation == "delete" {
				if _, err := f.db.ExecContext(ctx, `
					INSERT INTO sites (id, account_id, owner_team_id, domain, created_at, updated_at)
					VALUES (?, ?, ?, 'reused.example', ?, ?)
				`, f.siteID, f.ownerID, f.ownerID, f.now.Unix(), f.now.Unix()); err != nil {
					t.Fatalf("reuse site id: %v", err)
				}
				assertNoScopedRows(t, f.account.Writer(), f.siteID, nil)
				assertNoScopedRows(t, f.db, f.siteID, map[string]bool{"sites": true})
			}
		})
	}
}

// TestResetFailsClosedForNewSiteScopedTables proves schema growth cannot turn
// an analytics reset into an accidental configuration deletion.
func TestResetFailsClosedForNewSiteScopedTables(t *testing.T) {
	for _, database := range []string{"account", "control"} {
		t.Run(database, func(t *testing.T) {
			f := newDestructiveFixture(t)
			db := f.account.Writer()
			if database == "control" {
				db = f.db
			}
			if _, err := db.Exec(`
				CREATE TABLE future_site_configuration (
					id INTEGER PRIMARY KEY,
					site_id INTEGER NOT NULL,
					value TEXT NOT NULL
				);
				INSERT INTO future_site_configuration (site_id, value) VALUES (?, 'keep me');
			`, f.siteID); err != nil {
				t.Fatal(err)
			}

			err := f.service.ResetSite(context.Background(), f.ownerID, f.siteID)
			if err == nil || !strings.Contains(err.Error(), "unclassified site-scoped table future_site_configuration") {
				t.Fatalf("reset with future %s table = %v", database, err)
			}
			var rows int
			if err := db.QueryRow(`SELECT COUNT(*) FROM future_site_configuration WHERE site_id = ?`, f.siteID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("future %s configuration was deleted", database)
			}
		})
	}
}

// seedEverySiteScopedRow writes one row into every account and control table
// that the current schema ties to a site, including dependent child tables.
func seedEverySiteScopedRow(t *testing.T, f *destructiveFixture) []string {
	t.Helper()
	ctx := context.Background()
	upload := filepath.Join(t.TempDir(), "import.csv")
	export := filepath.Join(t.TempDir(), "export.csv")
	for _, path := range []string{upload, export} {
		if err := os.WriteFile(path, []byte("site data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	accountStatements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, has_details) VALUES (101, ?, 1, 1, 1, 1, 1)`, []any{f.siteID}},
		{`INSERT INTO event_details (event_id, props) VALUES (101, '{}')`, nil},
		{`INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at) VALUES (102, ?, 1, 1, 1)`, []any{f.siteID}},
		{`INSERT INTO recent_event_ids (event_uuid, received_at) VALUES (X'0102', 1)`, nil},
		{`INSERT INTO ingest_session_state (session_id, site_id, user_id, started_at, last_seen_at, payload) VALUES (102, ?, 1, 1, 1, X'01')`, []any{f.siteID}},
		{`INSERT INTO ingest_orphan_engagements (event_uuid, site_id, user_id, timestamp, payload) VALUES (X'0304', ?, 1, 1, X'01')`, []any{f.siteID}},
		{`INSERT INTO hostname_rejections (site_id, hostname, day, events) VALUES (?, 'preview.example', 1, 1)`, []any{f.siteID}},
		{`INSERT INTO rollup_state (site_id, grain, timezone, covered_from, covered_through, built_at) VALUES (?, 0, 'UTC', 1, 2, 2)`, []any{f.siteID}},
		{`INSERT INTO goals (id, site_id, kind, page_pattern, created_at, signature) VALUES (103, ?, 'page', '/', 1, 'page:/')`, []any{f.siteID}},
		{`INSERT INTO goal_properties (goal_id, name, value) VALUES (103, 'plan', 'paid')`, nil},
		{`INSERT INTO allowed_properties (site_id, name, scope, created_at) VALUES (?, 'plan', 'event', 1)`, []any{f.siteID}},
		{`INSERT INTO funnels (id, site_id, name, created_at) VALUES (104, ?, 'Checkout', 1)`, []any{f.siteID}},
		{`INSERT INTO funnel_steps (funnel_id, position, goal_id) VALUES (104, 1, 103)`, nil},
		{`INSERT INTO imports (id, site_id, source, label, upload_path, created_at) VALUES (105, ?, 'csv', 'history', ?, 1)`, []any{f.siteID, upload}},
		{`INSERT INTO imported_rollups (import_id, site_id, timestamp) VALUES (105, ?, 1)`, []any{f.siteID}},
		{`INSERT INTO search_console_daily (site_id, timestamp, query) VALUES (?, 1, 'query')`, []any{f.siteID}},
		{`INSERT INTO exports (site_id, token_hash, path, created_at, expires_at) VALUES (?, 'export-token', ?, 1, 2)`, []any{f.siteID, export}},
		{`INSERT INTO shield_rules (site_id, kind, value, created_at) VALUES (?, 'page', '/private', 1)`, []any{f.siteID}},
		{`INSERT INTO path_clean_rules (site_id, position, pattern, replacement, created_at) VALUES (?, 1, '/x', '/y', 1)`, []any{f.siteID}},
		{`INSERT INTO path_clean_map (site_id, source_id, target_id) VALUES (?, 1, 2)`, []any{f.siteID}},
		{`INSERT INTO google_connections (site_id, account_id, provider, created_at, updated_at) VALUES (?, ?, 'ga4', 1, 1)`, []any{f.siteID, f.ownerID}},
		{`INSERT INTO annotations (site_id, shown_on, body, created_at, updated_at) VALUES (?, '2026-08-31', 'launch', 1, 1)`, []any{f.siteID}},
		{`INSERT INTO ingest_health (site_id, observed_at, kind, count) VALUES (?, 1, 'accepted', 1)`, []any{f.siteID}},
		{`INSERT INTO ingest_last_request (site_id, received_at) VALUES (?, 1)`, []any{f.siteID}},
		{`INSERT INTO ingest_observations (site_id, observed_at, kind, value, count, first_seen_at, last_seen_at) VALUES (?, 1, 'ip_source', 'socket', 1, 1, 1)`, []any{f.siteID}},
	}
	rollups := []string{
		"rollup_visitors", "rollup_sources", "rollup_pages", "rollup_entry_pages",
		"rollup_exit_pages", "rollup_locations", "rollup_devices", "rollup_browsers",
		"rollup_operating_systems", "rollup_languages", "rollup_custom_events",
	}
	for _, table := range rollups {
		accountStatements = append(accountStatements, struct {
			query string
			args  []any
		}{"INSERT INTO " + quoteIdentifier(table) + " (site_id, grain, bucket, dimension, value_id) VALUES (?, 0, 1, 0, 0)", []any{f.siteID}})
	}
	for _, statement := range accountStatements {
		if _, err := f.account.Writer().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed account %q: %v", statement.query, err)
		}
	}

	controlStatements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users (id, email, created_at, updated_at) VALUES (2, 'guest@example.test', 1, 1)`, nil},
		{`INSERT INTO guest_memberships (site_id, user_id, role, created_at) VALUES (?, 2, 'guest_viewer', 1)`, []any{f.siteID}},
		{`INSERT INTO shared_links (site_id, name, slug, created_at) VALUES (?, 'share', 'site-share', 1)`, []any{f.siteID}},
		{`INSERT INTO share_password_attempts (link_id, source_key, window_started_at, attempts) SELECT id, 'source', 1, 1 FROM shared_links WHERE slug = 'site-share'`, nil},
		{`INSERT INTO site_tracker_config (site_id, updated_at) VALUES (?, 1)`, []any{f.siteID}},
		{`INSERT INTO site_custom_properties (site_id, key, created_at) VALUES (?, 'plan', 1)`, []any{f.siteID}},
		{`INSERT INTO webhook_endpoints (id, team_id, site_id, url, secret, created_at, updated_at) VALUES (201, ?, ?, 'https://hooks.example.test', 'secret', 1, 1)`, []any{f.ownerID, f.siteID}},
		{`INSERT INTO webhook_deliveries (endpoint_id, event_id, event_type, payload, created_at) VALUES (201, 'event-1', 'pageview', '{}', 1)`, nil},
		{`INSERT INTO team_invitations (team_id, site_id, email, role, token_hash, invited_by_user_id, created_at, expires_at) VALUES (?, ?, 'invite@example.test', 'guest_viewer', 'invite-token', ?, 1, 2)`, []any{f.ownerID, f.siteID, f.actorID}},
		{`INSERT INTO site_allowed_hostnames (site_id, hostname, created_at) VALUES (?, 'www.example.test', 1)`, []any{f.siteID}},
		{`INSERT INTO saved_segments (site_id, name, created_at) VALUES (?, 'segment', 1)`, []any{f.siteID}},
		{`INSERT INTO report_subscriptions (site_id, kind, recipients, created_at, updated_at) VALUES (?, 'weekly', '[]', 1, 1)`, []any{f.siteID}},
		{`INSERT INTO alert_rules (site_id, kind, threshold, created_at, updated_at) VALUES (?, 'spike', 10, 1, 1)`, []any{f.siteID}},
		{`INSERT INTO notifications_sent (site_id, kind, period_key, recipients, sent_at) VALUES (?, 'drop', '', 1, 1)`, []any{f.siteID}},
		{`INSERT INTO notification_claims (id, site_id, kind, period_key, state, lease_token, lease_until, payload, created_at) VALUES (202, ?, 'monthly', '2026-08', 'pending', 'lease', 2, '{}', 1)`, []any{f.siteID}},
		{`INSERT INTO notification_destinations (notification_id, destination_key, channel, target) VALUES (202, 'email:owner@example.test', 'email', 'owner@example.test')`, nil},
		{`INSERT INTO jobs (id, queue, kind, args, state, max_attempts, scheduled_at, site_id) VALUES (203, 'imports', 'import.csv', '{"site_id":30}', 'available', 3, 1, ?)`, []any{f.siteID}},
		{`INSERT INTO cron_slots (id, queue, kind, bucket, job_id, created_at) VALUES (204, 'imports', 'import.csv', 1, 203, 1)`, nil},
	}
	for _, statement := range controlStatements {
		if _, err := f.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed control %q: %v", statement.query, err)
		}
	}

	return []string{upload, export}
}

// assertResetDisposition checks every schema-derived table against the exact
// reset classification and fails if the schema contains an unclassified table.
func assertResetDisposition(t *testing.T, db *sql.DB, siteID int64, excluded map[string]bool,
	dispositions map[string]resetDisposition) {
	t.Helper()
	counts := scopedRowCounts(t, db, siteID, excluded)
	for table, count := range counts {
		disposition, classified := dispositions[table]
		if !classified {
			t.Errorf("site-scoped table %s has no reset classification", table)
			continue
		}
		if disposition == eraseOnReset && count != 0 {
			t.Errorf("analytics table %s retained %d rows", table, count)
		}
		if disposition == preserveOnReset && count == 0 {
			t.Errorf("configuration table %s was erased", table)
		}
	}
}

// assertEveryScopedTableSeeded fails when schema discovery finds a scoped
// table the fixture did not populate, forcing future features into this test.
func assertEveryScopedTableSeeded(t *testing.T, db *sql.DB, siteID int64, excluded map[string]bool) {
	t.Helper()
	counts := scopedRowCounts(t, db, siteID, excluded)
	var empty []string
	for table, count := range counts {
		if count == 0 {
			empty = append(empty, table)
		}
	}
	sort.Strings(empty)
	if len(empty) > 0 {
		t.Fatalf("site-scoped tables were not seeded: %s", strings.Join(empty, ", "))
	}
}

// assertNoScopedRows checks every schema-derived predicate after destruction.
func assertNoScopedRows(t *testing.T, db *sql.DB, siteID int64, excluded map[string]bool) {
	t.Helper()
	counts := scopedRowCounts(t, db, siteID, excluded)
	var retained []string
	for table, count := range counts {
		if count > 0 {
			retained = append(retained, fmt.Sprintf("%s=%d", table, count))
		}
	}
	sort.Strings(retained)
	if len(retained) > 0 {
		t.Fatalf("site-scoped rows survived: %s", strings.Join(retained, ", "))
	}
}

// scopedRowCounts applies the same structured schema walk as production and
// returns one count for every direct or inherited site predicate.
func scopedRowCounts(t *testing.T, db *sql.DB, siteID int64, excluded map[string]bool) map[string]int {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only test transaction

	tables, err := readSchemaTables(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]string{}
	visiting := map[string]bool{}
	counts := map[string]int{}
	for name := range tables {
		if excluded[name] {
			continue
		}
		predicate := sitePredicate(name, tables, excluded, known, visiting, strconv.FormatInt(siteID, 10))
		if predicate == "" {
			continue
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(name)+" WHERE "+predicate).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		counts[name] = count
	}

	return counts
}
