//
// accounts_test.go
// Tests for the per-account database manager and the schema it opens.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package accounts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newManager builds a manager over an empty data directory and closes whatever
// it opened. Every test starts from a machine that has never seen this account,
// which is the state an account's first event arrives in.
func newManager(t *testing.T) *Manager {
	t.Helper()

	manager := NewManager(t.TempDir())
	t.Cleanup(func() { manager.CloseAll() })

	return manager
}

// TestOpenCreatesTheDatabaseOnFirstUse covers the path an account's first event
// takes. A missing directory has to be created rather than reported, or signing
// up would work and the first pageview would not.
func TestOpenCreatesTheDatabaseOnFirstUse(t *testing.T) {
	ctx := context.Background()
	manager := newManager(t)

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(manager.Path(1)); err != nil {
		t.Fatalf("the database file was not created: %v", err)
	}

	// The layout is data/accounts/<zero-padded id>/analytics.db, and moving one
	// account between shards means moving that directory.
	if base := filepath.Base(filepath.Dir(manager.Path(1))); base != "000001" {
		t.Fatalf("account directory is %q, want 000001", base)
	}

	version, err := store.SchemaVersion(ctx, account.Writer())
	if err != nil {
		t.Fatal(err)
	}
	if version != migrate.Account().Version() {
		t.Fatalf("a new database opened at version %d, want %d", version, migrate.Account().Version())
	}

	// The cache is warmed on open, so it already holds the empty string every
	// dimension table ships with.
	if got := account.Intern.Size(intern.Pathname); got != 1 {
		t.Fatalf("the dimension cache holds %d values, want the seeded empty string", got)
	}
}

// TestOpenCachesTheHandle checks a second caller gets the same handle. A handle
// per request would mean a writer connection per request, which is the one
// thing SQLite cannot give us.
func TestOpenCachesTheHandle(t *testing.T) {
	ctx := context.Background()
	manager := newManager(t)

	first, err := manager.Open(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}

	second, err := manager.Open(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatal("a second open returned a different handle")
	}
	if manager.OpenCount() != 1 {
		t.Fatalf("%d handles are open, want 1", manager.OpenCount())
	}
}

// TestCloseThenReopen is what happens when an account goes quiet and comes
// back. The data has to still be there, and the reopened handle has to be a
// live one rather than the closed one.
func TestCloseThenReopen(t *testing.T) {
	ctx := context.Background()
	manager := newManager(t)

	account, err := manager.Open(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	id, err := account.Intern.ID(ctx, intern.Source, "Search")
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Close(2); err != nil {
		t.Fatal(err)
	}
	if manager.OpenCount() != 0 {
		t.Fatal("close left the handle in the cache")
	}

	// Closing an account nobody opened is not an error, so cleanup paths do not
	// have to check first.
	if err := manager.Close(2); err != nil {
		t.Fatal(err)
	}

	reopened, err := manager.Open(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	again, err := reopened.Intern.ID(ctx, intern.Source, "Search")
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("the id changed across a close: %d became %d", id, again)
	}
}

// TestOpenRejectsAnInvalidID stops a zero or negative id creating a directory
// that no account will ever own.
func TestOpenRejectsAnInvalidID(t *testing.T) {
	manager := newManager(t)

	if _, err := manager.Open(context.Background(), 0); err == nil {
		t.Fatal("account id 0 was accepted")
	}
}

// TestEnsureSchemaRefusesAnOutOfDateDatabase is the guard that keeps migrations
// off the serving path. Upgrading someone's data as a side effect of an
// incoming event is how two processes end up migrating one file at once.
func TestEnsureSchemaRefusesAnOutOfDateDatabase(t *testing.T) {
	ctx := context.Background()

	db, err := store.OpenDatabase(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := migrate.Run(ctx, db.Writer(), migrate.Account()); err != nil {
		t.Fatal(err)
	}

	// A build that carries one migration more than this database has.
	ahead := migrate.Set{
		Name: "account",
		Migrations: append(append([]migrate.Migration{}, migrate.Account().Migrations...),
			migrate.Migration{Version: migrate.Account().Version() + 1, Name: "later", SQL: "CREATE TABLE later (id INTEGER PRIMARY KEY)"},
		),
	}

	err = ensureSchema(ctx, db, ahead)
	if err == nil {
		t.Fatal("an out-of-date database was opened instead of being refused")
	}

	// The error has to name the fix, because the person reading it is looking
	// at a shard that has stopped accepting events.
	if !strings.Contains(err.Error(), "feasible db migrate") {
		t.Fatalf("the error does not say what to do: %v", err)
	}
}

// TestEventRoundTrip is the whole schema working end to end: intern the
// dimensions, write a hot row and its cold partner, and read back what a
// report would. It is the test that would catch a column added to one and not
// the other.
func TestEventRoundTrip(t *testing.T) {
	ctx := context.Background()
	manager := newManager(t)

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	ids := map[intern.Dimension]int64{}
	for dimension, value := range map[intern.Dimension]string{
		intern.EventName:  "pageview",
		intern.Hostname:   "example.com",
		intern.Pathname:   "/pricing",
		intern.Source:     "Hacker News",
		intern.Country:    "US",
		intern.Browser:    "Firefox",
		intern.DeviceType: "Desktop",
	} {
		id, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		ids[dimension] = id
	}

	result, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (
			site_id, timestamp, name_id, user_id, session_id,
			hostname_id, pathname_id, source_id, country_id, browser_id, device_type_id,
			engagement_time, has_details
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
	`,
		42, 1788130000, ids[intern.EventName], 9001, 5,
		ids[intern.Hostname], ids[intern.Pathname], ids[intern.Source],
		ids[intern.Country], ids[intern.Browser], ids[intern.DeviceType],
		4200,
	)
	if err != nil {
		t.Fatal(err)
	}

	eventID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO event_details (event_id, props, revenue_amount, revenue_currency, utm_content, full_url)
		VALUES (?, ?, ?, ?, ?, ?)
	`, eventID, `{"plan":"pro"}`, 9900, "USD", "sidebar", "https://example.com/pricing?utm_content=sidebar"); err != nil {
		t.Fatal(err)
	}

	// The read a report does: group by the id, and only then join the
	// dimension table for the handful of rows being displayed.
	var (
		path      string
		source    string
		pageviews int
	)

	err = account.Reader().QueryRowContext(ctx, `
		SELECT p.value, s.value, e.pageviews
		FROM (
			SELECT pathname_id, source_id, COUNT(*) AS pageviews
			FROM events
			WHERE site_id = ? AND timestamp >= ? AND name_id = ?
			GROUP BY pathname_id, source_id
		) e
		JOIN dim_pathname p ON p.id = e.pathname_id
		JOIN dim_source s ON s.id = e.source_id
	`, 42, 0, ids[intern.EventName]).Scan(&path, &source, &pageviews)
	if err != nil {
		t.Fatal(err)
	}

	if path != "/pricing" || source != "Hacker News" || pageviews != 1 {
		t.Fatalf("got %q %q %d", path, source, pageviews)
	}

	// The cold row is only fetched when a query asks for it, and has_details on
	// the hot row is what tells the common path not to bother.
	var (
		props   string
		revenue int64
		details int
	)

	err = account.Reader().QueryRowContext(ctx, `
		SELECT e.has_details, d.props, d.revenue_amount
		FROM events e
		JOIN event_details d ON d.event_id = e.id
		WHERE e.id = ?
	`, eventID).Scan(&details, &props, &revenue)
	if err != nil {
		t.Fatal(err)
	}

	if details != 1 || props != `{"plan":"pro"}` || revenue != 9900 {
		t.Fatalf("got %d %q %d", details, props, revenue)
	}

	// An unset dimension is id 0, not NULL, which is the entire reason the
	// schema needs no NULL handling.
	var region int64
	if err := account.Reader().QueryRowContext(ctx, "SELECT region_id FROM events WHERE id = ?", eventID).Scan(&region); err != nil {
		t.Fatal(err)
	}
	if region != intern.EmptyID {
		t.Fatalf("an unset dimension is %d, want %d", region, intern.EmptyID)
	}
}

// TestReaderRefusesToWrite checks the read pool cannot write. A query bug that
// wrote would take the write lock away from ingestion, and query_only turns
// that into an error instead.
func TestReaderRefusesToWrite(t *testing.T) {
	ctx := context.Background()
	manager := newManager(t)

	account, err := manager.Open(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Reader().ExecContext(ctx, "INSERT INTO dim_pathname (value) VALUES ('/nope')"); err == nil {
		t.Fatal("the read pool accepted a write")
	}
}

// TestDiscoverFindsEveryAccount is what `db migrate` and `db backup` walk. A
// data directory people have poked at must not stop the walk, and an account
// that is missed is an account that silently never gets migrated.
func TestDiscoverFindsEveryAccount(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	manager := NewManager(dir)
	defer manager.CloseAll()

	for _, id := range []int64{1, 4, 12} {
		if _, err := manager.Open(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	// Things that are not accounts: a backup directory, and an account
	// directory with no database in it yet.
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "backups"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "000099"), 0o755); err != nil {
		t.Fatal(err)
	}

	ids, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(ids) != 3 || ids[0] != 1 || ids[1] != 4 || ids[2] != 12 {
		t.Fatalf("discovered %v, want [1 4 12]", ids)
	}
}

// TestDiscoverOnAFreshInstall covers the very first run, where nothing exists.
// An empty list is the right answer; an error would make a new install look
// broken.
func TestDiscoverOnAFreshInstall(t *testing.T) {
	ids, err := Discover(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("discovered %v on an empty data directory", ids)
	}
}
