//
// auth.go
// The system store behind every identity, team and site operation.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package auth is the whole signed-in experience that is not the stats
// dashboard: registration, sign-in, sessions, two-factor, Google linking, the
// sites list, site settings and onboarding.
//
// It is server-rendered Go templates rather than part of the React bundle. The
// dashboard is a single-page application because it is one screen with a dozen
// interacting filters; none of that is true of a login form, and making
// somebody download a JavaScript application before they can type a password is
// how a sign-in page ends up slower than the product behind it.
//
// Everything here reads and writes system.db, which is the only database that
// knows who people are. The per-account analytics databases are opened only for
// the two things that genuinely need them — a sparkline on the sites list and
// resetting or deleting a site's stats.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Common failures callers branch on. They are sentinels rather than strings so
// a handler can tell "wrong password" from "no such user" without matching on
// text, while still showing the same message to the browser for both.
var (
	ErrNotFound        = errors.New("auth: not found")
	ErrEmailTaken      = errors.New("auth: that email address is already registered")
	ErrBadCredentials  = errors.New("auth: email or password is wrong")
	ErrNotVerified     = errors.New("auth: email address is not verified")
	ErrRateLimited     = errors.New("auth: too many attempts")
	ErrTokenExpired    = errors.New("auth: this link has expired")
	ErrTokenUsed       = errors.New("auth: this link has already been used")
	ErrDomainTaken     = errors.New("auth: that domain is already registered")
	ErrTwoFactorNeeded = errors.New("auth: a two-factor code is required")
	ErrSignupDisabled  = errors.New("auth: public account registration is disabled on this installation")
)

// Store is the system database plus the clock everything here reads. The clock
// is injectable because half of this package is about expiry windows, and a
// test that has to sleep for fourteen days is a test nobody runs.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore wraps an open system database. It does not migrate: migrations are
// deliberate and observable everywhere else in this binary, and a package that
// quietly upgraded the schema on construction would be the one exception.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// SetClock replaces the clock. Tests use it to walk a session up to its
// fourteenth idle day without waiting for one.
func (s *Store) SetClock(now func() time.Time) {
	s.now = now
}

// Now reports the current time through this store's clock, so nothing inside
// the package ever calls time.Now directly and accidentally escapes the clock a
// test installed.
func (s *Store) Now() time.Time {
	return s.now()
}

// DB exposes the underlying handle for the few callers that need to run their
// own query — the routing cache refresh, and the account deletion that has to
// span several tables in one transaction.
func (s *Store) DB() *sql.DB {
	return s.db
}

// nullInt64 turns a nullable unix timestamp column into a plain int64, with
// zero meaning "never". Every timestamp in system.db is unix seconds, and
// carrying sql.NullInt64 through the whole package would put a two-field check
// at every call site for a distinction only the database cares about.
func nullInt64(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}

	return v.Int64
}

// closeSQLRows closes a read cursor and joins any driver cleanup failure to the
// operation error, preserving both when iteration and cleanup fail together.
func closeSQLRows(rows *sql.Rows, err *error, operation string) {
	if closeErr := rows.Close(); closeErr != nil {
		*err = errors.Join(*err, fmt.Errorf("auth: %s: close rows: %w", operation, closeErr))
	}
}

// exists answers a one-row existence query. It is here rather than repeated
// because the sql.ErrNoRows branch is the part people forget, and forgetting it
// turns "no such row" into a 500.
func exists(ctx context.Context, db *sql.DB, query string, args ...any) (bool, error) {
	var one int

	err := db.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: %w", err)
	}

	return true, nil
}
