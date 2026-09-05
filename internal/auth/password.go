//
// password.go
// Hashing, checking and changing passwords.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// BcryptCost is the work factor. Twelve is roughly a quarter of a second on
// current hardware: slow enough that an offline attack on a stolen database is
// expensive, fast enough that a sign-in does not feel broken. It is a constant
// rather than bcrypt.DefaultCost because the default has not moved in years and
// hardware has.
const BcryptCost = 12

// MinPasswordLength is the shortest password accepted.
//
// Length is the only rule. Composition rules — a digit, a symbol, mixed case —
// measurably push people towards `Password1!` and towards writing it down,
// while doing nothing an attacker's wordlist has not already accounted for. A
// low floor plus a check against the obvious ones is a better trade than a
// regular expression nobody can satisfy, and online guessing is answered by
// rate limiting and bcrypt rather than by the length of the field.
const MinPasswordLength = 6

// MaxPasswordLength is the longest password accepted. bcrypt silently truncates
// at 72 bytes, so anything past it is not merely ignored — it makes two
// different passwords equal. Rejecting is the honest response.
const MaxPasswordLength = 72

// commonPasswords are the strings that are long enough to pass the length rule
// and still worthless. The list is deliberately short: the value is in catching
// the handful of things people type when a form asks for the minimum, not in
// shipping a dictionary.
var commonPasswords = map[string]bool{
	"password":         true,
	"password1":        true,
	"password123":      true,
	"password1234":     true,
	"passwordpassword": true,
	"123456":           true,
	"1234567":          true,
	"12345678":         true,
	"123456789":        true,
	"1234567890":       true,
	"123456789012":     true,
	"qwerty":           true,
	"qwerty123":        true,
	"qwertyuiop12":     true,
	"abc123":           true,
	"letmein":          true,
	"letmein12345":     true,
	"iloveyou":         true,
	"iloveyou1234":     true,
	"monkey":           true,
	"dragon":           true,
	"football":         true,
	"admin":            true,
	"administrator":    true,
	"changeme":         true,
	"changeme1234":     true,
	"welcome":          true,
	"welcome12345":     true,
	"secret":           true,
	"trustno1":         true,
}

// HashPassword produces the stored form of a password.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}

	return string(hash), nil
}

// CheckPassword compares a candidate against a stored hash.
//
// An account with no password — a Google-only signup — is rejected without
// calling bcrypt, but only after a comparison against a dummy hash, so that the
// response time does not tell an attacker which addresses have passwords and
// which do not.
func CheckPassword(hash, password string) bool {
	if hash == "" {
		// The cost here has to match BcryptCost or the timing does not match
		// either, which is the entire point of doing the work at all.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return false
	}

	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// dummyHash is a valid bcrypt hash at BcryptCost of a value nothing will ever
// submit. It exists only to burn the same amount of time a real comparison
// would, so that "no such user" and "wrong password" take the same wall clock.
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.5CmNsvW9y6zWfHIkFPZxsvSyfELIWlm"

// ValidatePassword rejects a password we should not store. The error text is
// what the form shows, so it says what to do rather than what went wrong.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("use at least %d characters — length is what makes a password hard to guess", MinPasswordLength)
	}

	if len(password) > MaxPasswordLength {
		return fmt.Errorf("use at most %d characters", MaxPasswordLength)
	}

	if commonPasswords[strings.ToLower(password)] {
		return fmt.Errorf("that is one of the most commonly used passwords — pick something else")
	}

	// A password of one repeated character passes any length rule and survives
	// no wordlist at all.
	if allSameRune(password) {
		return fmt.Errorf("that is the same character repeated — pick something else")
	}

	return nil
}

// allSameRune reports whether a string is one character over and over.
func allSameRune(s string) bool {
	if s == "" {
		return true
	}

	runes := []rune(s)
	for _, r := range runes[1:] {
		if r != runes[0] {
			return false
		}
	}

	return true
}

// LooksLikeEmail is the only address validation this package does.
//
// It checks for one at-sign with something either side and a dot in the domain,
// and nothing more. Every stricter rule anyone writes rejects addresses that
// genuinely deliver, and the actual proof that an address exists is the
// verification email — which is sent regardless.
func LooksLikeEmail(email string) bool {
	email = NormaliseEmail(email)

	at := strings.Index(email, "@")
	if at <= 0 || at == len(email)-1 {
		return false
	}

	if strings.Count(email, "@") != 1 {
		return false
	}

	domain := email[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}

	for _, r := range email {
		if unicode.IsSpace(r) {
			return false
		}
	}

	return true
}

// SetPassword writes a new password hash and signs every other browser out.
//
// The revocation is not optional. A password change that leaves the old
// sessions alive does nothing for the case the feature exists for — somebody
// else is signed in and should not be — and that is exactly the case where the
// user believes they have fixed the problem.
func (s *Store) SetPassword(ctx context.Context, userID int64, password string, keepSessionID int64) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: set password: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?
	`, hash, s.now().Unix(), userID); err != nil {
		return fmt.Errorf("auth: set password: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM user_sessions WHERE user_id = ? AND id <> ?
	`, userID, keepSessionID); err != nil {
		return fmt.Errorf("auth: revoke sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: set password: %w", err)
	}

	return nil
}

// constantTimeEqual compares two secrets without leaking their contents through
// how long the comparison took. It is used for the token and code hashes, which
// are compared in Go rather than in SQL wherever the query cannot use the
// unique index to do it.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
