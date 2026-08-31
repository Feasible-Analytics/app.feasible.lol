//
// rollup_test.go
// The roll-up seam: routing, splitting, and what may not be split.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"testing"
	"time"
)

// splittingRouter cuts a range at the start of today, which is the split a
// roll-up router makes: complete days come from a summary, the day in progress
// cannot. Nothing produces summaries yet, so both halves read raw — but the
// engine has to add them up correctly, and this is what proves it does.
type splittingRouter struct{}

// Route splits at today when there is a complete part to split off.
func (splittingRouter) Route(_ *Query, r Resolved) []Segment {
	complete, partial, split := SplitAtToday(r)
	if !split {
		return []Segment{{Range: r, Source: SourceRaw}}
	}

	return []Segment{
		{Range: complete, Source: SourceRollup},
		{Range: partial, Source: SourceRaw},
	}
}

// TestRawRouterAnswersTheWholeRange checks the default.
func TestRawRouterAnswersTheWholeRange(t *testing.T) {
	resolved, err := DateRange{Preset: RangeLast7Days}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	segments := RawRouter{}.Route(&Query{}, resolved)

	if len(segments) != 1 || segments[0].Source != SourceRaw {
		t.Fatalf("the raw router should answer with one raw segment, got %+v", segments)
	}
}

// TestSplitAtTodaySeparatesTheFinishedDays checks the boundary arithmetic a
// roll-up router will depend on.
func TestSplitAtTodaySeparatesTheFinishedDays(t *testing.T) {
	resolved, err := DateRange{Preset: RangeLast7Days}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	complete, partial, split := SplitAtToday(resolved)
	if !split {
		t.Fatal("a range that reaches today has a finished part and an unfinished one")
	}

	if got := complete.End.Format(time.RFC3339); got != "2026-08-30T00:00:00Z" {
		t.Errorf("the finished part ends at %s, want 2026-08-30T00:00:00Z", got)
	}

	if got := partial.Start.Format(time.RFC3339); got != "2026-08-30T00:00:00Z" {
		t.Errorf("the unfinished part starts at %s, want 2026-08-30T00:00:00Z", got)
	}

	past, err := DateRange{
		Preset: RangeCustom, DateOnly: true,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
	}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, split := SplitAtToday(past); split {
		t.Error("a range entirely in the past has nothing to split off")
	}
}

// TestAdditiveMetricsSurviveASplitRange checks that the seam actually works: a
// query answered from two segments has to produce the same number as one
// answered from one.
func TestAdditiveMetricsSurviveASplitRange(t *testing.T) {
	engine := newEngine(t)
	engine.Router = splittingRouter{}

	result := run(t, engine, baseQuery("pageviews", "visits"))

	closeTo(t, "pageviews across two segments", result.Results[0].Metrics[0], 7)

	if len(result.Meta.Sources) != 2 {
		t.Errorf("meta.sources = %v, want both sources named", result.Meta.Sources)
	}
}

// TestDistinctCountsAreNotSplit is the reason Splittable exists. Visitor A
// appears on both days, so adding the two halves would invent a fourth visitor
// who does not exist.
func TestDistinctCountsAreNotSplit(t *testing.T) {
	engine := newEngine(t)
	engine.Router = splittingRouter{}

	result := run(t, engine, baseQuery("visitors"))

	closeTo(t, "visitors across a split range", result.Results[0].Metrics[0], 3)

	if len(result.Meta.Sources) != 2 {
		t.Errorf("meta.sources = %v — the router still routed to both, even though the read collapsed", result.Meta.Sources)
	}
}

// TestSplittableKnowsWhichMetricsAddUp pins the rule itself.
func TestSplittableKnowsWhichMetricsAddUp(t *testing.T) {
	cases := map[string]bool{
		"pageviews":       true,
		"events":          true,
		"bounce_rate":     true,
		"visit_duration":  true,
		"views_per_visit": true,
		"visitors":        false,
		"time_on_page":    false,
		"scroll_depth":    false,
	}

	for name, want := range cases {
		q := Query{SiteIDs: []int64{1}, Metrics: []string{name}}
		q.Normalise()

		decided, err := decide(&q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if got := Splittable(&q, decided); got != want {
			t.Errorf("Splittable(%s) = %v, want %v", name, got, want)
		}
	}
}
