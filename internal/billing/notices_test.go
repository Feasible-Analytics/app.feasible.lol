//
// notices_test.go
// Tests for turning two stored subscription states into one commercial notice.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/slack"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// noticeService builds a service whose only wired dependency is Slack, plus the
// channel every delivered message arrives on.
func noticeService(t *testing.T) (*Service, chan string) {
	t.Helper()

	messages := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		messages <- payload.Text
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return &Service{Slack: slack.New(slack.Options{WebhookURL: server.URL, Env: "production"})}, messages
}

// expectNotice returns the delivered message.
func expectNotice(t *testing.T, messages chan string) string {
	t.Helper()

	select {
	case msg := <-messages:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no notice was delivered")
		return ""
	}
}

// expectSilence fails if anything was sent. Most reconciliations change nothing
// commercially, and a notice for each of them makes the channel unreadable.
func expectSilence(t *testing.T, messages chan string) {
	t.Helper()

	select {
	case msg := <-messages:
		t.Fatalf("a notice was sent for an unchanged subscription:\n%s", msg)
	case <-time.After(250 * time.Millisecond):
	}
}

// paid is a healthy paying subscription on one term.
func paid(plan string) Subscription {
	return Subscription{
		TeamID:       1,
		BillingEmail: "pay@example.test",
		Plan:         plan,
		Status:       stripe.StatusActive,
		PaymentState: PaymentPaid,
	}
}

// TestFirstPaidPeriodIsAnUpgrade is the notice worth the most: somebody started
// paying, and the term says how much.
func TestFirstPaidPeriodIsAnUpgrade(t *testing.T) {
	service, messages := noticeService(t)

	before := Subscription{TeamID: 1, PaymentState: PaymentPending}
	service.announcePlanChange(before, paid(Yearly.Key))

	msg := expectNotice(t, messages)
	for _, want := range []string{"Upgrade", "yearly", "pay@example.test"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("upgrade notice missing %q:\n%s", want, msg)
		}
	}
}

// TestTermChangesPickTheRightDirection covers the only ladder this catalogue
// has. There are no tiers, so the longer term is the upgrade.
func TestTermChangesPickTheRightDirection(t *testing.T) {
	service, messages := noticeService(t)

	service.announcePlanChange(paid(Monthly.Key), paid(Yearly.Key))
	if msg := expectNotice(t, messages); !strings.Contains(msg, "Upgrade") || !strings.Contains(msg, "yearly") {
		t.Fatalf("monthly to yearly was not an upgrade:\n%s", msg)
	}

	service.announcePlanChange(paid(Yearly.Key), paid(Monthly.Key))
	if msg := expectNotice(t, messages); !strings.Contains(msg, "Downgrade") || !strings.Contains(msg, "monthly") {
		t.Fatalf("yearly to monthly was not a downgrade:\n%s", msg)
	}
}

// TestEndingSaysWhetherTheyLeftOrTheCardDid is the difference that decides
// whether anybody should do something about it.
func TestEndingSaysWhetherTheyLeftOrTheCardDid(t *testing.T) {
	service, messages := noticeService(t)

	cancelled := paid(Yearly.Key)
	cancelled.Status = stripe.StatusCanceled
	cancelled.PaymentState = PaymentFailed

	service.announcePlanChange(paid(Yearly.Key), cancelled)
	if msg := expectNotice(t, messages); !strings.Contains(msg, "subscription cancelled") {
		t.Fatalf("a cancellation was not named:\n%s", msg)
	}

	declined := paid(Monthly.Key)
	declined.Status = stripe.StatusPastDue
	declined.PaymentState = PaymentFailed

	service.announcePlanChange(paid(Monthly.Key), declined)
	if msg := expectNotice(t, messages); !strings.Contains(msg, "payment failed") {
		t.Fatalf("a failed payment was not named:\n%s", msg)
	}
}

// TestUnchangedSubscriptionsSayNothing is what makes the channel usable. Stripe
// redelivers events and several event types reconcile the same state, so the
// common case has to be silent.
func TestUnchangedSubscriptionsSayNothing(t *testing.T) {
	service, messages := noticeService(t)

	service.announcePlanChange(paid(Monthly.Key), paid(Monthly.Key))
	expectSilence(t, messages)

	pending := Subscription{TeamID: 1, PaymentState: PaymentPending}
	service.announcePlanChange(pending, pending)
	expectSilence(t, messages)
}

// TestNoticesAreOptional covers the self-hosted install, where there is no
// Slack and reconciliation still has to work.
func TestNoticesAreOptional(t *testing.T) {
	(&Service{}).announcePlanChange(paid(Monthly.Key), paid(Yearly.Key))

	var missing *Service
	missing.announcePlanChange(paid(Monthly.Key), paid(Yearly.Key))
}
