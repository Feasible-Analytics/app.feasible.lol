//
// accounts.go
// Opening, caching and closing the per-account analytics databases.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package accounts owns the per-account analytics databases: where they live,
// when they are created, and who holds the handles. There is one database per
// account rather than per site because a team's dashboard, export and billing
// usage all span its sites, and cross-database joins in SQLite need ATTACH,
// which caps at ten. Per-account keeps every real query inside one file.
//
// A handle is expensive enough to be worth keeping — a writer connection, a
// reader pool and a warmed dimension cache — and cheap enough to open on demand
// for an account that has just sent its first event, which is why this is a
// cache rather than something built at start-up.
package accounts

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
	"sync"
	"sync/atomic"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// DatabaseName is the file inside each account's directory. The account id is
// the directory rather than the filename so that everything belonging to one
// account — the database, its WAL, and anything a later milestone puts beside
// them — can be moved between shards as a directory.
const DatabaseName = "analytics.db"

// dirWidth zero-pads the account directory name. Fixed-width names sort the
// same in a shell, in a file browser and in code, which matters the day someone
// is looking at a data directory with a thousand accounts in it.
const dirWidth = 6

// deletionWatchInterval is how often one goroutine reads the tombstone
// directory. Permanent deletion waits for that pass, so this is a latency bound
// on deletion and on nothing else — and deletion is already allowed to take
// seconds, because it fsyncs a marker and drains every open lease first.
const deletionWatchInterval = time.Second

// guardDirectory is outside the removable account directory. Its tombstones
// must survive account deletion and process restart, and its lock files must be
// shared by every app and ingest process using the data directory.
const guardDirectory = ".account-deletions"

// ErrDeleted identifies an account whose durable deletion tombstone is present.
// Writers treat it as an acknowledged stale route instead of retrying forever.
var ErrDeleted = errors.New("account permanently deleted")

// Account is one open account database with everything the ingest and query
// paths need to use it.
type Account struct {
	// ID is the account id, which is also teams.id in the system database.
	ID int64

	// DB carries the single writer and the reader pool for this account.
	DB *store.Database

	// Intern is this account's dimension cache, warmed when the handle was
	// opened. It belongs to the account rather than the process because ids are
	// only meaningful inside one database.
	Intern *intern.Cache

	// lifetimeLock is held shared for as long as this handle can use SQLite.
	// Permanent deletion waits for an exclusive lock before it can report
	// success, which fences handles cached by every cooperating process.
	lifetimeLock *os.File

	// rank orders handles for eviction and usedAt drives the idle sweep. They
	// are atomics on the handle rather than maps on the manager, so promoting
	// on the cached path needs only a read lock. A rank of zero is a handle a
	// scan opened: never promoted, first evicted, and put back on release.
	rank       atomic.Int64
	usedAt     atomic.Int64
	closeOnce  sync.Once
	closeErr   error
	useMu      sync.Mutex
	activeUses int
	closing    bool
}

// Writer is the single connection every write goes through. It is exposed as a
// method so call sites read as "this account's writer" rather than reaching
// two levels into a struct.
func (a *Account) Writer() *sql.DB {
	return a.DB.Writer()
}

// Reader is the pooled read-only handle for dashboard and API queries.
func (a *Account) Reader() *sql.DB {
	return a.DB.Reader()
}

// Manager hands out account handles and owns their lifetime. One process may
// serve hundreds of accounts, and each open handle costs connections, file
// descriptors and page cache, so handles are shared rather than opened per
// request and can be closed again when an account goes quiet.
type Manager struct {
	dataDir string

	// mu guards the maps. Opening holds it for writing across the whole of the
	// open, deliberately: opening involves creating a file and possibly
	// migrating it, and two goroutines doing that to the same account at once
	// is the one race worth paying a global lock to avoid. Handing back a
	// handle that is already open takes it for reading only.
	mu   sync.RWMutex
	open map[int64]*Account

	// blocked caches durable tombstones observed or created by this process. The
	// filesystem remains authoritative across managers and process restarts.
	blocked map[int64]struct{}

	// tick numbers promotions. Recency itself lives on each Account, so the
	// cached path can promote under a read lock rather than serialising every
	// account in the process behind one writer.
	tick atomic.Int64

	// MaxOpen bounds the handles held at once. Zero means DefaultMaxOpen.
	MaxOpen int

	// IdleTimeout closes a handle nothing has touched for this long, so a quiet
	// box gives its file descriptors back. Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration

	// Now is the clock recency and idleness are measured against.
	Now func() time.Time

	// OnWatchError is told when the tombstone watcher cannot read its
	// directory. Nil discards it, and the counter still records that it
	// happened.
	OnWatchError func(error)

	stats HandleStats

	// started counts the watchers this manager has ever run. A manager runs one
	// at a time and starts another only after CloseAll, so it is also the
	// answer to "did opening more handles start more watching".
	started int

	// watching is the live tombstone watcher, or nil. It starts on the first
	// open rather than at construction, because a manager that never opens an
	// account never needs one — and it can start again after CloseAll, because
	// a manager is allowed to be reused and a handle with no watcher is a
	// deletion that hangs for ever.
	watching *watcher
}

// The bounds a manager uses when none are set.
//
// A handle is about a third of a megabyte, four goroutines and up to fifteen
// file descriptors, and re-opening one takes roughly two milliseconds. Five
// hundred is about 165 MB and a miss is cheap, so the cap can be this tight. An
// hour idle is long enough that a site with any traffic keeps its handle.
const (
	DefaultMaxOpen     = 500
	DefaultIdleTimeout = time.Hour
)

// HandleStats is what the health surface reports. Evictions and idle closes are
// separate numbers because rolled into one, a box thrashing against its cap and
// a box quietly giving descriptors back look identical.
type HandleStats struct {
	// Open is how many handles are held now, and Max is the cap.
	Open int
	Max  int

	// Opens counts every handle this process has created, so a rate can be
	// derived. Evictions counts handles closed because the cap was reached,
	// IdleCloses those closed because nothing had touched them, and Overshoots
	// the times the cap was passed because every handle was in use.
	Opens      int64
	Evictions  int64
	IdleCloses int64
	Overshoots int64

	// Watchers counts the deletion watchers this manager has run. It is one at
	// a time, and one more after each CloseAll, so it never follows the number
	// of accounts.
	Watchers int

	// WatchFailures counts passes where the deletion watcher could not read its
	// directory. Any at all means this process may still be holding a handle to
	// an account somebody else has deleted.
	WatchFailures int64
}

// NewManager builds a manager rooted at a data directory. Nothing is opened or
// created here, so constructing one is free and a process that never touches an
// account never touches the disk.
func NewManager(dataDir string) *Manager {
	return &Manager{
		dataDir: dataDir,
		open:    map[int64]*Account{},
		blocked: map[int64]struct{}{},
	}
}

// maxOpen is the configured cap or the default.
func (m *Manager) maxOpen() int {
	if m.MaxOpen > 0 {
		return m.MaxOpen
	}

	return DefaultMaxOpen
}

// idleTimeout is how long a handle may go untouched before it is closed.
func (m *Manager) idleTimeout() time.Duration {
	if m.IdleTimeout > 0 {
		return m.IdleTimeout
	}

	return DefaultIdleTimeout
}

// now reads the manager's clock.
func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}

	return time.Now()
}

// Dir returns the directory holding one account's database.
func Dir(dataDir string, id int64) string {
	return filepath.Join(dataDir, config.AccountDatabaseDir, fmt.Sprintf("%0*d", dirWidth, id))
}

// Path returns the full path to one account's database file. It is a package
// function so the maintenance commands can name a file without opening it.
func Path(dataDir string, id int64) string {
	return filepath.Join(Dir(dataDir, id), DatabaseName)
}

// DeletedMarker is outside the account directory so RemoveAll cannot erase it.
// Team allocation reserves ids recorded by the immutable deletion audit; once
// present, every process must refuse to recreate the analytics database.
func DeletedMarker(dataDir string, id int64) string {
	return TombstonePath(dataDir, id)
}

// accountLockPath names the advisory lock shared by every process that may open
// or permanently remove one account database.
func accountLockPath(dataDir string, id int64) string {
	return filepath.Join(dataDir, config.AccountDatabaseDir, fmt.Sprintf(".lock-%0*d", dirWidth, id))
}

// accountLifetimeLockPath names the process-shared lease held by every usable
// database handle and acquired exclusively by permanent deletion.
func accountLifetimeLockPath(dataDir string, id int64) string {
	return filepath.Join(dataDir, config.AccountDatabaseDir, fmt.Sprintf(".lifetime-%0*d", dirWidth, id))
}

// lockAccount serializes the marker check plus file open/removal across
// processes. The kernel releases the lock if a process crashes.
func lockAccount(dataDir string, id int64) (*os.File, error) {
	path := accountLockPath(dataDir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("account %d: create lock directory: %w", id, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("account %d: open lock: %w", id, err)
	}
	if err := lockFile(file, lockExclusive); err != nil {
		closeErr := file.Close()
		lockErr := fmt.Errorf("account %d: lock: %w", id, err)
		if closeErr != nil {
			lockErr = errors.Join(lockErr, fmt.Errorf("account %d: close failed lock file: %w", id, closeErr))
		}
		return nil, lockErr
	}

	return file, nil
}

// unlockAccount releases and closes an account's advisory lock.
func unlockAccount(file *os.File) {
	if file == nil {
		return
	}
	_ = unlockFile(file)
	_ = file.Close()
}

// lockAccountLifetime acquires either a shared handle lease or the exclusive
// deletion fence. The coordination lock prevents a new shared lease from
// crossing creation of the deletion marker.
func lockAccountLifetime(dataDir string, id int64, exclusive bool) (*os.File, error) {
	path := accountLifetimeLockPath(dataDir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("account %d: create lifetime lock directory: %w", id, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("account %d: open lifetime lock: %w", id, err)
	}
	mode := lockShared
	if exclusive {
		mode = lockExclusive
	}
	if err := lockFile(file, mode); err != nil {
		closeErr := file.Close()
		lockErr := fmt.Errorf("account %d: acquire lifetime lock: %w", id, err)
		if closeErr != nil {
			lockErr = errors.Join(lockErr, fmt.Errorf("account %d: close failed lifetime lock file: %w", id, closeErr))
		}
		return nil, lockErr
	}

	return file, nil
}

// close marks the handle closing, closes SQLite and releases the lifetime lease
// exactly once.
func (a *Account) close() error {
	a.useMu.Lock()
	a.closing = true
	a.useMu.Unlock()
	a.closeResources()

	return a.closeErr
}

// beginUse registers one operation before the account can be closed — by the
// deletion watcher, by an eviction, or by a scan putting a borrowed handle
// back. It is taken under the manager lock, so a handle cannot be closed
// between being chosen and being registered.
func (a *Account) beginUse() error {
	a.useMu.Lock()
	defer a.useMu.Unlock()

	if a.closing {
		return ErrDeleted
	}
	a.activeUses++
	return nil
}

// inUse reports whether an operation is holding this account, which is what
// makes it ineligible for eviction.
func (a *Account) inUse() bool {
	a.useMu.Lock()
	defer a.useMu.Unlock()

	return a.activeUses > 0
}

// endUse releases one operation registration. The deletion watcher observes
// the zero count on its next pass and then releases the lifetime SQLite fence.
func (a *Account) endUse() {
	a.useMu.Lock()
	if a.activeUses > 0 {
		a.activeUses--
	}
	a.useMu.Unlock()
}

// markClosing claims an account for closing without doing the closing.
//
// It reports false while a lease is still active. The split matters for
// eviction: the claim happens under the manager lock and the close does not,
// because closing the last connection to a database checkpoints and truncates
// its write-ahead log, and doing that under the lock that serialises every
// account open would put a disk sync on the open path.
func (a *Account) markClosing() bool {
	a.useMu.Lock()
	defer a.useMu.Unlock()

	if a.activeUses > 0 {
		return false
	}

	a.closing = true

	return true
}

// closeResources makes the database unusable before releasing its shared
// lifetime lease. Delete cannot acquire the exclusive fence until this method
// has completed in every process.
func (a *Account) closeResources() {
	a.closeOnce.Do(func() {
		a.closeErr = a.DB.Close()
		unlockAccount(a.lifetimeLock)
	})
}

// Path returns the path this manager would use for an account.
func (m *Manager) Path(id int64) string {
	return Path(m.dataDir, id)
}

// Open returns the handle for an account, opening it on first use. Production
// operations must use Acquire so their shared deletion fence spans every use
// of the returned handle; Open remains for setup code and tests that own the
// manager for the handle's complete lifetime. The
// directory and file are created if they do not exist, because an account's
// first event must not fail on a missing file, and a brand-new file is brought
// up to the current schema immediately.
//
// Bringing a *new* file up to date is not the same as migrating on boot, which
// this deliberately does not do: a database that already holds data at an older
// version is refused with an error telling the operator to run `feasible db
// migrate`. Upgrading someone's data as a side effect of an incoming event is
// how two processes end up migrating the same file at once.
func (m *Manager) Open(ctx context.Context, id int64) (*Account, error) {
	if id < 1 {
		return nil, fmt.Errorf("account id %d is not valid", id)
	}

	guard, err := m.BeginWrite(id)
	if err != nil {
		return nil, err
	}
	defer guard.Release() //nolint:errcheck // the operation result is more useful than an unlock error

	return guard.Open(ctx)
}

// openGuarded returns or creates a handle while the caller holds this account's shared
// cross-process guard. Keeping the unguarded operation private prevents a new
// writer path from bypassing the deletion tombstone.
//
// It registers the use before returning, under the same lock that evicts. A
// caller that registered afterwards could have its handle closed out from under
// it in between, and would see a deletion error for a live account.
//
// promote is false for a background walk, which must not rewrite recency: a job
// that touches every account once an hour would otherwise leave the cache
// holding whichever accounts it visited last, and ingest would re-open the real
// working set one batch at a time.
func (m *Manager) openGuarded(ctx context.Context, id int64, promote bool) (account *Account, borrowed bool, evicting []*Account, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, blocked := m.blocked[id]; blocked {
		return nil, false, nil, fmt.Errorf("%w for account %d", ErrDeleted, id)
	}

	lock, err := lockAccount(m.dataDir, id)
	if err != nil {
		return nil, false, nil, err
	}
	defer unlockAccount(lock)

	if _, err := os.Stat(DeletedMarker(m.dataDir, id)); err == nil {
		// Another process may have deleted the account since this manager cached
		// its handle. Drop and close that handle before refusing the open so the
		// unlinked SQLite file cannot remain usable by later requests here.
		if account, ok := m.open[id]; ok {
			m.forgetLocked(id)
			_ = account.close()
		}
		m.blocked[id] = struct{}{}
		return nil, false, nil, fmt.Errorf("%w for account %d", ErrDeleted, id)
	} else if !os.IsNotExist(err) {
		return nil, false, nil, fmt.Errorf("account %d: inspect deletion marker: %w", id, err)
	}
	if held, ok := m.open[id]; ok {
		if err := held.beginUse(); err != nil {
			return nil, false, nil, fmt.Errorf("%w for account %d", err, id)
		}

		if promote {
			m.promote(held)

			return held, false, nil, nil
		}

		// A second scan on a handle the first scan opened has to put it back
		// too: the first one's release will refuse while this one holds it, and
		// then nobody would close it. A rank of zero is what says no real
		// traffic has claimed it.
		return held, held.rank.Load() == 0, nil, nil
	}

	lifetimeLock, err := lockAccountLifetime(m.dataDir, id, false)
	if err != nil {
		return nil, false, nil, err
	}

	path := Path(m.dataDir, id)

	db, err := store.OpenDatabase(path)
	if err != nil {
		unlockAccount(lifetimeLock)
		return nil, false, nil, err
	}

	if err := ensureSchema(ctx, db, migrate.Account()); err != nil {
		closeErr := db.Close()
		unlockAccount(lifetimeLock)
		openErr := fmt.Errorf("account %d: %w", id, err)
		if closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("account %d: close database after schema failure: %w", id, closeErr))
		}
		return nil, false, nil, openErr
	}

	cache := intern.New(db.Writer())
	if err := cache.Warm(ctx); err != nil {
		closeErr := db.Close()
		unlockAccount(lifetimeLock)
		openErr := fmt.Errorf("account %d: %w", id, err)
		if closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("account %d: close database after cache failure: %w", id, closeErr))
		}
		return nil, false, nil, openErr
	}

	account = &Account{
		ID: id, DB: db, Intern: cache, lifetimeLock: lifetimeLock,
	}
	// A struct literal built four lines above is never closing, so this cannot
	// fire. It closes the database anyway rather than only the lock, because a
	// branch that leaks a handle is worse than a branch that never runs.
	if err := account.beginUse(); err != nil {
		closeErr := db.Close()
		unlockAccount(lifetimeLock)

		return nil, false, nil, errors.Join(fmt.Errorf("%w for account %d", err, id), closeErr)
	}

	m.open[id] = account
	m.stats.Opens++
	m.startWatch()

	// A handle a scan opened keeps rank zero: put back when the scan releases
	// it, and until then the first thing evicted, so a walk never displaces an
	// account real traffic is on.
	if promote {
		m.promote(account)
	} else {
		borrowed = true
	}

	// A borrowed handle is closed again the moment the scan releases it, so it
	// does not need room made for it — and making room would mean evicting an
	// account real traffic is on, which the scan rule exists to prevent. The
	// cache is one over its cap for one step of the walk.
	//
	// The chosen handles are closed after the lock is dropped, by the caller.
	if !borrowed {
		evicting = m.chooseLocked()
	}

	return account, borrowed, evicting, nil
}

// watchTombstones closes any open handle whose account another process has
// tombstoned, and refuses it from then on.
//
// One goroutine reads the tombstone directory once a second, rather than one
// goroutine and one timer per open handle stat-ing its own marker ten times a
// second. At the default cap that is five hundred fewer goroutines and five
// thousand fewer stat calls a second, to notice something that happens a few
// times a year.
//
// Deletion waits for this. BeginDeletion takes the lifetime lock exclusively
// and every open handle holds it shared, so a tombstone only becomes a deletion
// once this loop has closed the handle — which makes the interval a latency
// bound on deletion rather than a correctness one.
func (m *Manager) watchTombstones(running *watcher) {
	defer close(running.done)

	ticker := time.NewTicker(deletionWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-running.stop:
			return
		case <-ticker.C:
			m.closeTombstoned()
		}
	}
}

// closeTombstoned is one pass of the watcher.
//
// A handle somebody is using is left alone and retried on the next tick, which
// is what stops a deletion cutting an acknowledged write in half. It is already
// refused to new callers by then, because the block is recorded first.
func (m *Manager) closeTombstoned() {
	deleted, err := tombstonedIDs(m.dataDir)
	if err != nil {
		// One unreadable directory blinds every account in the process, where
		// the per-handle stat it replaced blinded one. It is counted and
		// reported rather than swallowed.
		m.mu.Lock()
		m.stats.WatchFailures++
		m.mu.Unlock()

		if m.OnWatchError != nil {
			m.OnWatchError(err)
		}

		return
	}

	if len(deleted) == 0 {
		return
	}

	m.mu.Lock()

	var victims []*Account

	for id := range deleted {
		if _, open := m.open[id]; !open {
			continue
		}

		m.blocked[id] = struct{}{}

		if account := m.takeLocked(id); account != nil {
			victims = append(victims, account)
		}
	}

	m.mu.Unlock()

	closeAll(victims)
}

// tombstonedIDs reads every account id carrying a deletion tombstone, in one
// pass of the directory.
func tombstonedIDs(dataDir string) (map[int64]struct{}, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, guardDirectory))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	found := map[int64]struct{}{}

	for _, entry := range entries {
		name := entry.Name()

		if !strings.HasPrefix(name, tombstonePrefix) || !strings.HasSuffix(name, tombstoneSuffix) {
			continue
		}

		digits := name[len(tombstonePrefix) : len(name)-len(tombstoneSuffix)]

		id, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || id < 1 {
			continue
		}

		found[id] = struct{}{}
	}

	return found, nil
}

// watcher is one run of the tombstone loop. It is a value rather than a pair of
// fields on the manager so that stopping one and starting another is a single
// swap and cannot leave a half-stopped watcher behind.
type watcher struct {
	stop chan struct{}
	done chan struct{}
}

// startWatch begins the tombstone watcher if none is running. The caller holds
// the manager lock for writing.
func (m *Manager) startWatch() {
	if m.watching != nil {
		return
	}

	running := &watcher{stop: make(chan struct{}), done: make(chan struct{})}
	m.watching = running
	m.started++

	// The method itself is the goroutine's body rather than a closure around
	// it, so the watcher is identifiable in a stack dump from the moment it is
	// created rather than from the moment it is first scheduled.
	go m.watchTombstones(running)
}

// stopWatching ends the watcher and waits for it, so a closed manager leaves no
// goroutine reading a directory a test is about to remove.
//
// It takes the watcher away first and waits without the lock, because the
// watcher takes the same lock on every tick.
func (m *Manager) stopWatching() {
	m.mu.Lock()
	running := m.watching
	m.watching = nil
	m.mu.Unlock()

	if running == nil {
		return
	}

	close(running.stop)
	<-running.done
}

// promote marks one handle as the most recently used. It is safe under the read
// lock: the values live on the handle and the counter is atomic.
func (m *Manager) promote(account *Account) {
	account.rank.Store(m.tick.Add(1))
	account.usedAt.Store(m.now().UnixNano())
}

// chooseLocked picks the least-recently-used handles to close until the cap is
// met, and hands them back for closing outside the lock.
//
// A handle somebody is holding is never closed — the whole point of the use
// count — and if every handle is in use the cap is passed rather than a request
// being blocked behind an eviction. A temporary overshoot is cheaper than a
// latency cliff on the write path, and the counter says when it happened.
func (m *Manager) chooseLocked() []*Account {
	cap := m.maxOpen()

	var victims []*Account

	for len(m.open) > cap {
		var (
			oldest   int64
			oldestAt int64
			found    bool
		)

		for id := range m.open {
			if m.open[id].inUse() {
				continue
			}

			if rank := m.open[id].rank.Load(); !found || rank < oldestAt {
				oldest, oldestAt, found = id, rank, true
			}
		}

		if !found {
			m.stats.Overshoots++

			break
		}

		account := m.takeLocked(oldest)
		if account == nil {
			// Chosen and then busy. Under this lock that cannot happen today;
			// breaking rather than retrying is what stops it becoming a spin
			// if the locking ever changes.
			break
		}

		victims = append(victims, account)
		m.stats.Evictions++
	}

	return victims
}

// takeLocked removes one handle from the cache and hands it back to be closed,
// or nil if something is using it. The close itself happens outside the lock.
func (m *Manager) takeLocked(id int64) *Account {
	account, ok := m.open[id]
	if !ok {
		return nil
	}

	if !account.markClosing() {
		return nil
	}

	m.forgetLocked(id)

	return account
}

// forgetLocked drops one account from the cache. Recency lives on the handle, so
// there is nothing else to prune.
func (m *Manager) forgetLocked(id int64) {
	delete(m.open, id)
}

// closeAll finishes closing handles the cache has already let go of.
//
// It runs outside m.mu on purpose: closing the last connection to a database
// checkpoints and truncates its write-ahead log, and doing that under the lock
// that serialises every account open would make a box at its cap pay a disk
// sync on the open path.
func closeAll(victims []*Account) {
	for _, account := range victims {
		account.closeResources()
	}
}

// CloseIdle closes every handle nothing has touched for the idle timeout, and
// says how many it closed.
//
// An idle close is a box giving back what it no longer needs; an eviction is a
// box at its cap. The counters stay apart for that reason.
func (m *Manager) CloseIdle() int {
	m.mu.Lock()

	cutoff := m.now().Add(-m.idleTimeout()).UnixNano()

	var victims []*Account

	for id, account := range m.open {
		if account.usedAt.Load() > cutoff {
			continue
		}

		if account := m.takeLocked(id); account != nil {
			victims = append(victims, account)
			m.stats.IdleCloses++
		}
	}

	// A burst may have left the cache over its cap with nothing to trigger an
	// eviction, so the sweep is also where that comes back down.
	victims = append(victims, m.chooseLocked()...)

	m.mu.Unlock()

	closeAll(victims)

	return len(victims)
}

// IdleSweepInterval is how often the idle sweep runs. It is a fraction of the
// idle timeout so a handle is closed within a few minutes of becoming idle
// rather than up to an hour later.
const IdleSweepInterval = 5 * time.Minute

// CloseIdleUntil runs the idle sweep until the context is cancelled. A box that
// has gone quiet has no open path left to sweep on, so it is a loop.
func (m *Manager) CloseIdleUntil(ctx context.Context, closed func(int)) {
	ticker := time.NewTicker(IdleSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := m.CloseIdle(); n > 0 && closed != nil {
				closed(n)
			}
		}
	}
}

// Stats reports what the handle cache is doing.
func (m *Manager) Stats() HandleStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := m.stats
	stats.Open = len(m.open)
	stats.Max = m.maxOpen()
	stats.Watchers = m.started

	return stats
}

// WriteGuard is one operation's claim on an account.
//
// From the slow path it holds a shared cross-process lock taken after the final
// tombstone check, so a deletion cannot start between the check and the write.
// From the cached path it holds no lock file: the handle already holds the
// account's lifetime lease, which is what a deletion has to take exclusively
// before it removes anything.
type WriteGuard struct {
	manager *Manager
	id      int64
	file    *os.File
	account *Account

	// borrowed is set when a scan opened this handle rather than finding it.
	// A walk over every account on the box must leave the cache holding what
	// it held before, so the handle goes back on release.
	borrowed bool
}

// Lease keeps the shared deletion fence for the complete lifetime of an
// account operation. Callers must release it only after their final database
// read or write; returning the Account while dropping the lock would let a
// deletion unlink the database underneath a stale SQLite handle.
type Lease struct {
	Account *Account
	guard   *WriteGuard
}

// Acquire opens an account while retaining its shared cross-process fence.
// This is the production entry point for jobs and requests whose work extends
// beyond opening the SQLite handle.
func (m *Manager) Acquire(ctx context.Context, id int64) (*Lease, error) {
	return m.acquire(ctx, id, true)
}

// AcquireForScan is Acquire for a job that walks every account in turn. The
// handle is not promoted and is closed again on release, so a walk leaves the
// cache holding what it held before.
//
// It is a method of its own rather than a flag, because a walk that promoted
// would leave the cache holding whichever accounts it visited last and the
// accounts taking traffic would be re-opened one batch at a time — which is too
// quiet a failure to hang on a boolean.
func (m *Manager) AcquireForScan(ctx context.Context, id int64) (*Lease, error) {
	return m.acquire(ctx, id, false)
}

// acquire takes the fence and the handle.
//
// The fast path is a handle this process already holds. It costs one mutex and
// no filesystem call at all, where the slow path is four file opens, four flock
// calls and two stats — on a hot loop that runs once per account per write
// batch and once per dashboard request.
//
// It is safe because the open handle already holds the account's lifetime lock
// shared, and permanent deletion has to take that lock exclusively before it
// unlinks anything. So a deletion cannot get past a handle this process is
// holding, whichever process started it. What the fast path skips is only the
// tombstone check, and the watcher does that for every handle once a tick — so
// a handle stops being usable within one deletionWatchInterval of another
// process tombstoning the account.
func (m *Manager) acquire(ctx context.Context, id int64, promote bool) (*Lease, error) {
	if lease, ok := m.acquireCached(id, promote); ok {
		return lease, nil
	}

	guard, err := m.BeginWrite(id)
	if err != nil {
		return nil, err
	}

	account, err := guard.open(ctx, promote)
	if err != nil {
		_ = guard.Release()

		return nil, err
	}

	return &Lease{Account: account, guard: guard}, nil
}

// Release drops an account-use lease. Repeated release is harmless so callers
// can defer it immediately after Acquire succeeds.
func (l *Lease) Release() error {
	if l == nil || l.guard == nil {
		return nil
	}

	guard := l.guard
	l.guard = nil
	return guard.Release()
}

// acquireCached hands back a handle this process already holds, without
// touching the filesystem. It reports false when there is no such handle, or
// when the watcher has seen a tombstone for it.
func (m *Manager) acquireCached(id int64, promote bool) (*Lease, bool) {
	if id < 1 {
		return nil, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, blocked := m.blocked[id]; blocked {
		return nil, false
	}

	account, open := m.open[id]
	if !open {
		return nil, false
	}

	if err := account.beginUse(); err != nil {
		// Closing: the watcher or a deletion got there first. The slow path
		// will produce the right error.
		return nil, false
	}

	borrowed := false

	if promote {
		m.promote(account)
	} else {
		borrowed = account.rank.Load() == 0
	}

	return &Lease{Account: account, guard: &WriteGuard{manager: m, id: id, account: account, borrowed: borrowed}}, true
}

// BeginWrite acquires the account's shared lock and checks the durable
// tombstone after acquisition, closing the race between a pre-lock check and a
// purge that starts immediately afterwards.
func (m *Manager) BeginWrite(id int64) (*WriteGuard, error) {
	if id < 1 {
		return nil, fmt.Errorf("account id %d is not valid", id)
	}

	file, err := lock(m.dataDir, id, lockShared)
	if err != nil {
		return nil, err
	}

	deleted, err := tombstoned(m.dataDir, id)
	if err != nil {
		_ = unlock(file)
		return nil, err
	}
	if deleted {
		_ = unlock(file)
		m.mu.Lock()
		account := m.open[id]
		m.forgetLocked(id)
		m.blocked[id] = struct{}{}
		m.mu.Unlock()
		if account != nil {
			_ = account.close()
		}
		return nil, fmt.Errorf("%w for account %d", ErrDeleted, id)
	}

	return &WriteGuard{manager: m, id: id, file: file}, nil
}

// Open returns the account handle protected by this guard, as work the cache
// counts as real use.
func (g *WriteGuard) Open(ctx context.Context) (*Account, error) {
	return g.open(ctx, true)
}

// OpenForScan is Open for a job that walks every account. The handle is not
// promoted, so a walk cannot flush the accounts real traffic is using.
func (g *WriteGuard) OpenForScan(ctx context.Context) (*Account, error) {
	return g.open(ctx, false)
}

// open takes the handle and registers the use, which openGuarded does under the
// lock that evicts.
func (g *WriteGuard) open(ctx context.Context, promote bool) (*Account, error) {
	account, borrowed, evicting, err := g.manager.openGuarded(ctx, g.id, promote)

	// Outside the manager lock, whether or not the open succeeded.
	closeAll(evicting)

	if err != nil {
		return nil, err
	}

	g.account = account
	g.borrowed = borrowed

	return account, nil
}

// Release drops a shared account guard. Calling it more than once is harmless,
// which keeps deferred cleanup safe on every return path.
func (g *WriteGuard) Release() error {
	if g == nil {
		return nil
	}

	// A guard from the cached path holds no lock file, and still holds a use
	// count and possibly a borrowed handle.
	if g.file == nil && g.account == nil {
		return nil
	}

	file := g.file
	g.file = nil

	evict := false

	if g.account != nil {
		id := g.account.ID
		borrowed := g.borrowed

		g.account.endUse()
		g.account = nil
		g.borrowed = false

		if borrowed {
			g.manager.putBack(id)
		} else {
			evict = true
		}
	}

	// A burst that passed the cap because everything was in use has to come
	// back down when it is not. Without this the cache stays over its bound
	// until the next new account is opened, which on a box that has gone quiet
	// is never.
	if evict {
		g.manager.evict()
	}

	if file == nil {
		return nil
	}

	return unlock(file)
}

// evict brings the cache back to its cap.
func (m *Manager) evict() {
	m.mu.Lock()
	victims := m.chooseLocked()
	m.mu.Unlock()

	closeAll(victims)
}

// putBack closes a handle a scan opened, so a walk over every account leaves
// the cache holding what it held before.
//
// Nothing happens if something else took the account in the meantime: the
// recency stamp is what says whether it did, and real use rewrites it.
func (m *Manager) putBack(id int64) {
	m.mu.Lock()

	var account *Account

	// Rank zero is what says nobody has promoted it since the scan opened it.
	// Real use rewrites the rank, and a second scan still holding it makes
	// takeLocked refuse.
	if held, open := m.open[id]; open && held.rank.Load() == 0 {
		account = m.takeLocked(id)
	}

	m.mu.Unlock()

	if account != nil {
		closeAll([]*Account{account})
	}
}

// ensureSchema initialises a new database and refuses an out-of-date one. The
// split is the whole point: version zero means an empty file this call just
// created, and anything between that and current means real data that an
// explicit, observable migration run should move. The set is a parameter so a
// test can present a database that is behind a build without having to invent
// a second real migration.
func ensureSchema(ctx context.Context, db *store.Database, set migrate.Set) error {
	version, err := store.SchemaVersion(ctx, db.Writer())
	if err != nil {
		return err
	}

	if version == 0 {
		_, err := migrate.Run(ctx, db.Writer(), set)
		return err
	}

	if version < set.Version() {
		return fmt.Errorf("database is at schema version %d and this build expects %d — run `feasible db migrate`", version, set.Version())
	}

	if version > set.Version() {
		return fmt.Errorf("database is at schema version %d but this build only knows up to %d", version, set.Version())
	}

	return nil
}

// Close releases one account's handles and drops it from the cache. Closing an
// account that was never open is not an error, so a caller tidying up after a
// failure does not have to check first.
func (m *Manager) Close(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	account, ok := m.open[id]
	m.forgetLocked(id)

	if !ok {
		return nil
	}

	return account.close()
}

// Delete permanently closes and removes one account while holding the manager
// lock. The marker is created first and survives directory removal, preventing a
// concurrent or later Open from recreating a fresh database for the deleted id.
func (m *Manager) Delete(id int64) error {
	guard, err := m.BeginDeletion(id)
	if err != nil {
		return err
	}
	defer guard.Release() //nolint:errcheck // an earlier deletion error is more useful
	if err := guard.CloseAccount(); err != nil {
		return err
	}
	if err := os.RemoveAll(Dir(m.dataDir, id)); err != nil {
		return fmt.Errorf("account %d: remove analytics directory: %w", id, err)
	}

	return guard.Release()
}

// Block writes the durable tombstone, drains every guarded writer in every
// process, and closes this manager's cached handle. Repeating it after a crash
// is safe and repairs a missing in-memory block from the filesystem marker.
func (m *Manager) Block(id int64) error {
	guard, err := m.BeginDeletion(id)
	if err != nil {
		return err
	}
	return guard.Release()
}

// DeletionGuard holds the exclusive account fence after a durable tombstone
// has stopped new users. It must span artifact discovery, account-handle close,
// shard removal, and global-file removal so no process can continue writing an
// unlinked SQLite inode.
type DeletionGuard struct {
	manager  *Manager
	id       int64
	file     *os.File
	lifetime *os.File
	account  *Account
}

// BeginDeletion creates the durable tombstone, drains every shared account
// lease in every process, and closes this manager's cached handle. The guard it
// returns holds no handle: a purger that needs to inventory the shard reopens it
// through OpenAccount, under the exclusive fence.
func (m *Manager) BeginDeletion(id int64) (*DeletionGuard, error) {
	if id < 1 {
		return nil, fmt.Errorf("account id %d is not valid", id)
	}
	if err := writeTombstone(m.dataDir, id); err != nil {
		return nil, err
	}

	file, err := lock(m.dataDir, id, lockExclusive)
	if err != nil {
		return nil, err
	}
	lifetime, err := lockAccountLifetime(m.dataDir, id, true)
	if err != nil {
		_ = unlock(file)
		return nil, err
	}

	m.mu.Lock()
	account := m.open[id]
	m.forgetLocked(id)
	m.blocked[id] = struct{}{}
	m.mu.Unlock()
	if account != nil {
		if err := account.close(); err != nil {
			unlockAccount(lifetime)
			_ = unlock(file)
			return nil, err
		}
	}

	return &DeletionGuard{manager: m, id: id, file: file, lifetime: lifetime}, nil
}

// OpenAccount returns the detached handle or opens an existing shard while the
// exclusive deletion fence is held. It never creates a missing shard: absence
// means a prior attempt already removed it and artifact discovery must rely on
// the durable manifest.
func (g *DeletionGuard) OpenAccount(ctx context.Context) (*Account, error) {
	if g == nil {
		return nil, fmt.Errorf("account deletion guard is nil")
	}
	if g.account != nil {
		return g.account, nil
	}
	if _, err := os.Stat(Path(g.manager.dataDir, g.id)); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("account %d inspect database: %w", g.id, err)
	}

	db, err := store.OpenDatabase(Path(g.manager.dataDir, g.id))
	if err != nil {
		return nil, err
	}
	if err := ensureSchema(ctx, db, migrate.Account()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("account %d: %w", g.id, err)
	}

	cache := intern.New(db.Writer())
	if err := cache.Warm(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("account %d: %w", g.id, err)
	}

	g.account = &Account{ID: g.id, DB: db, Intern: cache}
	return g.account, nil
}

// CloseAccount closes this process's detached SQLite handle before its files
// are removed. It is idempotent so every checkpoint retry can call it.
func (g *DeletionGuard) CloseAccount() error {
	if g == nil || g.account == nil {
		return nil
	}

	account := g.account
	g.account = nil
	return account.close()
}

// Release closes any remaining detached handle and drops the exclusive fence.
// The durable tombstone remains authoritative after release.
func (g *DeletionGuard) Release() error {
	if g == nil || g.file == nil {
		return nil
	}

	closeErr := g.CloseAccount()
	file := g.file
	g.file = nil
	lifetime := g.lifetime
	g.lifetime = nil
	unlockAccount(lifetime)
	unlockErr := unlock(file)
	if closeErr != nil {
		return closeErr
	}
	return unlockErr
}

// TombstonePath returns the durable deletion marker for an account. It is
// exported for operational checks and tests, not as a path callers should edit.
func TombstonePath(dataDir string, id int64) string {
	return filepath.Join(dataDir, guardDirectory,
		fmt.Sprintf("%s%0*d%s", tombstonePrefix, dirWidth, id, tombstoneSuffix))
}

// The marker's filename, written by TombstonePath and read back by the watcher.
// One definition each, because a change only one of them followed would stop
// the watcher noticing anything.
const (
	tombstonePrefix = "account-"
	tombstoneSuffix = ".deleted"
)

// lockPath returns the advisory-lock file shared by all account managers.
func lockPath(dataDir string, id int64) string {
	return filepath.Join(dataDir, guardDirectory, fmt.Sprintf("account-%0*d.lock", dirWidth, id))
}

// lock opens and acquires one advisory account lock.
func lock(dataDir string, id int64, how lockMode) (*os.File, error) {
	dir := filepath.Join(dataDir, guardDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("account %d guard directory: %w", id, err)
	}

	file, err := os.OpenFile(lockPath(dataDir, id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("account %d guard lock: %w", id, err)
	}
	if err := lockFile(file, how); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("account %d guard lock: %w", id, err)
	}

	return file, nil
}

// unlock releases and closes one advisory account lock.
func unlock(file *os.File) error {
	if file == nil {
		return nil
	}

	unlockErr := unlockFile(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// tombstoned reports whether persistent deletion has started for an account.
func tombstoned(dataDir string, id int64) (bool, error) {
	_, err := os.Stat(TombstonePath(dataDir, id))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}

	return false, fmt.Errorf("account %d deletion tombstone: %w", id, err)
}

// writeTombstone creates and fsyncs the deletion marker before any control or
// account data is removed. A retry also syncs an existing marker, covering a
// prior attempt that created the directory entry but failed before its sync.
// The marker contains no customer data; its filename carries only the internal
// numeric account id needed to refuse stale writes.
func writeTombstone(dataDir string, id int64) error {
	dir := filepath.Join(dataDir, guardDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("account %d tombstone directory: %w", id, err)
	}

	file, err := os.OpenFile(TombstonePath(dataDir, id), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("account %d deletion tombstone: %w", id, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("account %d sync deletion tombstone: %w", id, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("account %d close deletion tombstone: %w", id, err)
	}

	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("account %d open tombstone directory: %w", id, err)
	}
	defer directory.Close() //nolint:errcheck // sync result is the durability signal
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("account %d sync tombstone directory: %w", id, err)
	}

	return nil
}

// CloseAll releases every open handle. Shutdown runs it so the WAL of every
// account is checkpointed on the way out rather than left for the next start-up
// to recover.
func (m *Manager) CloseAll() error {
	m.mu.Lock()
	accounts := make([]*Account, 0, len(m.open))
	for _, account := range m.open {
		accounts = append(accounts, account)
	}
	m.open = map[int64]*Account{}
	m.mu.Unlock()

	// After the map is cleared, so an open that raced this call reinstates the
	// watcher rather than leaving a handle nothing watches — which is a
	// deletion that waits for a close that never comes.
	m.stopWatching()

	// Every handle is closed even after one fails. Stopping at the first error
	// would leave the rest of the accounts open in a process that is exiting.
	var firstErr error
	for _, account := range accounts {
		if err := account.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// OpenCount reports how many handles are cached. It is what a metric or a debug
// page reads to answer "how many accounts is this shard actually serving".
func (m *Manager) OpenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.open)
}

// Discover lists the account ids that already have a database on disk. The
// migration and backup commands walk this rather than the system database so
// that they still work on a box whose system database is unreadable — which is
// exactly when someone is running maintenance commands.
func Discover(dataDir string) ([]int64, error) {
	root := filepath.Join(dataDir, config.AccountDatabaseDir)

	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	var ids []int64

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// A directory whose name is not an account id is somebody else's —
		// a backup, a scratch copy — and quietly skipping it is what keeps
		// this from failing on a data directory people have poked at.
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || id < 1 {
			continue
		}

		// A directory with no database in it is an account that has not been
		// opened yet, and is not this function's business. Any other failure is
		// reported: the migration and backup commands walk this list, and an
		// account skipped for an unreadable file would be reported as migrated
		// or backed up when it was neither.
		if _, err := os.Stat(Path(dataDir, id)); err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return nil, fmt.Errorf("inspect account %d database: %w", id, err)
		}

		ids = append(ids, id)
	}

	// A stable order makes a failed run stop in the same place every time, so
	// re-running it resumes rather than reshuffles.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	return ids, nil
}
