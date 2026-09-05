//
// notices_test.go
// Tests for what a signup notice can honestly say about where somebody came from.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// signupRequest builds a registration POST the way a browser sends one.
func signupRequest(t *testing.T, target, referer string, form url.Values) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	return req
}

// TestSignupReferralReadsTheCampaign covers the useful case: somebody followed a
// tagged link and the parameters survived onto the form.
func TestSignupReferralReadsTheCampaign(t *testing.T) {
	req := signupRequest(t,
		"https://app.example.test/register?utm_source=hn&utm_medium=social&utm_campaign=launch",
		"https://news.example.test/item?id=1", nil)

	referral := signupReferral(req)

	if referral.Source != "hn" || referral.Medium != "social" || referral.Campaign != "launch" {
		t.Fatalf("campaign parameters lost: %+v", referral)
	}
	if referral.Referrer != "https://news.example.test/item?id=1" {
		t.Fatalf("referrer lost: %+v", referral)
	}
	if referral.Empty() {
		t.Fatal("a populated referral reported itself empty")
	}
}

// TestSignupReferralReadsAHiddenField covers the other shape: the parameters
// arrive in the posted body rather than on the URL.
func TestSignupReferralReadsAHiddenField(t *testing.T) {
	req := signupRequest(t, "https://app.example.test/register", "",
		url.Values{"ref": {"a-partner"}})

	if referral := signupReferral(req); referral.Source != "a-partner" {
		t.Fatalf("a posted referral was not read: %+v", referral)
	}
}

// TestOurOwnPagesAreNotAReferral is the check that keeps the field meaningful.
// Every signup is posted from our own form, so reporting the last hop would make
// every account look self-referred and hide the real source.
func TestOurOwnPagesAreNotAReferral(t *testing.T) {
	req := signupRequest(t, "https://app.example.test/register",
		"https://app.example.test/pricing", nil)
	req.Host = "app.example.test"

	referral := signupReferral(req)

	if referral.Referrer != "" {
		t.Fatalf("our own page was reported as a referral: %+v", referral)
	}
	if !referral.Empty() {
		t.Fatalf("nothing was learned but the referral is not empty: %+v", referral)
	}
}

// TestReferralFieldsAreBounded covers text an attacker controls. A referrer is
// whatever the browser was told to send, and it ends up in a chat message, so it
// cannot carry newlines or run to any length it likes.
func TestReferralFieldsAreBounded(t *testing.T) {
	long := "https://example.test/" + strings.Repeat("a", maxReferralField*2)
	req := signupRequest(t, "https://app.example.test/register", long+"\nfake line", nil)

	referral := signupReferral(req)

	if strings.ContainsAny(referral.Referrer, "\r\n") {
		t.Fatalf("a referrer kept its line breaks: %q", referral.Referrer)
	}
	if len([]rune(referral.Referrer)) > maxReferralField+1 {
		t.Fatalf("a referrer was not clipped: %d runes", len([]rune(referral.Referrer)))
	}
}

// TestSignupReferralSurvivesNoRequest covers the path that has no request at
// all. A notice is worth sending without attribution; a panic on the signup path
// is not worth anything.
func TestSignupReferralSurvivesNoRequest(t *testing.T) {
	if referral := signupReferral(nil); !referral.Empty() {
		t.Fatalf("a missing request produced a referral: %+v", referral)
	}
}

// TestAnnouncingWithoutSlackIsSafe is the state every self-hosted install runs
// in. The notices sit on the signup and deletion paths, so a missing notifier
// has to be a no-op rather than a panic.
func TestAnnouncingWithoutSlackIsSafe(t *testing.T) {
	h := &Handler{}
	user := &User{ID: 1, Email: "jane@example.test", Name: "Jane Doe"}

	h.announceSignup(signupRequest(t, "https://app.example.test/register", "", nil), user, 2, SignupMethodPassword)
	h.announceGoogleSignup(t.Context(), nil, user)
	h.announceClosure(user, 2)
}
