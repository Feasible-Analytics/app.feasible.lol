//
// daterange_test.go
// Date presets, local bucketing, and comparing like with like.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"encoding/json"
	"testing"
	"time"
)

// losAngeles is the fixture timezone for the tests that care about a day
// boundary being somewhere other than UTC midnight.
func losAngeles(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no timezone database on this machine: %v", err)
	}

	return loc
}

// TestPresetsResolveToLocalBoundaries checks every preset against a fixed
// clock. A preset that is a day out is a whole day of traffic in the wrong
// bucket, and nothing downstream can detect it.
func TestPresetsResolveToLocalBoundaries(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		preset string
		start  string
		end    string
	}{
		{RangeDay, "2026-08-30T00:00:00Z", "2026-08-31T00:00:00Z"},
		{RangeLast7Days, "2026-08-24T00:00:00Z", "2026-08-31T00:00:00Z"},
		{RangeLast28Days, "2026-08-03T00:00:00Z", "2026-08-31T00:00:00Z"},
		{RangeLast91Days, "2026-06-01T00:00:00Z", "2026-08-31T00:00:00Z"},
		{RangeMonth, "2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z"},
		{RangeLastMonth, "2026-07-01T00:00:00Z", "2026-08-01T00:00:00Z"},
		{RangeYear, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{RangeLast12Months, "2025-09-01T00:00:00Z", "2026-09-01T00:00:00Z"},
		{RangeRealtime, "2026-08-30T11:30:00Z", "2026-08-30T12:00:01Z"},
		{RangeLast24Hours, "2026-08-29T12:00:00Z", "2026-08-30T12:00:01Z"},
	}

	for _, tc := range cases {
		t.Run(tc.preset, func(t *testing.T) {
			resolved, err := DateRange{Preset: tc.preset}.Resolve(now, time.UTC, time.Time{})
			if err != nil {
				t.Fatal(err)
			}

			if got := resolved.Start.Format(time.RFC3339); got != tc.start {
				t.Errorf("start = %s, want %s", got, tc.start)
			}

			if got := resolved.End.Format(time.RFC3339); got != tc.end {
				t.Errorf("end = %s, want %s", got, tc.end)
			}
		})
	}
}

// TestCustomDateOnlyRangeCoversTheWholeFinalDay checks the inclusive end. A
// picker that says "1 to 1 August" means one day, not zero seconds.
func TestCustomDateOnlyRangeCoversTheWholeFinalDay(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	rng := DateRange{
		Preset:   RangeCustom,
		DateOnly: true,
		Start:    time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}

	resolved, err := rng.Resolve(now, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if resolved.End.Sub(resolved.Start) != 24*time.Hour {
		t.Fatalf("a one-day custom range covers %s, want 24h", resolved.End.Sub(resolved.Start))
	}
}

// TestPresetsUseTheSiteTimezone checks that a day boundary follows the site
// rather than the server.
func TestPresetsUseTheSiteTimezone(t *testing.T) {
	loc := losAngeles(t)

	// 03:00 UTC on the 30th is 20:00 on the 29th in Los Angeles, so "today" is
	// still the 29th there.
	now := time.Date(2026, 8, 30, 3, 0, 0, 0, time.UTC)

	resolved, err := DateRange{Preset: RangeDay}.Resolve(now, loc, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if got := resolved.Start.In(loc).Format(dateLayout); got != "2026-08-29" {
		t.Fatalf("today in Los Angeles started on %s, want 2026-08-29", got)
	}
}

// TestComparisonTruncatesToTheElapsedTime is the like-for-like rule. Comparing
// a partial period against a whole one always shows a collapse that is not
// there.
func TestComparisonTruncatesToTheElapsedTime(t *testing.T) {
	now := time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC)

	resolved, err := DateRange{Preset: RangeDay}.Resolve(now, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	comparison, err := resolved.Compare(&Comparison{Mode: ComparePreviousPeriod})
	if err != nil {
		t.Fatal(err)
	}

	if got := comparison.Start.Format(time.RFC3339); got != "2026-08-29T00:00:00Z" {
		t.Errorf("comparison starts at %s, want 2026-08-29T00:00:00Z", got)
	}

	if got := comparison.End.Format(time.RFC3339); got != "2026-08-29T16:00:00Z" {
		t.Errorf("comparison ends at %s, want 2026-08-29T16:00:00Z — sixteen hours against sixteen", got)
	}
}

// TestComparisonOfAFinishedPeriodUsesItsWholeLength checks the other half: a
// period that has ended is compared against a whole one.
func TestComparisonOfAFinishedPeriodUsesItsWholeLength(t *testing.T) {
	now := time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC)

	resolved, err := DateRange{Preset: RangeLastMonth}.Resolve(now, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	comparison, err := resolved.Compare(&Comparison{Mode: ComparePreviousPeriod})
	if err != nil {
		t.Fatal(err)
	}

	if got := comparison.Start.Format(time.RFC3339); got != "2026-06-01T00:00:00Z" {
		t.Errorf("comparison starts at %s, want 2026-06-01T00:00:00Z", got)
	}

	if got := comparison.End.Format(time.RFC3339); got != "2026-07-01T00:00:00Z" {
		t.Errorf("comparison ends at %s, want 2026-07-01T00:00:00Z", got)
	}
}

// TestYearOverYearShiftsTheCalendar checks the second comparison mode.
func TestYearOverYearShiftsTheCalendar(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	resolved, err := DateRange{Preset: RangeDay}.Resolve(now, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	comparison, err := resolved.Compare(&Comparison{Mode: CompareYearOverYear})
	if err != nil {
		t.Fatal(err)
	}

	if got := comparison.Start.Format(time.RFC3339); got != "2025-08-30T00:00:00Z" {
		t.Errorf("comparison starts at %s, want 2025-08-30T00:00:00Z", got)
	}
}

// TestLabelsCoverEveryBucketIncludingEmptyOnes checks the gap list a graph
// draws its axis from.
func TestLabelsCoverEveryBucketIncludingEmptyOnes(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	resolved, err := DateRange{Preset: RangeLast7Days}.Resolve(now, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	labels := resolved.Labels()
	if len(labels) != 7 {
		t.Fatalf("got %d labels, want 7", len(labels))
	}

	if labels[0] != "2026-08-24" || labels[6] != "2026-08-30" {
		t.Fatalf("labels run %s..%s", labels[0], labels[6])
	}

	index := resolved.PresentIndex()
	if index == nil || *index != 6 {
		t.Fatalf("present index = %v, want 6", index)
	}
}

// TestWeekBucketsStartOnMonday pins the bucket label the SQL has to reproduce
// character for character.
func TestWeekBucketsStartOnMonday(t *testing.T) {
	// 2026-08-30 is a Sunday, so its week started on the 24th.
	sunday := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	if got := bucketLabel(bucketStart(sunday, IntervalWeek, time.UTC), IntervalWeek); got != "2026-08-24" {
		t.Errorf("the week containing Sunday 30 August starts on %s, want 2026-08-24", got)
	}

	monday := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if got := bucketLabel(bucketStart(monday, IntervalWeek, time.UTC), IntervalWeek); got != "2026-08-24" {
		t.Errorf("a Monday should start its own week, got %s", got)
	}
}

// TestZoneOffsetsFindDaylightSavingTransitions checks that a range spanning a
// clock change compiles to two offsets rather than one. One offset for the
// whole range puts every event on one side of the change into the wrong local
// day.
func TestZoneOffsetsFindDaylightSavingTransitions(t *testing.T) {
	loc := losAngeles(t)

	from := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)

	spans := zoneOffsets(loc, from, to)
	if len(spans) != 2 {
		t.Fatalf("got %d offset spans across a clock change, want 2", len(spans))
	}

	if spans[0].Offset != -7*3600 {
		t.Errorf("first offset = %d, want -25200 (PDT)", spans[0].Offset)
	}

	if spans[1].Offset != -8*3600 {
		t.Errorf("second offset = %d, want -28800 (PST)", spans[1].Offset)
	}

	// Daylight saving ends at 02:00 local, which is 09:00 UTC.
	transition := time.Unix(spans[0].Until, 0).UTC()
	if got := transition.Format(time.RFC3339); got != "2026-11-01T09:00:00Z" {
		t.Errorf("transition at %s, want 2026-11-01T09:00:00Z", got)
	}
}

// TestDateRangeJSONRoundTrip checks both wire forms.
func TestDateRangeJSONRoundTrip(t *testing.T) {
	var preset DateRange
	if err := json.Unmarshal([]byte(`"7d"`), &preset); err != nil {
		t.Fatal(err)
	}

	if preset.Preset != RangeLast7Days {
		t.Errorf("preset = %q", preset.Preset)
	}

	var custom DateRange
	if err := json.Unmarshal([]byte(`["2026-08-01","2026-08-31"]`), &custom); err != nil {
		t.Fatal(err)
	}

	if custom.Preset != RangeCustom || !custom.DateOnly {
		t.Fatalf("custom range parsed as %+v", custom)
	}

	encoded, err := json.Marshal(custom)
	if err != nil {
		t.Fatal(err)
	}

	if string(encoded) != `["2026-08-01","2026-08-31"]` {
		t.Errorf("re-encoded as %s", encoded)
	}
}

// TestBadDateRangeIsACallerError checks that nonsense comes back as a message
// rather than as a panic or an empty result.
func TestBadDateRangeIsACallerError(t *testing.T) {
	var rng DateRange
	if err := json.Unmarshal([]byte(`["yesterday","today"]`), &rng); err == nil {
		t.Fatal("a non-date bound must be refused")
	}

	if err := (DateRange{Preset: "fortnight"}).validate(); err == nil {
		t.Fatal("an unknown preset must be refused")
	}
}
