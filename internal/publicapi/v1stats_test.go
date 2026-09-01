//
// v1stats_test.go
// Every v1 shim, checked against the v2 query it is supposed to be.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// The shims exist so an existing integration migrates by changing a hostname.
// That promise is only worth anything if the numbers are the same, so every one
// of these tests runs the v1 endpoint *and* the v2 query it claims to be and
// compares the results — rather than asserting against a constant, which would
// pass just as happily if both were wrong.

// v2Aggregate runs the v2 equivalent of an aggregate call.
func (h *harness) v2Aggregate(t *testing.T, body string) []float64 {
	t.Helper()

	status, raw := h.post(t, "/api/v2/query", body)
	if status != http.StatusOK {
		t.Fatalf("the v2 equivalent failed: %d (%s)", status, raw)
	}

	var result query.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}

	if len(result.Results) == 0 {
		return nil
	}

	return result.Results[0].Metrics
}

// TestAggregateMatchesV2 checks the totals endpoint.
func TestAggregateMatchesV2(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/stats/aggregate?site_id=example.com&period=7d&metrics=visitors,pageviews")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results map[string]struct {
			Value float64 `json:"value"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	want := h.v2Aggregate(t, `{"site_id":"example.com","metrics":["visitors","pageviews"],"date_range":"7d"}`)

	if answer.Results["visitors"].Value != want[0] {
		t.Errorf("v1 visitors = %v, v2 = %v", answer.Results["visitors"].Value, want[0])
	}

	if answer.Results["pageviews"].Value != want[1] {
		t.Errorf("v1 pageviews = %v, v2 = %v", answer.Results["pageviews"].Value, want[1])
	}

	// The fixture is fixed, so the numbers are also checked against what
	// somebody worked out by hand. Two implementations agreeing on a wrong
	// answer is exactly the failure a cross-check alone cannot catch.
	if answer.Results["visitors"].Value != currentVisitors {
		t.Errorf("visitors = %v, want %d from the fixture", answer.Results["visitors"].Value, currentVisitors)
	}

	if answer.Results["pageviews"].Value != currentPageviews {
		t.Errorf("pageviews = %v, want %d from the fixture", answer.Results["pageviews"].Value, currentPageviews)
	}
}

// TestV1QueriesRemainExactUnderTheSamplingThreshold protects the compatibility
// response shape. It has no meta object where sampling could be disclosed, so
// the shared v1 query conversion must force exactness even when v2 would sample
// the same large estimated scan.
func TestV1QueriesRemainExactUnderTheSamplingThreshold(t *testing.T) {
	h := newHarness(t)
	seedLargeEstimate(t, h)
	h.API.SampleThreshold = 1

	status, body := h.get(t, "/api/v1/stats/aggregate?site_id=example.com&period=7d&metrics=visitors")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results map[string]struct {
			Value float64 `json:"value"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	if answer.Results["visitors"].Value != currentVisitors {
		t.Fatalf("v1 returned %v visitors under a sampling threshold, want the exact %d",
			answer.Results["visitors"].Value, currentVisitors)
	}

	if keys := decode(t, body); len(keys) != 1 {
		t.Fatalf("v1 response shape changed while preserving exactness: %v", keys)
	}
}

// TestAggregateThirtyDayPeriodMatchesTheDatesItMeans checks the period the two
// vocabularies do not share. v1's 30d has no v2 preset, and resolving it as the
// nearest one — 28 days — would quietly change every number a migrating
// customer compares against their old dashboard.
func TestAggregateThirtyDayPeriodMatchesTheDatesItMeans(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/stats/aggregate?site_id=example.com&period=30d&metrics=visitors")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results map[string]struct {
			Value float64 `json:"value"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	// 30 days back from 30 August is 1 August, inclusive of both ends.
	want := h.v2Aggregate(t,
		`{"site_id":"example.com","metrics":["visitors"],"date_range":["2026-08-01","2026-08-30"]}`)

	if answer.Results["visitors"].Value != want[0] {
		t.Errorf("v1 30d = %v, the same 30 days in v2 = %v", answer.Results["visitors"].Value, want[0])
	}

	if answer.Results["visitors"].Value != allVisitors {
		t.Errorf("visitors = %v, want %d from the fixture", answer.Results["visitors"].Value, allVisitors)
	}
}

// TestAggregateComparisonMatchesV2 checks that compare= reaches the engine.
func TestAggregateComparisonMatchesV2(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t,
		"/api/v1/stats/aggregate?site_id=example.com&period=7d&metrics=visitors&compare=previous_period")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results map[string]struct {
			Value  float64  `json:"value"`
			Change *float64 `json:"change"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	if answer.Results["visitors"].Change == nil {
		t.Fatal("compare=previous_period produced no change")
	}

	// Five visitors this week against nine the week before is a fall of just
	// over 44%, which is the number the engine computes and the number the shim
	// has to pass through untouched.
	expected := 100 * (float64(currentVisitors) - float64(previousVisitors)) / float64(previousVisitors)

	if difference := *answer.Results["visitors"].Change - expected; difference > 0.01 || difference < -0.01 {
		t.Errorf("change = %v, want about %v", *answer.Results["visitors"].Change, expected)
	}
}

// TestTimeseriesMatchesV2 checks the per-bucket endpoint row for row.
func TestTimeseriesMatchesV2(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/stats/timeseries?site_id=example.com&period=7d&metrics=visitors&interval=date")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results []struct {
			Date     string  `json:"date"`
			Visitors float64 `json:"visitors"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	status, raw := h.post(t, "/api/v2/query",
		`{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","dimensions":["time:day"]}`)
	if status != http.StatusOK {
		t.Fatalf("the v2 equivalent failed: %d (%s)", status, raw)
	}

	var expected query.Result
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}

	if len(answer.Results) != len(expected.Results) {
		t.Fatalf("v1 returned %d buckets, v2 returned %d", len(answer.Results), len(expected.Results))
	}

	// Seven days means seven rows, including the ones with no traffic. A graph
	// handed only the days that had visits cannot tell a quiet Tuesday from a
	// Tuesday the tracker was broken.
	if len(answer.Results) != 7 {
		t.Fatalf("got %d buckets, want one per day of the period", len(answer.Results))
	}

	for i, row := range answer.Results {
		if row.Date != expected.Results[i].Dimensions[0] {
			t.Errorf("bucket %d: v1 date %q, v2 %q", i, row.Date, expected.Results[i].Dimensions[0])
		}

		if row.Visitors != expected.Results[i].Metrics[0] {
			t.Errorf("bucket %d (%s): v1 %v, v2 %v", i, row.Date, row.Visitors, expected.Results[i].Metrics[0])
		}
	}
}

// TestTimeseriesDefaultIntervalFollowsThePeriod checks the default nobody
// passes. A period of one day drawn in daily buckets is a single bar.
func TestTimeseriesDefaultIntervalFollowsThePeriod(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/stats/timeseries?site_id=example.com&period=day&metrics=visitors")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results []struct {
			Date string `json:"date"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	if len(answer.Results) != 24 {
		t.Fatalf("got %d buckets for one day, want 24 hourly ones", len(answer.Results))
	}
}

// TestBreakdownMatchesV2 checks the grouped endpoint.
func TestBreakdownMatchesV2(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t,
		"/api/v1/stats/breakdown?site_id=example.com&period=7d&property=visit:source&metrics=visitors,pageviews")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results []struct {
			Source    string  `json:"source"`
			Visitors  float64 `json:"visitors"`
			Pageviews float64 `json:"pageviews"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	status, raw := h.post(t, "/api/v2/query", `{
		"site_id":"example.com","metrics":["visitors","pageviews"],
		"date_range":"7d","dimensions":["visit:source"]}`)
	if status != http.StatusOK {
		t.Fatalf("the v2 equivalent failed: %d (%s)", status, raw)
	}

	var expected query.Result
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}

	if len(answer.Results) != len(expected.Results) {
		t.Fatalf("v1 returned %d groups, v2 returned %d", len(answer.Results), len(expected.Results))
	}

	for i, row := range answer.Results {
		// The key is the property name with its scope prefix dropped, which is
		// the established shape a client is already indexing by.
		if row.Source != expected.Results[i].Dimensions[0] {
			t.Errorf("group %d: v1 %q, v2 %q", i, row.Source, expected.Results[i].Dimensions[0])
		}

		if row.Visitors != expected.Results[i].Metrics[0] || row.Pageviews != expected.Results[i].Metrics[1] {
			t.Errorf("group %q: v1 %v/%v, v2 %v", row.Source,
				row.Visitors, row.Pageviews, expected.Results[i].Metrics)
		}
	}

	if len(answer.Results) != 2 {
		t.Fatalf("got %d sources, want Google and Twitter", len(answer.Results))
	}
}

// TestBreakdownPagesWithTheOffsetItMeans checks that page two is the second
// page rather than the first one again.
func TestBreakdownPagesWithTheOffsetItMeans(t *testing.T) {
	h := newHarness(t)

	first := h.breakdownSources(t, "&limit=1&page=1")
	second := h.breakdownSources(t, "&limit=1&page=2")

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("pages returned %d and %d rows, want one each", len(first), len(second))
	}

	if first[0] == second[0] {
		t.Fatalf("page two returned the same row as page one: %q", first[0])
	}
}

// breakdownSources runs a source breakdown and returns just the labels.
func (h *harness) breakdownSources(t *testing.T, extra string) []string {
	t.Helper()

	status, body := h.get(t, "/api/v1/stats/breakdown?site_id=example.com&period=7d&property=visit:source"+extra)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results []struct {
			Source string `json:"source"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	labels := make([]string, 0, len(answer.Results))
	for _, row := range answer.Results {
		labels = append(labels, row.Source)
	}

	return labels
}

// TestRealtimeVisitorsMatchesV2 checks the badge endpoint, whose body is a bare
// integer because that is what every status page and shell script already
// parses.
func TestRealtimeVisitorsMatchesV2(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/stats/realtime/visitors?site_id=example.com")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var visitors int64
	if err := json.Unmarshal(body, &visitors); err != nil {
		t.Fatalf("the body must be a bare integer, got %q", string(body))
	}

	want := h.v2Aggregate(t, `{"site_id":"example.com","metrics":["visitors"],"date_range":"realtime"}`)

	expected := 0.0
	if len(want) > 0 {
		expected = want[0]
	}

	if float64(visitors) != expected {
		t.Errorf("v1 realtime = %d, v2 = %v", visitors, expected)
	}
}

// TestRealtimeVisitorsStayExactUnderTheSamplingThreshold checks the one v1
// response that is only a number. The seeded visitor is deliberately outside
// the smallest sample, so a missing Exact flag would turn one visitor into
// zero with no response field capable of admitting it.
func TestRealtimeVisitorsStayExactUnderTheSamplingThreshold(t *testing.T) {
	h := newHarness(t)
	seedLargeEstimate(t, h)
	seedRealtimeVisitor(t, h)
	h.API.SampleThreshold = 1

	status, body := h.get(t, "/api/v1/stats/realtime/visitors?site_id=example.com")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var visitors int64
	if err := json.Unmarshal(body, &visitors); err != nil {
		t.Fatalf("the body must remain a bare integer, got %q", string(body))
	}

	if visitors != 1 {
		t.Fatalf("realtime visitors = %d, want the exact visitor", visitors)
	}
}

// TestV1FilterSyntaxMatchesTheV2Filter checks the filter grammar an existing
// integration already has in its configuration.
func TestV1FilterSyntaxMatchesTheV2Filter(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name string
		v1   string
		v2   string
	}{
		{
			"equality",
			"visit:source==Google",
			`[["is","visit:source",["Google"]]]`,
		},
		{
			"inequality",
			"visit:source!=Google",
			`[["is_not","visit:source",["Google"]]]`,
		},
		{
			"several values",
			"visit:source==Google|Twitter",
			`[["is","visit:source",["Google","Twitter"]]]`,
		},
		{
			"contains",
			"event:page~pric",
			`[["contains","event:page",["pric"]]]`,
		},
		{
			"does not contain",
			"event:page!~pric",
			`[["contains_not","event:page",["pric"]]]`,
		},
		{
			"two clauses",
			"visit:source==Google;event:page==/home",
			`[["is","visit:source",["Google"]],["is","event:page",["/home"]]]`,
		},
		{
			"a wildcard becomes a pattern",
			"event:page==/hom*",
			`[["matches","event:page",["^/hom[^/]*$"]]]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.get(t,
				"/api/v1/stats/aggregate?site_id=example.com&period=7d&metrics=visitors&filters="+urlEncode(tc.v1))
			if status != http.StatusOK {
				t.Fatalf("status = %d (%s)", status, body)
			}

			var answer struct {
				Results map[string]struct {
					Value float64 `json:"value"`
				} `json:"results"`
			}

			if err := json.Unmarshal(body, &answer); err != nil {
				t.Fatal(err)
			}

			want := h.v2Aggregate(t,
				`{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","filters":`+tc.v2+`}`)

			expected := 0.0
			if len(want) > 0 {
				expected = want[0]
			}

			if answer.Results["visitors"].Value != expected {
				t.Errorf("v1 filter %q gave %v, the equivalent v2 filter gave %v",
					tc.v1, answer.Results["visitors"].Value, expected)
			}
		})
	}
}

// TestV1FilterActuallyNarrows guards against the failure mode the test above
// cannot see: a filter that is dropped on both sides agrees with itself and
// still returns every row.
func TestV1FilterActuallyNarrows(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t,
		"/api/v1/stats/aggregate?site_id=example.com&period=7d&metrics=visitors&filters="+urlEncode("visit:source==Google"))
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Results map[string]struct {
			Value float64 `json:"value"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	// Three of this week's five visitors came from Google.
	if answer.Results["visitors"].Value != 3 {
		t.Fatalf("filtered visitors = %v, want 3 — the filter did not narrow anything",
			answer.Results["visitors"].Value)
	}
}

// urlEncode escapes a query-string value.
func urlEncode(value string) string {
	escaped := ""

	for _, r := range value {
		switch r {
		case ';':
			escaped += "%3B"
		case '|':
			escaped += "%7C"
		case '=':
			escaped += "%3D"
		case '~':
			escaped += "%7E"
		case '/':
			escaped += "%2F"
		case '*':
			escaped += "%2A"
		default:
			escaped += string(r)
		}
	}

	return escaped
}

// seedLargeEstimate gives the sampling planner a high daily event rate without
// marking any range as covered by roll-ups. Queries still read raw events, but
// a low threshold now has enough history to decide they are large.
func seedLargeEstimate(t *testing.T, h *harness) {
	t.Helper()

	account, err := h.API.Accounts.Open(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(context.Background(), `
		INSERT INTO rollup_visitors (site_id, grain, bucket, dimension, value_id, events)
		VALUES (?, 0, ?, 0, 0, 1000000)`, siteID, testNow.AddDate(0, 0, -30).Unix()); err != nil {
		t.Fatal(err)
	}
}

// seedRealtimeVisitor writes one visitor inside the live window whose id does
// not survive the minimum one-in-a-thousand sample.
func seedRealtimeVisitor(t *testing.T, h *harness) {
	t.Helper()

	ctx := context.Background()
	account, err := h.API.Accounts.Open(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}

	pageview, err := account.Intern.ID(ctx, intern.EventName, ingest.EventPageview)
	if err != nil {
		t.Fatal(err)
	}

	page, err := account.Intern.ID(ctx, intern.Pathname, "/live")
	if err != nil {
		t.Fatal(err)
	}

	source, err := account.Intern.ID(ctx, intern.Source, "Direct")
	if err != nil {
		t.Fatal(err)
	}

	at := testNow.Unix() - 60
	const (
		userID    = int64(2001)
		sessionID = int64(10001)
	)

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
		                      pageviews, events, entry_page_id, exit_page_id, source_id)
		VALUES (?, ?, ?, ?, ?, 0, 1, 1, 1, ?, ?, ?)`,
		sessionID, siteID, userID, at, at, page, page, source); err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (site_id, timestamp, name_id, user_id, session_id, pathname_id, source_id, scroll_depth)
		VALUES (?, ?, ?, ?, ?, ?, ?, 255)`, siteID, at, pageview, userID, sessionID, page, source); err != nil {
		t.Fatal(err)
	}
}
