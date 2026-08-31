//
// traffic_test.go
// The week, the day, the spike and the gap are where they are supposed to be.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"testing"
	"time"
)

// TestAllocateSpendsExactlyTheBudget checks the arithmetic that turns one total
// into a number per day per site. Rounding two thousand cells independently
// loses or gains hundreds of pageviews, and the number printed at the end of a
// run has to be the number that was asked for.
func TestAllocateSpendsExactlyTheBudget(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 29).Add(23*time.Hour + 59*time.Minute)

	sites := []siteFixture{
		{Domain: "one.example", Kind: kindMarketing, Weight: 0.7},
		{Domain: "two.example", Kind: kindDocs, Weight: 0.3},
	}

	budget := allocate(1_000_000, 30, start, now, sites)

	var total, first, second int64

	for day := range budget {
		for site := range budget[day] {
			if budget[day][site] < 0 {
				t.Fatalf("day %d site %d was allocated %d pageviews", day, site, budget[day][site])
			}

			total += budget[day][site]
		}

		first += budget[day][0]
		second += budget[day][1]
	}

	if total != 1_000_000 {
		t.Errorf("allocated %d pageviews, want exactly 1000000", total)
	}

	// The weights are shares of the run, so the busier site has to end up with
	// roughly its share rather than merely more.
	if ratio := float64(first) / float64(first+second); ratio < 0.6 || ratio > 0.8 {
		t.Errorf("the first site took %.0f%% of the traffic, want about 70%%", ratio*100)
	}
}

// TestTheGapAndTheSpikeAreReal pins the two days a dataset needs and nobody
// generates by accident: one with no traffic at all, and one several times
// normal.
func TestTheGapAndTheSpikeAreReal(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := start.AddDate(0, 0, 30)

	sites := []siteFixture{{Domain: "one.example", Kind: kindMarketing, Weight: 1}}

	budget := allocate(600_000, 30, start, now, sites)

	gap := gapDay(30)
	if gap < 0 {
		t.Fatal("a thirty-day run has no gap day")
	}

	if budget[gap][0] != 0 {
		t.Errorf("the gap day was allocated %d pageviews, want none", budget[gap][0])
	}

	spike := spikeDay(30)
	if spike < 0 {
		t.Fatal("a thirty-day run has no spike day")
	}

	busiest, busiestDay := int64(0), -1
	for day := range budget {
		if budget[day][0] > busiest {
			busiest, busiestDay = budget[day][0], day
		}
	}

	if busiestDay != spike {
		t.Errorf("the busiest day is %d, want the spike on day %d", busiestDay, spike)
	}

	// A spike that is only slightly above a normal day is not a spike, and an
	// alert built against it would have nothing to fire on.
	var ordinary int64
	for day := range budget {
		if day != spike && day != gap {
			ordinary += budget[day][0]
		}
	}

	average := float64(ordinary) / float64(len(budget)-2)
	if float64(busiest)/average < 2.5 {
		t.Errorf("the spike is %.1fx an ordinary day, want at least 2.5x", float64(busiest)/average)
	}
}

// TestTheWeekHasAShape checks that a weekend is not a weekday, and that the two
// site kinds that should disagree about it do. Without it the hourly and daily
// roll-ups are tested against buckets no real site produces.
func TestTheWeekHasAShape(t *testing.T) {
	saturday := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	wednesday := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if saturday.Weekday() != time.Saturday || wednesday.Weekday() != time.Wednesday {
		t.Fatal("the fixture dates are not the days this test thinks they are")
	}

	docs := weekFactor(saturday, kindDocs) / weekFactor(wednesday, kindDocs)
	if docs > 0.5 {
		t.Errorf("a documentation site keeps %.0f%% of its traffic on a Saturday, want far less", docs*100)
	}

	blog := weekFactor(saturday, kindBlog) / weekFactor(wednesday, kindBlog)
	if blog < 0.8 {
		t.Errorf("a blog keeps %.0f%% of its traffic on a Saturday, want almost all of it", blog*100)
	}

	// The last day of a run is only as long as the day has been so far.
	morning := wednesday.Add(9 * time.Hour)
	if fraction := dayFraction(wednesday, morning, kindMarketing); fraction <= 0 || fraction > 0.45 {
		t.Errorf("nine in the morning is %.2f of the day's traffic, want a modest fraction", fraction)
	}

	if fraction := dayFraction(wednesday, wednesday.Add(48*time.Hour), kindMarketing); fraction != 1 {
		t.Errorf("a finished day is %.2f of itself, want 1", fraction)
	}
}
