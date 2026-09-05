//
// handler_test.go
// The endpoint's contract: shapes, statuses, and never a 500 on bad input.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package statsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// endpointNow is the clock every test resolves its dates against.
var endpointNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// newServer builds the endpoint over a seeded account, mounted exactly as
// `feasible serve` mounts it so the route pattern is exercised too.
func newServer(t *testing.T) *httptest.Server {
	return newServerWithOptions(t, AllowAll, 0)
}

// newServerWithAuthorization builds the endpoint with the requested access
// decision, including nil to prove the direct handler fails closed.
func newServerWithAuthorization(t *testing.T, authorize func(*http.Request, sites.Site) (Authorization, error)) *httptest.Server {
	return newServerWithOptions(t, authorize, 0)
}

// newServerWithThreshold builds the normal endpoint with an explicit
// automatic row-work ceiling so recovery contracts can force sampling without
// fabricating a huge HTTP fixture.
func newServerWithThreshold(t *testing.T, threshold int64) *httptest.Server {
	return newServerWithOptions(t, AllowAll, threshold)
}

// newServerWithOptions builds one fully authorized and sampling-configured
// endpoint so the access and query recovery contracts are exercised together.
func newServerWithOptions(t *testing.T, authorize func(*http.Request, sites.Site) (Authorization, error), threshold int64) *httptest.Server {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}

	seed(t, account)

	cache := sites.New(nil)
	cache.Set(sites.Site{ID: 3, AccountID: 7, Domain: "example.com", Timezone: "UTC"})

	handler := New(cache, manager, nil)
	handler.Now = func() time.Time { return endpointNow }
	handler.Authorize = authorize
	handler.SampleThreshold = threshold

	mux := http.NewServeMux()
	mux.Handle(Pattern, handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

// TestDirectStatsRequestWithoutAuthorizationIsDenied checks the handler itself,
// so a future route-table mistake cannot reopen every site's numbers.
func TestDirectStatsRequestWithoutAuthorizationIsDenied(t *testing.T) {
	server := newServerWithAuthorization(t, nil)

	status, body := post(t, server, "example.com", `{"metrics":["visitors"],"date_range":"7d"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("direct request status = %d, body = %+v", status, body)
	}
}

// TestPinnedFiltersAreAppendedServerSide checks that client filters can narrow
// a shared segment but cannot omit or replace it.
func TestPinnedFiltersAreAppendedServerSide(t *testing.T) {
	pinned := query.Filter{Operator: "is", Dimension: "event:page", Values: []string{"/home"}}
	server := newServerWithAuthorization(t, func(*http.Request, sites.Site) (Authorization, error) {
		return Authorization{PinnedFilters: []query.Filter{pinned}, CacheKey: "segment:1"}, nil
	})

	status, body := post(t, server, "example.com", `{
		"metrics":["visitors"],"date_range":"7d",
		"filters":[["is","visit:country",["US"]]]
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, body)
	}

	echoed := body["query"].(map[string]any)
	filters := echoed["filters"].([]any)
	if len(filters) != 2 {
		t.Fatalf("effective filters = %+v, want pinned and client filters", filters)
	}
}

// TestMembershipSamplingRefusalCarriesExactRecoveryCode gives dashboard
// clients a stable, explicit action without asking them to parse the message or
// automatically retry an expensive query.
func TestMembershipSamplingRefusalCarriesExactRecoveryCode(t *testing.T) {
	server := newServerWithThreshold(t, 1)

	status, body := post(t, server, "example.com", `{"metrics":["visitors","pageviews"],"date_range":"7d"}`)
	if status != http.StatusBadRequest || body["code"] != "sampling_requires_exact" {
		t.Fatalf("membership refusal = %d %+v", status, body)
	}

	status, body = post(t, server, "example.com", `{"metrics":["visitors","pageviews"],"date_range":"7d","exact":true}`)
	if status != http.StatusOK {
		t.Fatalf("explicit exact fallback = %d %+v", status, body)
	}
}

// seed writes two pageviews and one visit, which is enough for the endpoint's
// contract; the metric arithmetic itself is proved in the query package.
func seed(t *testing.T, account *accounts.Account) {
	t.Helper()

	ctx := context.Background()

	pageview, err := account.Intern.ID(ctx, intern.EventName, ingest.EventPageview)
	if err != nil {
		t.Fatal(err)
	}

	home, err := account.Intern.ID(ctx, intern.Pathname, "/home")
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC).Unix()

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce, pageviews, events, entry_page_id, exit_page_id)
		VALUES (1, 3, 500, ?, ?, 30, 0, 2, 2, ?, ?)`, at, at+30, home, home); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO events (site_id, timestamp, name_id, user_id, session_id, pathname_id, scroll_depth)
			VALUES (3, ?, ?, 500, 1, ?, 255)`, at+int64(i), pageview, home); err != nil {
			t.Fatal(err)
		}
	}
}

// post sends a query and returns the status and the decoded body.
func post(t *testing.T, server *httptest.Server, domain, body string) (int, map[string]any) {
	t.Helper()

	response, err := http.Post(server.URL+"/api/stats/"+domain+"/query", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	decoded := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("response was not JSON: %v", err)
	}

	return response.StatusCode, decoded
}

// TestQueryReturnsResultsMetaAndTheResolvedQuery checks the response shape the
// whole dashboard is written against.
func TestQueryReturnsResultsMetaAndTheResolvedQuery(t *testing.T) {
	server := newServer(t)

	status, body := post(t, server, "example.com", `{
		"site_id": "example.com",
		"metrics": ["visitors", "pageviews", "bounce_rate"],
		"date_range": "7d",
		"dimensions": ["visit:source"],
		"order_by": [["visitors", "desc"]],
		"pagination": {"limit": 100, "offset": 0},
		"include": {"time_labels": true}
	}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, body)
	}

	results, ok := body["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %+v", body["results"])
	}

	row := results[0].(map[string]any)
	metrics := row["metrics"].([]any)

	if metrics[0].(float64) != 1 || metrics[1].(float64) != 2 {
		t.Errorf("metrics = %v, want one visitor and two pageviews", metrics)
	}

	meta := body["meta"].(map[string]any)

	if _, present := meta["present_index"]; !present {
		t.Error("meta.present_index must always be present, even when it is null")
	}

	labels, ok := meta["time_labels"].([]any)
	if !ok || len(labels) != 7 {
		t.Errorf("meta.time_labels = %+v, want seven days", meta["time_labels"])
	}

	echoed, ok := body["query"].(map[string]any)
	if !ok {
		t.Fatal("the resolved query must be echoed back")
	}

	dateRange := echoed["date_range"].([]any)
	if dateRange[0] != "2026-08-24T00:00:00Z" || dateRange[1] != "2026-08-31T00:00:00Z" {
		t.Errorf("echoed date range = %v", dateRange)
	}

	if echoed["timezone"] != "UTC" {
		t.Errorf("echoed timezone = %v, want the site's own", echoed["timezone"])
	}
}

// TestPresentIndexMarksTheBucketInProgress checks the second piece of graph
// metadata over the wire.
func TestPresentIndexMarksTheBucketInProgress(t *testing.T) {
	server := newServer(t)

	status, body := post(t, server, "example.com", `{
		"metrics": ["pageviews"],
		"date_range": "7d",
		"dimensions": ["time:day"]
	}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, body)
	}

	meta := body["meta"].(map[string]any)

	index, ok := meta["present_index"].(float64)
	if !ok || int(index) != 6 {
		t.Fatalf("present_index = %v, want 6", meta["present_index"])
	}

	results := body["results"].([]any)
	if len(results) != 7 {
		t.Fatalf("got %d rows, want one per bucket including the empty ones", len(results))
	}
}

// TestSamplingMetadataNamesScaledAndDirectMetrics checks the public JSON
// disclosure. A client must be able to tell that the pageview total was
// inverse-rate expanded while the rate and numeric average were calculated
// directly within selected fact rows and remain estimates.
func TestSamplingMetadataNamesScaledAndDirectMetrics(t *testing.T) {
	server := newServer(t)

	status, body := post(t, server, "example.com", `{
		"metrics": ["pageviews", "bounce_rate", "avg(event:props:lcp)"],
		"date_range": "7d",
		"sample_rate": 0.5
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, body)
	}

	meta := body["meta"].(map[string]any)
	sampling, ok := meta["sampling"].(map[string]any)
	if !ok {
		t.Fatalf("sampling metadata is absent: %+v", meta)
	}

	scaled := sampling["scaled_metrics"].([]any)
	direct := sampling["direct_metrics"].([]any)
	if len(scaled) != 1 || scaled[0] != "pageviews" {
		t.Fatalf("scaled_metrics = %v, want pageviews", scaled)
	}
	if len(direct) != 2 || direct[0] != "bounce_rate" || direct[1] != "avg(event:props:lcp)" {
		t.Fatalf("direct_metrics = %v", direct)
	}
}

// TestBadInputIsAlwaysFourHundred is the promise that matters most here. Every
// one of these is a value a client can send by accident, and every one of them
// has to come back as a message the client can read rather than as a 500 that
// only we can explain.
func TestBadInputIsAlwaysFourHundred(t *testing.T) {
	server := newServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"no metrics", `{"date_range":"7d"}`},
		{"unknown metric", `{"metrics":["vistors"],"date_range":"7d"}`},
		{"page is not a number", `{"metrics":["visitors"],"date_range":"7d","pagination":{"limit":"foo"}}`},
		{"negative offset", `{"metrics":["visitors"],"date_range":"7d","pagination":{"offset":-1}}`},
		{"oversized limit", `{"metrics":["visitors"],"date_range":"7d","pagination":{"limit":999999}}`},
		{"unknown date range", `{"metrics":["visitors"],"date_range":"fortnight"}`},
		{"unknown timezone", `{"metrics":["visitors"],"date_range":"7d","timezone":"Mars/Olympus"}`},
		{"unknown dimension", `{"metrics":["visitors"],"date_range":"7d","dimensions":["visit:planet"]}`},
		{"unknown filter operator", `{"metrics":["visitors"],"date_range":"7d","filters":[["starts_with","event:page",["/"]]]}`},
		{"malformed filter", `{"metrics":["visitors"],"date_range":"7d","filters":["is"]}`},
		{"bad regular expression", `{"metrics":["visitors"],"date_range":"7d","filters":[["matches","event:page",["([a-z"]]]}`},
		{"unknown field", `{"metrics":["visitors"],"date_range":"7d","dimenions":["visit:source"]}`},
		{"mismatched site", `{"site_id":"other.com","metrics":["visitors"],"date_range":"7d"}`},
		{"bounce rate per event name", `{"metrics":["bounce_rate"],"date_range":"7d","dimensions":["event:name"]}`},
		{"conversion rate with no goal", `{"metrics":["conversion_rate"],"date_range":"7d"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, server, "example.com", tc.body)

			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %+v)", status, body)
			}

			if message, ok := body["error"].(string); !ok || message == "" {
				t.Errorf("a refusal must carry a reason, got %+v", body)
			}
		})
	}
}

// TestUnknownSiteIsNotFound checks the routing failure.
func TestUnknownSiteIsNotFound(t *testing.T) {
	server := newServer(t)

	status, body := post(t, server, "nobody.example", `{"metrics":["visitors"],"date_range":"7d"}`)

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %+v)", status, body)
	}
}

// TestSourceLabelIsAClosedSet checks the one label the query timings carry. It
// has to stay three or four fixed words: a label taken from the request would
// put a customer's site on an operations endpoint and grow a new series for
// every value that ever arrives.
func TestSourceLabelIsAClosedSet(t *testing.T) {
	cases := []struct {
		sources []string
		want    string
	}{
		{nil, "none"},
		{[]string{"raw"}, "raw"},
		{[]string{"rollup"}, "rollup"},
		{[]string{"rollup", "raw"}, "mixed"},
	}

	for _, c := range cases {
		if got := sourceLabel(c.sources); got != c.want {
			t.Errorf("sourceLabel(%v) = %q, want %q", c.sources, got, c.want)
		}
	}
}

// TestOnlyPostIsAccepted checks the method guard.
func TestOnlyPostIsAccepted(t *testing.T) {
	server := newServer(t)

	response, err := http.Get(server.URL + "/api/stats/example.com/query")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	// The mux itself refuses a method the pattern does not name, which is the
	// same answer the handler would give.
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.StatusCode)
	}
}

// TestDomainIsNormalised checks that a site registered without the www prefix
// still answers for one that has it.
func TestDomainIsNormalised(t *testing.T) {
	server := newServer(t)

	status, body := post(t, server, "WWW.Example.com", `{"metrics":["visitors"],"date_range":"7d"}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, body)
	}
}

// TestComparisonComesBackWithTheRows checks that the comparison request reaches
// the engine and its window is reported.
func TestComparisonComesBackWithTheRows(t *testing.T) {
	server := newServer(t)

	status, body := post(t, server, "example.com", `{
		"metrics": ["pageviews"],
		"date_range": "day",
		"include": {"comparisons": {"mode": "previous_period"}}
	}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %+v", status, body)
	}

	row := body["results"].([]any)[0].(map[string]any)

	comparison, ok := row["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("no comparison on the row: %+v", row)
	}

	if _, ok := comparison["change"].([]any); !ok {
		t.Errorf("comparison = %+v, want a change per metric", comparison)
	}

	meta := body["meta"].(map[string]any)
	if _, ok := meta["comparison_date_range"].([]any); !ok {
		t.Error("the comparison window must be echoed back")
	}
}

// TestAnOmittedIncludeKeepsTheWireDefault is the case a test on Include alone
// cannot reach.
//
// `imports` is stored inverted so a Go caller gets migrated history without
// asking, and a custom UnmarshalJSON puts the wire back the right way round —
// but Go never calls it for a key that is not in the body. An API client that
// sends no `include` at all has to keep getting the native-only answer it has
// always had, and the only place that can go wrong is here.
func TestAnOmittedIncludeKeepsTheWireDefault(t *testing.T) {
	for _, body := range []string{
		`{"metrics":["visitors"]}`,
		`{"metrics":["visitors"],"include":{}}`,
		`{"metrics":["visitors"],"include":{"bots":true}}`,
	} {
		parsed, err := decode([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}

		if !parsed.Include.ExcludeImports {
			t.Errorf("%s: imported history was included without the caller asking", body)
		}
	}

	// And the caller who does ask still gets it.
	parsed, err := decode([]byte(`{"metrics":["visitors"],"include":{"imports":true}}`))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.Include.ExcludeImports {
		t.Error(`"imports": true was not honoured`)
	}
}
