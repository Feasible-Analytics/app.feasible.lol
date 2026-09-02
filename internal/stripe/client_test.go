//
// client_test.go
// The transport rules every call shares: retries, backoff and what must not be repeated.
//
// Created: 2026-09-02
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
	"time"
)

// TestRateLimitIsRetriedOnceThenSucceeds proves a 429 costs one short wait
// rather than a failed webhook, and that Stripe's Retry-After is what sets it.
func TestRateLimitIsRetriedOnceThenSucceeds(t *testing.T) {
	calls := 0
	var gaps []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gaps = append(gaps, time.Now())
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"cus_1","email":"owner@example.com"}`))
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL

	customer, err := client.GetCustomer(context.Background(), "cus_1")
	if err != nil {
		t.Fatal(err)
	}
	if customer.Email != "owner@example.com" || calls != 2 {
		t.Fatalf("customer=%+v after %d calls, want the second answer", customer, calls)
	}
	if waited := gaps[1].Sub(gaps[0]); waited < time.Second {
		t.Fatalf("retry waited %s, want at least the Retry-After second", waited)
	}
}

// TestRetriesAreBounded keeps a real outage from becoming an unbounded loop:
// after the configured attempts the last error is returned as-is.
func TestRetriesAreBounded(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, `{"error":{"type":"api_error","message":"down"}}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL
	client.Retry = RetryPolicy{Attempts: 3, Delay: time.Millisecond}

	_, err := client.GetCustomer(context.Background(), "cus_1")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("exhausted retries returned %v, want the 503", err)
	}
	if calls != 3 {
		t.Fatalf("made %d calls, want exactly 3", calls)
	}
}

// TestServerErrorIsNotRetriedWithoutAnIdempotencyKey pins the one write that
// must not be repeated: a 5xx on a keyless POST may already have created the
// object, and a second create is the double charge this client exists to avoid.
func TestServerErrorIsNotRetriedWithoutAnIdempotencyKey(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, `{"error":{"type":"api_error","message":"down"}}`, http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	client := New("sk_test_fake")
	client.BaseURL = server.URL
	client.Retry = RetryPolicy{Delay: time.Millisecond}

	if _, err := client.CreatePortalSession(context.Background(), "cus_1", "https://feasible.lol/billing"); err == nil {
		t.Fatal("keyless write succeeded through a 502")
	}
	if calls != 1 {
		t.Fatalf("keyless write was sent %d times, want 1", calls)
	}

	calls = 0
	if err := client.callWithVersion(context.Background(), http.MethodPost, "/v1/subscriptions/sub_1",
		url.Values{"pause_collection": {""}}, "keyed-write", APIVersion, nil); err == nil {
		t.Fatal("keyed write succeeded through a 502")
	}
	if calls != 3 {
		t.Fatalf("keyed write was sent %d times, want the full 3 attempts", calls)
	}
}

// TestRetryAfterIsCapped stops a bad header from parking a webhook handler.
func TestRetryAfterIsCapped(t *testing.T) {
	policy := RetryPolicy{Delay: 100 * time.Millisecond}
	if got := policy.delay(0, "3600"); got != maxRetryAfter {
		t.Fatalf("Retry-After of an hour waited %s, want the %s cap", got, maxRetryAfter)
	}
	if got := policy.delay(1, "junk"); got != 200*time.Millisecond {
		t.Fatalf("second retry waited %s, want doubled base delay", got)
	}
}
