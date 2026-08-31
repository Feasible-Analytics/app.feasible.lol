//
// usage_test.go
// What counts, what does not, and where the rungs are.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import (
	"testing"
	"time"
)

// TestBillableIsPageviewsPlusCustomEvents is the definition the whole product
// bills on. Engagement pings are excluded at the point they are counted, in the
// shard writer, so they never reach these structs at all.
func TestBillableIsPageviewsPlusCustomEvents(t *testing.T) {
	counts := Counts{Pageviews: 900_000, CustomEvents: 150_000}

	if got := counts.Billable(); got != 1_050_000 {
		t.Fatalf("billable is %d, want 1,050,000", got)
	}
}

// TestCountsAdd covers the in-memory accumulation the recorder does between
// flushes.
func TestCountsAdd(t *testing.T) {
	total := Counts{Pageviews: 10, CustomEvents: 2}
	total.Add(Counts{Pageviews: 5, CustomEvents: 3})

	if total.Pageviews != 15 || total.CustomEvents != 5 {
		t.Fatalf("adding gave %+v", total)
	}
}

// TestPeriodIsTheUTCCalendarMonth pins the billing boundary. It is UTC rather
// than the site's timezone because an account can hold sites in a dozen
// timezones and a bill has to be one number, not a dozen overlapping months.
func TestPeriodIsTheUTCCalendarMonth(t *testing.T) {
	cases := map[time.Time]string{
		time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC):      "2026-03",
		time.Date(2026, time.March, 31, 23, 59, 59, 0, time.UTC):  "2026-03",
		time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC):      "2026-04",
		time.Date(2026, time.December, 31, 22, 0, 0, 0, time.UTC): "2026-12",
	}

	for at, want := range cases {
		if got := Period(at); got != want {
			t.Errorf("%s is %q, want %q", at, got, want)
		}
	}

	// A timestamp in a non-UTC zone still lands in its UTC month.
	tokyo := time.FixedZone("JST", 9*3600)
	newYearInTokyo := time.Date(2027, time.January, 1, 6, 0, 0, 0, tokyo)

	if got := Period(newYearInTokyo); got != "2026-12" {
		t.Errorf("6am on 1 January in Tokyo is %q, want 2026-12", got)
	}
}

// TestPreviousPeriodCrossesTheYear is the arithmetic the consecutive-months rule
// depends on. Doing it on the string rather than on a parsed time is how January
// breaks.
func TestPreviousPeriodCrossesTheYear(t *testing.T) {
	cases := map[string]string{
		"2026-03": "2026-02",
		"2026-01": "2025-12",
		"2026-12": "2026-11",
	}

	for input, want := range cases {
		got, err := PreviousPeriod(input)
		if err != nil {
			t.Fatal(err)
		}

		if got != want {
			t.Errorf("the month before %s is %q, want %q", input, got, want)
		}
	}

	if _, err := PreviousPeriod("not-a-month"); err == nil {
		t.Error("a malformed period was accepted")
	}
}

// TestLevelFor pins the three rungs at the counts the emails quote.
func TestLevelFor(t *testing.T) {
	cases := []struct {
		billable int64
		want     Level
	}{
		{0, LevelOK},
		{699_999, LevelOK},
		{700_000, LevelWarn},
		{849_999, LevelWarn},
		{850_000, LevelNear},
		{999_999, LevelNear},
		{1_000_000, LevelReached},
		{4_000_000, LevelReached},
	}

	for _, tc := range cases {
		if got := LevelFor(tc.billable); got != tc.want {
			t.Errorf("%d is %q, want %q", tc.billable, got, tc.want)
		}
	}
}

// TestReachedListsEveryRungPassed is what a sweep owes an account that went from
// nothing to a million between two runs. Skipping to the highest would mean the
// 70% conversation — the one that is actually useful — never happens.
func TestReachedListsEveryRungPassed(t *testing.T) {
	cases := map[int64]int{
		500_000:   0,
		700_000:   1,
		850_000:   2,
		1_000_000: 3,
		9_000_000: 3,
	}

	for billable, want := range cases {
		if got := len(Reached(billable)); got != want {
			t.Errorf("%d passed %d rungs, want %d", billable, got, want)
		}
	}

	// And in order, lowest first.
	levels := Reached(1_000_000)
	if levels[0] != LevelWarn || levels[1] != LevelNear || levels[2] != LevelReached {
		t.Errorf("the rungs came back as %v", levels)
	}
}

// TestProjectionRefusesToGuessTooEarly is why the 85% email sometimes says
// nothing about the month end. A projection from two hours of data is noise, and
// printing it as a number would make it look like a fact.
func TestProjectionRefusesToGuessTooEarly(t *testing.T) {
	tooEarly := time.Date(2026, time.March, 1, 6, 0, 0, 0, time.UTC)

	if got := Projection(50_000, tooEarly); got != 0 {
		t.Errorf("a projection from six hours gave %d, want 0", got)
	}
}

// TestProjectionScalesToTheMonth checks the arithmetic the 85% email quotes.
func TestProjectionScalesToTheMonth(t *testing.T) {
	// Halfway through a 31-day month.
	halfway := time.Date(2026, time.March, 16, 12, 0, 0, 0, time.UTC)

	got := Projection(500_000, halfway)

	if got < 950_000 || got > 1_050_000 {
		t.Errorf("500,000 halfway through March projects to %d, want about 1,000,000", got)
	}
}

// TestPercentIsNotCapped is a small piece of honesty. A customer at 140% should
// see 140%: rounding an overage down to a full bar is the kind of quiet
// inaccuracy that costs trust.
func TestPercentIsNotCapped(t *testing.T) {
	if got := Percent(1_400_000); got != 140 {
		t.Fatalf("1,400,000 is %d%%, want 140", got)
	}
	if got := Percent(700_000); got != 70 {
		t.Fatalf("700,000 is %d%%, want 70", got)
	}
}
