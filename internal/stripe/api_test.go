//
// api_test.go
// The Stripe requests that create and manage subscriptions.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package stripe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestCreateCheckoutSessionUsesManagedPayments pins the merchant-of-record
// decision to the request Stripe receives. It also rejects the legacy Stripe
// Tax fields that Managed Payments does not accept.
func TestCreateCheckoutSessionUsesManagedPayments(t *testing.T) {
	var requestMethod string
	var requestPath string
	var requestVersion string
	var requestIdempotencyKey string
	var requestForm url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMethod = r.Method
		requestPath = r.URL.Path
		requestVersion = r.Header.Get("Stripe-Version")
		requestIdempotencyKey = r.Header.Get("Idempotency-Key")

		if err := r.ParseForm(); err != nil {
			t.Errorf("parse checkout form: %v", err)
		}
		requestForm = r.Form

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test_managed","url":"https://checkout.stripe.test/managed"}`))
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL

	session, err := client.CreateCheckoutSession(context.Background(), CheckoutParams{
		TeamID:         42,
		PriceID:        "price_yearly",
		CustomerID:     "cus_existing",
		SuccessURL:     "https://feasible.lol/billing/done",
		CancelURL:      "https://feasible.lol/pricing",
		IdempotencyKey: "checkout-42-yearly",
	})
	if err != nil {
		t.Fatal(err)
	}

	if requestMethod != http.MethodPost || requestPath != "/v1/checkout/sessions" {
		t.Errorf("request was %s %s", requestMethod, requestPath)
	}
	if requestVersion != "2025-03-31.basil" {
		t.Errorf("Stripe-Version is %q, want the Managed Payments API version", requestVersion)
	}
	if requestIdempotencyKey != "checkout-42-yearly" {
		t.Errorf("idempotency key is %q", requestIdempotencyKey)
	}
	if got := requestForm.Get("managed_payments[enabled]"); got != "true" {
		t.Errorf("managed_payments[enabled] is %q, want true", got)
	}

	for _, unsupported := range []string{
		"automatic_tax[enabled]",
		"tax_id_collection[enabled]",
		"customer_update[address]",
		"customer_update[name]",
	} {
		if requestForm.Has(unsupported) {
			t.Errorf("Managed Payments checkout includes unsupported field %q", unsupported)
		}
	}

	for field, want := range map[string]string{
		"mode":                       "subscription",
		"line_items[0][price]":       "price_yearly",
		"line_items[0][quantity]":    "1",
		"billing_address_collection": "required",
		"customer":                   "cus_existing",
		"metadata[feasible_team_id]": "42",
		"subscription_data[metadata][feasible_team_id]": "42",
	} {
		if got := requestForm.Get(field); got != want {
			t.Errorf("%s is %q, want %q", field, got, want)
		}
	}

	if session.ID != "cs_test_managed" || session.URL != "https://checkout.stripe.test/managed" {
		t.Errorf("decoded session is %+v", session)
	}
}
