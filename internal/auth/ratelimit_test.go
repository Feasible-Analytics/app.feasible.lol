//
// ratelimit_test.go
// The attempt limiter in front of login, reset and two-factor verification.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestLimiterCountsAndExpires checks the two properties the sign-in form
// depends on: the limit bites, and the window really rolls forward afterwards.
func TestLimiterCountsAndExpires(t *testing.T) {
	limiter := NewLimiter()

	now := time.Now()
	limiter.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if !limiter.Allow("key", 3, time.Minute) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}

	if limiter.Allow("key", 3, time.Minute) {
		t.Error("the fourth attempt should be refused")
	}

	now = now.Add(61 * time.Second)

	if !limiter.Allow("key", 3, time.Minute) {
		t.Error("a new window should start once the old one has passed")
	}
}

// TestHammeringDoesNotRollTheWindow checks that a locked key stays locked. A
// limiter that reset its window on every refused attempt would be no limiter at
// all under the load an attacker generates.
func TestHammeringDoesNotRollTheWindow(t *testing.T) {
	limiter := NewLimiter()

	now := time.Now()
	limiter.SetClock(func() time.Time { return now })

	for i := 0; i < 20; i++ {
		limiter.Allow("key", 3, time.Minute)
	}

	now = now.Add(30 * time.Second)

	if limiter.Allow("key", 3, time.Minute) {
		t.Error("the key should still be locked half way through the window")
	}
}

// TestResetClearsAKey checks that a successful sign-in gives somebody their
// attempts back, so a few mistyped passwords do not leave them one attempt from
// a lockout for the next quarter of an hour.
func TestResetClearsAKey(t *testing.T) {
	limiter := NewLimiter()

	limiter.Allow("key", 3, time.Minute)
	limiter.Allow("key", 3, time.Minute)

	if got := limiter.Remaining("key", 3, time.Minute); got != 1 {
		t.Errorf("want 1 attempt left, got %d", got)
	}

	limiter.Reset("key")

	if got := limiter.Remaining("key", 3, time.Minute); got != 3 {
		t.Errorf("a reset key should have every attempt back, got %d", got)
	}
}

// TestKeysAreIndependent checks that the two dimensions really are separate.
// Limiting by source alone lets a botnet spread one account's guesses; limiting
// by subject alone lets one source walk through every account.
func TestKeysAreIndependent(t *testing.T) {
	limiter := NewLimiter()

	request := httptest.NewRequest("POST", "/login", nil)
	request.RemoteAddr = "203.0.113.10:54321"

	source := ClientKey(request, "login")
	subject := SubjectKey("a@example.com", "login")

	if source == subject {
		t.Fatal("the source and subject keys must not collide")
	}

	for i := 0; i < 3; i++ {
		limiter.Allow(source, 3, time.Minute)
	}

	if limiter.Allow(source, 3, time.Minute) {
		t.Error("the source should be limited")
	}

	if !limiter.Allow(subject, 3, time.Minute) {
		t.Error("limiting one source must not limit an unrelated address")
	}

	// The address is normalised into the key, so a difference in case is the
	// same subject.
	if SubjectKey("A@Example.com", "login") != subject {
		t.Error("subject keys should be case-insensitive")
	}
}

// TestClientKeyIgnoresForwardedHeaders checks the key comes from the connection
// rather than a header. A rate limit an attacker can bypass by writing a
// different number in a header is worse than no limit, because it looks like
// one.
func TestClientKeyIgnoresForwardedHeaders(t *testing.T) {
	request := httptest.NewRequest("POST", "/login", nil)
	request.RemoteAddr = "203.0.113.10:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.7")

	key := ClientKey(request, "login")

	if key != "login|203.0.113.10" {
		t.Errorf("want the connection address, got %q", key)
	}
}
