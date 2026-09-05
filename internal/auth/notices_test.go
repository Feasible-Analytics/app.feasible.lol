//
// notices_test.go
// Tests for the commercial events this package announces.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"testing"
)

// TestSlackReferralCarriesEveryField is the join between the stored row and the
// message. A field added to one and forgotten in the other is a notice that
// quietly says less than the database knows.
func TestSlackReferralCarriesEveryField(t *testing.T) {
	stored := Referral{
		Referrer:    "https://news.example.test/item?id=1",
		Source:      "hn",
		Medium:      "social",
		Campaign:    "launch",
		LandingPage: "/pricing",
		FirstSeenAt: 1788591600,
	}

	message := slackReferral(stored)

	if message.Referrer != stored.Referrer || message.Source != stored.Source ||
		message.Medium != stored.Medium || message.Campaign != stored.Campaign ||
		message.Landing != stored.LandingPage {
		t.Fatalf("the notice lost part of the stored referral:\n row %+v\n msg %+v", stored, message)
	}

	if message.Empty() {
		t.Fatal("a populated referral reported itself empty to the notice")
	}
}

// TestAnnouncingWithoutSlackIsSafe is the state every self-hosted install runs
// in. The notices sit on the signup and deletion paths, so a missing notifier
// has to be a no-op rather than a panic.
func TestAnnouncingWithoutSlackIsSafe(t *testing.T) {
	h := &Handler{}
	user := &User{ID: 1, Email: "jane@example.test", Name: "Jane Doe"}

	h.announceSignup(user, 2, SignupMethodPassword, Referral{Source: "hn"})
	h.announceGoogleSignup(t.Context(), user, Referral{})
	h.announceClosure(user, 2)
}
