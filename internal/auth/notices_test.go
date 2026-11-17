//
// notices_test.go
// Tests for the commercial events this package announces and records.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/posthog"
)

// TestSlackReferralCarriesEveryField is the join between the stored row and the
// message. A field added to one and forgotten in the other is a notice that
// quietly says less than the database knows.
func TestSlackReferralCarriesEveryField(t *testing.T) {
	stored := Referral{
		Referrer:    "news.example.test",
		Source:      "hn",
		Medium:      "social",
		Campaign:    "launch",
		LandingPage: "/pricing",
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

// postHogEvents starts a fake PostHog and returns a client for it, plus the
// channel every captured event body arrives on.
func postHogEvents(t *testing.T) (*posthog.Client, chan map[string]any) {
	t.Helper()

	events := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		events <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return posthog.New(posthog.Options{Token: "phc_test", Host: server.URL, Env: "production"}), events
}

// expectPostHog returns the next captured event.
func expectPostHog(t *testing.T, events chan map[string]any) (map[string]any, map[string]any) {
	t.Helper()

	select {
	case event := <-events:
		props, _ := event["properties"].(map[string]any)
		return event, props
	case <-time.After(5 * time.Second):
		t.Fatal("no posthog event was delivered")
		return nil, nil
	}
}

// TestGoogleSignupIsRecordedWithoutSlack checks PostHog hears about a signup
// on an install with no Slack configured.
func TestGoogleSignupIsRecordedWithoutSlack(t *testing.T) {
	client, events := postHogEvents(t)
	h := &Handler{PostHog: client}

	h.announceGoogleSignup(t.Context(), &User{ID: 1, Email: "jane@example.test", Name: "Jane"}, Referral{Medium: "social"})

	event, props := expectPostHog(t, events)
	if event["event"] != posthog.EventSignedUp || event["distinct_id"] != "jane@example.test" ||
		props["signup_method"] != SignupMethodGoogle || props["utm_medium"] != "social" {
		t.Fatalf("event = %v", event)
	}
}

// TestRegisteringAndAddingASiteAreRecorded walks the real forms: a password
// signup and the first site, both against the address the person signed up
// with.
func TestRegisteringAndAddingASiteAreRecorded(t *testing.T) {
	app := newTestApp(t)
	client, events := postHogEvents(t)
	app.PostHog = client

	c := registerAndVerify(t, app)

	signup, props := expectPostHog(t, events)
	if signup["event"] != posthog.EventSignedUp || signup["distinct_id"] != "person@example.com" ||
		props["signup_method"] != SignupMethodPassword || props["email"] != "person@example.com" {
		t.Fatalf("signup = %v", signup)
	}

	resp := c.post("/sites/new", url.Values{"domain": {"https://www.Example.com/"}, "timezone": {"Etc/UTC"}})
	closeResponseBody(t, resp)

	added, props := expectPostHog(t, events)
	if added["event"] != posthog.EventSiteAdded || added["distinct_id"] != "person@example.com" ||
		props["domain"] != "example.com" || props["source"] != posthog.SourceDashboard || props["site_id"] == nil {
		t.Fatalf("site added = %v", added)
	}
}
