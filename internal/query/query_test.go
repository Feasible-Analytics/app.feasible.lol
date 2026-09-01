//
// query_test.go
// Validation, defaults, and the array wire form of a filter.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// valid is a query that passes, so each case below can break exactly one thing.
func valid() Query {
	q := Query{
		SiteIDs:   []int64{1},
		Metrics:   []string{"visitors"},
		DateRange: DateRange{Preset: RangeLast7Days},
	}
	q.Normalise()

	return q
}

// TestValidationRejectsBadInput checks that every parameter a caller can get
// wrong comes back as a message rather than as a failure deeper in the stack.
// A bad page number must never be a 500: the caller cannot read our logs, and
// the only person who can fix it is the one holding the request.
func TestValidationRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Query)
		expects string
	}{
		{"no site", func(q *Query) { q.SiteIDs = nil }, "site"},
		{"no metric", func(q *Query) { q.Metrics = nil }, "metric"},
		{"unknown metric", func(q *Query) { q.Metrics = []string{"vistors"} }, "unknown metric"},
		{"duplicate metric", func(q *Query) { q.Metrics = []string{"visitors", "visitors"} }, "twice"},
		{"too many metrics", func(q *Query) {
			q.Metrics = make([]string, MaxMetrics+1)
			for i := range q.Metrics {
				q.Metrics[i] = fmt.Sprintf("sum(event:props:value_%d)", i)
			}
		}, "at most"},
		{"unknown dimension", func(q *Query) { q.Dimensions = []string{"visit:planet"} }, "unknown dimension"},
		{"duplicate dimension", func(q *Query) { q.Dimensions = []string{"visit:source", "visit:source"} }, "twice"},
		{"too many dimensions", func(q *Query) {
			q.Dimensions = []string{"visit:source", "visit:country", "visit:city", "visit:browser", "visit:os", "visit:device"}
		}, "at most"},
		{"unknown timezone", func(q *Query) { q.Timezone = "Mars/Olympus" }, "timezone"},
		{"unknown date range", func(q *Query) { q.DateRange = DateRange{Preset: "fortnight"} }, "date_range"},
		{"negative offset", func(q *Query) { q.Pagination.Offset = -1 }, "offset"},
		{"zero limit", func(q *Query) { q.Pagination.Limit = 0 }, "limit"},
		{"oversized limit", func(q *Query) { q.Pagination.Limit = MaxLimit + 1 }, "limit"},
		{"order by a metric nobody asked for", func(q *Query) { q.OrderBy = []Order{{Key: "pageviews"}} }, "order by"},
		{"sample rate above one", func(q *Query) { q.SampleRate = 2 }, "sample_rate"},
		{"sample rate below predicate precision", func(q *Query) { q.SampleRate = 0.0005 }, "increments"},
		{"sample rate between predicate buckets", func(q *Query) { q.SampleRate = 0.1234 }, "increments"},
		{"unknown operator", func(q *Query) {
			q.Filters = []Filter{{Operator: "starts_with", Dimension: "event:page", Values: []string{"/"}}}
		}, "operator"},
		{"filter with no values", func(q *Query) {
			q.Filters = []Filter{{Operator: OpIs, Dimension: "event:page"}}
		}, "at least one value"},
		{"has_done with no inner filter", func(q *Query) {
			q.Filters = []Filter{{Operator: OpHasDone}}
		}, "inner filter"},
		{"nested has_done", func(q *Query) {
			q.Filters = []Filter{{Operator: OpHasDone, Child: &Filter{Operator: OpHasDone}}}
		}, "has_done"},
		{"filter on time", func(q *Query) {
			q.Filters = []Filter{{Operator: OpIs, Dimension: "time:day", Values: []string{"2026-08-30"}}}
		}, "date range"},
		{"unknown comparison mode", func(q *Query) {
			q.Include.Comparisons = &Comparison{Mode: "last_tuesday"}
		}, "comparison mode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := valid()
			tc.mutate(&q)

			err := q.Validate()
			if err == nil {
				t.Fatal("want a validation error")
			}

			if _, ok := err.(*Error); !ok {
				t.Fatalf("want a caller-facing error, got %T", err)
			}

			if !strings.Contains(err.Error(), tc.expects) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.expects)
			}
		})
	}
}

// TestNormaliseFillsDefaults checks what a caller gets when they leave things
// out, which is also what the echoed query has to show.
func TestNormaliseFillsDefaults(t *testing.T) {
	q := Query{SiteIDs: []int64{1}, Metrics: []string{"visitors", "pageviews"}}
	q.Normalise()

	if q.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", q.Timezone)
	}

	if q.Pagination.Limit != DefaultLimit {
		t.Errorf("limit = %d, want %d", q.Pagination.Limit, DefaultLimit)
	}

	if q.SampleRate != 1 {
		t.Errorf("sample rate = %v, want 1", q.SampleRate)
	}

	if q.DateRange.Preset != RangeLast28Days {
		t.Errorf("date range = %q, want %q", q.DateRange.Preset, RangeLast28Days)
	}

	if len(q.OrderBy) != 1 || q.OrderBy[0].Key != "visitors" || !q.OrderBy[0].Descending {
		t.Errorf("default order = %+v, want visitors descending", q.OrderBy)
	}
}

// TestTimeSeriesDefaultsToChronologicalOrder checks the one case where biggest
// first is the wrong default.
func TestTimeSeriesDefaultsToChronologicalOrder(t *testing.T) {
	q := Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, Dimensions: []string{"time:day"}}
	q.Normalise()

	if len(q.OrderBy) != 1 || q.OrderBy[0].Key != "time:day" || q.OrderBy[0].Descending {
		t.Errorf("default order = %+v, want time:day ascending", q.OrderBy)
	}
}

// TestFilterWireForm checks the array shape in both directions, including that
// a filter is case sensitive unless it says otherwise.
func TestFilterWireForm(t *testing.T) {
	var filter Filter
	if err := json.Unmarshal([]byte(`["is","visit:country",["US","CA"]]`), &filter); err != nil {
		t.Fatal(err)
	}

	if filter.Operator != OpIs || filter.Dimension != "visit:country" || len(filter.Values) != 2 {
		t.Fatalf("parsed as %+v", filter)
	}

	if filter.CaseInsensitive {
		t.Error("a filter with no modifiers must be case sensitive")
	}

	if err := json.Unmarshal([]byte(`["contains","event:page",["/blog"],{"case_sensitive":false}]`), &filter); err != nil {
		t.Fatal(err)
	}

	if !filter.CaseInsensitive {
		t.Error("case_sensitive:false must turn case sensitivity off")
	}

	encoded, err := json.Marshal(filter)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(encoded), `"case_sensitive":false`) {
		t.Errorf("the echoed filter must state its case sensitivity, got %s", encoded)
	}
}

// TestHasDoneWireForm checks the nested shape.
func TestHasDoneWireForm(t *testing.T) {
	var filter Filter
	if err := json.Unmarshal([]byte(`["has_done",["is","event:name",["Signup"]]]`), &filter); err != nil {
		t.Fatal(err)
	}

	if filter.Operator != OpHasDone || filter.Child == nil || filter.Child.Dimension != "event:name" {
		t.Fatalf("parsed as %+v", filter)
	}

	if err := filter.validate(false); err != nil {
		t.Fatal(err)
	}
}

// TestOrderWireForm checks the two-element array and its one legal pair of
// directions.
func TestOrderWireForm(t *testing.T) {
	var order Order
	if err := json.Unmarshal([]byte(`["visitors","desc"]`), &order); err != nil {
		t.Fatal(err)
	}

	if order.Key != "visitors" || !order.Descending {
		t.Fatalf("parsed as %+v", order)
	}

	if err := json.Unmarshal([]byte(`["visitors","sideways"]`), &order); err == nil {
		t.Fatal("an unknown direction must be refused")
	}
}

// TestPropertyDimensionNames checks the event:props:<key> family, including the
// characters a key may not contain.
func TestPropertyDimensionNames(t *testing.T) {
	resolved, err := resolveDimension("event:props:plan")
	if err != nil {
		t.Fatal(err)
	}

	if resolved.PropKey != "plan" || resolved.jsonPath() != `$."plan"` {
		t.Fatalf("resolved as %+v", resolved)
	}

	for _, name := range []string{"event:props:", `event:props:a"b`, `event:props:a\b`} {
		if _, err := resolveDimension(name); err == nil {
			t.Errorf("%q should be refused", name)
		}
	}
}

// TestClampHoldsRatesInsideTheirRange checks the guard against a derived rate
// that cannot be true. A percentage outside 0 to 100 is a bug, and it should
// look like a small wrong number rather than a screenshot on the internet.
func TestClampHoldsRatesInsideTheirRange(t *testing.T) {
	if got := clamp(4294967271, true, false); got != 100 {
		t.Errorf("clamp(4294967271) = %v, want 100", got)
	}

	if got := clamp(-3, true, false); got != 0 {
		t.Errorf("clamp(-3) = %v, want 0", got)
	}

	if got := clamp(-3, false, false); got != 0 {
		t.Errorf("a negative count clamps to 0, got %v", got)
	}

	if got := ratio(5, 0); got != 0 {
		t.Errorf("a zero denominator gives %v, want 0", got)
	}
}
