//
// auth_test.go
// The credential and the rate limit: who gets in, and how often.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// TestAuthenticationIsRequired covers every way a caller can arrive without a
// usable credential. All of them are 401 with a sentence, because "unauthorized"
// with no body is the single least helpful response an API can give.
func TestAuthenticationIsRequired(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name   string
		header string
	}{
		{"no header at all", ""},
		{"the wrong scheme", "Basic abc123"},
		{"bearer with nothing after it", "Bearer "},
		{"a token that is not one of ours", "Bearer sk-live-abcdef"},
		{"a key of the right shape that does not exist", "Bearer " + apikeys.Prefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, h.Server.URL+"/api/v2/query", nil)
			if err != nil {
				t.Fatal(err)
			}

			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}

			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.StatusCode)
			}

			if response.Header.Get("WWW-Authenticate") == "" {
				t.Error("a 401 must say which scheme it wanted")
			}
		})
	}
}

// TestRevokedKeyStopsWorking is the whole point of revocation.
func TestRevokedKeyStopsWorking(t *testing.T) {
	h := newHarness(t)

	status, _ := h.post(t, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`)
	if status != http.StatusOK {
		t.Fatalf("the key did not work before revocation: %d", status)
	}

	keys := apikeys.NewStore(h.Control)

	list, err := keys.List(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}

	if err := keys.Revoke(context.Background(), teamID, list[0].ID); err != nil {
		t.Fatal(err)
	}

	status, _ = h.post(t, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d after revocation, want 401", status)
	}
}

// TestAnotherTeamsSiteIsNotFound checks that authorisation failure looks exactly
// like a site that does not exist. Distinguishing the two turns the API into an
// oracle for which domains are registered with us, which is a fact about
// somebody else's customers.
func TestAnotherTeamsSiteIsNotFound(t *testing.T) {
	h := newHarness(t)

	status, body := h.post(t, "/api/v2/query",
		`{"site_id":"notyours.com","metrics":["visitors"],"date_range":"7d"}`)

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", status, body)
	}

	missing, _ := h.post(t, "/api/v2/query",
		`{"site_id":"nosuchsite.example","metrics":["visitors"],"date_range":"7d"}`)

	if missing != status {
		t.Fatalf("a site owned by somebody else answered %d and a site that does not exist answered %d — they must be indistinguishable", status, missing)
	}
}

// TestRateLimitRefusesAndSaysWhen drives a key past its ceiling.
//
// The limit is set to something small rather than the shipped ten thousand,
// because a test that sends ten thousand requests to prove a limit is a test
// nobody runs twice.
func TestRateLimitRefusesAndSaysWhen(t *testing.T) {
	h := newHarness(t)
	h.API.Limiter = apikeys.NewLimiter(3)

	query := `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`

	for i := 0; i < 3; i++ {
		if status, body := h.post(t, "/api/v2/query", query); status != http.StatusOK {
			t.Fatalf("request %d was refused early: %d (%s)", i+1, status, body)
		}
	}

	status, body := h.post(t, "/api/v2/query", query)
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", status, body)
	}

	decoded := decode(t, body)
	if message, ok := decoded["error"].(string); !ok || message == "" {
		t.Error("a rate-limit refusal must carry a reason")
	}
}

// TestRateLimitHeadersCountDown checks the headers a client backs off from. A
// client told only "no" retries immediately, which turns a rate limit into a
// tight loop against the thing it was meant to protect.
func TestRateLimitHeadersCountDown(t *testing.T) {
	h := newHarness(t)
	h.API.Limiter = apikeys.NewLimiter(2)

	request, err := http.NewRequest(http.MethodPost, h.Server.URL+"/api/v2/query", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.Key)

	first, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()

	if got := first.Header.Get("X-RateLimit-Limit"); got != "2" {
		t.Errorf("X-RateLimit-Limit = %q, want 2", got)
	}

	if got := first.Header.Get("X-RateLimit-Remaining"); got != "1" {
		t.Errorf("X-RateLimit-Remaining = %q, want 1", got)
	}

	reset, err := strconv.ParseInt(first.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || time.Unix(reset, 0).Before(time.Now()) {
		t.Errorf("X-RateLimit-Reset = %q, want a future unix time", first.Header.Get("X-RateLimit-Reset"))
	}
}

// TestPerKeyLimitBeatsTheDefault checks that a key carrying its own ceiling uses
// it. Without this, raising one integration's limit would mean raising
// everybody's.
func TestPerKeyLimitBeatsTheDefault(t *testing.T) {
	limiter := apikeys.NewLimiter(10000)

	key := &apikeys.Key{ID: 1, HourlyLimit: 2}

	if decision := limiter.Allow(key); decision.Limit != 2 {
		t.Fatalf("limit = %d, want the key's own 2", decision.Limit)
	}

	limiter.Allow(key)

	if decision := limiter.Allow(key); decision.Allowed {
		t.Fatal("a key with a limit of 2 allowed a third request")
	}
}

// TestRateLimitWindowRolls checks that a refused key is allowed again once its
// window has passed, rather than being locked out until a restart.
func TestRateLimitWindowRolls(t *testing.T) {
	clock := testNow

	limiter := apikeys.NewLimiter(1)
	limiter.Now = func() time.Time { return clock }

	key := &apikeys.Key{ID: 1}

	if decision := limiter.Allow(key); !decision.Allowed {
		t.Fatal("the first request was refused")
	}

	if decision := limiter.Allow(key); decision.Allowed {
		t.Fatal("the second request inside the window was allowed")
	}

	clock = clock.Add(time.Hour + time.Second)

	if decision := limiter.Allow(key); !decision.Allowed {
		t.Fatal("the window did not roll over")
	}
}

// TestScopedKeyIsRefusedOutsideItsScope checks the opt-in narrowing. An unscoped
// key does everything, so this can only ever refuse somebody who deliberately
// limited their own key.
func TestScopedKeyIsRefusedOutsideItsScope(t *testing.T) {
	h := newHarness(t)

	keys := apikeys.NewStore(h.Control)

	_, readOnly, err := keys.Create(context.Background(), teamID, 1, "read only", []string{apikeys.ScopeStatsRead}, 0)
	if err != nil {
		t.Fatal(err)
	}

	status, body := h.do(t, http.MethodPost, "/api/v1/sites", `{"domain":"new.example"}`, readOnly)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", status, body)
	}

	status, _ = h.do(t, http.MethodPost, "/api/v2/query",
		`{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`, readOnly)
	if status != http.StatusOK {
		t.Fatalf("a stats:read key was refused the stats API: %d", status)
	}
}

// TestLastUsedIsRecorded proves the column an operator reads to decide whether a
// key is still in use actually gets written.
func TestLastUsedIsRecorded(t *testing.T) {
	h := newHarness(t)

	if status, body := h.post(t, "/api/v2/query",
		`{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`); status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	keys := apikeys.NewStore(h.Control)

	list, err := keys.List(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range list {
		if key.Name == "test" && key.LastUsedAt.IsZero() {
			t.Fatal("last_used_at was not recorded")
		}
	}
}
