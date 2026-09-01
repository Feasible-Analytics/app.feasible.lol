//
// rollupread_test.go
// The arithmetic behind reading a summary: buckets, group boundaries, corrections.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"strings"
	"testing"
	"time"
)

// TestABucketValueIsTheLocalWallClock pins what the `bucket` column holds. The
// whole read path depends on it: the label a graph draws is rendered straight
// off this number with no timezone arithmetic, so if it were the UTC instant
// instead, every day on every chart would be named wrong.
func TestABucketValueIsTheLocalWallClock(t *testing.T) {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}

	// Local midnight on 30 August is 07:00 UTC, seven hours of daylight time
	// behind. The stored value is the wall clock, so it reads as midnight.
	midnight := time.Date(2026, 8, 30, 0, 0, 0, 0, location)

	stored := RollupLocalUnix(midnight, location)

	if got := time.Unix(stored, 0).UTC().Format("2006-01-02 15:04:05"); got != "2026-08-30 00:00:00" {
		t.Errorf("a bucket for local midnight reads as %s, want 2026-08-30 00:00:00", got)
	}

	if stored-midnight.Unix() != -7*3600 {
		t.Errorf("the stored offset is %d seconds, want -25200", stored-midnight.Unix())
	}
}

// TestBucketStartsSnapBackThroughTheCalendar checks that a day is a calendar
// day rather than 86,400 seconds. On the morning the clocks go back, a day is
// twenty-five hours long, and subtracting a constant would put an hour of
// traffic in the wrong bucket.
func TestBucketStartsSnapBackThroughTheCalendar(t *testing.T) {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}

	// 1 November 2026 is the Sunday the clocks go back in the United States.
	afternoon := time.Date(2026, 11, 1, 15, 0, 0, 0, location)

	start := RollupBucketStart(afternoon, GrainDay, location)
	next := RollupNextBucket(start, GrainDay, location)

	if got := next.Sub(start); got != 25*time.Hour {
		t.Errorf("the day the clocks go back is %v long, want 25h", got)
	}

	if got := next.Format("2006-01-02 15:04:05"); got != "2026-11-02 00:00:00" {
		t.Errorf("the bucket after 1 November starts at %s, want 2026-11-02 00:00:00", got)
	}
}

// TestGroupFirstBucketsAreWhereTheCorrectionStops is the rule that makes a
// distinct count re-aggregate. Every bucket in a group but the first has its
// carry-over subtracted; the first bucket's carry-over belongs to whatever came
// before the group and is not in the sum at all.
func TestGroupFirstBucketsAreWhereTheCorrectionStops(t *testing.T) {
	resolved, err := DateRange{Preset: RangeLast28Days}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	daily := &executor{resolved: resolved, plan: &plan{}}

	// A daily graph: every bucket begins its own group, so nothing is ever
	// subtracted and the correction can be skipped entirely.
	if got := daily.rollupGroupFirsts(rollupRead{grain: GrainDay, timeIndex: 0, perBucket: true}); got != nil {
		t.Errorf("a daily graph asked for %d group boundaries, want none", len(got))
	}

	// The headline numbers: one group over the whole range, so exactly one
	// bucket — the first day — escapes the correction.
	totals := daily.rollupGroupFirsts(rollupRead{grain: GrainDay, timeIndex: -1})
	if len(totals) != 1 {
		t.Fatalf("a report with no time dimension has %d group boundaries, want 1", len(totals))
	}

	if got := totals[0].(int64); got != RollupLocalUnix(resolved.Start, resolved.Location) {
		t.Errorf("the single boundary is %d, want the start of the range at %d",
			got, RollupLocalUnix(resolved.Start, resolved.Location))
	}
}

// TestAMonthlyGraphSubtractsInsideEachMonth checks the case the correction
// exists for: a group that spans many buckets. Each month's first day escapes
// the correction and the rest of its days do not.
func TestAMonthlyGraphSubtractsInsideEachMonth(t *testing.T) {
	resolved, err := DateRange{Preset: RangeLast12Months}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	// A year of daily buckets is a chart nobody can read, so the range picks
	// weeks on its own; an explicit time:month dimension is what asks for
	// months, and it is the widest group the correction has to handle.
	x := &executor{resolved: resolved.WithInterval(IntervalMonth), plan: &plan{}}

	firsts := x.rollupGroupFirsts(rollupRead{grain: GrainDay, timeIndex: 0})

	if len(firsts) != 12 {
		t.Fatalf("a twelve-month graph has %d group boundaries, want 12", len(firsts))
	}

	for i, value := range firsts {
		at := time.Unix(value.(int64), 0).UTC()

		if at.Day() != 1 {
			t.Errorf("boundary %d is on day %d of the month, want the 1st", i, at.Day())
		}
	}
}

// TestTheCorrectionOnlyAppearsWhenItIsNeeded checks the SQL itself. A plain sum
// must not carry a subtraction, because a bucket that inherited nothing still
// costs a CASE per row to prove it.
func TestTheCorrectionOnlyAppearsWhenItIsNeeded(t *testing.T) {
	firsts := []any{int64(1), int64(2)}

	plain := rollupColumnExpr(rollupComponent{column: "pageviews"}, "r", false, firsts)
	if strings.Contains(plain.SQL, "CASE") {
		t.Errorf("a pageview count carries a carry-over correction: %s", plain.SQL)
	}

	perBucket := rollupColumnExpr(rollupComponent{column: "event_visitors", carried: "event_visitors_carried"}, "r", true, firsts)
	if strings.Contains(perBucket.SQL, "CASE") {
		t.Errorf("a single-bucket group carries a correction it cannot need: %s", perBucket.SQL)
	}

	corrected := rollupColumnExpr(rollupComponent{column: "event_visitors", carried: "event_visitors_carried"}, "r", false, firsts)
	if !strings.Contains(corrected.SQL, "event_visitors_carried") {
		t.Errorf("a visitor count summed across buckets has no correction: %s", corrected.SQL)
	}

	if len(corrected.Args) != len(firsts) {
		t.Errorf("the correction bound %d boundaries, want %d", len(corrected.Args), len(firsts))
	}
}

// TestEveryMetricEitherMapsOntoTheSummaryOrIsRefused makes the registry's
// coverage explicit. A metric silently missing from the mapping would not be
// wrong — the router would refuse it — but it would be quietly slow forever,
// which is the failure this milestone exists to remove.
func TestEveryMetricEitherMapsOntoTheSummaryOrIsRefused(t *testing.T) {
	cases := map[string]bool{
		"visitors":        true,
		"visits":          true,
		"pageviews":       true,
		"events":          true,
		"bounce_rate":     true,
		"visit_duration":  true,
		"views_per_visit": true,

		// Measured over the visits whose tracker reported something, which is a
		// distinct count of visits that no stored column can correct.
		"time_on_page": false,
		"scroll_depth": false,

		// Composites, each needing a second query of its own shape.
		"exit_rate":             false,
		"conversion_rate":       false,
		"group_conversion_rate": false,

		// Money is not summarised. The roll-up tables carry no revenue column,
		// and adding one would mean picking a reporting currency at write time
		// — which is the one thing a report has to be free to choose. These
		// fall back to raw, and the numbers are small because a revenue report
		// is scoped to the visits that converted.
		"total_revenue":       false,
		"average_revenue":     false,
		"revenue_per_visitor": false,
	}

	for _, name := range MetricNames() {
		want, listed := cases[name]
		if !listed {
			t.Errorf("metric %q is not in this test — decide whether the summary can answer it", name)
			continue
		}

		definition, _ := metricByName(name)

		table := tableEvents
		if definition.Scope == scopeSession {
			table = tableSessions
		}

		if _, ok := rollupComponents(name, table); ok != want {
			t.Errorf("rollupComponents(%q) = %v, want %v", name, ok, want)
		}
	}

	// The numeric property aggregates are a family rather than registry
	// entries, so they are enumerated separately — but they are classified for
	// the same reason and by the same rule.
	//
	// Every one of them is answered from raw events. The summary tables carry a
	// fixed set of counter columns and no properties at all: a property is a
	// key a customer invents, so summarising one would mean a column per key
	// per site. Nor would a stored total be enough — a percentile needs the
	// values themselves, and an average needs its own denominator because the
	// events that carried a number are not the events the bucket counted.
	for _, agg := range AggregateNames() {
		name := agg + "(event:props:price)"

		if _, ok := metricByName(name); !ok {
			t.Errorf("%q is not a metric — AggregateNames and the parser disagree", name)
			continue
		}

		if _, ok := rollupComponents(name, tableEvents); ok {
			t.Errorf("rollupComponents(%q) claims the summary can answer a property", name)
		}
	}
}
