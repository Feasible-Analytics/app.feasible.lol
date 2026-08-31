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
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
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

// gatedRoutes is every route this API answers, one per line.
//
// The list is exhaustive on purpose. The bug this guards against is a lock that
// covered the endpoint somebody thought of and not the fourteen others that
// reach the same account, so the test enumerates the whole surface rather than
// sampling the ones that look like reports.
var gatedRoutes = []struct {
	name   string
	method string
	path   string
	body   string
}{
	{"the v2 query endpoint", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`},
	{"the v1 aggregate shim", http.MethodGet, "/api/v1/stats/aggregate?site_id=example.com&metrics=visitors", ""},
	{"the v1 timeseries shim", http.MethodGet, "/api/v1/stats/timeseries?site_id=example.com&metrics=visitors", ""},
	{"the v1 breakdown shim", http.MethodGet, "/api/v1/stats/breakdown?site_id=example.com&property=event:page", ""},
	{"the v1 realtime shim", http.MethodGet, "/api/v1/stats/realtime/visitors?site_id=example.com", ""},
	{"the site list", http.MethodGet, "/api/v1/sites", ""},
	{"one site", http.MethodGet, "/api/v1/sites/example.com", ""},
	{"a site's tracker settings", http.MethodGet, "/api/v1/sites/example.com/tracker", ""},
	{"goals", http.MethodGet, "/api/v1/sites/goals?site_id=example.com", ""},
	{"shared links", http.MethodGet, "/api/v1/sites/shared-links?site_id=example.com", ""},
	{"guests", http.MethodGet, "/api/v1/sites/guests?site_id=example.com", ""},
	{"custom properties", http.MethodGet, "/api/v1/sites/custom-props?site_id=example.com", ""},
	{"team memberships", http.MethodGet, "/api/v1/teams/memberships", ""},
	{"the webhook list", http.MethodGet, "/api/v1/webhooks", ""},
	{"the webhook event types", http.MethodGet, "/api/v1/webhooks/event-types", ""},
	{"creating a site", http.MethodPost, "/api/v1/sites", `{"domain":"another.example"}`},
	{"deleting a site", http.MethodDelete, "/api/v1/sites/example.com", ""},
}

// TestALockedAccountIsRefusedOnEveryRoute is the hole this whole gate exists to
// close. An API key belonging to a locked account could read every number the
// account owned, which made the lock a banner rather than a lock.
func TestALockedAccountIsRefusedOnEveryRoute(t *testing.T) {
	h := newHarness(t)
	h.API.Access.Set(teamID, access.ReasonLifecycle)

	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			status, body := h.do(t, route.method, route.path, route.body, h.Key)

			if status != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402: %s", status, body)
			}

			// A status with no body is a support ticket. The customer has to be
			// able to read why, and follow a link that fixes it.
			var refusal struct {
				Error  string `json:"error"`
				Reason string `json:"reason"`
				Action string `json:"action"`
			}

			if err := json.Unmarshal(body, &refusal); err != nil {
				t.Fatalf("the refusal is not JSON: %s", body)
			}

			if refusal.Reason != string(access.ReasonLifecycle) {
				t.Errorf("reason is %q", refusal.Reason)
			}
			if refusal.Action != "/billing" {
				t.Errorf("action is %q, want /billing", refusal.Action)
			}
			if !strings.Contains(refusal.Error, "not currently paying") {
				t.Errorf("the message does not explain itself: %q", refusal.Error)
			}
		})
	}
}

// TestADormantAccountIsRefusedTheSameWay covers the phase after collection
// stops. It is refused identically, and told something different: an account
// that has been dormant has a gap in its history, and a customer must hear that
// before they pay rather than after.
func TestADormantAccountIsRefusedTheSameWay(t *testing.T) {
	h := newHarness(t)
	h.API.Access.Set(teamID, access.ReasonDormant)

	status, body := h.post(t, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`)
	if status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: %s", status, body)
	}

	if !strings.Contains(string(body), "stopped collecting") {
		t.Errorf("a dormant account is not told collection stopped: %s", body)
	}
}

// TestAnActiveAccountIsUnaffected is the case that must never break. The gate
// runs in front of every route, so a bug in it takes the whole API down.
func TestAnActiveAccountIsUnaffected(t *testing.T) {
	h := newHarness(t)

	for _, route := range gatedRoutes {
		t.Run(route.name, func(t *testing.T) {
			status, body := h.do(t, route.method, route.path, route.body, h.Key)

			if status == http.StatusPaymentRequired {
				t.Fatalf("a paying account was told to pay: %s", body)
			}

			// The gate runs in front of the mux, so a path that does not exist
			// would be refused with a 402 just like a real one and the list
			// above would prove nothing. This is what keeps it honest.
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
				t.Fatalf("%s %s is not a route this API answers: %d (%s)", route.method, route.path, status, body)
			}
		})
	}
}

// TestOnlyTheLockedAccountIsRefused checks the gate is keyed on the account
// rather than on the endpoint. One customer's unpaid invoice must not take
// another customer's key down with it.
func TestOnlyTheLockedAccountIsRefused(t *testing.T) {
	h := newHarness(t)
	h.API.Access.Set(teamID, access.ReasonLifecycle)

	status, body := h.do(t, http.MethodGet, "/api/v1/sites", "", h.Other)
	if status != http.StatusOK {
		t.Fatalf("another team's key was refused: %d (%s)", status, body)
	}
}

// TestPayingRestoresTheAPI is the recovery path as an integration experiences
// it: the invoice clears, the gate's next refresh drops the account, and the
// key that was refused works again with nothing reissued.
func TestPayingRestoresTheAPI(t *testing.T) {
	h := newHarness(t)
	h.API.Access.Set(teamID, access.ReasonLifecycle)

	if status, _ := h.get(t, "/api/v1/sites"); status != http.StatusPaymentRequired {
		t.Fatalf("a locked account was not refused: %d", status)
	}

	// What a successful payment does: the account leaves the locked set on the
	// next rebuild.
	if err := h.API.Access.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	status, body := h.get(t, "/api/v1/sites")
	if status != http.StatusOK {
		t.Fatalf("paying did not restore the API: %d (%s)", status, body)
	}
}
