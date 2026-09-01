//
// salts.go
// The rotating fingerprint salts: created daily at 00:00 UTC, deleted after 48 hours.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package salts owns the secret that turns a visitor into a hash. Everything in
// here exists to make one claim true: after 48 hours, a stored fingerprint
// cannot be reconstructed by anyone, including us. That is the entire basis of
// "no cookies, no persistent identifiers", and weakening any part of it —
// keeping rows longer, rotating on a local midnight, storing the salt in the
// clear — makes the claim false.
//
// Three rules, and none of them are negotiable:
//
//   - Rotation is at 00:00 UTC, never a local midnight. A timezone-local
//     rotation would give two accounts different visitor identities for the
//     same person on the same day, and no later job could reconcile them.
//   - Exactly two salts are usable: today's, which hashes, and yesterday's,
//     which is a session-lookup fallback. Tomorrow's random material may be
//     pre-provisioned for rollover but is never used early.
//   - Rows older than 48 hours are deleted, not archived.
package salts

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"sync"
	"time"
)

// Size is the salt length in bytes. SipHash-2-4 takes a 128-bit key and the
// salt is that key, so this is fixed by the fingerprint formula rather than
// chosen.
const Size = 16

// RefreshInterval is how often a process re-reads the table. Every process
// refreshes independently because in a multi-process deploy one process holding
// a stale salt fragments sessions across processes, and nothing in the system
// would report that as an error.
const RefreshInterval = 90 * time.Second

// Retention is how long a salt row survives. Two days rather than one, because
// the previous salt has to outlive the day it hashed for so that a session
// running over midnight can still be found.
const Retention = 48 * time.Hour

// daySeconds is the rotation period in seconds. It is used as an integer
// divisor so a timestamp maps to a UTC day with no timezone anywhere in the
// arithmetic — which is precisely the trap this package exists to avoid.
const daySeconds int64 = 86400

// Pair is the two live salts. Previous is nil on an install's first day, and
// every caller has to cope with that rather than assume two are always there.
type Pair struct {
	Current  []byte
	Previous []byte
	Next     []byte

	// Day is the UTC day number Current belongs to, which is what tells a
	// cached Pair apart from one that rotation has made stale.
	Day int64

	// SourceShard identifies the consolidated deployment authority. Only zero
	// may generate material; any other marker is refused during loading.
	SourceShard int
}

// Erase overwrites every secret slice owned by this snapshot. Store callers
// receive isolated backing arrays so request-local erasure cannot race the
// shared cache.
func (p *Pair) Erase() {
	zeroBytes(p.Current)
	zeroBytes(p.Previous)
	zeroBytes(p.Next)
	*p = Pair{}
}

// Store reads and rotates the salts in control.db. It is safe for concurrent
// use and is read on the ingest hot path, so the common case is an atomic read
// of a cached pair with no lock held across any I/O.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD

	// now is injectable because every interesting property of this package is
	// about what happens at a particular instant, and a test that had to wait
	// for real midnight would never be written.
	now func() time.Time

	// random is where new salts and their nonces come from. It is injectable
	// for one caller only — the seed generator, which has to produce the same
	// visitor ids on every run, and cannot while the salt underneath them is
	// random. Serving processes never replace it.
	random io.Reader

	mu     sync.RWMutex
	cached Pair
}

// NewStore builds a store over control.db with the key that encrypts the rows.
// The key is required rather than optional: a store that silently fell back to
// plaintext would make the encryption claim depend on configuration nobody
// checks.
func NewStore(db *sql.DB, key []byte) (*Store, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("salts: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("salts: %w", err)
	}

	return &Store{db: db, aead: aead, now: time.Now, random: rand.Reader}, nil
}

// SetClock replaces the store's clock. It exists for the rotation tests, which
// have to observe what happens across a UTC midnight without waiting for one.
func (s *Store) SetClock(now func() time.Time) {
	s.now = now
}

// SetRandom replaces the source new salts are drawn from. Only the seed
// generator calls it: a fake dataset has to hash to the same visitor ids every
// time it is generated, and it cannot while the salt is freshly random on every
// run. Nothing that serves traffic may call this — a predictable salt is a
// reversible fingerprint.
func (s *Store) SetRandom(random io.Reader) {
	s.random = random
}

// Day returns the UTC day number a timestamp falls in. Integer division on a
// unix timestamp is the whole implementation, and that is the point: there is
// no location, no DST and no timezone database anywhere in the rotation.
func Day(t time.Time) int64 {
	return t.Unix() / daySeconds
}

// Pair returns the two live salts, creating today's if the day has turned since
// the last refresh. The fast path is a read lock and a comparison; the slow
// path only runs in the first moments after midnight, or on the first call.
func (s *Store) Pair(ctx context.Context) (Pair, error) {
	today := Day(s.now())

	s.mu.RLock()
	if s.cached.Day == today && len(s.cached.Current) == Size {
		answer := clonePair(s.cached)
		s.mu.RUnlock()
		return answer, nil
	}
	s.mu.RUnlock()

	// The periodic refresh runs every 90 seconds, which would leave a window
	// after midnight where events hashed with yesterday's salt. Refreshing on
	// demand closes it, so rotation is exactly at 00:00 UTC rather than within
	// a minute and a half of it.
	return s.Refresh(ctx)
}

// Refresh makes sure today's salt exists, reloads the two newest and deletes
// anything past retention. It is what the background loop calls, and what the
// hot path falls back to when the day has turned.
func (s *Store) Refresh(ctx context.Context) (Pair, error) {
	now := s.now()

	if err := s.ensureDay(ctx, Day(now)); err != nil {
		return Pair{}, err
	}
	if err := s.ensureDay(ctx, Day(now)+1); err != nil {
		return Pair{}, err
	}

	pair, err := s.load(ctx, now)
	if err != nil {
		return Pair{}, err
	}

	// A store that cannot erase expired material fails closed. Continuing to
	// hash after the privacy boundary is known to be broken would turn a 202
	// into a false claim.
	if err := s.Prune(ctx, now); err != nil {
		zeroPair(&pair)
		s.invalidateCache()
		return Pair{}, err
	}

	s.mu.Lock()
	zeroPair(&s.cached)
	s.cached = pair
	answer := clonePair(s.cached)
	s.mu.Unlock()

	return answer, nil
}

// ensureDay inserts random authority material for one UTC day. The next day is
// generated ahead of time so every process can cross midnight without using a
// stale identity.
func (s *Store) ensureDay(ctx context.Context, day int64) error {
	raw := make([]byte, Size)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return fmt.Errorf("salts: generate: %w", err)
	}
	defer zeroBytes(raw)

	sealed, err := s.seal(raw)
	if err != nil {
		return err
	}

	// created_at is pinned to the start of the UTC day rather than the current
	// instant. The row's day is derived from this column, and a row written at
	// 23:59:59 would otherwise land in a different bucket depending on how long
	// the insert took.
	startOfDay := day * daySeconds

	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO salts (salt, created_at, source_shard) VALUES (?, ?, 0) ON CONFLICT DO NOTHING",
		sealed, startOfDay,
	); err != nil {
		return fmt.Errorf("salts: insert: %w", err)
	}

	return nil
}

// load reads the previous, current and pre-provisioned next generations.
func (s *Store) load(ctx context.Context, now time.Time) (Pair, error) {
	today := Day(now)
	rows, err := s.db.QueryContext(ctx, `
		SELECT salt, created_at, source_shard
		FROM salts
		WHERE created_at >= ? AND created_at <= ?
		ORDER BY created_at`, (today-1)*daySeconds, (today+1)*daySeconds)
	if err != nil {
		return Pair{}, fmt.Errorf("salts: read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var pair Pair
	success := false
	defer func() {
		if !success {
			pair.Erase()
		}
	}()

	for rows.Next() {
		var (
			sealed    []byte
			createdAt int64
			source    int
		)

		if err := rows.Scan(&sealed, &createdAt, &source); err != nil {
			return Pair{}, fmt.Errorf("salts: read: %w", err)
		}

		raw, err := s.open(sealed)
		if err != nil {
			return Pair{}, err
		}

		if source != 0 {
			zeroBytes(raw)
			return Pair{}, fmt.Errorf("salts: day %d came from non-authority source %d", createdAt/daySeconds, source)
		}

		switch createdAt / daySeconds {
		case today - 1:
			pair.Previous = raw
		case today:
			pair.Current = raw
			pair.Day = today
			pair.SourceShard = source
		case today + 1:
			pair.Next = raw
		default:
			zeroBytes(raw)
		}
	}

	if err := rows.Err(); err != nil {
		return Pair{}, fmt.Errorf("salts: read: %w", err)
	}

	if len(pair.Current) != Size {
		return Pair{}, fmt.Errorf("salts: no salt for %s", now.UTC().Format(time.DateOnly))
	}

	// A salt that is not today's would fingerprint every visitor under a stale
	// identity, and nothing downstream could tell. Better to fail the refresh
	// and keep serving with the previous cached pair.
	if pair.Day != today {
		zeroPair(&pair)
		return Pair{}, fmt.Errorf("salts: newest salt is for day %d, expected %d", pair.Day, Day(now))
	}
	if pair.SourceShard != 0 {
		zeroPair(&pair)
		return Pair{}, fmt.Errorf("salts: current salt came from non-authority source %d", pair.SourceShard)
	}

	success = true
	return pair, nil
}

// Prune deletes salts past retention. This is the deletion the privacy claim
// rests on, so it is a plain unconditional DELETE with no soft-delete column
// and no archive table for anyone to recover from later.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-Retention).Unix()

	if _, err := s.db.ExecContext(ctx, "DELETE FROM salts WHERE created_at <= ?", cutoff); err != nil {
		return fmt.Errorf("salts: prune: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("salts: erase WAL: %w", err)
	}

	return nil
}

// clonePair gives callers isolated slices so a cache rollover can overwrite
// retired storage without racing an in-flight fingerprint operation.
func clonePair(pair Pair) Pair {
	return Pair{
		Current:     append([]byte(nil), pair.Current...),
		Previous:    append([]byte(nil), pair.Previous...),
		Next:        append([]byte(nil), pair.Next...),
		Day:         pair.Day,
		SourceShard: pair.SourceShard,
	}
}

// zeroPair overwrites every secret slice before releasing its backing arrays.
func zeroPair(pair *Pair) {
	pair.Erase()
}

// zeroBytes overwrites secret material before its slice is released.
func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// invalidateCache erases the usable snapshot after a retention failure.
func (s *Store) invalidateCache() {
	s.mu.Lock()
	zeroPair(&s.cached)
	s.mu.Unlock()
}

// Run refreshes on a ticker until the context is cancelled. Every process reads
// the same SQLite authority rows; uniqueness makes concurrent random proposals
// converge on one current and one pre-provisioned next-day value.
func (s *Store) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pair, err := s.Refresh(ctx)
			pair.Erase()
			if err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// seal encrypts a salt for storage. The nonce is random per row and prepended,
// which is the standard GCM layout and means the stored blob is self-contained.
func (s *Store) seal(raw []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return nil, fmt.Errorf("salts: nonce: %w", err)
	}

	return s.aead.Seal(nonce, nonce, raw, nil), nil
}

// open decrypts a stored salt. A failure here means the key has changed, which
// makes every stored fingerprint unmatchable — worth an explicit error rather
// than silently rotating everyone's identity.
func (s *Store) open(sealed []byte) ([]byte, error) {
	size := s.aead.NonceSize()
	if len(sealed) < size {
		return nil, fmt.Errorf("salts: stored value is too short to be encrypted")
	}

	raw, err := s.aead.Open(nil, sealed[:size], sealed[size:], nil)
	if err != nil {
		return nil, fmt.Errorf("salts: cannot decrypt — has the salt key changed?")
	}

	if len(raw) != Size {
		return nil, fmt.Errorf("salts: decrypted %d bytes, want %d", len(raw), Size)
	}

	return raw, nil
}
