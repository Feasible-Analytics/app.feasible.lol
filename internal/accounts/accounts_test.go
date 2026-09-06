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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	t.Cleanup(func() { checkClose(t, "account manager", manager.CloseAll) })

	return manager
}

// checkClose runs a test cleanup and reports a failure against the test that
// owns the resource instead of silently discarding it.
func checkClose(t testing.TB, name string, close func() error) {
	t.Helper()
	if err := close(); err != nil {
		t.Errorf("close %s: %v", name, err)
	}
}

// TestAccountLeaseHelper is the subprocess half of the stale-handle test. It
// holds a shared lease across a real write until the parent permits release.
func TestAccountLeaseHelper(t *testing.T) {
	if os.Getenv("FEASIBLE_ACCOUNT_LEASE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	dir := os.Getenv("FEASIBLE_ACCOUNT_LEASE_DIR")
	ready := os.Getenv("FEASIBLE_ACCOUNT_LEASE_READY")
	release := os.Getenv("FEASIBLE_ACCOUNT_LEASE_RELEASE")
	manager := NewManager(dir)
	lease, err := manager.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release() //nolint:errcheck // helper is exiting
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(release); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parent never released helper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := lease.Account.Writer().Exec(`CREATE TABLE IF NOT EXISTS lease_probe (value TEXT); INSERT INTO lease_probe VALUES ('written')`); err != nil {
		t.Fatal(err)
	}
}

// TestDeletionFenceDrainsACrossProcessStaleHandle proves an exclusive deletion
// waits for the full operation lifetime in another process, then removes the
// shard only after that process's final write and handle release.
func TestDeletionFenceDrainsACrossProcessStaleHandle(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir)
	if _, err := manager.Open(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")
	command := exec.Command(os.Args[0], "-test.run=^TestAccountLeaseHelper$")
	command.Env = append(os.Environ(),
		"FEASIBLE_ACCOUNT_LEASE_HELPER=1",
		"FEASIBLE_ACCOUNT_LEASE_DIR="+dir,
		"FEASIBLE_ACCOUNT_LEASE_READY="+ready,
		"FEASIBLE_ACCOUNT_LEASE_RELEASE="+release,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("subprocess never acquired its lease")
		}
		time.Sleep(10 * time.Millisecond)
	}

	acquired := make(chan *DeletionGuard, 1)
	errors := make(chan error, 1)
	go func() {
		guard, err := manager.BeginDeletion(1)
		if err != nil {
			errors <- err
			return
		}
		acquired <- guard
	}()
	select {
	case guard := <-acquired:
		_ = guard.Release()
		t.Fatal("deletion acquired its fence while another process still held a lease")
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	var guard *DeletionGuard
	select {
	case guard = <-acquired:
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("deletion did not acquire after the subprocess released")
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := guard.CloseAccount(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(Dir(dir, 1)); err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Dir(dir, 1)); !os.IsNotExist(err) {
		t.Fatalf("account directory survived deletion: %v", err)
	}
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

// TestDeleteLeavesAnUnrecreatableAccountTombstone races the permanent lifecycle
// operation against the manager's ordinary open behavior. The removed id must
// remain refused even though Open normally creates a missing database.
func TestDeleteLeavesAnUnrecreatableAccountTombstone(t *testing.T) {
	ctx := context.Background()
	manager := newManager(t)
	if _, err := manager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DeletedMarker(manager.dataDir, 1)); err != nil {
		t.Fatalf("deletion marker is missing: %v", err)
	}
	if _, err := manager.Open(ctx, 1); err == nil || !strings.Contains(err.Error(), "permanently deleted") {
		t.Fatalf("deleted account reopened with %v", err)
	}
	if _, err := os.Stat(manager.Path(1)); !os.IsNotExist(err) {
		t.Fatalf("deleted database was recreated: %v", err)
	}
}

// TestOpenClosesAHandleCachedBeforeAnotherManagerDeletedTheAccount covers two
// serving processes with independent caches. Delete must not return until the
// already-returned handle is fenced, without requiring another Open call.
func TestOpenClosesAHandleCachedBeforeAnotherManagerDeletedTheAccount(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	deleting := NewManager(dataDir)
	stale := NewManager(dataDir)
	t.Cleanup(func() {
		checkClose(t, "deleting account manager", deleting.CloseAll)
		checkClose(t, "stale account manager", stale.CloseAll)
	})

	if _, err := deleting.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cached, err := stale.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleting.Delete(1); err != nil {
		t.Fatal(err)
	}
	if err := cached.Reader().PingContext(ctx); err == nil {
		t.Fatal("cached reader remained usable after deletion returned")
	}
	if _, err := cached.Writer().ExecContext(ctx, "INSERT INTO dim_pathname (value) VALUES ('/after-delete')"); err == nil {
		t.Fatal("cached writer accepted data after deletion returned")
	}
	if _, err := stale.Open(ctx, 1); err == nil || !strings.Contains(err.Error(), "permanently deleted") {
		t.Fatalf("stale manager reopened the deleted account with %v", err)
	}
	if stale.OpenCount() != 0 {
		t.Fatal("stale manager retained the deleted account in its cache")
	}
}

// TestOpenCannotCrossTheDeletionMarkerCriticalSection deterministically pauses
// an opener behind the same filesystem lock deletion holds. Once the marker is
// committed, that opener must refuse rather than recreate a fresh database.
func TestOpenCannotCrossTheDeletionMarkerCriticalSection(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	manager := NewManager(dataDir)
	t.Cleanup(func() { checkClose(t, "account manager", manager.CloseAll) })

	lock, err := lockAccount(dataDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, openErr := manager.Open(ctx, 1)
		result <- openErr
	}()
	<-started

	select {
	case err := <-result:
		unlockAccount(lock)
		t.Fatalf("open crossed the held deletion lock with %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	marker := DeletedMarker(dataDir, 1)
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		unlockAccount(lock)
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		unlockAccount(lock)
		t.Fatal(err)
	}
	unlockAccount(lock)

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "permanently deleted") {
			t.Fatalf("blocked open resumed with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked open did not resume after deletion released its lock")
	}
	if _, err := os.Stat(manager.Path(1)); !os.IsNotExist(err) {
		t.Fatalf("blocked open recreated the database: %v", err)
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

// TestDeletionTombstoneSurvivesManagersAndRestart proves the guard is durable
// state, not a process-local cache that a split ingest process can miss.
func TestDeletionTombstoneSurvivesManagersAndRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	first := NewManager(dataDir)
	second := NewManager(dataDir)
	t.Cleanup(func() { _ = first.CloseAll(); _ = second.CloseAll() })

	if _, err := first.Open(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if err := second.Block(9); err != nil {
		t.Fatal(err)
	}

	for name, manager := range map[string]*Manager{
		"already open manager": first,
		"deleting manager":     second,
		"restarted manager":    NewManager(dataDir),
	} {
		if _, err := manager.Open(ctx, 9); !errors.Is(err, ErrDeleted) {
			t.Errorf("%s open error = %v, want ErrDeleted", name, err)
		}
	}
	if _, err := os.Stat(TombstonePath(dataDir, 9)); err != nil {
		t.Fatalf("durable tombstone is absent: %v", err)
	}
}

// TestBlockWaitsForAnIndependentManagerWriter proves the advisory lock drains
// an in-flight writer before deletion proceeds, including across managers that
// model separate app and ingest processes.
func TestBlockWaitsForAnIndependentManagerWriter(t *testing.T) {
	dataDir := t.TempDir()
	writerManager := NewManager(dataDir)
	deletionManager := NewManager(dataDir)

	guard, err := writerManager.BeginWrite(4)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- deletionManager.Block(4) }()

	select {
	case err := <-done:
		t.Fatalf("deletion passed an active writer guard: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deletion did not resume after the writer guard released")
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
	defer checkClose(t, "account database", db.Close)

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
	defer checkClose(t, "account manager", manager.CloseAll)

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

// TestTheOpenHandlesStayUnderTheCap is the acceptance criterion: memory must
// stop growing with the number of accounts a process has served.
func TestTheOpenHandlesStayUnderTheCap(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 4

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	for id := int64(1); id <= 20; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatalf("account %d: %v", id, err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}

		if open := manager.OpenCount(); open > manager.MaxOpen {
			t.Fatalf("after account %d, %d handles are open, want at most %d", id, open, manager.MaxOpen)
		}
	}

	stats := manager.Stats()

	if stats.Opens != 20 {
		t.Errorf("%d handles were opened, want 20", stats.Opens)
	}

	if stats.Evictions != 16 {
		t.Errorf("%d handles were evicted, want 16", stats.Evictions)
	}

	if stats.Overshoots != 0 {
		t.Errorf("the cap was passed %d times with nothing held", stats.Overshoots)
	}
}

// TestAHeldHandleIsNeverEvicted is the one thing eviction must not do. A lease
// holds the *Account directly, so closing one out from under it would hand a
// live operation an unlinked database.
//
// The guarantee is the use count's: nothing is closed while it is above zero.
// Eviction skipping held handles when it chooses one is an optimisation on top,
// so this asserts the composition rather than either half.
func TestAHeldHandleIsNeverEvicted(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 2

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	held, err := manager.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	for id := int64(2); id <= 10; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatalf("account %d: %v", id, err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	// Still usable, which is the whole claim.
	if _, err := held.Account.Writer().ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("the held account was closed underneath its lease: %v", err)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestEveryHandleInUseOvershootsRatherThanBlocking keeps a memory limit from
// becoming a latency cliff on the write path.
func TestEveryHandleInUseOvershootsRatherThanBlocking(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 2

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()
	held := []*Lease{}

	for id := int64(1); id <= 5; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatalf("account %d: %v", id, err)
		}

		held = append(held, lease)
	}

	if open := manager.OpenCount(); open != 5 {
		t.Errorf("%d handles are open, want the cap passed rather than a request blocked", open)
	}

	if stats := manager.Stats(); stats.Overshoots == 0 {
		t.Error("the cap was passed and nothing counted it")
	}

	for _, lease := range held {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnEvictedAccountReopensWithItsData is what makes a miss survivable.
func TestAnEvictedAccountReopensWithItsData(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 1

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	first, err := manager.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	id, err := first.Account.Intern.ID(ctx, intern.Pathname, "/kept")
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	// A second account evicts the first.
	second, err := manager.Acquire(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	again, err := manager.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("the evicted account did not re-open: %v", err)
	}

	defer again.Release() //nolint:errcheck // the assertion below is the point

	// The dimension cache is rebuilt on open, so the same string interns to the
	// same id rather than to a second row.
	back, err := again.Account.Intern.ID(ctx, intern.Pathname, "/kept")
	if err != nil {
		t.Fatal(err)
	}

	if back != id {
		t.Errorf("the re-opened account interned /kept as %d, was %d", back, id)
	}
}

// TestIdleHandlesCloseAndAreCountedSeparately keeps a quiet box giving its file
// descriptors back, and keeps that number distinguishable from thrashing.
func TestIdleHandlesCloseAndAreCountedSeparately(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.IdleTimeout = time.Hour

	now := time.Unix(1_800_000_000, 0)
	manager.Now = func() time.Time { return now }

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	for id := int64(1); id <= 3; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	if closed := manager.CloseIdle(); closed != 0 {
		t.Fatalf("%d handles closed before anything was idle", closed)
	}

	now = now.Add(2 * time.Hour)

	// One is used again, so it is not idle.
	lease, err := manager.Acquire(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	if closed := manager.CloseIdle(); closed != 2 {
		t.Errorf("%d idle handles closed, want 2", closed)
	}

	stats := manager.Stats()

	if stats.Open != 1 {
		t.Errorf("%d handles are open, want the one that was used", stats.Open)
	}

	if stats.IdleCloses != 2 || stats.Evictions != 0 {
		t.Errorf("idle closes = %d and evictions = %d, want them counted apart",
			stats.IdleCloses, stats.Evictions)
	}
}

// TestAScanDoesNotFlushTheWorkingSet is the test the whole scan rule exists
// for. Without it, an hourly walk over every account leaves the cache holding
// whichever ones it visited last, and ingest re-opens the real working set one
// batch at a time.
func TestAScanDoesNotFlushTheWorkingSet(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 3

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	// The hot set: three accounts taking real traffic.
	hot := []int64{1, 2, 3}
	for _, id := range hot {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	// A roll-up pass over three times the cap.
	for id := int64(4); id <= 12; id++ {
		lease, err := manager.AcquireForScan(ctx, id)
		if err != nil {
			t.Fatalf("account %d: %v", id, err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	stats := manager.Stats()

	if stats.Open > 3 {
		t.Fatalf("%d handles are open, want at most the cap", stats.Open)
	}

	// Every hot account is still resident: a scanned handle is never more
	// recent than one real traffic touched, so it is always the one evicted.
	before := manager.Stats().Opens

	for _, id := range hot {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	if opened := manager.Stats().Opens - before; opened != 0 {
		t.Errorf("the scan cost the hot set %d re-opens", opened)
	}
}

// TestAScanPutsItsHandleBack keeps a walk over every account from leaving the
// cache holding what the walk touched instead of what traffic is on.
func TestAScanPutsItsHandleBack(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 100

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	lease, err := manager.AcquireForScan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if manager.OpenCount() != 1 {
		t.Fatal("the scan did not open the account")
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	if open := manager.OpenCount(); open != 0 {
		t.Errorf("%d handles are still open after the scan released them", open)
	}

	// An account real traffic is already on is left alone: the scan finds it,
	// uses it and does not close it.
	held, err := manager.Acquire(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	scanned, err := manager.AcquireForScan(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := scanned.Release(); err != nil {
		t.Fatal(err)
	}

	if open := manager.OpenCount(); open != 1 {
		t.Errorf("the scan closed a handle it did not open: %d open", open)
	}
}

// TestCloseAllStillClosesEverything keeps shutdown whole. Every handle has a
// write-ahead log to checkpoint, and one left open is one not checkpointed.
func TestCloseAllStillClosesEverything(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 2

	ctx := context.Background()

	for id := int64(1); id <= 6; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	if err := manager.CloseAll(); err != nil {
		t.Fatalf("close all: %v", err)
	}

	if open := manager.OpenCount(); open != 0 {
		t.Errorf("%d handles survived shutdown", open)
	}
}

// TestAnOvershootHealsWhenTheLeasesGoAway keeps a burst from leaving the cache
// permanently over its bound.
//
// A box that bursts past the cap while everything is in use has to come back
// down when it is not, without waiting for a new account to arrive — on a box
// that has gone quiet, one never does.
func TestAnOvershootHealsWhenTheLeasesGoAway(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 2

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()
	held := []*Lease{}

	for id := int64(1); id <= 6; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		held = append(held, lease)
	}

	if open := manager.OpenCount(); open != 6 {
		t.Fatalf("%d handles are open, want the cap passed while everything is held", open)
	}

	for _, lease := range held {
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	if open := manager.OpenCount(); open > manager.MaxOpen {
		t.Errorf("%d handles are still open after every lease was released, want at most %d",
			open, manager.MaxOpen)
	}
}

// TestAScanDoesNotCloseAHandleTrafficTook is the guard putBack turns on. A walk
// that closed a handle a request had just started using would take the
// bounding decision away from the thing that measures use.
func TestAScanDoesNotCloseAHandleTrafficTook(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 100

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	// The scan opens it first, so it is the scan's to put back.
	scan, err := manager.AcquireForScan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Real traffic takes the same account, uses it, and lets go — all before
	// the scan does. Nothing is holding the handle when the scan releases, so
	// the only thing that can save it is the promotion the traffic left behind.
	traffic, err := manager.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := traffic.Account.Writer().ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	if err := traffic.Release(); err != nil {
		t.Fatal(err)
	}

	if err := scan.Release(); err != nil {
		t.Fatal(err)
	}

	if open := manager.OpenCount(); open != 1 {
		t.Fatalf("the scan closed a handle real traffic had claimed: %d open", open)
	}

	// And the handle still works, which is what closing it would have cost.
	again, err := manager.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := again.Account.Writer().ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("the handle was closed: %v", err)
	}

	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestTwoOverlappingScansBothPutTheHandleBack keeps a walk from stranding a
// handle nobody will close. Three jobs walk every account on the box, and two
// of them can be on the same one.
func TestTwoOverlappingScansBothPutTheHandleBack(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 100

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	first, err := manager.AcquireForScan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	second, err := manager.AcquireForScan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	if open := manager.OpenCount(); open != 0 {
		t.Errorf("%d handles left resident after both scans released", open)
	}
}

// TestAnEvictedAccountCanStillBeDeleted keeps eviction from leaving a lifetime
// lock behind. A deletion takes the exclusive side of that lock, so one left
// held by a handle nobody holds any more would hang the purge for ever.
func TestAnEvictedAccountCanStillBeDeleted(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 1

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	first, err := manager.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	// A second account evicts the first.
	second, err := manager.Acquire(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}

	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- manager.Delete(1) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deleting an evicted account: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("deleting an evicted account hung, so eviction left its lifetime lock held")
	}

	if _, err := manager.Acquire(ctx, 1); !errors.Is(err, ErrDeleted) {
		t.Errorf("a deleted account re-opened after eviction: %v", err)
	}
}

// BenchmarkAcquireCached is the number this cache exists to keep small. It runs
// once per account per write batch and once per dashboard request, so a change
// that puts the file locks back on the cached path shows up here.
func BenchmarkAcquireCached(b *testing.B) {
	manager := NewManager(b.TempDir())

	b.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			b.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	warm, err := manager.Acquire(ctx, 1)
	if err != nil {
		b.Fatal(err)
	}

	if err := warm.Release(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		lease, err := manager.Acquire(ctx, 1)
		if err != nil {
			b.Fatal(err)
		}

		if err := lease.Release(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAcquireCachedParallel is the same acquire from many goroutines,
// because the cached path used to serialise every account behind one mutex and
// that is exactly when a box is busy.
func BenchmarkAcquireCachedParallel(b *testing.B) {
	manager := NewManager(b.TempDir())

	b.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			b.Errorf("close: %v", err)
		}
	})

	ctx := context.Background()

	for id := int64(1); id <= 16; id++ {
		warm, err := manager.Acquire(ctx, id)
		if err != nil {
			b.Fatal(err)
		}

		if err := warm.Release(); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	var next atomic.Int64

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := next.Add(1)%16 + 1

			lease, err := manager.Acquire(ctx, id)
			if err != nil {
				b.Fatal(err)
			}

			if err := lease.Release(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestACachedHandleStopsWorkingAfterAnotherManagerDeletesIt is the guarantee the
// fast path must not weaken.
//
// The cached acquire skips the tombstone check, so a handle stays usable until
// the watcher notices — at most one deletionWatchInterval, currently 100 ms.
// This asserts that bound rather than assuming it.
func TestACachedHandleStopsWorkingAfterAnotherManagerDeletesIt(t *testing.T) {
	dir := t.TempDir()

	mine := NewManager(dir)
	t.Cleanup(func() { _ = mine.CloseAll() })

	theirs := NewManager(dir)
	t.Cleanup(func() { _ = theirs.CloseAll() })

	ctx := context.Background()

	// Cached here, so a later acquire takes the fast path.
	lease, err := mine.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	if again, err := mine.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	} else if err := again.Release(); err != nil {
		t.Fatal(err)
	}

	deleted := make(chan error, 1)
	go func() { deleted <- theirs.Delete(1) }()

	// The watcher has to close the handle before the other manager can take the
	// lifetime lock, so the deletion completing is itself proof it ran.
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("the other manager could not delete the account: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the deletion hung, so the watcher never closed the cached handle")
	}

	if _, err := mine.Acquire(ctx, 1); !errors.Is(err, ErrDeleted) {
		t.Errorf("the cached handle is still usable after the account was deleted: %v", err)
	}
}

// TestTheWatcherIsOneGoroutineWhateverTheAccountCount is the other half of the
// saving. One stat loop per open handle at 100 ms is twenty thousand stat calls
// a second on a two-thousand-account shard.
func TestTheWatcherIsOneGoroutineWhateverTheAccountCount(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.MaxOpen = 100

	ctx := context.Background()

	before := runtime.NumGoroutine()

	for id := int64(1); id <= 40; id++ {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}

		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}

	// Four goroutines per handle belong to SQLite; what must not scale is the
	// deletion watching, so the budget is generous and the shape is the point.
	perHandle := (runtime.NumGoroutine() - before) / 40

	if perHandle > 5 {
		t.Errorf("%d goroutines per open handle, want the watcher not to be one of them", perHandle)
	}

	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}
}
