//
// propagg_test.go
// Numeric aggregates over a custom property, including the values that are not numbers.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// checkoutFilter selects the events written by writeCheckouts.
var checkoutFilter = Filter{Operator: OpIs, Dimension: "event:name", Values: []string{"Checkout"}}

// TestPropAggregateNameParsing pins the wire form. It is the contract a client
// types by hand, so a name that parses one way today and another way tomorrow
// is a dashboard that silently changes what it is measuring.
func TestPropAggregateNameParsing(t *testing.T) {
	cases := []struct {
		name       string
		ok         bool
		agg        string
		percentile float64
		key        string
	}{
		{name: "sum(event:props:price)", ok: true, agg: AggSum, key: "price"},
		{name: "avg(event:props:price)", ok: true, agg: AggAvg, key: "price"},
		{name: "min(event:props:price)", ok: true, agg: AggMin, key: "price"},
		{name: "max(event:props:price)", ok: true, agg: AggMax, key: "price"},
		{name: "p95(event:props:load_ms)", ok: true, percentile: 0.95, key: "load_ms"},

		// A property name may itself contain a colon, which is why the
		// aggregate is a prefix in brackets rather than a suffix.
		{name: "sum(event:props:cart:total)", ok: true, agg: AggSum, key: "cart:total"},

		{name: "median(event:props:price)"},
		{name: "p37(event:props:price)"},
		{name: "sum(visit:country)"},
		{name: "sum(event:props:)"},
		{name: "sum(price)"},
		{name: "visitors"},
		{name: "sum(event:props:price"},
	}

	for _, tc := range cases {
		parsed, ok := parsePropAggregate(tc.name)

		if ok != tc.ok {
			t.Errorf("parsePropAggregate(%q) ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}

		if !ok {
			continue
		}

		if parsed.Agg != tc.agg || parsed.Percentile != tc.percentile || parsed.Dim.PropKey != tc.key {
			t.Errorf("parsePropAggregate(%q) = %+v, want agg %q percentile %v key %q",
				tc.name, parsed, tc.agg, tc.percentile, tc.key)
		}
	}
}

// TestPropAggregatesAreComputedByHand is the arithmetic. The five checkout
// events carry 10, 20, 30, 40 and 50, so every answer below is one somebody can
// check on paper.
func TestPropAggregatesAreComputedByHand(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeCheckouts(t, account)

	q := baseQuery(
		"sum(event:props:price)",
		"avg(event:props:price)",
		"min(event:props:price)",
		"max(event:props:price)",
		"p50(event:props:price)",
		"p95(event:props:price)",
	)
	q.Filters = []Filter{checkoutFilter}

	row := run(t, engine, q).Results[0]

	closeTo(t, "sum", row.Metrics[0], 150)
	closeTo(t, "avg", row.Metrics[1], 30)
	closeTo(t, "min", row.Metrics[2], 10)
	closeTo(t, "max", row.Metrics[3], 50)

	// Nearest rank over five values: the median is the third, and the 95th
	// percentile is the fifth. Both are values somebody actually sent, which is
	// the whole reason this is nearest-rank rather than interpolated.
	closeTo(t, "p50", row.Metrics[4], 30)
	closeTo(t, "p95", row.Metrics[5], 50)
}

// TestPropAggregateBreaksDownByPage checks that a numeric aggregate composes
// with a breakdown the same way every other metric does — the second query is
// joined back on the group key rather than paginated on its own.
func TestPropAggregateBreaksDownByPage(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeCheckouts(t, account)

	q := baseQuery("sum(event:props:price)")
	q.Dimensions = []string{"event:page"}
	q.Filters = []Filter{checkoutFilter}

	got := map[string]float64{}
	for _, row := range run(t, engine, q).Results {
		got[row.Dimensions[0]] = row.Metrics[0]
	}

	// 10 and 20 on /home, 30, 40 and 50 on /pricing.
	closeTo(t, "/home", got["/home"], 30)
	closeTo(t, "/pricing", got["/pricing"], 120)
}

// TestNonNumericPropValuesAreExcludedAndReported is the promise that nothing
// goes missing quietly.
//
// A property that holds a number on five events and the word "free" on a sixth
// averages over the five, and the warning names both counts. The alternative —
// SQLite's own CAST — would read "free" as zero and drag the average from 30 to
// 25 with nothing on the screen to say why.
func TestNonNumericPropValuesAreExcludedAndReported(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeCheckouts(t, account)
	writeCheckout(t, account, 36, 3, visitorA, at(30, 9, 9), "/pricing", "free")

	q := baseQuery("avg(event:props:price)")
	q.Filters = []Filter{checkoutFilter}

	result := run(t, engine, q)

	closeTo(t, "avg", result.Results[0].Metrics[0], 30)

	warning, ok := result.Meta.MetricWarnings["avg(event:props:price)"]
	if !ok {
		t.Fatal("a property with a non-numeric value produced no warning")
	}

	if warning.Code != WarnNotNumeric {
		t.Fatalf("warning code = %q, want %q", warning.Code, WarnNotNumeric)
	}

	if !strings.Contains(warning.Warning, "5 of the 6") {
		t.Fatalf("the warning does not name both counts: %q", warning.Warning)
	}
}

// TestPropAggregateOverTextOnlySaysSoRatherThanAnsweringZero covers the case
// that looks exactly like a real zero: a property nobody ever sent a number in.
func TestPropAggregateOverTextOnlySaysSoRatherThanAnsweringZero(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeCheckout(t, account, 30, 3, visitorA, at(30, 9, 5), "/home", "free")

	q := baseQuery("sum(event:props:price)")
	q.Filters = []Filter{checkoutFilter}

	result := run(t, engine, q)

	closeTo(t, "sum", result.Results[0].Metrics[0], 0)

	warning := result.Meta.MetricWarnings["sum(event:props:price)"]
	if warning.Code != WarnNotNumeric {
		t.Fatalf("warning code = %q, want %q", warning.Code, WarnNotNumeric)
	}
}

// TestPropAggregateOverAnAbsentPropertySaysSo covers the other zero: a property
// name nobody has ever sent at all, which is what a typo looks like.
func TestPropAggregateOverAnAbsentPropertySaysSo(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeCheckouts(t, account)

	q := baseQuery("sum(event:props:pirce)")

	result := run(t, engine, q)

	if warning := result.Meta.MetricWarnings["sum(event:props:pirce)"]; warning.Code != WarnNoCoverage {
		t.Fatalf("warning code = %q, want %q", warning.Code, WarnNoCoverage)
	}
}

// TestSessionScopedPropIsCountedOncePerVisit is the reason a property carries a
// declared scope at all.
//
// The order value is repeated on all three events of the visit, which is what a
// tracker sending a session-scoped property does. Summed per event it would be
// three hundred; summed per visit it is a hundred, which is what the customer
// was actually paid.
func TestSessionScopedPropIsCountedOncePerVisit(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	for i, timestamp := range []int64{at(30, 9, 5), at(30, 9, 6), at(30, 9, 7)} {
		writeCheckout(t, account, int64(40+i), 3, visitorA, timestamp, "/pricing", "100")
	}

	q := baseQuery("sum(event:props:price)")
	q.Filters = []Filter{checkoutFilter}

	// Event scope is the default, and reads every hit that carried the value.
	closeTo(t, "event scope", run(t, engine, q).Results[0].Metrics[0], 300)

	declareProperty(t, account, "price", propScopeSession)

	closeTo(t, "session scope", run(t, engine, q).Results[0].Metrics[0], 100)
}

// TestSessionScopedPropContributesToOnlyTheVisitsStartBucket covers a visit
// whose events cross an hour boundary. The property belongs to the visit, so a
// time series places it once by started_at instead of adding it once in every
// event bucket the visit touched.
func TestSessionScopedPropContributesToOnlyTheVisitsStartBucket(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeCheckout(t, account, 50, 3, visitorA, at(30, 9, 55), "/pricing", "100")
	writeCheckout(t, account, 51, 3, visitorA, at(30, 10, 5), "/pricing", "100")
	declareProperty(t, account, "price", propScopeSession)

	q := baseQuery("sum(event:props:price)")
	q.Dimensions = []string{"time:hour"}
	q.Filters = []Filter{checkoutFilter}
	q.Pagination.Limit = MaxLimit

	got := map[string]float64{}
	var total float64
	for _, row := range run(t, engine, q).Results {
		got[row.Dimensions[0]] = row.Metrics[0]
		total += row.Metrics[0]
	}
	closeTo(t, "visit start hour", got["2026-08-30 09:00:00"], 100)
	closeTo(t, "later event hour", got["2026-08-30 10:00:00"], 0)
	closeTo(t, "all buckets", total, 100)
}

// TestPropAggregateIsNotSummarisable pins the routing decision. The summary
// tables carry no property columns, so a query asking for one has to reach the
// raw events — being slow is fine, being answered from a table that never saw
// the property is not.
func TestPropAggregateIsNotSummarisable(t *testing.T) {
	for _, name := range []string{
		"sum(event:props:price)", "avg(event:props:price)", "min(event:props:price)",
		"max(event:props:price)", "p95(event:props:price)",
	} {
		if _, ok := rollupComponents(name, tableEvents); ok {
			t.Errorf("rollupComponents(%q) claims the summary can answer it", name)
		}
	}
}

// TestOnlySumUsesInverseSampleScaling checks the arithmetic method, not the
// uncertainty. A total read from a tenth of fact rows must be expanded; an
// average, extremum or percentile is calculated within selected fact rows
// without that multiplication, while still remaining a population estimate.
func TestOnlySumUsesInverseSampleScaling(t *testing.T) {
	cases := map[string]bool{
		"sum(event:props:price)": true,
		"avg(event:props:price)": false,
		"min(event:props:price)": false,
		"max(event:props:price)": false,
		"p95(event:props:price)": false,
	}

	for name, want := range cases {
		definition, ok := metricByName(name)
		if !ok {
			t.Fatalf("%q is not a metric", name)
		}

		if definition.Scaled != want {
			t.Errorf("%q scaled = %v, want %v", name, definition.Scaled, want)
		}
	}
}

// writeCheckouts writes five priced checkout events across two pages.
func writeCheckouts(t *testing.T, account *accounts.Account) {
	t.Helper()

	writeCheckout(t, account, 30, 3, visitorA, at(30, 9, 5), "/home", "10")
	writeCheckout(t, account, 31, 3, visitorA, at(30, 9, 6), "/home", "20")
	writeCheckout(t, account, 32, 4, visitorC, at(30, 10, 2), "/pricing", "30")
	writeCheckout(t, account, 33, 4, visitorC, at(30, 10, 3), "/pricing", "40")
	writeCheckout(t, account, 34, 4, visitorC, at(30, 10, 4), "/pricing", "50")
}

// writeCheckout writes one event carrying a `price` property, hot row and cold
// row together, exactly as the ingest writer does: every property value is
// stored as text, which is why aggregating one has to parse it.
func writeCheckout(t *testing.T, account *accounts.Account, id, session, user, timestamp int64, page, price string) {
	t.Helper()

	ctx := context.Background()

	name, err := account.Intern.ID(ctx, intern.EventName, "Checkout")
	if err != nil {
		t.Fatal(err)
	}

	pathname, err := account.Intern.ID(ctx, intern.Pathname, page)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id, scroll_depth, has_details)
		VALUES (?, 1, ?, ?, ?, ?, ?, 255, 1)`,
		id, timestamp, name, user, session, pathname); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(map[string]string{"price": price})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO event_details (event_id, props) VALUES (?, ?)", id, string(encoded)); err != nil {
		t.Fatal(err)
	}

	// A declared session property is read from the ingest path's bounded
	// first-event representation rather than by walking every event in a visit.
	if _, err := account.Writer().ExecContext(ctx,
		"UPDATE sessions SET entry_props = COALESCE(entry_props, ?) WHERE id = ?", string(encoded), session); err != nil {
		t.Fatal(err)
	}
}

// declareProperty registers a property under a scope, the way the settings
// screen does.
func declareProperty(t *testing.T, account *accounts.Account, name, scope string) {
	t.Helper()

	if _, err := account.Writer().ExecContext(context.Background(),
		"INSERT INTO allowed_properties (site_id, name, scope, created_at) VALUES (1, ?, ?, 0)",
		name, scope); err != nil {
		t.Fatal(err)
	}
}
