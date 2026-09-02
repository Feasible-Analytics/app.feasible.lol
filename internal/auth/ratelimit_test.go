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

	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
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

	limiter.Reset("key")

	// A reset key starts a fresh window, so the whole allowance is available
	// again and only the attempt past it is refused.
	for i := 0; i < 3; i++ {
		if !limiter.Allow("key", 3, time.Minute) {
			t.Fatalf("attempt %d after a reset should be allowed", i+1)
		}
	}

	if limiter.Allow("key", 3, time.Minute) {
		t.Error("a reset must give back the allowance, not remove the limit")
	}
}

// TestKeysAreIndependent checks that the two dimensions really are separate.
// Limiting by source alone lets a botnet spread one account's guesses; limiting
// by subject alone lets one source walk through every account.
func TestKeysAreIndependent(t *testing.T) {
	limiter := NewLimiter()

	request := httptest.NewRequest("POST", "/login", nil)
	request.RemoteAddr = "203.0.113.10:54321"

	source := ClientKey(request, nil, "login")
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
// when the peer is not a proxy we put there. A rate limit an attacker can
// bypass by writing a different number in a header is worse than no limit,
// because it looks like one.
func TestClientKeyIgnoresForwardedHeaders(t *testing.T) {
	request := httptest.NewRequest("POST", "/login", nil)
	request.RemoteAddr = "203.0.113.10:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.7")

	// Nil is the default an installation gets before anything is configured,
	// and it must trust nobody.
	if key := ClientKey(request, nil, "login"); key != "login|203.0.113.10" {
		t.Errorf("want the connection address, got %q", key)
	}

	elsewhere, err := clientip.ParseTrustedProxies([]string{"192.0.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}

	if key := ClientKey(request, elsewhere, "login"); key != "login|203.0.113.10" {
		t.Errorf("a header from a peer that is not on the list was believed: %q", key)
	}
}

// TestClientKeyFollowsATrustedProxy checks the other half. Behind our own
// reverse proxy every connection arrives from the proxy's address, so a key
// built from the socket alone would put every visitor in one bucket and let ten
// bad passwords from anybody lock sign-in for everybody.
func TestClientKeyFollowsATrustedProxy(t *testing.T) {
	trusted, err := clientip.ParseTrustedProxies([]string{"203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest("POST", "/login", nil)
	request.RemoteAddr = "203.0.113.10:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.7")

	if key := ClientKey(request, trusted, "login"); key != "login|198.51.100.7" {
		t.Errorf("want the visitor behind the proxy, got %q", key)
	}

	// Two visitors behind the same proxy must land in different buckets, which
	// is the whole point of reading the header at all.
	other := httptest.NewRequest("POST", "/login", nil)
	other.RemoteAddr = "203.0.113.10:54322"
	other.Header.Set("X-Forwarded-For", "198.51.100.8")

	if ClientKey(other, trusted, "login") == ClientKey(request, trusted, "login") {
		t.Error("two visitors behind one proxy share a rate-limit bucket")
	}
}
