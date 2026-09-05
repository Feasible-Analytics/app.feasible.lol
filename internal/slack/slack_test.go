//
// slack_test.go
// Tests for the operator notices: wording, delivery, and staying out of the way.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// collector is a stand-in Slack that hands each delivered message to the test.
type collector struct {
	server   *httptest.Server
	messages chan string
}

// newCollector starts a server that answers like Slack and records what it got.
func newCollector(t *testing.T) *collector {
	t.Helper()

	c := &collector{messages: make(chan string, 8)}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		c.messages <- payload.Text
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)

	return c
}

// next returns the message the notifier delivered, or fails.
func (c *collector) next(t *testing.T) string {
	t.Helper()

	select {
	case msg := <-c.messages:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no message was delivered")
		return ""
	}
}

// TestDisabledNotifierDoesNothing is the state every self-hosted install and
// almost every test runs in. A nil notifier has to be as safe as a configured
// one, because the alternative is a nil check at every call site and one of them
// eventually being forgotten on the signup path.
func TestDisabledNotifierDoesNothing(t *testing.T) {
	var nilNotifier *Notifier

	if nilNotifier.Enabled() {
		t.Fatal("a nil notifier reports itself enabled")
	}

	// The point of the test: none of these may panic.
	nilNotifier.SignedUp(Signup{Email: "a@example.test"})
	nilNotifier.Upgraded(PlanChange{TeamID: 1})
	nilNotifier.Downgraded(PlanChange{TeamID: 1})
	nilNotifier.AccountClosed(Closure{TeamID: 1})

	unconfigured := New(Options{})
	if unconfigured.Enabled() {
		t.Fatal("a notifier with no webhook reports itself enabled")
	}

	unconfigured.SignedUp(Signup{Email: "a@example.test"})
}

// TestSignupCarriesTheAskedForFields checks the three things a signup notice
// exists to carry: who they are, how to reach them, and where they came from.
func TestSignupCarriesTheAskedForFields(t *testing.T) {
	c := newCollector(t)

	n := New(Options{WebhookURL: c.server.URL, BaseURL: "https://app.example.test", Env: "production"})
	n.SignedUp(Signup{
		Name:   "Jane Doe",
		Email:  "jane@example.test",
		TeamID: 42,
		Method: "password",
		Referral: Referral{
			Referrer: "https://news.example.test/item?id=1",
			Source:   "hn",
			Medium:   "social",
			Campaign: "launch",
		},
	})

	msg := c.next(t)
	for _, want := range []string{
		"New signup", "Jane Doe", "jane@example.test",
		"https://app.example.test/billing?team=42", "password",
		"news.example.test", "hn", "social", "launch",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("signup notice missing %q:\n%s", want, msg)
		}
	}
}

// TestSignupSaysWhenThereIsNoReferral covers the difference between "we know
// they came directly" and "we did not record it". We do not persist attribution,
// so only the second is a claim we can make.
func TestSignupSaysWhenThereIsNoReferral(t *testing.T) {
	c := newCollector(t)

	New(Options{WebhookURL: c.server.URL, Env: "production"}).
		SignedUp(Signup{Email: "jane@example.test", TeamID: 7})

	if msg := c.next(t); !strings.Contains(msg, "none recorded") {
		t.Fatalf("signup notice did not say the referral is unknown:\n%s", msg)
	}
}

// TestSubscriptionNoticesNameTheTerm is the whole point of the upgrade notice:
// monthly and yearly are worth very different amounts and the message is useless
// without saying which.
func TestSubscriptionNoticesNameTheTerm(t *testing.T) {
	c := newCollector(t)
	n := New(Options{WebhookURL: c.server.URL, Env: "production"})

	n.Upgraded(PlanChange{TeamID: 1, Email: "pay@example.test", Plan: "yearly"})

	msg := c.next(t)
	if !strings.Contains(msg, "Upgrade") || !strings.Contains(msg, "yearly") {
		t.Fatalf("upgrade notice lost the term:\n%s", msg)
	}
	if !strings.Contains(msg, "pay@example.test") {
		t.Fatalf("upgrade notice lost the billing address:\n%s", msg)
	}

	n.Downgraded(PlanChange{TeamID: 1, Previous: "yearly", Reason: "payment failed"})

	msg = c.next(t)
	for _, want := range []string{"Downgrade", "no longer on a paid plan", "payment failed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("downgrade notice missing %q:\n%s", want, msg)
		}
	}
}

// TestClosureCarriesTheAddress covers the one notice that cannot be reconstructed
// later. Once the account is gone there is nothing left to look the person up in.
func TestClosureCarriesTheAddress(t *testing.T) {
	c := newCollector(t)

	New(Options{WebhookURL: c.server.URL, Env: "production"}).
		AccountClosed(Closure{TeamID: 9, Name: "Jane Doe", Email: "jane@example.test"})

	msg := c.next(t)
	if !strings.Contains(msg, "Account closed") || !strings.Contains(msg, "jane@example.test") {
		t.Fatalf("closure notice lost who left:\n%s", msg)
	}
}

// TestNonProductionIsLabelled stops a signup on a staging box being read as a
// real customer, which is the mistake this label exists to prevent.
func TestNonProductionIsLabelled(t *testing.T) {
	c := newCollector(t)

	New(Options{WebhookURL: c.server.URL, Env: "development"}).
		SignedUp(Signup{Email: "jane@example.test", TeamID: 1})

	if msg := c.next(t); !strings.HasPrefix(msg, "[development] ") {
		t.Fatalf("a non-production notice was not labelled:\n%s", msg)
	}
}

// TestPostReportsABadStatus is the "never fail silently" half. A webhook that
// was revoked answers 403 forever, and a delivery that treats that as success
// leaves us believing nobody has signed up.
func TestPostReportsABadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	n := New(Options{WebhookURL: server.URL})

	err := n.post(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a rejected delivery was not reported: %v", err)
	}
}
