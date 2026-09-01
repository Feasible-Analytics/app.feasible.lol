//
// apikeys.go
// Minting, hashing and authenticating the one key type the public API uses.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package apikeys owns the credential the public API is authenticated with.
//
// There is exactly one kind of key. The incumbent ships two — one for reading
// stats and a separate one for provisioning sites, the second of which was not
// even self-serve for a long time — and every integrator building against them
// pays for that split twice: once working out which key they need and once
// emailing support for it. One key, issued from the dashboard or the CLI, works
// for everything the holder's team can do.
//
// The key is stored as a SHA-256 hash and shown to its owner exactly once. A
// plain SHA-256 rather than a password hash is deliberate: this is a 256-bit
// random string, not a password, so there is no dictionary to slow an attacker
// down and a per-request bcrypt would only slow the API down.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prefix marks our keys so that one found in a log, a git repository or a
// support ticket is recognisable as ours and can be revoked without anybody
// having to work out what service it belongs to.
const Prefix = "feas_"

// secretBytes is how much randomness a key carries. Thirty-two bytes is a
// 256-bit secret, which is not brute-forceable and not worth making longer.
const secretBytes = 32

// displayPrefixLength is how much of the key is kept in the clear so somebody
// can tell their keys apart in a list. It covers the marker and a few
// characters of the secret — enough to identify, far too little to guess.
const displayPrefixLength = len(Prefix) + 6

// lastUsedResolution is how stale `last_used_at` is allowed to get before we
// write it again. Without it every authenticated request would take the single
// control.db write lock, which is the one lock the whole deployment shares:
// a busy integration would then serialise itself behind its own bookkeeping.
const lastUsedResolution = time.Minute

// Scopes a key may carry. An empty scope list means all of them, which is what
// a self-serve key gets, and the named scopes exist so an integrator who wants
// a read-only key can have one.
const (
	ScopeStatsRead      = "stats:read"
	ScopeSitesRead      = "sites:read"
	ScopeSitesProvision = "sites:provision"
	ScopeWebhooks       = "webhooks:write"
)

// Errors a caller has to tell apart. Authenticate returns these rather than a
// formatted string so the HTTP layer can pick a status code without matching on
// English.
var (
	// ErrNotFound means the key does not exist or was revoked. The two are one
	// error on purpose: telling a caller that a key exists but is revoked
	// confirms the key was real.
	ErrNotFound = errors.New("api key is not valid")

	// ErrMalformed means the string was never a key of ours, which is worth
	// separating so the API can say "this does not look like a key" instead of
	// "this key is not valid" to somebody who pasted the wrong thing.
	ErrMalformed = errors.New("api key is malformed")
)

// Key is one issued credential.
type Key struct {
	ID     int64
	TeamID int64
	UserID int64
	Name   string
	Prefix string
	Scopes []string

	// HourlyLimit is this key's own request ceiling, or zero to take the
	// deployment's configured default.
	HourlyLimit int

	LastUsedAt time.Time
	CreatedAt  time.Time
	RevokedAt  time.Time
}

// Allows reports whether a key may do something. An empty scope list is every
// scope: a key created with no scopes is the self-serve default and has to work
// for stats and provisioning alike, because making people mint a second key is
// exactly the friction this package exists to remove.
func (k *Key) Allows(scope string) bool {
	if len(k.Scopes) == 0 {
		return true
	}

	for _, held := range k.Scopes {
		if held == scope || held == "*" {
			return true
		}
	}

	return false
}

// Store reads and writes keys in control.db.
type Store struct {
	db *sql.DB

	// Now is the clock, injectable so a test can prove that `last_used_at` is
	// written and throttled without sleeping through a minute.
	Now func() time.Time
}

// NewStore builds a store over the control database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the store's clock.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now()
}

// Generate mints a new key string. It is separate from Create so that the
// caller holding the plaintext is the one that decided to show it to somebody,
// rather than it being an incidental return value of a database write.
func Generate() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("apikeys: generate: %w", err)
	}

	return Prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Hash is the stored form of a key. It is exported because the CLI and the
// tests both need to look a key up by hash without going through Authenticate.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))

	return hex.EncodeToString(sum[:])
}

// Create issues a key for a team and returns both the row and the plaintext.
// The plaintext is returned rather than stored because this is the only moment
// it exists: a customer who loses it mints another one.
func (s *Store) Create(ctx context.Context, teamID, userID int64, name string, scopes []string, hourlyLimit int) (*Key, string, error) {
	if teamID < 1 {
		return nil, "", fmt.Errorf("apikeys: create: a key needs a team")
	}

	if hourlyLimit < 0 {
		return nil, "", fmt.Errorf("apikeys: create: hourly limit cannot be negative")
	}

	plaintext, err := Generate()
	if err != nil {
		return nil, "", err
	}

	if scopes == nil {
		scopes = []string{}
	}

	encoded, err := json.Marshal(scopes)
	if err != nil {
		return nil, "", fmt.Errorf("apikeys: create: %w", err)
	}

	now := s.now()
	display := plaintext[:displayPrefixLength]

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (team_id, user_id, name, key_hash, key_prefix, scopes, hourly_limit, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		teamID, userID, name, Hash(plaintext), display, string(encoded), hourlyLimit, now.Unix())
	if err != nil {
		return nil, "", fmt.Errorf("apikeys: create: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, "", fmt.Errorf("apikeys: create: %w", err)
	}

	return &Key{
		ID: id, TeamID: teamID, UserID: userID, Name: name,
		Prefix: display, Scopes: scopes, HourlyLimit: hourlyLimit, CreatedAt: now,
	}, plaintext, nil
}

// Authenticate turns a presented key into the row it belongs to, and records
// that it was used.
//
// The lookup is by hash rather than by a scan and compare, so it is one indexed
// read no matter how many keys exist. The constant-time compare afterwards is
// belt and braces against a future where the column is not unique-indexed.
func (s *Store) Authenticate(ctx context.Context, presented string) (*Key, error) {
	presented = strings.TrimSpace(presented)

	if presented == "" || !strings.HasPrefix(presented, Prefix) || len(presented) <= displayPrefixLength {
		return nil, ErrMalformed
	}

	hashed := Hash(presented)

	key, storedHash, err := s.byHash(ctx, hashed)
	if err != nil {
		return nil, err
	}

	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hashed)) != 1 {
		return nil, ErrNotFound
	}

	s.touch(ctx, key)

	return key, nil
}

// byHash reads one key row.
func (s *Store) byHash(ctx context.Context, hashed string) (*Key, string, error) {
	var (
		key        Key
		storedHash string
		scopes     string
		lastUsed   sql.NullInt64
		created    int64
		revoked    sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, team_id, user_id, name, key_hash, key_prefix, scopes, hourly_limit, last_used_at, created_at, revoked_at
		FROM api_keys WHERE key_hash = ?`, hashed).
		Scan(&key.ID, &key.TeamID, &key.UserID, &key.Name, &storedHash, &key.Prefix,
			&scopes, &key.HourlyLimit, &lastUsed, &created, &revoked)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("apikeys: authenticate: %w", err)
	}

	// A revoked key is reported as simply not existing. Distinguishing the two
	// tells whoever is holding a stolen key that it was once real.
	if revoked.Valid {
		return nil, "", ErrNotFound
	}

	if err := json.Unmarshal([]byte(scopes), &key.Scopes); err != nil {
		// A scope list we cannot parse must not fail open into "every scope".
		return nil, "", fmt.Errorf("apikeys: key %d has an unreadable scope list: %w", key.ID, err)
	}

	key.CreatedAt = time.Unix(created, 0).UTC()
	if lastUsed.Valid {
		key.LastUsedAt = time.Unix(lastUsed.Int64, 0).UTC()
	}

	return &key, storedHash, nil
}

// touch records that a key was used, at most once a minute.
//
// A failure here is deliberately swallowed: `last_used_at` is bookkeeping for a
// human deciding whether a key is still in use, and refusing an otherwise valid
// request because the bookkeeping write lost a race with the ingest path would
// trade a real feature for a cosmetic one.
func (s *Store) touch(ctx context.Context, key *Key) {
	now := s.now()

	if !key.LastUsedAt.IsZero() && now.Sub(key.LastUsedAt) < lastUsedResolution {
		return
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now.Unix(), key.ID); err == nil {
		key.LastUsedAt = now
	}
}

// List returns a team's keys, newest first. The hash is never returned: there is
// nothing a caller could do with it except try to reverse it.
func (s *Store) List(ctx context.Context, teamID int64) (keys []Key, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, team_id, user_id, name, key_prefix, scopes, hourly_limit, last_used_at, created_at, revoked_at
		FROM api_keys WHERE team_id = ? ORDER BY id DESC`, teamID)
	if err != nil {
		return nil, fmt.Errorf("apikeys: list: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("apikeys: list: close rows: %w", closeErr))
		}
	}()

	for rows.Next() {
		var (
			key      Key
			scopes   string
			lastUsed sql.NullInt64
			created  int64
			revoked  sql.NullInt64
		)

		if err := rows.Scan(&key.ID, &key.TeamID, &key.UserID, &key.Name, &key.Prefix,
			&scopes, &key.HourlyLimit, &lastUsed, &created, &revoked); err != nil {
			return nil, fmt.Errorf("apikeys: list: %w", err)
		}

		if err := json.Unmarshal([]byte(scopes), &key.Scopes); err != nil {
			key.Scopes = nil
		}

		key.CreatedAt = time.Unix(created, 0).UTC()
		if lastUsed.Valid {
			key.LastUsedAt = time.Unix(lastUsed.Int64, 0).UTC()
		}
		if revoked.Valid {
			key.RevokedAt = time.Unix(revoked.Int64, 0).UTC()
		}

		keys = append(keys, key)
	}

	return keys, rows.Err()
}

// Revoke marks a key unusable. The row is kept rather than deleted so that the
// audit answer to "what was this key and when did it stop working" survives.
func (s *Store) Revoke(ctx context.Context, teamID, id int64) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND team_id = ? AND revoked_at IS NULL`,
		s.now().Unix(), id, teamID)
	if err != nil {
		return fmt.Errorf("apikeys: revoke: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("apikeys: revoke: %w", err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}
