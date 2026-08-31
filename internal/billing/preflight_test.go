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
	var checkoutForms []url.Values

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
			fmt.Fprintf(w, `{"object":"list","has_more":false,"data":[{"id":"we_1","url":"https://feasible.lol/webhooks/stripe","status":"enabled","api_version":%q,"enabled_events":["*"]}]}`, stripe.ManagedPaymentsAPIVersion)
		case "/v1/checkout/sessions":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse checkout form: %v", err)
			}
			priceID := r.Form.Get("line_items[0][price]")
			sessionID := "cs_monthly"
			if priceID == "price_yearly" {
				sessionID = "cs_yearly"
			}
			mu.Lock()
			posts = append(posts, "create:"+priceID)
			checkoutForms = append(checkoutForms, r.Form)
			mu.Unlock()
			fmt.Fprintf(w, `{"id":%q,"url":"https://checkout.stripe.test/preflight","status":"open"}`, sessionID)
		case "/v1/checkout/sessions/cs_monthly/expire", "/v1/checkout/sessions/cs_yearly/expire":
			sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), "/expire")
			mu.Lock()
			posts = append(posts, "expire:"+sessionID)
			mu.Unlock()
			fmt.Fprintf(w, `{"id":%q,"status":"expired"}`, sessionID)
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
	if got := strings.Join(posts, ","); got != "create:price_monthly,create:price_yearly,expire:cs_monthly,expire:cs_yearly" {
		t.Fatalf("smoke writes are %v", posts)
	}
	if len(checkoutForms) != 2 {
		t.Fatalf("smoke created %d checkout forms, want 2", len(checkoutForms))
	}
	for i, form := range checkoutForms {
		if form.Get("managed_payments[enabled]") != "true" {
			t.Errorf("checkout %d did not use Managed Payments: %v", i, form)
		}
		if form.Has("customer") || form.Has("customer_email") || form.Has("metadata[feasible_team_id]") {
			t.Errorf("checkout %d could create customer state: %v", i, form)
		}
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
			fmt.Fprintf(w, `{"data":[{"url":"https://feasible.lol/webhooks/stripe","status":"enabled","api_version":%q,"enabled_events":["checkout.session.completed"]}]}`, stripe.ManagedPaymentsAPIVersion)
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
	for _, want := range []string{"explicit exclusive", "belongs to prod_other", stripe.EventCheckoutAsyncPaymentSucceeded, stripe.EventCheckoutAsyncPaymentFailed, stripe.EventInvoiceFinalizationFailed} {
		if !strings.Contains(preflightText(report), want) {
			t.Errorf("report does not identify %q: %+v", want, report.Checks)
		}
	}
}

// TestPreflightRejectsWebhookAPIVersion pins the endpoint's event rendering to
// the same Basil shape the invoice decoder expects.
func TestPreflightRejectsWebhookAPIVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"url":"https://feasible.lol/webhooks/stripe","status":"enabled","api_version":"2024-06-20","enabled_events":["*"]}]}`)
	}))
	t.Cleanup(server.Close)

	client := stripe.New("sk_test_fake")
	client.BaseURL = server.URL
	service := &Service{Stripe: client, BaseURL: "https://feasible.lol"}

	var report PreflightReport
	service.preflightWebhook(context.Background(), &report)

	detail := preflightDetail(report, "Webhook endpoint")
	if !strings.Contains(detail, `"2024-06-20"`) || !strings.Contains(detail, `"`+stripe.ManagedPaymentsAPIVersion+`"`) {
		t.Fatalf("version mismatch is not explicit: %q", detail)
	}
}

// TestPreflightCheckoutCleansUpAfterFailures proves one failed price or one
// failed expiration cannot skip attempts for the remaining created sessions.
func TestPreflightCheckoutCleansUpAfterFailures(t *testing.T) {
	cases := []struct {
		name          string
		createFailure string
		expireFailure string
		wantActions   string
		wantDetail    []string
	}{
		{
			name:          "create failure still cleans the other price",
			createFailure: "price_monthly",
			wantActions:   "create:price_monthly,create:price_yearly,expire:cs_yearly",
			wantDetail:    []string{"monthly checkout creation failed"},
		},
		{
			name:          "expire failure still cleans the other session",
			expireFailure: "cs_monthly",
			wantActions:   "create:price_monthly,create:price_yearly,expire:cs_monthly,expire:cs_yearly",
			wantDetail:    []string{"monthly cleanup failed for session cs_monthly", "expire it in the Dashboard"},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var actions []string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				if r.URL.Path == "/v1/checkout/sessions" {
					if err := r.ParseForm(); err != nil {
						t.Errorf("parse checkout form: %v", err)
					}
					priceID := r.Form.Get("line_items[0][price]")
					actions = append(actions, "create:"+priceID)
					if priceID == test.createFailure {
						http.Error(w, "create failed", http.StatusBadRequest)
						return
					}

					sessionID := "cs_monthly"
					if priceID == "price_yearly" {
						sessionID = "cs_yearly"
					}
					fmt.Fprintf(w, `{"id":%q,"status":"open"}`, sessionID)
					return
				}

				sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), "/expire")
				actions = append(actions, "expire:"+sessionID)
				if sessionID == test.expireFailure {
					http.Error(w, "expire failed", http.StatusBadRequest)
					return
				}

				fmt.Fprintf(w, `{"id":%q,"status":"expired"}`, sessionID)
			}))
			t.Cleanup(server.Close)

			client := stripe.New("sk_test_fake")
			client.BaseURL = server.URL
			service := &Service{
				Stripe:  client,
				Plans:   Plans{Monthly: "price_monthly", Yearly: "price_yearly"},
				BaseURL: "https://feasible.lol",
				Now:     func() time.Time { return time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC) },
			}

			var report PreflightReport
			service.preflightCheckout(context.Background(), &report)

			if got := strings.Join(actions, ","); got != test.wantActions {
				t.Fatalf("actions are %q, want %q", got, test.wantActions)
			}
			if report.Ready() {
				t.Fatal("a partial smoke failure was reported ready")
			}
			detail := preflightDetail(report, "Managed Payments checkout smoke")
			for _, want := range test.wantDetail {
				if !strings.Contains(detail, want) {
					t.Errorf("failure detail %q does not contain %q", detail, want)
				}
			}
		})
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
