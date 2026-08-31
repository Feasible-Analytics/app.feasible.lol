//
// preflight_test.go
// Deployment checks for Stripe Managed Payments.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// TestPreflightRequiresAndRunsCheckoutSmoke proves the default is read-only and
// deliberately not green, while the opt-in smoke uses the real Managed Payments
// parameters and expires its customerless session.
func TestPreflightRequiresAndRunsCheckoutSmoke(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	var checkoutForm url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Stripe-Version"); got != stripe.ManagedPaymentsAPIVersion {
			t.Errorf("Stripe-Version is %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/products/prod_feasible":
			fmt.Fprint(w, `{"id":"prod_feasible","name":"feasible.lol","active":true,"livemode":true,"shippable":false,"tax_code":"txcd_10103001"}`)
		case "/v1/prices/price_monthly":
			fmt.Fprint(w, `{"id":"price_monthly","active":true,"livemode":true,"product":"prod_feasible","type":"recurring","currency":"usd","unit_amount":999,"tax_behavior":"exclusive","recurring":{"interval":"month","interval_count":1}}`)
		case "/v1/prices/price_yearly":
			fmt.Fprint(w, `{"id":"price_yearly","active":true,"livemode":true,"product":"prod_feasible","type":"recurring","currency":"usd","unit_amount":10000,"tax_behavior":"exclusive","recurring":{"interval":"year","interval_count":1}}`)
		case "/v1/webhook_endpoints":
			fmt.Fprint(w, `{"object":"list","has_more":false,"data":[{"id":"we_1","url":"https://feasible.lol/webhooks/stripe","status":"enabled","enabled_events":["*"]}]}`)
		case "/v1/checkout/sessions":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse checkout form: %v", err)
			}
			mu.Lock()
			posts = append(posts, r.URL.Path)
			checkoutForm = r.Form
			mu.Unlock()
			fmt.Fprint(w, `{"id":"cs_preflight","url":"https://checkout.stripe.test/preflight","status":"open"}`)
		case "/v1/checkout/sessions/cs_preflight/expire":
			mu.Lock()
			posts = append(posts, r.URL.Path)
			mu.Unlock()
			fmt.Fprint(w, `{"id":"cs_preflight","status":"expired"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := stripe.New("sk_live_fake")
	client.BaseURL = server.URL
	service := &Service{
		Stripe:        client,
		Plans:         Plans{Product: "prod_feasible", Monthly: "price_monthly", Yearly: "price_yearly"},
		WebhookSecret: "whsec_fake",
		BaseURL:       "https://feasible.lol",
		Now:           func() time.Time { return time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC) },
	}

	readOnly := service.Preflight(context.Background(), false)
	if readOnly.Ready() {
		t.Fatal("read-only checks claimed Managed Payments was fully verified")
	}
	if got := preflightDetail(readOnly, "Managed Payments checkout smoke"); !strings.Contains(got, "--checkout-smoke") {
		t.Fatalf("read-only result does not give the exact next action: %q", got)
	}
	mu.Lock()
	if len(posts) != 0 {
		t.Fatalf("read-only preflight made writes: %v", posts)
	}
	mu.Unlock()

	smoke := service.Preflight(context.Background(), true)
	if !smoke.Ready() {
		t.Fatalf("successful smoke is not ready: %+v", smoke.Checks)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || posts[0] != "/v1/checkout/sessions" || posts[1] != "/v1/checkout/sessions/cs_preflight/expire" {
		t.Fatalf("smoke writes are %v", posts)
	}
	if checkoutForm.Get("managed_payments[enabled]") != "true" || checkoutForm.Get("line_items[0][price]") != "price_monthly" {
		t.Fatalf("smoke did not exercise the production checkout fields: %v", checkoutForm)
	}
	if checkoutForm.Has("customer") || checkoutForm.Has("customer_email") || checkoutForm.Has("metadata[feasible_team_id]") {
		t.Fatalf("smoke session could create customer state: %v", checkoutForm)
	}
}

// TestPreflightRejectsIncompatibleCatalogueAndWebhook catches the mistakes a
// retrieval-only check can prove without sending a customer to Checkout.
func TestPreflightRejectsIncompatibleCatalogueAndWebhook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/products/prod_feasible":
			fmt.Fprint(w, `{"id":"prod_feasible","name":"feasible.lol","active":true,"tax_code":"txcd_10103001"}`)
		case "/v1/prices/price_monthly":
			fmt.Fprint(w, `{"id":"price_monthly","active":true,"product":"prod_feasible","type":"recurring","currency":"usd","unit_amount":999,"tax_behavior":"unspecified","recurring":{"interval":"month","interval_count":1}}`)
		case "/v1/prices/price_yearly":
			fmt.Fprint(w, `{"id":"price_yearly","active":true,"product":"prod_other","type":"recurring","currency":"usd","unit_amount":10000,"tax_behavior":"exclusive","recurring":{"interval":"year","interval_count":1}}`)
		case "/v1/webhook_endpoints":
			fmt.Fprint(w, `{"data":[{"url":"https://feasible.lol/webhooks/stripe","status":"enabled","enabled_events":["checkout.session.completed"]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := stripe.New("sk_test_fake")
	client.BaseURL = server.URL
	service := &Service{
		Stripe:        client,
		Plans:         Plans{Product: "prod_feasible", Monthly: "price_monthly", Yearly: "price_yearly"},
		WebhookSecret: "whsec_fake",
		BaseURL:       "https://feasible.lol",
	}

	report := service.Preflight(context.Background(), false)
	if report.Ready() {
		t.Fatal("incompatible Stripe objects passed preflight")
	}
	for _, want := range []string{"explicit exclusive", "belongs to prod_other", stripe.EventCheckoutAsyncPaymentSucceeded, stripe.EventCheckoutAsyncPaymentFailed} {
		if !strings.Contains(preflightText(report), want) {
			t.Errorf("report does not identify %q: %+v", want, report.Checks)
		}
	}
}

// preflightDetail returns the detail for one named check.
func preflightDetail(report PreflightReport, name string) string {
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Detail
		}
	}

	return ""
}

// preflightText flattens a report for focused substring assertions.
func preflightText(report PreflightReport) string {
	var lines []string
	for _, check := range report.Checks {
		lines = append(lines, check.Name+": "+check.Detail)
	}

	return strings.Join(lines, "\n")
}
