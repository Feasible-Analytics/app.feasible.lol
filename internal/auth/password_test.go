//
// password_test.go
// Hashing, the length rule, and the revocation that comes with a change.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"strings"
	"testing"
)

// TestPasswordRules checks the length floor at both ends. The upper bound is
// not cosmetic: bcrypt truncates at 72 bytes, so anything past it does not
// merely get ignored, it makes two different passwords equal.
func TestPasswordRules(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Error("a short password should be rejected")
	}

	// The floor itself, pinned from both sides. A rule nobody tests at its
	// boundary is a rule that drifts by one the next time somebody edits it.
	if err := ValidatePassword(strings.Repeat("ab", MinPasswordLength)[:MinPasswordLength-1]); err == nil {
		t.Error("one character under the floor should be rejected")
	}

	if err := ValidatePassword(strings.Repeat("ab", MinPasswordLength)[:MinPasswordLength]); err != nil {
		t.Errorf("exactly the floor should be accepted: %v", err)
	}

	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLength+1)); err == nil {
		t.Error("a password longer than bcrypt's 72 bytes should be rejected")
	}

	if err := ValidatePassword(strings.Repeat("a", MinPasswordLength)); err == nil {
		t.Error("a repeated character should be rejected")
	}

	if err := ValidatePassword("password1234"); err == nil {
		t.Error("a common password should be rejected")
	}

	if err := ValidatePassword("correct horse battery staple"); err != nil {
		t.Errorf("a passphrase should be accepted: %v", err)
	}
}

// TestCheckPassword covers the three outcomes, including the one that exists
// only for timing: an account with no password must be refused after the same
// work a real comparison would have cost.
func TestCheckPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	if !CheckPassword(hash, "correct horse battery staple") {
		t.Error("the right password should verify")
	}

	if CheckPassword(hash, "something else") {
		t.Error("the wrong password should not verify")
	}

	if CheckPassword("", "anything") {
		t.Error("an account with no password must never accept one")
	}
}

// TestPasswordChangeRevokesOtherSessions checks the revocation that makes a
// password change mean something when somebody else is already signed in — the
// only case the feature exists for.
func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	keepToken, keep, err := s.CreateSession(ctx, user.ID, "Chrome on macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	otherToken, _, err := s.CreateSession(ctx, user.ID, "Firefox on Linux")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := s.SetPassword(ctx, user.ID, "a-long-enough-password", keep.ID); err != nil {
		t.Fatalf("set password: %v", err)
	}

	if _, err := s.SessionByToken(ctx, keepToken); err != nil {
		t.Errorf("the browser making the change should stay signed in: %v", err)
	}

	if _, err := s.SessionByToken(ctx, otherToken); err != ErrNotFound {
		t.Errorf("every other browser should be signed out, got %v", err)
	}

	reloaded, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if !CheckPassword(reloaded.PasswordHash, "a-long-enough-password") {
		t.Error("the new password should verify against the stored hash")
	}
}

// TestLooksLikeEmail checks the deliberately permissive validation. Every
// stricter rule anybody writes rejects addresses that genuinely deliver, and
// the real proof is the verification email.
func TestLooksLikeEmail(t *testing.T) {
	valid := []string{"a@b.co", "first.last+tag@sub.example.com", "x@example.museum"}
	invalid := []string{"", "no-at-sign", "a@b", "@example.com", "a@@example.com", "a b@example.com", "a@.com"}

	for _, address := range valid {
		if !LooksLikeEmail(address) {
			t.Errorf("%q should be accepted", address)
		}
	}

	for _, address := range invalid {
		if LooksLikeEmail(address) {
			t.Errorf("%q should be rejected", address)
		}
	}
}
