//
// signup_test.go
// Tests for reporting a new account as a Signup event.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// registration is the request that created an account, as it reaches the app
// through a proxy.
func registration(t *testing.T) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "https://app.example.test/register", strings.NewReader("email=jane%40example.test"))
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("User-Agent", "Mozilla/5.0 (test browser)")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Cookie", "feasible_session=secret")
	req.Header.Set("Referer", "https://app.example.test/register")

	return req
}

// TestSignupEventCarriesTheVisitorAndNotTheAccount checks the two halves of the
// event: it has what identifies the visit, so it joins the pages that visitor
// read, and it has nothing that identifies the person.
func TestSignupEventCarriesTheVisitorAndNotTheAccount(t *testing.T) {
	reporter := &signupReporter{Domain: "example.test", PageURL: "https://app.example.test/register"}

	event, err := reporter.request(registration(t), auth.SignupMethodGoogle, auth.Referral{
		Referrer: "news.example.test", Source: "hn", Medium: "social", Campaign: "launch", LandingPage: "/pricing/",
	})
	if err != nil {
		t.Fatal(err)
	}

	if event.RemoteAddr != "127.0.0.1:40000" || event.Header.Get("X-Forwarded-For") != "203.0.113.9" ||
		event.Header.Get("User-Agent") != "Mozilla/5.0 (test browser)" {
		t.Fatalf("the event lost the visitor's address or user agent: %q %v", event.RemoteAddr, event.Header)
	}

	for _, name := range []string{"Cookie", "Authorization", "Referer"} {
		if event.Header.Get(name) != "" {
			t.Errorf("the event still carries the %s header", name)
		}
	}

	body, err := io.ReadAll(event.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "jane") {
		t.Fatalf("the event names the account holder: %s", body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"n": "Signup", "d": "example.test", "u": "https://app.example.test/register",
		"utm_source": "hn", "utm_medium": "social", "utm_campaign": "launch",
		"referrer": "https://news.example.test/",
	}
	for name, value := range want {
		if payload[name] != value {
			t.Errorf("payload %q is %v, want %q", name, payload[name], value)
		}
	}

	props, _ := payload["p"].(map[string]any)
	if props["method"] != auth.SignupMethodGoogle {
		t.Errorf("the event does not say how the account was created: %v", payload["p"])
	}
}

// TestSignupEventLeavesOutWhatItDoesNotKnow keeps an untagged sign-up from
// overriding the source of the session it belongs to with empty values.
func TestSignupEventLeavesOutWhatItDoesNotKnow(t *testing.T) {
	reporter := &signupReporter{Domain: "example.test", PageURL: "https://app.example.test/register"}

	event, err := reporter.request(registration(t), auth.SignupMethodPassword, auth.Referral{})
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]any
	if err := json.NewDecoder(event.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"utm_source", "utm_medium", "utm_campaign", "referrer"} {
		if _, present := payload[name]; present {
			t.Errorf("an empty %s was sent", name)
		}
	}
}

// TestSignupEventReachesTheIngestHandler sends one event the whole way and
// checks the handler was given a request it accepts as an event.
func TestSignupEventReachesTheIngestHandler(t *testing.T) {
	received := make(chan *http.Request, 1)

	reporter := &signupReporter{
		Domain:  "example.test",
		PageURL: "https://app.example.test/register",
		Ingest: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(ingest.HeaderDropped, "not_a_real_drop")
			w.WriteHeader(http.StatusAccepted)
			received <- r
		}),
	}

	reporter.Hook()(registration(t), auth.SignupMethodPassword, auth.Referral{Source: "hn"})

	event := <-received
	if event.Method != http.MethodPost || event.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("the handler was given %s with content type %q", event.Method, event.Header.Get("Content-Type"))
	}
}

// TestNoSignupEventWithoutASite is the self-hosted default: no site was named,
// so the account handler is given nothing to call.
func TestNoSignupEventWithoutASite(t *testing.T) {
	e := &env{cfg: &config.Config{}}

	if hook := newSignupReporter(e, &ingest.Service{Handler: &ingest.Handler{}}).Hook(); hook != nil {
		t.Fatal("a reporter was built with no site to report to")
	}

	e.cfg.App.SignupEventDomain = "example.test"
	if hook := newSignupReporter(e, &ingest.Service{Handler: &ingest.Handler{}}).Hook(); hook == nil {
		t.Fatal("no reporter was built for a named site")
	}
}
