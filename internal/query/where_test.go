//
// where_test.go
// Every filter operator, and the rules for how filters combine.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"strings"
	"testing"
)

// filterCase is one filter and the numbers it should produce against the
// fixture. Every expectation is counted by hand from the ten events in
// engine_test.go.
type filterCase struct {
	name      string
	filters   []Filter
	pageviews float64
	visitors  float64
}

// TestFilterOperators walks every operator, in both its positive and negative
// form, against the same fixture.
func TestFilterOperators(t *testing.T) {
	engine := newEngine(t)

	cases := []filterCase{
		{
			name:      "is",
			filters:   []Filter{{Operator: OpIs, Dimension: "visit:country", Values: []string{"US"}}},
			pageviews: 5, visitors: 3,
		},
		{
			name:      "is_not",
			filters:   []Filter{{Operator: OpIsNot, Dimension: "visit:country", Values: []string{"US"}}},
			pageviews: 2, visitors: 1,
		},
		{
			name:      "is on the empty value",
			filters:   []Filter{{Operator: OpIs, Dimension: "visit:source", Values: []string{""}}},
			pageviews: 2, visitors: 1,
		},
		{
			name:      "contains",
			filters:   []Filter{{Operator: OpContains, Dimension: "event:page", Values: []string{"pric"}}},
			pageviews: 3, visitors: 2,
		},
		{
			name:      "contains_not",
			filters:   []Filter{{Operator: OpContainsNot, Dimension: "event:page", Values: []string{"pric"}}},
			pageviews: 4, visitors: 2,
		},
		{
			// A session-scoped dimension is the one place a filter has to reach
			// across the join from the event table, and the suggestion list
			// behind every filter box searches exactly these.
			name:      "contains on a session dimension",
			filters:   []Filter{{Operator: OpContains, Dimension: "visit:entry_page", Values: []string{"pric"}}},
			pageviews: 1, visitors: 1,
		},
		{
			name:      "contains_not on a session dimension",
			filters:   []Filter{{Operator: OpContainsNot, Dimension: "visit:entry_page", Values: []string{"pric"}}},
			pageviews: 6, visitors: 2,
		},
		{
			name:      "contains on an interned session dimension",
			filters:   []Filter{{Operator: OpContains, Dimension: "visit:source", Values: []string{"goo"}, CaseInsensitive: true}},
			pageviews: 4, visitors: 2,
		},
		{
			name:      "matches",
			filters:   []Filter{{Operator: OpMatches, Dimension: "event:page", Values: []string{"^/pri"}}},
			pageviews: 3, visitors: 2,
		},
		{
			name:      "matches_not",
			filters:   []Filter{{Operator: OpMatchesNot, Dimension: "event:page", Values: []string{"^/pri"}}},
			pageviews: 4, visitors: 2,
		},
		{
			name: "has_done",
			filters: []Filter{{
				Operator: OpHasDone,
				Child:    &Filter{Operator: OpIs, Dimension: "event:name", Values: []string{"Signup"}},
			}},
			pageviews: 2, visitors: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := baseQuery("pageviews", "visitors")
			q.Filters = tc.filters

			result := run(t, engine, q)

			closeTo(t, tc.name+" pageviews", result.Results[0].Metrics[0], tc.pageviews)
			closeTo(t, tc.name+" visitors", result.Results[0].Metrics[1], tc.visitors)
		})
	}
}

// TestValuesInsideOneFilterOr checks the first half of the filter grammar: one
// filter with several values widens.
func TestValuesInsideOneFilterOr(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "event:page", Values: []string{"/home", "/about"}}}

	result := run(t, engine, q)

	closeTo(t, "pageviews on /home or /about", result.Results[0].Metrics[0], 4)
}

// TestSeparateFiltersAnd checks the second half: several filters narrow.
func TestSeparateFiltersAnd(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Filters = []Filter{
		{Operator: OpIs, Dimension: "visit:country", Values: []string{"US"}},
		{Operator: OpIs, Dimension: "event:page", Values: []string{"/home"}},
	}

	result := run(t, engine, q)

	// Three visits saw /home, but the one from Canada is excluded.
	closeTo(t, "US pageviews on /home", result.Results[0].Metrics[0], 2)
}

// TestCaseSensitivity checks that the flag actually changes the answer, in both
// directions and on both operators that take a literal.
func TestCaseSensitivity(t *testing.T) {
	engine := newEngine(t)

	sensitive := baseQuery("pageviews")
	sensitive.Filters = []Filter{{Operator: OpIs, Dimension: "visit:browser", Values: []string{"chrome"}}}

	closeTo(t, "case-sensitive is", run(t, engine, sensitive).Results[0].Metrics[0], 0)

	insensitive := baseQuery("pageviews")
	insensitive.Filters = []Filter{{
		Operator: OpIs, Dimension: "visit:browser", Values: []string{"chrome"}, CaseInsensitive: true,
	}}

	closeTo(t, "case-insensitive is", run(t, engine, insensitive).Results[0].Metrics[0], 6)

	contains := baseQuery("pageviews")
	contains.Filters = []Filter{{Operator: OpContains, Dimension: "event:page", Values: []string{"HOME"}}}

	closeTo(t, "case-sensitive contains", run(t, engine, contains).Results[0].Metrics[0], 0)

	contains.Filters[0].CaseInsensitive = true
	closeTo(t, "case-insensitive contains", run(t, engine, contains).Results[0].Metrics[0], 3)

	// The same two answers across the session join. Folding one side with Go's
	// rules and the other with SQLite's is a mistake that only shows up here.
	entry := baseQuery("pageviews")
	entry.Filters = []Filter{{Operator: OpContains, Dimension: "visit:entry_page", Values: []string{"PRIC"}}}

	closeTo(t, "case-sensitive contains on a session dimension", run(t, engine, entry).Results[0].Metrics[0], 0)

	entry.Filters[0].CaseInsensitive = true
	closeTo(t, "case-insensitive contains on a session dimension", run(t, engine, entry).Results[0].Metrics[0], 1)
}

// TestCaseInsensitiveRegex checks the flag reaches the matcher.
func TestCaseInsensitiveRegex(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Filters = []Filter{{Operator: OpMatches, Dimension: "event:page", Values: []string{"^/HOME$"}}}

	closeTo(t, "case-sensitive matches", run(t, engine, q).Results[0].Metrics[0], 0)

	q.Filters[0].CaseInsensitive = true
	closeTo(t, "case-insensitive matches", run(t, engine, q).Results[0].Metrics[0], 3)
}

// TestPropertyFilterAndBreakdown checks the one dimension that lives in the
// cold table.
func TestPropertyFilterAndBreakdown(t *testing.T) {
	engine := newEngine(t)

	filtered := baseQuery("events")
	filtered.Filters = []Filter{{Operator: OpIs, Dimension: "event:props:plan", Values: []string{"pro"}}}

	closeTo(t, "events with plan=pro", run(t, engine, filtered).Results[0].Metrics[0], 1)

	missing := baseQuery("events")
	missing.Filters = []Filter{{Operator: OpIs, Dimension: "event:props:plan", Values: []string{"free"}}}

	closeTo(t, "events with plan=free", run(t, engine, missing).Results[0].Metrics[0], 0)

	breakdown := baseQuery("events")
	breakdown.Dimensions = []string{"event:props:plan"}

	result := run(t, engine, breakdown)

	if len(result.Results) != 1 || result.Results[0].Dimensions[0] != "pro" {
		t.Fatalf("property breakdown returned %+v, want one row for pro", result.Results)
	}
}

// TestFilterValuesAreParameters checks that a value which would break a
// hand-built statement is simply a value that matches nothing.
func TestFilterValuesAreParameters(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Filters = []Filter{{
		Operator:  OpIs,
		Dimension: "event:page",
		Values:    []string{"/it's'; DROP TABLE events; --"},
	}}

	result := run(t, engine, q)
	closeTo(t, "pageviews for a quoted value", result.Results[0].Metrics[0], 0)

	// The table is still there, which is the actual assertion.
	closeTo(t, "pageviews afterwards", run(t, engine, baseQuery("pageviews")).Results[0].Metrics[0], 7)
}

// TestNegatedFilterAtVisitGrainMeansNoMatchingEvent checks the placement of the
// NOT. "Visits with no signup" and "visits with an event that is not a signup"
// are different sets, and the second is almost every visit ever.
func TestNegatedFilterAtVisitGrainMeansNoMatchingEvent(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("visits", "bounce_rate")
	q.Filters = []Filter{{Operator: OpIsNot, Dimension: "event:name", Values: []string{"Signup"}}}

	result := run(t, engine, q)

	// Three of the four visits contain no signup at all.
	closeTo(t, "visits without a signup", result.Results[0].Metrics[0], 3)
}

// TestUnknownDimensionIsACallerError checks that a typo comes back as something
// the caller can act on, with the alternatives listed.
func TestUnknownDimensionIsACallerError(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("visitors")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "visit:contry", Values: []string{"US"}}}

	_, err := engine.Run(context.Background(), q)
	if err == nil {
		t.Fatal("a misspelt dimension must be refused")
	}

	if !strings.Contains(err.Error(), "visit:country") {
		t.Errorf("the error should list the real dimensions, got %q", err.Error())
	}
}

// TestInvalidRegexIsACallerError checks that a bad pattern is a 400 rather than
// a failure inside SQLite.
func TestInvalidRegexIsACallerError(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("visitors")
	q.Filters = []Filter{{Operator: OpMatches, Dimension: "event:page", Values: []string{"([a-z"}}}

	_, err := engine.Run(context.Background(), q)
	if err == nil {
		t.Fatal("an invalid regular expression must be refused")
	}

	if _, ok := err.(*Error); !ok {
		t.Fatalf("want a caller-facing error, got %T", err)
	}
}
