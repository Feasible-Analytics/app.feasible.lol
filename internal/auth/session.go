//
// session.go
// Sign-in sessions: the row, the cookie, and the rolling idle window.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SessionCookieName is the cookie that carries the session token. The __Host-
// prefix is not used, and that is a decision rather than an oversight: browsers
// only accept it over HTTPS, and a self-hoster reaching the dashboard over
// plain HTTP on their own LAN is a first-class case for this product.
const SessionCookieName = "feasible_session"

// SessionIdleWindow is how long a session survives without being used. It is a
// rolling deadline pushed forward on every request, not a fixed lifetime: an
// absolute expiry signs out the person who uses the product every day, which is
// exactly backwards.
const SessionIdleWindow = 14 * 24 * time.Hour

// sessionTokenBytes is the entropy in a session token. Thirty-two bytes is far
// past guessing and is what the cookie carries; the database stores only its
// SHA-256, so a stolen copy of control.db cannot be replayed at the login page.
const sessionTokenBytes = 32

// touchInterval is how stale the last-seen timestamp is allowed to get before a
// request writes it. Writing on every request would put a control-database
// write on every page load and every dashboard XHR, all contending for the one
// writer connection, to move a timestamp nobody reads to the second.
const touchInterval = 5 * time.Minute

// Session is one signed-in browser.
type Session struct {
	ID          int64
	UserID      int64
	DeviceLabel string
	CreatedAt   int64
	LastSeenAt  int64
	ExpiresAt   int64
}

// NewSessionToken mints a token and its stored hash. The pair is returned
// together so there is no way to write the raw token to the database by
// accident: the only value with a String type here is the one that goes in the
// cookie.
func NewSessionToken() (token, hash string, err error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("auth: session token: %w", err)
	}

	token = base64.RawURLEncoding.EncodeToString(raw)

	return token, HashToken(token), nil
}

// HashToken is the one-way transform every stored credential in this package
// goes through. SHA-256 rather than bcrypt because these are 256-bit random
// values, not passwords: there is nothing to brute-force, and a slow hash would
// only make every request slower.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// CreateSession records a signed-in browser and returns the cookie token.
func (s *Store) CreateSession(ctx context.Context, userID int64, deviceLabel string) (string, *Session, error) {
	token, hash, err := NewSessionToken()
	if err != nil {
		return "", nil, err
	}

	now := s.now()
	expires := now.Add(SessionIdleWindow)

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO user_sessions (user_id, token_hash, device_label, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, userID, hash, deviceLabel, now.Unix(), now.Unix(), expires.Unix())
	if err != nil {
		return "", nil, fmt.Errorf("auth: create session: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return "", nil, fmt.Errorf("auth: create session: %w", err)
	}

	return token, &Session{
		ID:          id,
		UserID:      userID,
		DeviceLabel: deviceLabel,
		CreatedAt:   now.Unix(),
		LastSeenAt:  now.Unix(),
		ExpiresAt:   expires.Unix(),
	}, nil
}

// SessionByToken resolves a cookie to its session, pushing the idle deadline
// forward when it is worth a write. An expired session is deleted here rather
// than left for a cleanup job, because the request that just found it is the
// one place we know for certain the row is dead.
func (s *Store) SessionByToken(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}

	var session Session

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, device_label, created_at, last_seen_at, expires_at
		FROM user_sessions WHERE token_hash = ?
	`, HashToken(token)).Scan(&session.ID, &session.UserID, &session.DeviceLabel,
		&session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read session: %w", err)
	}

	now := s.now()

	if session.ExpiresAt <= now.Unix() {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM user_sessions WHERE id = ?", session.ID)
		return nil, ErrNotFound
	}

	if now.Unix()-session.LastSeenAt >= int64(touchInterval.Seconds()) {
		expires := now.Add(SessionIdleWindow)

		if _, err := s.db.ExecContext(ctx, `
			UPDATE user_sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?
		`, now.Unix(), expires.Unix(), session.ID); err != nil {
			return nil, fmt.Errorf("auth: touch session: %w", err)
		}

		session.LastSeenAt = now.Unix()
		session.ExpiresAt = expires.Unix()

		_ = s.TouchUser(ctx, session.UserID, now)
	}

	return &session, nil
}

// ListSessions returns a person's signed-in browsers, most recently used
// first. This is what the login-management screen renders, so the order is the
// one that puts "this browser" and anything suspicious at the top.
func (s *Store) ListSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, device_label, created_at, last_seen_at, expires_at
		FROM user_sessions
		WHERE user_id = ? AND expires_at > ?
		ORDER BY last_seen_at DESC
	`, userID, s.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session

	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.UserID, &session.DeviceLabel,
			&session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt); err != nil {
			return nil, fmt.Errorf("auth: list sessions: %w", err)
		}

		sessions = append(sessions, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}

	return sessions, nil
}

// RevokeSession signs one browser out. The user id is part of the WHERE clause
// so that a forged id in the form cannot revoke somebody else's session.
func (s *Store) RevokeSession(ctx context.Context, userID, sessionID int64) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM user_sessions WHERE id = ? AND user_id = ?", sessionID, userID); err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}

	return nil
}

// RevokeAllSessions signs every browser out except the one asking, which is
// what the "sign out everywhere else" button does.
func (s *Store) RevokeAllSessions(ctx context.Context, userID, keepSessionID int64) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM user_sessions WHERE user_id = ? AND id <> ?", userID, keepSessionID); err != nil {
		return fmt.Errorf("auth: revoke sessions: %w", err)
	}

	return nil
}

// PruneSessions deletes the expired rows. SessionByToken already removes the
// ones somebody comes back to; this is for the far more common case of a
// browser that is simply never used again.
func (s *Store) PruneSessions(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM user_sessions WHERE expires_at <= ?", s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("auth: prune sessions: %w", err)
	}

	return result.RowsAffected()
}

// SetSessionCookie writes the session cookie.
//
// There is deliberately no Domain attribute, and it must stay that way. A
// cookie with no Domain is a host-only cookie: the browser sends it back to
// exactly the host that set it, whatever that host happens to be. Deriving a
// Domain from the configured base URL instead breaks every way of reaching the
// dashboard that is not that hostname — a port-forward to localhost, a second
// domain, internal cluster DNS, a NAS on its LAN name — and it breaks them
// invisibly. The browser simply declines to store the cookie, the next request
// arrives with no session, and the user sees a bare 403 with nothing in any
// log. The incumbent shipped exactly this and broke their self-hosters for a
// whole release before reverting it.
//
// Secure is set only for an https base URL for the same family of reasons: a
// Secure cookie over plain HTTP is silently dropped, and plain HTTP on a
// private network is a supported way to run this product.
func SetSessionCookie(w http.ResponseWriter, token, baseURL string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(baseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionIdleWindow.Seconds()),
	})
}

// ClearSessionCookie removes the session cookie. The attributes have to match
// the ones it was set with — a delete that differs in Path or Domain creates a
// second cookie instead of removing the first, which is how a sign-out button
// ends up doing nothing at all.
func ClearSessionCookie(w http.ResponseWriter, baseURL string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(baseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// DeviceLabel turns a user agent into something a person recognises on the
// login-management screen.
//
// It is a coarse browser-and-platform guess on purpose. The screen exists to
// answer "is one of these not me", and "Chrome on macOS, last seen 3 minutes
// ago" answers it; a full version string does not answer it any better and
// makes the list unreadable.
func DeviceLabel(userAgent string) string {
	if strings.TrimSpace(userAgent) == "" {
		return "Unknown device"
	}

	browser := "Unknown browser"

	switch {
	case strings.Contains(userAgent, "Edg/"):
		browser = "Edge"
	case strings.Contains(userAgent, "OPR/"), strings.Contains(userAgent, "Opera"):
		browser = "Opera"
	case strings.Contains(userAgent, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(userAgent, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(userAgent, "Safari/"):
		browser = "Safari"
	case strings.Contains(userAgent, "curl/"):
		browser = "curl"
	}

	platform := "Unknown platform"

	switch {
	case strings.Contains(userAgent, "iPhone"):
		platform = "iPhone"
	case strings.Contains(userAgent, "iPad"):
		platform = "iPad"
	case strings.Contains(userAgent, "Android"):
		platform = "Android"
	case strings.Contains(userAgent, "Mac OS X"), strings.Contains(userAgent, "Macintosh"):
		platform = "macOS"
	case strings.Contains(userAgent, "Windows"):
		platform = "Windows"
	case strings.Contains(userAgent, "CrOS"):
		platform = "ChromeOS"
	case strings.Contains(userAgent, "Linux"):
		platform = "Linux"
	}

	return browser + " on " + platform
}

// HasSeenDevice reports whether this account has signed in from a device with
// this label before. It is what decides whether the new-sign-in email is worth
// sending: one on every login is noise people learn to ignore, which is the
// same as not sending it.
func (s *Store) HasSeenDevice(ctx context.Context, userID int64, label string) (bool, error) {
	return exists(ctx, s.db,
		"SELECT 1 FROM user_sessions WHERE user_id = ? AND device_label = ? LIMIT 1", userID, label)
}
