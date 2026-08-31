//
// totp_test.go
// Two-factor enrolment, encryption at rest, and single-use recovery codes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/pquerna/otp/totp"
)

// TestTOTPSecretIsEncryptedAtRest checks the stored value is not the secret. A
// two-factor secret in plaintext means a stolen copy of control.db defeats
// two-factor for every account that has it turned on.
func TestTOTPSecretIsEncryptedAtRest(t *testing.T) {
	s, db := newTestStore(t)
	sealer := newTestSealer(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	key, err := s.BeginTOTP(ctx, sealer, user)
	if err != nil {
		t.Fatalf("begin totp: %v", err)
	}

	var stored string
	if err := db.QueryRow("SELECT totp_secret FROM users WHERE id = ?", user.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}

	if strings.Contains(stored, key.Secret()) {
		t.Fatal("the stored value contains the plaintext secret")
	}

	opened, err := sealer.Open(stored)
	if err != nil {
		t.Fatalf("open sealed secret: %v", err)
	}

	if opened != key.Secret() {
		t.Errorf("the sealed secret did not round-trip: got %q", opened)
	}
}

// TestEnrolmentIsNotEnabledUntilACodeIsProven checks that storing the secret
// and switching two-factor on are separate steps. Storing it immediately is
// what lets a page reload redraw the same QR code instead of stranding the
// entry somebody has already added to their phone.
func TestEnrolmentIsNotEnabledUntilACodeIsProven(t *testing.T) {
	s, _ := newTestStore(t)
	sealer := newTestSealer(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	key, err := s.BeginTOTP(ctx, sealer, user)
	if err != nil {
		t.Fatalf("begin totp: %v", err)
	}

	midway, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if midway.TwoFactorEnabled() {
		t.Fatal("two-factor must not be demanded before the user has proven a code")
	}

	if midway.TOTPSecret == "" {
		t.Fatal("the secret should be stored so a reload redraws the same QR code")
	}

	// The reloaded key has to be the same one, or a page refresh mid-setup
	// invalidates the entry on the phone.
	same, err := s.TOTPKey(ctx, sealer, midway)
	if err != nil {
		t.Fatalf("reload totp key: %v", err)
	}

	if same.Secret() != key.Secret() {
		t.Error("reloading the enrolment must not issue a new secret")
	}

	if _, err := s.EnableTOTP(ctx, user.ID); err != nil {
		t.Fatalf("enable totp: %v", err)
	}

	enabled, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if !enabled.TwoFactorEnabled() {
		t.Error("two-factor should be on once enrolment finished")
	}
}

// TestTOTPVerifiesARealCode drives the real algorithm rather than a stub, so a
// change to the period, digit count or hash is caught here rather than by
// somebody being locked out of their account.
func TestTOTPVerifiesARealCode(t *testing.T) {
	s, _ := newTestStore(t)
	sealer := newTestSealer(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	key, err := s.BeginTOTP(ctx, sealer, user)
	if err != nil {
		t.Fatalf("begin totp: %v", err)
	}

	reloaded, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	code, err := totp.GenerateCode(key.Secret(), s.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	valid, err := s.VerifyTOTP(sealer, reloaded, code)
	if err != nil {
		t.Fatalf("verify totp: %v", err)
	}

	if !valid {
		t.Error("a freshly generated code should verify")
	}

	valid, err = s.VerifyTOTP(sealer, reloaded, "000000")
	if err != nil {
		t.Fatalf("verify totp: %v", err)
	}

	if valid {
		t.Error("a wrong code must not verify")
	}

	// People paste their password into the code box often enough that a
	// malformed value has to be a wrong code, not a 500.
	if _, err := s.VerifyTOTP(sealer, reloaded, "not a code at all"); err != nil {
		t.Errorf("a malformed code should be a wrong code, not an error: %v", err)
	}
}

// TestQRCodeRenders checks the enrolment image is a real PNG, since the whole
// setup screen is unusable without it.
func TestQRCodeRenders(t *testing.T) {
	s, _ := newTestStore(t)
	sealer := newTestSealer(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	key, err := s.BeginTOTP(ctx, sealer, user)
	if err != nil {
		t.Fatalf("begin totp: %v", err)
	}

	png, err := QRCodePNG(key)
	if err != nil {
		t.Fatalf("render qr code: %v", err)
	}

	if !bytes.HasPrefix(png, []byte{0x89, 'P', 'N', 'G'}) {
		t.Error("the QR code should be a PNG")
	}
}

// TestRecoveryCodesAreSingleUse checks the whole meaning of a recovery code: a
// code read off a photograph of somebody's printout works once, and the owner
// can see one is missing.
func TestRecoveryCodesAreSingleUse(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	codes, err := s.EnableTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("enable totp: %v", err)
	}

	if len(codes) != RecoveryCodeCount {
		t.Fatalf("want %d recovery codes, got %d", RecoveryCodeCount, len(codes))
	}

	reloaded, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if strings.Contains(reloaded.TOTPRecovery, codes[0]) {
		t.Fatal("recovery codes are stored in plaintext")
	}

	used, err := s.ConsumeRecoveryCode(ctx, reloaded, codes[0])
	if err != nil {
		t.Fatalf("consume recovery code: %v", err)
	}

	if !used {
		t.Fatal("a valid recovery code should be accepted")
	}

	reloaded, err = s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	used, err = s.ConsumeRecoveryCode(ctx, reloaded, codes[0])
	if err != nil {
		t.Fatalf("consume recovery code: %v", err)
	}

	if used {
		t.Error("a spent recovery code must not work twice")
	}

	if left := RecoveryCodesLeft(reloaded); left != RecoveryCodeCount-1 {
		t.Errorf("want %d codes left, got %d", RecoveryCodeCount-1, left)
	}
}

// TestRecoveryCodesIgnoreFormatting checks that the hyphen, the spaces and the
// case a person gets wrong when typing a code off paper do not matter.
func TestRecoveryCodesIgnoreFormatting(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	codes, err := s.EnableTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("enable totp: %v", err)
	}

	reloaded, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	messy := " " + strings.ToUpper(strings.ReplaceAll(codes[0], "-", " ")) + " "

	used, err := s.ConsumeRecoveryCode(ctx, reloaded, messy)
	if err != nil {
		t.Fatalf("consume recovery code: %v", err)
	}

	if !used {
		t.Errorf("a code typed as %q should still match %q", messy, codes[0])
	}
}

// TestDisableWipesEverything checks that turning two-factor off leaves no
// secret and no recovery codes behind.
func TestDisableWipesEverything(t *testing.T) {
	s, _ := newTestStore(t)
	sealer := newTestSealer(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := s.BeginTOTP(ctx, sealer, user); err != nil {
		t.Fatalf("begin totp: %v", err)
	}

	if _, err := s.EnableTOTP(ctx, user.ID); err != nil {
		t.Fatalf("enable totp: %v", err)
	}

	if err := s.DisableTOTP(ctx, user.ID); err != nil {
		t.Fatalf("disable totp: %v", err)
	}

	reloaded, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if reloaded.TOTPSecret != "" || reloaded.TOTPRecovery != "" || reloaded.TwoFactorEnabled() {
		t.Error("disabling two-factor should leave nothing behind")
	}
}
