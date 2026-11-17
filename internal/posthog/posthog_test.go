//
// posthog_test.go
// Tests for the product events sent to PostHog.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// captured is one request the fake PostHog received.
type captured struct {
	Path string
	Body map[string]any
}

// capturing starts a fake PostHog that answers status and hands every event it
// receives to the returned channel.
func capturing(t *testing.T, status int) (string, chan captured) {
	t.Helper()

	received := make(chan captured, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		received <- captured{Path: r.URL.Path, Body: body}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server.URL, received
}

// client builds an enabled client against host with a fixed clock, logging to
// the returned buffer.
func client(host string) (*Client, *bytes.Buffer) {
	var logs bytes.Buffer

	return New(Options{
		Token: "phc_test",
		Host:  host + "/",
		Env:   "production",
		Log:   logger.New(logger.Options{Level: "debug", Output: &logs}),
		Now:   func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) },
	}), &logs
}

// expectEvent returns the next event the fake PostHog received.
func expectEvent(t *testing.T, received chan captured) captured {
	t.Helper()

	select {
	case event := <-received:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no event was delivered")
		return captured{}
	}
}

// expectNothing fails if any event arrives.
func expectNothing(t *testing.T, received chan captured) {
	t.Helper()

	select {
	case event := <-received:
		t.Fatalf("an event was sent: %v", event.Body)
	case <-time.After(250 * time.Millisecond):
	}
}

// properties reads the properties object off a captured event.
func properties(t *testing.T, event captured) map[string]any {
	t.Helper()

	props, ok := event.Body["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the event has no properties: %v", event.Body)
	}

	return props
}

// TestSignedUpKeysThePersonOnTheirEmail checks the event a funnel starts from:
// it is recorded against the lowercased address, sets the email and name on
// the person, and carries what the signup request knew about where they came
// from.
func TestSignedUpKeysThePersonOnTheirEmail(t *testing.T) {
	host, received := capturing(t, http.StatusOK)
	c, _ := client(host)

	c.SignedUp(Signup{
		Email: "Jane@Example.test", Name: " Jane Doe ", TeamID: 7, Method: "google",
		Source: "hn", Campaign: "launch", Landing: "/pricing/",
	})

	event := expectEvent(t, received)
	if event.Path != "/i/v0/e/" {
		t.Fatalf("posted to %q, want /i/v0/e/", event.Path)
	}

	for key, want := range map[string]any{
		"api_key": "phc_test", "event": EventSignedUp, "distinct_id": "jane@example.test",
		"timestamp": "2026-11-16T12:00:00Z",
	} {
		if event.Body[key] != want {
			t.Errorf("%s = %v, want %v", key, event.Body[key], want)
		}
	}

	props := properties(t, event)
	for key, want := range map[string]any{
		"email": "Jane@Example.test", "team_id": float64(7), "signup_method": "google",
		"utm_source": "hn", "utm_campaign": "launch", "landing_page": "/pricing/",
		"environment": "production",
	} {
		if props[key] != want {
			t.Errorf("properties.%s = %v, want %v", key, props[key], want)
		}
	}

	for _, absent := range []string{"utm_medium", "referrer"} {
		if _, ok := props[absent]; ok {
			t.Errorf("properties.%s was sent although the signup carried none", absent)
		}
	}

	set, _ := props["$set"].(map[string]any)
	if set["email"] != "Jane@Example.test" || set["name"] != "Jane Doe" {
		t.Fatalf("$set = %v, want the email and trimmed name", set)
	}
}

// TestUpgradedSaysWhichTerm checks the property the yearly-or-monthly question
// is answered from, on the event and on the person.
func TestUpgradedSaysWhichTerm(t *testing.T) {
	host, received := capturing(t, http.StatusOK)
	c, _ := client(host)

	c.Upgraded(PlanChange{Email: "owner@example.test", TeamID: 3, Plan: "yearly", BillingEmail: "pay@example.test"})

	event := expectEvent(t, received)
	if event.Body["event"] != EventUpgraded || event.Body["distinct_id"] != "owner@example.test" {
		t.Fatalf("got %v", event.Body)
	}

	props := properties(t, event)
	if props["plan"] != "yearly" || props["previous_plan"] != PlanNone || props["billing_email"] != "pay@example.test" {
		t.Fatalf("properties = %v", props)
	}

	if set, _ := props["$set"].(map[string]any); set["plan"] != "yearly" {
		t.Fatalf("$set = %v, want plan yearly", set)
	}
}

// TestDowngradedNamesTheReason checks a plan ending is recorded as no plan,
// with why.
func TestDowngradedNamesTheReason(t *testing.T) {
	host, received := capturing(t, http.StatusOK)
	c, _ := client(host)

	c.Downgraded(PlanChange{Email: "owner@example.test", TeamID: 3, Previous: "monthly", Reason: "payment failed"})

	props := properties(t, expectEvent(t, received))
	if props["plan"] != PlanNone || props["previous_plan"] != "monthly" || props["reason"] != "payment failed" {
		t.Fatalf("properties = %v", props)
	}
}

// TestSiteAddedCarriesTheSite checks the site event.
func TestSiteAddedCarriesTheSite(t *testing.T) {
	host, received := capturing(t, http.StatusOK)
	c, _ := client(host)

	c.SiteAdded(SiteAdded{Email: "ed@example.test", TeamID: 3, SiteID: 11, Domain: "example.test", Source: SourceAPI})

	event := expectEvent(t, received)
	if event.Body["event"] != EventSiteAdded {
		t.Fatalf("event = %v", event.Body["event"])
	}

	props := properties(t, event)
	if props["domain"] != "example.test" || props["site_id"] != float64(11) || props["source"] != SourceAPI {
		t.Fatalf("properties = %v", props)
	}
}

// TestNoTokenSendsNothing is the self-hosted install.
func TestNoTokenSendsNothing(t *testing.T) {
	host, received := capturing(t, http.StatusOK)

	disabled := New(Options{Host: host})
	if disabled.Enabled() {
		t.Fatal("a client with no token reports itself enabled")
	}

	disabled.SignedUp(Signup{Email: "a@example.test"})
	disabled.SiteAdded(SiteAdded{Email: "a@example.test"})

	var missing *Client
	missing.Upgraded(PlanChange{Email: "a@example.test"})
	if err := missing.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	expectNothing(t, received)
}

// TestAnEventWithNoEmailIsReported checks that an event with nobody to record
// it against is logged rather than sent with an empty person.
func TestAnEventWithNoEmailIsReported(t *testing.T) {
	host, received := capturing(t, http.StatusOK)
	c, logs := client(host)

	c.SiteAdded(SiteAdded{TeamID: 3, Domain: "example.test"})

	expectNothing(t, received)
	if !strings.Contains(logs.String(), "posthog event dropped") {
		t.Fatalf("the drop was not logged:\n%s", logs.String())
	}
}

// TestARefusedEventIsLogged checks a failed delivery leaves a line behind.
func TestARefusedEventIsLogged(t *testing.T) {
	host, received := capturing(t, http.StatusUnauthorized)
	c, logs := client(host)

	c.SignedUp(Signup{Email: "a@example.test"})
	expectEvent(t, received)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(logs.String(), "could not send a posthog event") || !strings.Contains(logs.String(), "401") {
		t.Fatalf("the refusal was not logged:\n%s", logs.String())
	}
}

// TestCloseWaitsForDeliveries checks that an event already admitted is sent
// before Close returns, so a restart does not lose it.
func TestCloseWaitsForDeliveries(t *testing.T) {
	release := make(chan struct{})
	delivered := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		delivered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	c, _ := client(server.URL)
	c.SignedUp(Signup{Email: "a@example.test"})

	closed := make(chan error, 1)
	go func() { closed <- c.Close(context.Background()) }()

	select {
	case <-closed:
		t.Fatal("Close returned while a delivery was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the delivery finished")
	}

	select {
	case <-delivered:
	default:
		t.Fatal("Close returned before the event was delivered")
	}
}
