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
	"sync"
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

// deletionWatchInterval keeps long-lived cached handles responsive to a
// cross-process tombstone without turning every open account into a hot stat
// loop. Permanent deletion waits for the watcher, so this affects latency only.
const deletionWatchInterval = 100 * time.Millisecond

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
	stopWatch    chan struct{}
	watchDone    chan struct{}
	stopOnce     sync.Once
	closeOnce    sync.Once
	closeErr     error
	useMu        sync.Mutex
	activeUses   int
	closing      bool
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

	// mu guards the map and serialises opening. Holding it across the whole of
	// Open is deliberate: opening involves creating a file and possibly
	// migrating it, and two goroutines doing that to the same account at once
	// is the one race worth paying a global lock to avoid.
	mu   sync.Mutex
	open map[int64]*Account

	// blocked caches durable tombstones observed or created by this process. The
	// filesystem remains authoritative across managers and process restarts.
	blocked map[int64]struct{}
}

// NewManager builds a manager rooted at a data directory. Nothing is opened or
// created here, so constructing one is free and a process that never touches an
// account never touches the disk.
func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir, open: map[int64]*Account{}, blocked: map[int64]struct{}{}}
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

// close stops deletion monitoring, closes SQLite, and releases the lifetime
// lease exactly once. Waiting for the watcher keeps shutdown from leaking a
// goroutine that still references the account.
func (a *Account) close() error {
	if a.stopWatch != nil {
		a.stopOnce.Do(func() { close(a.stopWatch) })
	}
	a.useMu.Lock()
	a.closing = true
	a.useMu.Unlock()
	a.closeResources()
	if a.watchDone != nil {
		<-a.watchDone
	}

	return a.closeErr
}

// beginUse registers one operation before the account can be closed by its
// deletion watcher. The caller still holds the cross-process shared guard, so
// a successful registration spans exactly the same operation lifetime.
func (a *Account) beginUse() error {
	a.useMu.Lock()
	defer a.useMu.Unlock()

	if a.closing {
		return ErrDeleted
	}
	a.activeUses++
	return nil
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

// closeForDeletion closes an account only after every operation that acquired
// it has finished. It returns false while a lease is still active so the
// watcher can retry without interrupting a write already acknowledged in RAM.
func (a *Account) closeForDeletion() bool {
	a.useMu.Lock()
	if a.activeUses > 0 {
		a.useMu.Unlock()
		return false
	}
	a.closing = true
	a.useMu.Unlock()
	a.closeResources()
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

// watchDeletion cooperatively closes a cached handle when another manager or
// process publishes the durable deletion marker.
func (a *Account) watchDeletion(marker string) {
	defer close(a.watchDone)
	ticker := time.NewTicker(deletionWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stopWatch:
			return
		case <-ticker.C:
			if _, err := os.Stat(marker); err == nil {
				if a.closeForDeletion() {
					return
				}
			}
		}
	}
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
func (m *Manager) openGuarded(ctx context.Context, id int64) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, blocked := m.blocked[id]; blocked {
		return nil, fmt.Errorf("%w for account %d", ErrDeleted, id)
	}

	lock, err := lockAccount(m.dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlockAccount(lock)

	if _, err := os.Stat(DeletedMarker(m.dataDir, id)); err == nil {
		// Another process may have deleted the account since this manager cached
		// its handle. Drop and close that handle before refusing the open so the
		// unlinked SQLite file cannot remain usable by later requests here.
		if account, ok := m.open[id]; ok {
			delete(m.open, id)
			_ = account.close()
		}
		m.blocked[id] = struct{}{}
		return nil, fmt.Errorf("%w for account %d", ErrDeleted, id)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("account %d: inspect deletion marker: %w", id, err)
	}
	if account, ok := m.open[id]; ok {
		return account, nil
	}

	lifetimeLock, err := lockAccountLifetime(m.dataDir, id, false)
	if err != nil {
		return nil, err
	}

	path := Path(m.dataDir, id)

	db, err := store.OpenDatabase(path)
	if err != nil {
		unlockAccount(lifetimeLock)
		return nil, err
	}

	if err := ensureSchema(ctx, db, migrate.Account()); err != nil {
		closeErr := db.Close()
		unlockAccount(lifetimeLock)
		openErr := fmt.Errorf("account %d: %w", id, err)
		if closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("account %d: close database after schema failure: %w", id, closeErr))
		}
		return nil, openErr
	}

	cache := intern.New(db.Writer())
	if err := cache.Warm(ctx); err != nil {
		closeErr := db.Close()
		unlockAccount(lifetimeLock)
		openErr := fmt.Errorf("account %d: %w", id, err)
		if closeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("account %d: close database after cache failure: %w", id, closeErr))
		}
		return nil, openErr
	}

	account := &Account{
		ID: id, DB: db, Intern: cache, lifetimeLock: lifetimeLock,
		stopWatch: make(chan struct{}), watchDone: make(chan struct{}),
	}
	m.open[id] = account
	go account.watchDeletion(DeletedMarker(m.dataDir, id))

	return account, nil
}

// WriteGuard holds a shared cross-process lock from the final tombstone check
// through the account transaction. A deletion takes the exclusive side before
// removing files, so an already-open writer cannot continue into an unlinked
// database inode.
type WriteGuard struct {
	manager *Manager
	id      int64
	file    *os.File
	account *Account
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
	guard, err := m.BeginWrite(id)
	if err != nil {
		return nil, err
	}

	account, err := guard.Open(ctx)
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
		delete(m.open, id)
		m.blocked[id] = struct{}{}
		m.mu.Unlock()
		if account != nil {
			_ = account.close()
		}
		return nil, fmt.Errorf("%w for account %d", ErrDeleted, id)
	}

	return &WriteGuard{manager: m, id: id, file: file}, nil
}

// Open returns the account handle protected by this guard.
func (g *WriteGuard) Open(ctx context.Context) (*Account, error) {
	account, err := g.manager.openGuarded(ctx, g.id)
	if err != nil {
		return nil, err
	}
	if err := account.beginUse(); err != nil {
		return nil, fmt.Errorf("%w for account %d", err, g.id)
	}
	g.account = account
	return account, nil
}

// Release drops a shared account guard. Calling it more than once is harmless,
// which keeps deferred cleanup safe on every return path.
func (g *WriteGuard) Release() error {
	if g == nil || g.file == nil {
		return nil
	}

	file := g.file
	g.file = nil
	if g.account != nil {
		g.account.endUse()
		g.account = nil
	}
	return unlock(file)
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
	delete(m.open, id)

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
	delete(m.open, id)
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
	return filepath.Join(dataDir, guardDirectory, fmt.Sprintf("account-%0*d.deleted", dirWidth, id))
}

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
