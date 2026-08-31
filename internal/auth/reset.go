//
// reset.go
// Single-use, time-limited password reset tokens.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// ResetWindow is how long a reset link stays usable. An hour is the shortest
// window that still works for somebody who requested the reset, went to a
// meeting and came back — and a reset link is the single most valuable thing
// this system ever puts in an email.
const ResetWindow = time.Hour

// resetTokenBytes is the entropy in a reset token.
const resetTokenBytes = 32

// CreateReset issues a reset token for a user, invalidating any outstanding
// one. Two live reset links for one account means an old email someone else
// already read still opens the door after the owner has used the new one.
func (s *Store) CreateReset(ctx context.Context, userID int64) (string, error) {
	raw := make([]byte, resetTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: reset token: %w", err)
	}

	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("auth: create reset: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM password_reset_tokens WHERE user_id = ?", userID); err != nil {
		return "", fmt.Errorf("auth: create reset: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO password_reset_tokens (user_id, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?)
	`, userID, HashToken(token), now.Unix(), now.Add(ResetWindow).Unix()); err != nil {
		return "", fmt.Errorf("auth: create reset: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("auth: create reset: %w", err)
	}

	return token, nil
}

// ResetUserID resolves a token to the account it resets, without consuming it.
// The reset form needs to know whose password it is about to change before the
// user has typed the new one, and a token that were consumed by merely
// rendering the form would break the back button.
func (s *Store) ResetUserID(ctx context.Context, token string) (int64, error) {
	if token == "" {
		return 0, ErrNotFound
	}

	var (
		userID    int64
		expiresAt int64
		consumed  sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, expires_at, consumed_at FROM password_reset_tokens WHERE token_hash = ?
	`, HashToken(token)).Scan(&userID, &expiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("auth: read reset: %w", err)
	}

	if consumed.Valid {
		return 0, ErrTokenUsed
	}

	if expiresAt <= s.now().Unix() {
		return 0, ErrTokenExpired
	}

	return userID, nil
}

// ConsumeReset marks a token used and returns whose account it was for.
//
// The UPDATE carries the "not yet consumed" test in its own WHERE clause rather
// than trusting a check made a moment earlier, so two requests submitting the
// same link at once cannot both succeed — one of them updates zero rows and is
// told the link is spent.
func (s *Store) ConsumeReset(ctx context.Context, token string) (int64, error) {
	userID, err := s.ResetUserID(ctx, token)
	if err != nil {
		return 0, err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE password_reset_tokens
		SET consumed_at = ?
		WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ?
	`, s.now().Unix(), HashToken(token), s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("auth: consume reset: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("auth: consume reset: %w", err)
	}

	if affected == 0 {
		return 0, ErrTokenUsed
	}

	return userID, nil
}

// PruneResets deletes spent and expired tokens. It runs on the same schedule as
// the session prune: nothing depends on it, but a table of dead reset tokens is
// a table of things that only become interesting if the file is stolen.
func (s *Store) PruneResets(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM password_reset_tokens WHERE expires_at <= ? OR consumed_at IS NOT NULL", s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("auth: prune resets: %w", err)
	}

	return result.RowsAffected()
}
