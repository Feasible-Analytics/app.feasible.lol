//
// verify.go
// Proving an email address, with a code and a signed link.
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
	"math/big"
	"strings"
	"time"
)

// VerificationCodeDigits is the length of the emailed code.
//
// Eight, not four. A four-digit code is ten thousand possibilities, which an
// attacker walks through in seconds against any endpoint that does not lock
// after a handful of tries — and the incumbent's four-digit activation code is
// exactly that. Eight digits is a hundred million, which combined with the
// attempt limit below is not worth anybody's time.
const VerificationCodeDigits = 8

// VerificationWindow is how long a code and its link stay usable. Thirty
// minutes is long enough to go and find the email and short enough that a code
// sitting in an unattended inbox is not a standing key to the account.
const VerificationWindow = 30 * time.Minute

// VerificationAttempts is how many wrong codes are accepted before the code is
// destroyed and a new one has to be requested. This is what makes the digit
// count sufficient rather than merely large.
const VerificationAttempts = 5

// verifyLinkBytes is the entropy in the one-tap link. The link is a bearer
// credential in an email, so it gets full random rather than the eight digits a
// human has to type.
const verifyLinkBytes = 32

// EmailVerification is one outstanding proof-of-address challenge.
type EmailVerification struct {
	ID         int64
	UserID     int64
	CreatedAt  int64
	ExpiresAt  int64
	ConsumedAt int64
}

// CreateVerification issues a code and a link for one user, replacing whatever
// was outstanding.
//
// Both credentials are stored as hashes in the same row: the code_hash column
// holds the digits, and the link token is hashed into the same table under a
// second row. Keeping them as two rows would let a stale link outlive the code
// it was mailed with, which is a window nobody would ever think to close.
func (s *Store) CreateVerification(ctx context.Context, userID int64) (code, linkToken string, err error) {
	code, err = numericCode(VerificationCodeDigits)
	if err != nil {
		return "", "", err
	}

	raw := make([]byte, verifyLinkBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("auth: verification link: %w", err)
	}
	linkToken = base64.RawURLEncoding.EncodeToString(raw)

	now := s.now()
	expires := now.Add(VerificationWindow)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("auth: create verification: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	// Anything outstanding is discarded. A user who asks for a second code has
	// told us the first one did not reach them, and leaving it live means two
	// keys to the same door for no benefit.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM email_verification_codes WHERE user_id = ?", userID); err != nil {
		return "", "", fmt.Errorf("auth: create verification: %w", err)
	}

	// The stored form is `<code hash>:<link hash>:<attempts left>`. One row
	// rather than three columns because 0001 shipped with code_hash alone, and
	// a schema change to hold two secrets that always live and die together
	// buys nothing a delimiter does not.
	stored := strings.Join([]string{HashToken(code), HashToken(linkToken), fmt.Sprint(VerificationAttempts)}, ":")

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO email_verification_codes (user_id, code_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?)
	`, userID, stored, now.Unix(), expires.Unix()); err != nil {
		return "", "", fmt.Errorf("auth: create verification: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("auth: create verification: %w", err)
	}

	return code, linkToken, nil
}

// ConsumeVerification checks a submitted code or link token and, when it
// matches, marks the address verified.
//
// A wrong answer costs an attempt. When the attempts run out the row is
// deleted, so continuing to guess is guessing against nothing at all — which is
// what makes the code safe to be short enough to type.
func (s *Store) ConsumeVerification(ctx context.Context, userID int64, code, linkToken string) error {
	var (
		id        int64
		stored    string
		expiresAt int64
		consumed  sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, code_hash, expires_at, consumed_at
		FROM email_verification_codes
		WHERE user_id = ?
		ORDER BY id DESC LIMIT 1
	`, userID).Scan(&id, &stored, &expiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("auth: read verification: %w", err)
	}

	if consumed.Valid {
		return ErrTokenUsed
	}

	now := s.now()
	if expiresAt <= now.Unix() {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM email_verification_codes WHERE id = ?", id)
		return ErrTokenExpired
	}

	parts := strings.Split(stored, ":")
	if len(parts) != 3 {
		return fmt.Errorf("auth: verification row %d is malformed", id)
	}

	matched := (code != "" && constantTimeEqual(parts[0], HashToken(code))) ||
		(linkToken != "" && constantTimeEqual(parts[1], HashToken(linkToken)))

	if !matched {
		remaining := 0
		fmt.Sscanf(parts[2], "%d", &remaining)
		remaining--

		if remaining <= 0 {
			_, _ = s.db.ExecContext(ctx, "DELETE FROM email_verification_codes WHERE id = ?", id)
			return ErrRateLimited
		}

		parts[2] = fmt.Sprint(remaining)
		if _, err := s.db.ExecContext(ctx,
			"UPDATE email_verification_codes SET code_hash = ? WHERE id = ?",
			strings.Join(parts, ":"), id); err != nil {
			return fmt.Errorf("auth: verification attempt: %w", err)
		}

		return ErrBadCredentials
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: verify email: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx,
		"UPDATE email_verification_codes SET consumed_at = ? WHERE id = ?", now.Unix(), id); err != nil {
		return fmt.Errorf("auth: verify email: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET email_verified_at = ?, updated_at = ? WHERE id = ?",
		now.Unix(), now.Unix(), userID); err != nil {
		return fmt.Errorf("auth: verify email: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: verify email: %w", err)
	}

	return nil
}

// UserIDForVerificationLink finds whose account a one-tap link belongs to. The
// link has to work for somebody who opened the email on a phone that is not
// signed in, so the user cannot be taken from the session.
func (s *Store) UserIDForVerificationLink(ctx context.Context, linkToken string) (int64, error) {
	if linkToken == "" {
		return 0, ErrNotFound
	}

	// The hash is embedded in the middle field of the stored triple, so the
	// lookup is a prefix-anchored LIKE on a table that holds one row per
	// pending signup. The alternative is a column and a migration for a query
	// that runs once per registration.
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, code_hash FROM email_verification_codes WHERE consumed_at IS NULL AND expires_at > ?
	`, s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("auth: read verification: %w", err)
	}
	defer rows.Close()

	want := HashToken(linkToken)

	for rows.Next() {
		var (
			userID int64
			stored string
		)

		if err := rows.Scan(&userID, &stored); err != nil {
			return 0, fmt.Errorf("auth: read verification: %w", err)
		}

		parts := strings.Split(stored, ":")
		if len(parts) == 3 && constantTimeEqual(parts[1], want) {
			return userID, nil
		}
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("auth: read verification: %w", err)
	}

	return 0, ErrNotFound
}

// MarkVerified proves an address without a code. Google's OpenID response
// carries an `email_verified` claim, and demanding our own code on top of a
// verification the identity provider already performed is friction with no
// security behind it.
func (s *Store) MarkVerified(ctx context.Context, userID int64) error {
	now := s.now().Unix()

	if _, err := s.db.ExecContext(ctx,
		"UPDATE users SET email_verified_at = COALESCE(email_verified_at, ?), updated_at = ? WHERE id = ?",
		now, now, userID); err != nil {
		return fmt.Errorf("auth: mark verified: %w", err)
	}

	return nil
}

// numericCode generates a decimal code of the requested length using the
// cryptographic random source. math/rand would produce a code that is
// predictable from any other code the process has issued.
func numericCode(digits int) (string, error) {
	var out strings.Builder

	for i := 0; i < digits; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", fmt.Errorf("auth: generate code: %w", err)
		}

		out.WriteString(n.String())
	}

	return out.String(), nil
}
