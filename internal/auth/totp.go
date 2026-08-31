//
// totp.go
// Two-factor authentication: enrolment, verification and recovery codes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// Issuer is the name that appears in the authenticator app. It has to stay
// stable: changing it does not rename an existing entry, it makes the app show
// what looks like a second, unrelated account.
const Issuer = "feasible.lol"

// TOTPSkew is how many 30-second steps either side of now are accepted. One
// step means a code is valid for roughly ninety seconds in total, which covers
// a phone whose clock has drifted and a person who started typing just before
// the code rolled. Anything larger widens the guessing window for no usability
// gain.
const TOTPSkew = 1

// RecoveryCodeCount and recoveryCodeBytes size the printable backup codes. Ten
// codes is enough that somebody can lose a few and still get in; each is 80
// bits, which is a password nobody will guess and short enough to write down.
const (
	RecoveryCodeCount = 10
	recoveryCodeBytes = 10
)

// QRSize is the edge length of the enrolment QR code in pixels. Big enough for
// a phone to read off a laptop screen from arm's length.
const QRSize = 240

// BeginTOTP creates an unenrolled secret for a user and returns the key.
//
// The secret is stored immediately, encrypted, but totp_enabled_at stays NULL:
// enrolment is not finished until the user has proved they can produce a code
// from it. Storing it only after verification would mean the QR code the user
// scanned came from a value the server had thrown away, so a page reload during
// setup would silently invalidate the entry they just added to their phone.
func (s *Store) BeginTOTP(ctx context.Context, sealer *Sealer, user *User) (*otp.Key, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      Issuer,
		AccountName: user.Email,
		Period:      30,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: generate totp secret: %w", err)
	}

	sealed, err := sealer.Seal(key.Secret())
	if err != nil {
		return nil, err
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET totp_secret = ?, totp_enabled_at = NULL, updated_at = ? WHERE id = ?
	`, sealed, s.now().Unix(), user.ID); err != nil {
		return nil, fmt.Errorf("auth: store totp secret: %w", err)
	}

	return key, nil
}

// TOTPKey rebuilds the otp.Key for a user's stored secret, so the setup screen
// can redraw the QR code on a reload without issuing a new secret and stranding
// the entry the user has already scanned.
func (s *Store) TOTPKey(ctx context.Context, sealer *Sealer, user *User) (*otp.Key, error) {
	if user.TOTPSecret == "" {
		return nil, ErrNotFound
	}

	secret, err := sealer.Open(user.TOTPSecret)
	if err != nil {
		return nil, err
	}

	return otp.NewKeyFromURL(fmt.Sprintf(
		"otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		Issuer, user.Email, secret, Issuer))
}

// QRCodePNG renders an enrolment key as a PNG. It is a method on nothing
// because it needs no state: the key already carries everything the image
// encodes.
func QRCodePNG(key *otp.Key) ([]byte, error) {
	image, err := key.Image(QRSize, QRSize)
	if err != nil {
		return nil, fmt.Errorf("auth: render qr code: %w", err)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, image); err != nil {
		return nil, fmt.Errorf("auth: encode qr code: %w", err)
	}

	return buf.Bytes(), nil
}

// VerifyTOTP checks a code against a user's stored secret without changing any
// state. It is used both to finish enrolment and to answer the sign-in
// challenge, so that the two paths cannot drift apart in what they accept.
func (s *Store) VerifyTOTP(sealer *Sealer, user *User, code string) (bool, error) {
	if user.TOTPSecret == "" {
		return false, ErrNotFound
	}

	secret, err := sealer.Open(user.TOTPSecret)
	if err != nil {
		return false, err
	}

	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))

	valid, err := totp.ValidateCustom(code, secret, s.now(), totp.ValidateOpts{
		Period:    30,
		Skew:      TOTPSkew,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		// A malformed code is a wrong code, not a server error: people paste
		// their password into the box often enough that it must not 500.
		return false, nil
	}

	return valid, nil
}

// EnableTOTP finishes enrolment and returns the recovery codes.
//
// The codes are returned in plaintext exactly once, here, and only their hashes
// are stored. A recovery code is a password that bypasses the second factor, so
// keeping a readable copy would mean a stolen database defeats two-factor for
// every account that has it turned on.
func (s *Store) EnableTOTP(ctx context.Context, userID int64) ([]string, error) {
	codes, hashes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(hashes)
	if err != nil {
		return nil, fmt.Errorf("auth: encode recovery codes: %w", err)
	}

	now := s.now().Unix()

	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET totp_enabled_at = ?, totp_recovery_codes = ?, updated_at = ? WHERE id = ?
	`, now, string(encoded), now, userID); err != nil {
		return nil, fmt.Errorf("auth: enable two-factor: %w", err)
	}

	return codes, nil
}

// RegenerateRecoveryCodes issues a fresh set and discards the old ones, which
// is what somebody does after using one or losing the printout.
func (s *Store) RegenerateRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
	return s.EnableTOTP(ctx, userID)
}

// DisableTOTP turns two-factor off and wipes both the secret and the recovery
// codes. The handler requires the current password before calling it: a
// disable that only needed a live session would mean a stolen session cookie
// removes the factor that exists to make a stolen session survivable.
func (s *Store) DisableTOTP(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET totp_secret = '', totp_recovery_codes = '', totp_enabled_at = NULL, updated_at = ?
		WHERE id = ?
	`, s.now().Unix(), userID); err != nil {
		return fmt.Errorf("auth: disable two-factor: %w", err)
	}

	return nil
}

// ConsumeRecoveryCode spends one backup code, returning whether it matched.
//
// The matched code is removed from the stored list before the sign-in
// completes, which is what "single-use" means: a code read off a photograph of
// someone's printout works once, and the owner can see one is missing.
func (s *Store) ConsumeRecoveryCode(ctx context.Context, user *User, code string) (bool, error) {
	if user.TOTPRecovery == "" {
		return false, nil
	}

	var hashes []string
	if err := json.Unmarshal([]byte(user.TOTPRecovery), &hashes); err != nil {
		return false, fmt.Errorf("auth: read recovery codes: %w", err)
	}

	want := HashToken(normaliseRecoveryCode(code))

	kept := make([]string, 0, len(hashes))
	matched := false

	for _, hash := range hashes {
		if !matched && constantTimeEqual(hash, want) {
			matched = true
			continue
		}

		kept = append(kept, hash)
	}

	if !matched {
		return false, nil
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return false, fmt.Errorf("auth: write recovery codes: %w", err)
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET totp_recovery_codes = ?, updated_at = ? WHERE id = ?
	`, string(encoded), s.now().Unix(), user.ID); err != nil {
		return false, fmt.Errorf("auth: spend recovery code: %w", err)
	}

	return true, nil
}

// RecoveryCodesLeft counts what is still usable, so the security screen can
// warn somebody before they run out rather than after.
func RecoveryCodesLeft(user *User) int {
	if user.TOTPRecovery == "" {
		return 0
	}

	var hashes []string
	if err := json.Unmarshal([]byte(user.TOTPRecovery), &hashes); err != nil {
		return 0
	}

	return len(hashes)
}

// generateRecoveryCodes mints the printable codes and their stored hashes.
func generateRecoveryCodes() ([]string, []string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	hashes := make([]string, 0, RecoveryCodeCount)

	for i := 0; i < RecoveryCodeCount; i++ {
		raw := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("auth: generate recovery code: %w", err)
		}

		// Lowercase base32 without padding, hyphenated in the middle. It
		// survives being written on paper and read back: no case to get wrong,
		// and none of the characters that look like each other in handwriting.
		body := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
		code := body[:8] + "-" + body[8:]

		codes = append(codes, code)
		hashes = append(hashes, HashToken(normaliseRecoveryCode(code)))
	}

	return codes, hashes, nil
}

// normaliseRecoveryCode strips the formatting a person is likely to get wrong
// when typing a code back in — the hyphen, spaces, and the case. What is hashed
// is the bare characters, so "ABCD EFGH-IJKL" and "abcdefghijkl" are the same
// code.
func normaliseRecoveryCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")

	return code
}

// TwoFactorPendingWindow is how long the half-finished sign-in cookie lives
// between the password step and the code step. Five minutes is long enough to
// find a phone and short enough that walking away from the machine does not
// leave a usable half-credential sitting in the browser.
const TwoFactorPendingWindow = 5 * time.Minute
