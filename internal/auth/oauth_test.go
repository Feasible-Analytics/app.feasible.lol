//
// oauth_test.go
// PKCE, the missing-credentials path, and linking by verified email only.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPKCEChallengeIsTheHashOfTheVerifier checks the S256 transform. A
// challenge that is not the hash of the verifier means Google rejects every
// exchange with an error that names neither of them.
func TestPKCEChallengeIsTheHashOfTheVerifier(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("new pkce: %v", err)
	}

	sum := sha256.Sum256([]byte(pkce.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])

	if pkce.Challenge != want {
		t.Errorf("the challenge is not the S256 hash of the verifier")
	}

	if pkce.State == "" {
		t.Error("a state value is required to bind the callback to this attempt")
	}
}

// TestAuthURLCarriesEverythingGoogleNeeds checks the redirect the browser is
// sent to, including the redirect URI derived from the base URL — the value
// that has to match what is registered in the Google console exactly.
func TestAuthURLCarriesEverythingGoogleNeeds(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("new pkce: %v", err)
	}

	url := NewGoogle("client-id", "client-secret", "https://example.com/").AuthURL(pkce)

	for _, fragment := range []string{
		"code_challenge_method=S256",
		"response_type=code",
		"client_id=client-id",
		"redirect_uri=https%3A%2F%2Fexample.com%2Fauth%2Fgoogle%2Fcallback",
	} {
		if !strings.Contains(url, fragment) {
			t.Errorf("the authorization URL is missing %q: %s", fragment, url)
		}
	}

	// Only the identifying scopes at sign-in. The same client is reused for
	// Search Console later, and asking for those up front puts an alarming
	// consent screen in front of somebody who has not seen the product yet.
	if strings.Contains(url, "webmasters") || strings.Contains(url, "analytics") {
		t.Errorf("sign-in should request only the identifying scopes: %s", url)
	}
}

// TestGoogleIsOptional checks that absent credentials hide the button and
// produce a readable reason rather than a start-up failure. The credentials are
// not available yet, and the product has to work without them.
func TestGoogleIsOptional(t *testing.T) {
	google := NewGoogle("", "", "https://example.com")

	if google.Configured() {
		t.Fatal("no credentials should mean not configured")
	}

	if !strings.Contains(google.DisabledReason(), "FEASIBLE_GOOGLE_CLIENT_ID") {
		t.Errorf("the reason should name the variable to set: %q", google.DisabledReason())
	}

	if NewGoogle("id", "secret", "https://example.com").DisabledReason() != "" {
		t.Error("a configured client should have no disabled reason")
	}

	if _, err := google.Exchange(context.Background(), "code", "verifier"); err == nil {
		t.Error("an unconfigured client should refuse to exchange a code")
	}
}

// TestResolveProfileLinksOnlyVerifiedEmail is the important half of federated
// login. Linking on an unverified address on either side lets whoever can
// obtain an address at a provider take over an account registered with it.
func TestResolveProfileLinksOnlyVerifiedEmail(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Our side is unverified, so linking is refused even though Google says its
	// side is verified.
	if _, _, err := s.ResolveProfile(ctx, &Profile{Sub: "sub-1", Email: "a@example.com", EmailVerified: true}); err == nil {
		t.Fatal("linking to an unverified local account should be refused")
	}

	if err := s.MarkVerified(ctx, user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	// Google's side unverified is refused too.
	if _, _, err := s.ResolveProfile(ctx, &Profile{Sub: "sub-1", Email: "a@example.com", EmailVerified: false}); err == nil {
		t.Fatal("linking from an unverified Google address should be refused")
	}

	linked, created, err := s.ResolveProfile(ctx, &Profile{Sub: "sub-1", Email: "a@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}

	if created {
		t.Error("linking should not create a second account")
	}

	if linked.ID != user.ID {
		t.Errorf("want user %d, got %d", user.ID, linked.ID)
	}
}

// TestResolveProfileMatchesOnSubjectNotEmail checks the identity rule. An
// address can be reassigned at the provider; the subject id cannot.
func TestResolveProfileMatchesOnSubjectNotEmail(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	created, isNew, err := s.ResolveProfile(ctx, &Profile{Sub: "sub-1", Email: "a@example.com", EmailVerified: true, Name: "A"})
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}

	if !isNew {
		t.Fatal("an unknown subject with an unknown address should create an account")
	}

	if !created.Verified() {
		t.Error("Google has already proven the address, so it should start verified")
	}

	found, isNew, err := s.ResolveProfile(ctx, &Profile{Sub: "sub-1", Email: "moved@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}

	if isNew || found.ID != created.ID {
		t.Error("a changed Google email must still resolve to the same account by subject id")
	}
}

// TestOAuthStateCookieRoundTrips checks the signed cookie that carries the PKCE
// verifier between the redirect out and the callback in, and that it is cleared
// on the way back so an abandoned attempt leaves nothing to replay.
func TestOAuthStateCookieRoundTrips(t *testing.T) {
	sealer := newTestSealer(t)

	pkce, err := NewPKCE()
	if err != nil {
		t.Fatalf("new pkce: %v", err)
	}

	set := httptest.NewRecorder()

	if err := SetOAuthStateCookie(set, sealer, pkce, "/sites", "https://example.com"); err != nil {
		t.Fatalf("set oauth state: %v", err)
	}

	cookie := set.Result().Cookies()[0]

	if cookie.Domain != "" {
		t.Errorf("the OAuth state cookie must not carry a Domain either, got %q", cookie.Domain)
	}

	request := httptest.NewRequest("GET", "/auth/google/callback", nil)
	request.AddCookie(cookie)

	read := httptest.NewRecorder()

	verifier, state, next, err := ReadOAuthStateCookie(read, request, sealer, "https://example.com")
	if err != nil {
		t.Fatalf("read oauth state: %v", err)
	}

	if verifier != pkce.Verifier || state != pkce.State || next != "/sites" {
		t.Errorf("the state did not round-trip: %q %q %q", verifier, state, next)
	}

	// It is cleared on the way back, including on failure, so a verifier does
	// not sit in the browser for the next attempt to reuse.
	if read.Result().Cookies()[0].MaxAge >= 0 {
		t.Error("reading the state cookie should also clear it")
	}

	// A tampered cookie must not verify.
	tampered := httptest.NewRequest("GET", "/auth/google/callback", nil)
	tampered.AddCookie(&(*cookie))
	tampered.Header.Set("Cookie", strings.Replace(tampered.Header.Get("Cookie"), cookie.Value, cookie.Value+"x", 1))

	if _, _, _, err := ReadOAuthStateCookie(httptest.NewRecorder(), tampered, sealer, "https://example.com"); err == nil {
		t.Error("a tampered state cookie should be rejected")
	}
}
