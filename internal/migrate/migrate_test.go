//
// migrate_test.go
// Tests for the migration runner and the embedded schema it applies.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newDatabase opens an empty database in a temporary directory. Every test here
// starts from a file that has never been migrated, which is the state a fresh
// install and a brand-new account both begin in.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// tableExists reports whether the schema holds a table. Asserting on the schema
// rather than on the runner's return value is what proves the SQL actually ran,
// as opposed to the version merely being stamped.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int

	query := "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	if err := db.QueryRowContext(context.Background(), query, name).Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count > 0
}

// TestControlMigratesAFreshDatabase is the first command a new install runs. If
// this does not produce a complete control schema, nobody can even sign up.
func TestControlMigratesAFreshDatabase(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	result, err := Run(ctx, db, Control())
	if err != nil {
		t.Fatal(err)
	}

	if result.From != 0 || result.To != Control().Version() {
		t.Fatalf("migrated from %d to %d, want 0 to %d", result.From, result.To, Control().Version())
	}
	if !result.Changed() {
		t.Fatal("a fresh database reported no changes")
	}

	for _, table := range []string{
		"users", "user_sessions", "teams", "team_memberships", "team_invitations",
		"site_folders", "sites", "guest_memberships", "subscriptions",
		"usage_counters", "api_keys", "shared_links", "salts", "jobs",
		"email_verification_codes", "password_reset_tokens",
	} {
		if !tableExists(t, db, table) {
			t.Errorf("control schema is missing %s", table)
		}
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != Control().Version() {
		t.Fatalf("schema version is %d, want %d", version, Control().Version())
	}
}

// TestControlUpgradesPopulatedBillingFromFiveToSix proves the production shape
// keeps existing customer, subscription, lifecycle, and event data while the
// durable payment-ordering tables and defaults are added.
func TestControlUpgradesPopulatedBillingFromFiveToSix(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	throughFive := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version <= 5 {
			throughFive.Migrations = append(throughFive.Migrations, migration)
		}
	}
	if _, err := Run(ctx, db, throughFive); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, created_at, updated_at)
		VALUES (1, 'owner@example.com', 1, 1);
		INSERT INTO teams (id, name, created_at, updated_at)
		VALUES (1, 'Existing account', 1, 1);
		INSERT INTO team_memberships (team_id, user_id, role, created_at)
		VALUES (1, 1, 'owner', 1);
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, billing_email, created_at, updated_at)
		VALUES (1, 'cus_existing', 'sub_existing', 'active', 'monthly',
		        'price_monthly', 'billing@example.com', 1, 1);
		INSERT INTO account_lifecycle
			(team_id, trigger, started_at, created_at, updated_at)
		VALUES (1, 'lapse', 10, 10, 10);
		INSERT INTO stripe_events
			(event_id, type, team_id, payload, received_at, handled_at, outcome)
		VALUES ('evt_existing', 'invoice.payment_failed', 1, '{}', 10, 10, 'applied');
	`); err != nil {
		t.Fatal(err)
	}

	result, err := Run(ctx, db, Control())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != 6 {
		t.Fatalf("upgrade applied %v, want only migration 6", result.Applied)
	}

	var customer, subscription, paymentState string
	var failedAt sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, stripe_subscription_id, payment_state, payment_failed_at
		FROM subscriptions WHERE team_id = 1
	`).Scan(&customer, &subscription, &paymentState, &failedAt); err != nil {
		t.Fatal(err)
	}
	if customer != "cus_existing" || subscription != "sub_existing" || paymentState != "" || failedAt.Valid {
		t.Fatalf("populated subscription changed to customer=%q subscription=%q payment=%q failed_at=%v",
			customer, subscription, paymentState, failedAt)
	}

	for _, table := range []string{"billing_account_leases", "billing_checkouts"} {
		if !tableExists(t, db, table) {
			t.Errorf("upgraded control schema is missing %s", table)
		}
	}

	var events, clocks int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM stripe_events WHERE event_id = 'evt_existing'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_lifecycle WHERE team_id = 1").Scan(&clocks); err != nil {
		t.Fatal(err)
	}
	if events != 1 || clocks != 1 {
		t.Fatalf("upgrade retained events=%d clocks=%d, want one each", events, clocks)
	}
}

// TestAccountMigratesAFreshDatabase covers the schema every event is written
// into, including the two decisions that are cheap now and a rewrite later: the
// interned dimension tables and the hot/cold split.
func TestAccountMigratesAFreshDatabase(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Account()); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"events", "event_details", "sessions", "dim_pathname", "dim_source", "dim_event_name"} {
		if !tableExists(t, db, table) {
			t.Errorf("account schema is missing %s", table)
		}
	}

	// The three filter indexes are the whole reason a filtered query does not
	// fall back to a full scan, and an index is easy to drop by accident in a
	// later migration.
	for _, index := range []string{"events_main", "events_session", "events_page", "events_source", "events_country"} {
		var count int

		query := "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?"
		if err := db.QueryRowContext(ctx, query, index).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Errorf("account schema is missing index %s", index)
		}
	}
}

// TestEveryDimensionTableSeedsTheEmptyString pins the rule the whole ingest
// path depends on: id 0 is the empty string everywhere, so "not set" is an
// ordinary id and no query needs a NULL branch.
func TestEveryDimensionTableSeedsTheEmptyString(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Account()); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'dim_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(tables) == 0 {
		t.Fatal("no dimension tables were created")
	}

	for _, table := range tables {
		var value string

		if err := db.QueryRowContext(ctx, "SELECT value FROM "+table+" WHERE id = 0").Scan(&value); err != nil {
			t.Errorf("%s has no id 0: %v", table, err)
			continue
		}
		if value != "" {
			t.Errorf("%s id 0 is %q, want the empty string", table, value)
		}
	}
}

// TestRunningTwiceChangesNothing is the property that makes an interrupted run
// resumable: migrate is re-run until it succeeds, so a second pass over an
// already-current database has to be a no-op rather than an error.
func TestRunningTwiceChangesNothing(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Account()); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO dim_pathname (value) VALUES ('/pricing')"); err != nil {
		t.Fatal(err)
	}

	result, err := Run(ctx, db, Account())
	if err != nil {
		t.Fatal(err)
	}

	if result.Changed() {
		t.Fatalf("a second run applied %v", result.Applied)
	}
	if result.From != result.To {
		t.Fatalf("a second run moved the version from %d to %d", result.From, result.To)
	}

	// Data written between the two runs has to survive, or "idempotent" would
	// mean "rebuilt from scratch every time".
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM dim_pathname WHERE value = '/pricing'").Scan(&value); err != nil {
		t.Fatalf("the second run lost data: %v", err)
	}
}

// TestRunRefusesADatabaseFromANewerBuild covers a rolled-back deploy. Running
// queries against a schema this binary does not know about would fail somewhere
// far away from the cause, so it is refused up front.
func TestRunRefusesADatabaseFromANewerBuild(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if err := store.SetSchemaVersion(ctx, db, 999); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, db, Control()); err == nil {
		t.Fatal("expected an error for a database newer than the binary")
	}
}

// TestFreshEmptiesAndRebuilds covers the development flag. It has to leave a
// database that migrates cleanly again, otherwise the only way out is deleting
// files by hand.
func TestFreshEmptiesAndRebuilds(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Control()); err != nil {
		t.Fatal(err)
	}

	// A row with a foreign key pointing at it, so the drop order actually
	// matters rather than being trivially safe.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Acme', 0, 0);
		INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', 0, 0);
	`); err != nil {
		t.Fatal(err)
	}

	if err := Fresh(ctx, db); err != nil {
		t.Fatal(err)
	}

	if tableExists(t, db, "users") {
		t.Fatal("fresh left the schema behind")
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("fresh left the version at %d", version)
	}

	result, err := Run(ctx, db, Control())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed() {
		t.Fatal("the rebuilt database applied nothing")
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sites").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh kept %d rows", count)
	}
}

// TestForeignKeysSurviveFresh checks the pinned connection is handed back the
// way every other connection in the process is configured. A pool connection
// that quietly lost foreign key enforcement would let orphans through days
// later, with nothing pointing back at this command.
func TestForeignKeysSurviveFresh(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Control()); err != nil {
		t.Fatal(err)
	}
	if err := Fresh(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, db, Control()); err != nil {
		t.Fatal(err)
	}

	// team_memberships has no team 42, so this must be refused.
	_, err := db.ExecContext(ctx, "INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (42, 42, 'owner', 0)")
	if err == nil {
		t.Fatal("foreign keys are not being enforced after fresh")
	}
}

// TestSetsAreOrdered guards the loader. Migrations applying out of order, or a
// version appearing twice, is a class of bug that only shows up on someone
// else's filesystem.
func TestSetsAreOrdered(t *testing.T) {
	for _, set := range []Set{Control(), Account()} {
		if len(set.Migrations) == 0 {
			t.Fatalf("%s has no migrations", set.Name)
		}

		previous := 0
		for _, migration := range set.Migrations {
			if migration.Version <= previous {
				t.Fatalf("%s: version %d follows %d", set.Name, migration.Version, previous)
			}
			if migration.SQL == "" {
				t.Fatalf("%s: migration %d is empty", set.Name, migration.Version)
			}

			previous = migration.Version
		}

		if set.Version() != previous {
			t.Fatalf("%s reports version %d, want %d", set.Name, set.Version(), previous)
		}
	}
}

// TestParseName covers the filename rules. The version lives in the filename so
// it is visible in a diff, which only works if a malformed name is rejected
// rather than guessed at.
func TestParseName(t *testing.T) {
	version, name, err := parseName("0012_add_rollups.sql")
	if err != nil {
		t.Fatal(err)
	}
	if version != 12 || name != "add_rollups" {
		t.Fatalf("got %d %q", version, name)
	}

	for _, bad := range []string{"initial.sql", "abc_initial.sql", "0000_zero.sql"} {
		if _, _, err := parseName(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestPublicAPIMigrationKeepsExistingKeys runs the control migrations in two
// halves with data written in between.
//
// The 0004 migration rebuilds api_keys to change a column default, which SQLite
// can only express by writing the table again — and a table rebuild is the one
// migration shape that can silently drop rows. Migrating a fresh database would
// never notice, because there is nothing in the table to lose.
func TestPublicAPIMigrationKeepsExistingKeys(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Stop short of 0004, so the database is at the shape somebody upgrading
	// from an earlier build actually has.
	earlier := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version < 3 {
			earlier.Migrations = append(earlier.Migrations, migration)
		}
	}

	if _, err := Run(ctx, db, earlier); err != nil {
		t.Fatal(err)
	}

	seed := []string{
		`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', 0, 0)`,
		`INSERT INTO users (id, email, created_at, updated_at) VALUES (1, 'a@example.test', 0, 0)`,
		`INSERT INTO api_keys (id, team_id, user_id, name, key_hash, scopes, hourly_limit, created_at)
		 VALUES (1, 1, 1, 'existing', 'deadbeef', '["stats:read"]', 600, 0)`,
	}

	for _, statement := range seed {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	if _, err := Run(ctx, db, Control()); err != nil {
		t.Fatalf("the 0004 migration failed on a database with data in it: %v", err)
	}

	var (
		name   string
		hash   string
		scopes string
		limit  int
	)

	if err := db.QueryRow(
		`SELECT name, key_hash, scopes, hourly_limit FROM api_keys WHERE id = 1`).
		Scan(&name, &hash, &scopes, &limit); err != nil {
		t.Fatalf("the existing key did not survive the rebuild: %v", err)
	}

	if name != "existing" || hash != "deadbeef" || scopes != `["stats:read"]` {
		t.Errorf("the key came back as %q/%q/%q", name, hash, scopes)
	}

	// The old hard-coded 600 becomes "take the deployment's configured limit",
	// which is the whole point of the rebuild: the shipped default moves without
	// anybody editing a database by hand.
	if limit != 0 {
		t.Errorf("hourly_limit = %d, want 0 meaning the configured default", limit)
	}

	// The new columns exist and the new default applies to a key created after
	// the migration.
	if _, err := db.Exec(
		`INSERT INTO api_keys (team_id, user_id, key_hash, key_prefix, created_at) VALUES (1, 1, 'cafe', 'feas_ab', 0)`); err != nil {
		t.Fatal(err)
	}

	var fresh sql.NullInt64
	if err := db.QueryRow(`SELECT hourly_limit FROM api_keys WHERE key_hash = 'cafe'`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}

	if fresh.Int64 != 0 {
		t.Errorf("a new key defaulted to %d", fresh.Int64)
	}

	// Everything else the migration adds has to be there too, because a
	// half-applied migration is exactly what the transaction is supposed to make
	// impossible.
	for _, table := range []string{
		"site_tracker_config", "site_custom_properties",
		"webhook_endpoints", "webhook_deliveries",
		"mcp_oauth_clients", "mcp_oauth_codes", "mcp_oauth_tokens",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}

		if count != 1 {
			t.Errorf("table %s was not created", table)
		}
	}
}
