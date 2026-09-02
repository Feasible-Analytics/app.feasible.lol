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
	"strings"
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

// TestDeleteCustomerDistinguishesAbsentFromRejected proves retries may treat a
// provider-side 404 as the desired erased state while rotated or invalid
// credentials remain a hard failure.
func TestDeleteCustomerDistinguishesAbsentFromRejected(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "already absent", status: http.StatusNotFound},
		{name: "rotated credentials", status: http.StatusUnauthorized, wantErr: "401"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/v1/customers/cus_delete" {
					t.Fatalf("provider request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"error":{"message":"credential rejected"}}`))
			}))
			defer server.Close()

			client := New("sk_test")
			client.BaseURL = server.URL
			err := client.DeleteCustomer(context.Background(), "cus_delete")
			if test.wantErr == "" && err != nil {
				t.Fatalf("already absent customer deletion: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("rejected customer deletion error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

// TestSubscriptionsPreserveMixedPagedTruth proves pagination returns every
// status instead of collapsing the customer to one fallback before billing can
// detect a retryable subscription.
func TestSubscriptionsPreserveMixedPagedTruth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("starting_after") == "" {
			_, _ = w.Write([]byte(`{"has_more":true,"data":[
				{"id":"sub_canceled_annual","created":10,"status":"canceled","current_period_end":9999,"items":{"data":[{"price":{"id":"price_yearly"}}]}},
				{"id":"sub_past_due_monthly","created":30,"status":"past_due","current_period_end":300,"items":{"data":[{"price":{"id":"price_monthly"}}]}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"has_more":false,"data":[
			{"id":"sub_active_monthly","created":20,"status":"active","current_period_end":200,"items":{"data":[{"price":{"id":"price_monthly"}}]}}
		]}`))
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL
	subscriptions, err := client.Subscriptions(context.Background(), "cus_mixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptions) != 3 {
		t.Fatalf("subscription history has %d rows, want 3", len(subscriptions))
	}
	selected := SelectSubscription(subscriptions)
	if selected == nil || selected.ID != "sub_active_monthly" {
		t.Fatalf("selected subscription is %+v, want active monthly", selected)
	}
}

// TestSubscriptionSelectionAndBlockingAreDeterministic pins both independent
// decisions: display recency cannot be distorted by an old annual period end,
// and every nonterminal or unknown provider status blocks another checkout.
func TestSubscriptionSelectionAndBlockingAreDeterministic(t *testing.T) {
	selected := SelectSubscription([]Subscription{
		{ID: "sub_old_annual", Created: 10, Status: StatusCanceled, CurrentPeriodEnd: 9999},
		{ID: "sub_new_monthly", Created: 20, Status: StatusCanceled, CurrentPeriodEnd: 100},
	})
	if selected == nil || selected.ID != "sub_new_monthly" {
		t.Fatalf("terminal selection is %+v, want newer monthly", selected)
	}

	for _, status := range []string{
		StatusActive, StatusTrialing, StatusPastDue, StatusUnpaid,
		StatusIncomplete, StatusPaused, "future_settling_status",
	} {
		if !(&Subscription{Status: status}).BlocksCheckout() {
			t.Errorf("status %q did not block checkout", status)
		}
	}
	for _, status := range []string{StatusCanceled, StatusIncompleteExpired} {
		if (&Subscription{Status: status}).BlocksCheckout() {
			t.Errorf("terminal status %q blocked checkout", status)
		}
	}
}

// TestProviderListsPaginateAndVoidInvoiceUsesIdempotency covers the discovery
// reads and recovery writes that close untracked Stripe objects: sessions are
// listed for one customer, customers are found through metadata search, and
// both walk every page.
func TestProviderListsPaginateAndVoidInvoiceUsesIdempotency(t *testing.T) {
	var voidKey string
	var deleteKey string
	var sessionCustomers []string
	var searchQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions" && r.URL.Query().Get("starting_after") == "":
			sessionCustomers = append(sessionCustomers, r.URL.Query().Get("customer"))
			_, _ = w.Write([]byte(`{"has_more":true,"data":[{"id":"cs_1","status":"open"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
			sessionCustomers = append(sessionCustomers, r.URL.Query().Get("customer"))
			_, _ = w.Write([]byte(`{"has_more":false,"data":[{"id":"cs_2","status":"open"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/customers/search" && r.URL.Query().Get("page") == "":
			searchQueries = append(searchQueries, r.URL.Query().Get("query"))
			_, _ = w.Write([]byte(`{"has_more":true,"next_page":"cursor_2","data":[{"id":"cus_1","metadata":{"feasible_team_id":"7"}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/customers/search" && r.URL.Query().Get("page") == "cursor_2":
			_, _ = w.Write([]byte(`{"has_more":false,"data":[{"id":"cus_2","metadata":{"feasible_team_id":"7"}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/invoices/in_open/void":
			voidKey = r.Header.Get("Idempotency-Key")
			_, _ = w.Write([]byte(`{"id":"in_open","status":"void"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/invoices/in_draft":
			deleteKey = r.Header.Get("Idempotency-Key")
			_, _ = w.Write([]byte(`{"id":"in_draft","deleted":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL
	sessions, err := client.CheckoutSessionsForCustomer(context.Background(), "cus_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "cs_1" || sessions[1].ID != "cs_2" {
		t.Fatalf("open checkout pages are %+v", sessions)
	}
	if len(sessionCustomers) != 2 || sessionCustomers[0] != "cus_1" || sessionCustomers[1] != "cus_1" {
		t.Fatalf("session pages were filtered by customers %v, want cus_1 on every page", sessionCustomers)
	}
	customers, err := client.SearchCustomersByTeam(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(customers) != 2 || customers[0].ID != "cus_1" || customers[1].ID != "cus_2" || customers[1].Meta.TeamID() != 7 {
		t.Fatalf("customer pages are %+v", customers)
	}
	if len(searchQueries) != 1 || searchQueries[0] != "metadata['feasible_team_id']:'7'" {
		t.Fatalf("customer search queries are %q", searchQueries)
	}
	invoice, err := client.VoidInvoice(context.Background(), "in_open", "void-in-open")
	if err != nil {
		t.Fatal(err)
	}
	if invoice.Status != "void" || voidKey != "void-in-open" {
		t.Fatalf("void invoice=%+v idempotency=%q", invoice, voidKey)
	}
	if err := client.DeleteDraftInvoice(context.Background(), "in_draft", "delete-in-draft"); err != nil {
		t.Fatal(err)
	}
	if deleteKey != "delete-in-draft" {
		t.Fatalf("draft deletion idempotency=%q", deleteKey)
	}
}

// TestSubscriptionPauseUsesExplicitReversibleBehavior pins the Stripe contract
// used before an authoritative deletion claim: preparation keeps new invoices
// as drafts, while restoration clears pause_collection entirely.
func TestSubscriptionPauseUsesExplicitReversibleBehavior(t *testing.T) {
	var forms []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse subscription form: %v", err)
		}
		forms = append(forms, r.Form)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sub_pause","status":"active"}`))
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL
	if _, err := client.SetSubscriptionCollectionPaused(context.Background(), "sub_pause", true, "keep_as_draft", "pause-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SetSubscriptionCollectionPaused(context.Background(), "sub_pause", false, "", "restore-key"); err != nil {
		t.Fatal(err)
	}
	if len(forms) != 2 || forms[0].Get("pause_collection[behavior]") != "keep_as_draft" ||
		!forms[1].Has("pause_collection") || forms[1].Get("pause_collection") != "" {
		t.Fatalf("subscription pause forms are %+v", forms)
	}
}
