//
// verify_test.go
// The code length, the attempt limit, and the one-tap link.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"testing"
	"time"
)

// TestVerificationCodeIsLongEnough guards the digit count. Four digits is ten
// thousand possibilities and is brute-forceable against any endpoint that does
// not lock; the incumbent ships exactly that, and this constant is the reason
// we do not.
func TestVerificationCodeIsLongEnough(t *testing.T) {
	if VerificationCodeDigits < 6 {
		t.Fatalf("verification codes must be at least 6 digits, got %d", VerificationCodeDigits)
	}

	code, err := numericCode(VerificationCodeDigits)
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	if len(code) != VerificationCodeDigits {
		t.Errorf("want %d digits, got %q", VerificationCodeDigits, code)
	}

	for _, r := range code {
		if r < '0' || r > '9' {
			t.Fatalf("the code should be all digits, got %q", code)
		}
	}
}

// TestVerificationConsumesOnce checks that the right code verifies the address
// and that the same code cannot be replayed.
func TestVerificationConsumesOnce(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	code, _, err := s.CreateVerification(ctx, user.ID)
	if err != nil {
		t.Fatalf("create verification: %v", err)
	}

	if err := s.ConsumeVerification(ctx, user.ID, code, ""); err != nil {
		t.Fatalf("consume verification: %v", err)
	}

	reloaded, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if !reloaded.Verified() {
		t.Error("the address should be verified after the code was accepted")
	}

	if err := s.ConsumeVerification(ctx, user.ID, code, ""); err != ErrTokenUsed {
		t.Errorf("a spent code must not work twice, got %v", err)
	}
}

// TestVerificationLocksOutAfterWrongGuesses checks the attempt limit, which is
// what makes a code short enough to type also safe enough to email.
func TestVerificationLocksOutAfterWrongGuesses(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	code, _, err := s.CreateVerification(ctx, user.ID)
	if err != nil {
		t.Fatalf("create verification: %v", err)
	}

	var last error
	for i := 0; i < VerificationAttempts; i++ {
		last = s.ConsumeVerification(ctx, user.ID, "00000000", "")
	}

	if last != ErrRateLimited {
		t.Fatalf("want ErrRateLimited on the last attempt, got %v", last)
	}

	// The row is destroyed rather than merely locked, so continuing to guess is
	// guessing against nothing at all — and the real code is dead too.
	if err := s.ConsumeVerification(ctx, user.ID, code, ""); err != ErrNotFound {
		t.Errorf("the exhausted code should be gone, got %v", err)
	}
}

// TestVerificationExpires checks the 30-minute window is real.
func TestVerificationExpires(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	s.SetClock(func() time.Time { return now })

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	code, _, err := s.CreateVerification(ctx, user.ID)
	if err != nil {
		t.Fatalf("create verification: %v", err)
	}

	now = now.Add(VerificationWindow + time.Minute)

	if err := s.ConsumeVerification(ctx, user.ID, code, ""); err != ErrTokenExpired {
		t.Errorf("want ErrTokenExpired, got %v", err)
	}
}

// TestVerificationLinkResolvesWithoutASession checks the one-tap link works for
// a browser that is not signed in — the case it exists for, since the email is
// usually opened on a phone.
func TestVerificationLinkResolvesWithoutASession(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	_, link, err := s.CreateVerification(ctx, user.ID)
	if err != nil {
		t.Fatalf("create verification: %v", err)
	}

	found, err := s.UserIDForVerificationLink(ctx, link)
	if err != nil {
		t.Fatalf("resolve link: %v", err)
	}

	if found != user.ID {
		t.Errorf("want user %d, got %d", user.ID, found)
	}

	if err := s.ConsumeVerification(ctx, user.ID, "", link); err != nil {
		t.Fatalf("consume by link: %v", err)
	}

	if _, err := s.UserIDForVerificationLink(ctx, "not-a-token"); err != ErrNotFound {
		t.Errorf("an unknown token must not resolve, got %v", err)
	}
}

// TestAskingForANewCodeCancelsTheOld checks that two live codes never exist for
// one account, because that is two keys to the same door for no benefit.
func TestAskingForANewCodeCancelsTheOld(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	first, _, err := s.CreateVerification(ctx, user.ID)
	if err != nil {
		t.Fatalf("create verification: %v", err)
	}

	second, _, err := s.CreateVerification(ctx, user.ID)
	if err != nil {
		t.Fatalf("create verification: %v", err)
	}

	if err := s.ConsumeVerification(ctx, user.ID, first, ""); err != ErrBadCredentials {
		t.Errorf("the first code should be dead, got %v", err)
	}

	if err := s.ConsumeVerification(ctx, user.ID, second, ""); err != nil {
		t.Errorf("the newest code should work: %v", err)
	}
}

// TestMarkVerifiedIsIdempotent checks the path Google sign-in uses, where the
// identity provider has already proven the address.
func TestMarkVerifiedIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.MarkVerified(ctx, user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	first, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if err := s.MarkVerified(ctx, user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	second, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if first.EmailVerifiedAt != second.EmailVerifiedAt {
		t.Error("marking an already-verified address should not move the timestamp")
	}
}
