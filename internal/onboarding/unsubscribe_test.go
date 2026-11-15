//
// unsubscribe_test.go
// The stop link: minted for one team, redeemed once, and nobody else's.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSealer stands in for the application key: reversible, and recognisable
// as its own.
type fakeSealer struct{}

// SealToken encodes the claim.
func (fakeSealer) SealToken(plaintext string) (string, error) {
	return "sealed." + base64.RawURLEncoding.EncodeToString([]byte(plaintext)), nil
}

// OpenToken decodes a claim it sealed and refuses anything else.
func (fakeSealer) OpenToken(token string) (string, error) {
	raw, ok := strings.CutPrefix(token, "sealed.")
	if !ok {
		return "", errors.New("not sealed here")
	}

	plaintext, err := base64.RawURLEncoding.DecodeString(raw)

	return string(plaintext), err
}

// tokenOf pulls the token off the end of a stop link.
func tokenOf(t *testing.T, link string) string {
	t.Helper()

	_, token, ok := strings.Cut(link, StopPath)
	if !ok || token == "" {
		t.Fatalf("link %q has no token", link)
	}

	return token
}

// TestTheStopLinkStopsOneTeam mints a link, redeems it twice, and checks the
// team drops out of the candidates while its neighbour stays.
func TestTheStopLinkStopsOneTeam(t *testing.T) {
	db, _ := openSystem(t)
	s := NewStore(db)

	signedUp := start.Add(time.Hour)
	addTeam(t, db, 1, signedUp)
	addTeam(t, db, 2, signedUp)

	stopper := &Stopper{Store: s, Sealer: fakeSealer{}, BaseURL: "https://app.example/", Now: func() time.Time { return signedUp }}

	link := stopper.Link(1)
	if !strings.HasPrefix(link, "https://app.example/unsubscribe/") {
		t.Fatalf("link = %q", link)
	}

	token := tokenOf(t, link)

	if !stopper.Owns(token) {
		t.Fatal("the stopper does not recognise its own token")
	}

	site, list, ok := stopper.Describe(context.Background(), token)
	if !ok || list != ListTips || site != "" {
		t.Fatalf("describe = %q, %q, %v", site, list, ok)
	}

	if got := candidateIDs(t, s, start); len(got) != 2 {
		t.Fatalf("describing the link changed the candidates: %v", got)
	}

	for range 2 {
		if err := stopper.Remove(context.Background(), token); err != nil {
			t.Fatal(err)
		}
	}

	got := candidateIDs(t, s, start)
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("candidates after the stop = %v, want only team 2", got)
	}
}

// TestAForeignTokenIsNotOurs covers a report unsubscribe token sealed with the
// same key, a forged one, and an unconfigured stopper.
func TestAForeignTokenIsNotOurs(t *testing.T) {
	db, _ := openSystem(t)
	stopper := &Stopper{Store: NewStore(db), Sealer: fakeSealer{}, BaseURL: "https://app.example"}

	report, _ := fakeSealer{}.SealToken("report\x001\x00weekly\x00sam@example.com")
	zero, _ := fakeSealer{}.SealToken(claimPrefix + "\x000")

	for _, token := range []string{report, zero, "garbage", ""} {
		if stopper.Owns(token) {
			t.Errorf("token %q was claimed", token)
		}

		if _, _, ok := stopper.Describe(context.Background(), token); ok {
			t.Errorf("token %q was described", token)
		}

		if err := stopper.Remove(context.Background(), token); err != nil {
			t.Errorf("token %q: remove = %v, want a quiet no-op", token, err)
		}
	}

	var stopped int
	if err := db.QueryRow(`SELECT COUNT(*) FROM onboarding_optouts`).Scan(&stopped); err != nil {
		t.Fatal(err)
	}
	if stopped != 0 {
		t.Fatalf("%d teams were stopped by tokens that were not theirs", stopped)
	}
}

// TestNoLinkWithoutAKeyOrAnAddress proves the sweeper is handed an empty link,
// which it refuses to send with, rather than a relative or unsealed one.
func TestNoLinkWithoutAKeyOrAnAddress(t *testing.T) {
	var none *Stopper

	for name, stopper := range map[string]*Stopper{
		"nil":         none,
		"no sealer":   {BaseURL: "https://app.example"},
		"no base url": {Sealer: fakeSealer{}},
	} {
		if link := stopper.Link(1); link != "" {
			t.Errorf("%s: link = %q, want none", name, link)
		}
	}

	if link := (&Stopper{Sealer: fakeSealer{}, BaseURL: "https://app.example"}).Link(0); link != "" {
		t.Errorf("link for team 0 = %q, want none", link)
	}
}
