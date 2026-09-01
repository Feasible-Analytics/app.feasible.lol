//
// schedule_test.go
// A year of hourly ticks, in a millisecond.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"testing"
	"time"
)

// sites is the fixture: one site per interesting timezone. Kathmandu is in
// there because it is UTC+5:45, and a scheduler that assumed whole-hour offsets
// works for everything else and fails only there.
func scheduledSites() []ScheduledSite {
	return []ScheduledSite{
		{SiteID: 1, Domain: "utc.example", Timezone: "Etc/UTC", Weekly: true, Monthly: true},
		{SiteID: 2, Domain: "nyc.example", Timezone: "America/New_York", Weekly: true, Monthly: true},
		{SiteID: 3, Domain: "tokyo.example", Timezone: "Asia/Tokyo", Weekly: true, Monthly: true},
		{SiteID: 4, Domain: "kathmandu.example", Timezone: "Asia/Kathmandu", Weekly: true, Monthly: true},
		{SiteID: 5, Domain: "auckland.example", Timezone: "Pacific/Auckland", Weekly: true, Monthly: true},
	}
}

// TestEachSiteIsDueExactlyOncePerLocalWeek is the acceptance criterion, and the
// whole reason scheduling is a worker rather than cron.
//
// A year of hourly ticks is run through the scheduler and every weekly report
// is counted. Fifty-two or fifty-three per site, no more and no fewer: a site
// that fires twice has sent a duplicate, and one that fires 51 times has
// silently lost a week — which is exactly what testing the local *hour* rather
// than the local *day* would do in a zone that springs forward at midnight.
func TestEachSiteIsDueExactlyOncePerLocalWeek(t *testing.T) {
	sites := scheduledSites()

	weekly := map[int64]map[string]int{}
	monthly := map[int64]map[string]int{}

	for _, site := range sites {
		weekly[site.SiteID] = map[string]int{}
		monthly[site.SiteID] = map[string]int{}
	}

	start := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	for hour := 0; hour < 366*24; hour++ {
		for _, due := range DueAt(start.Add(time.Duration(hour)*time.Hour), sites) {
			switch due.Kind {
			case KindWeekly:
				weekly[due.SiteID][due.PeriodKey]++
			case KindMonthly:
				monthly[due.SiteID][due.PeriodKey]++
			}
		}
	}

	for _, site := range sites {
		for period, count := range weekly[site.SiteID] {
			if count != 1 {
				t.Errorf("%s was due %d times for week %s", site.Domain, count, period)
			}
		}

		// A year is 52 or 53 ISO weeks depending on where the boundary falls;
		// the run starts on 1 January rather than on a Monday, and a site east
		// of UTC crosses one more local midnight inside the same UTC span.
		if weeks := len(weekly[site.SiteID]); weeks < 52 || weeks > 54 {
			t.Errorf("%s was due for %d distinct weeks in a year", site.Domain, weeks)
		}

		for period, count := range monthly[site.SiteID] {
			if count != 1 {
				t.Errorf("%s was due %d times for month %s", site.Domain, count, period)
			}
		}

		// Twelve monthly reports, or thirteen when the run's 366-hour-a-day span
		// reaches a second 1 January — which it does for a site whose offset
		// puts the next local New Year inside the last hours of the run.
		if months := len(monthly[site.SiteID]); months < 12 || months > 13 {
			t.Errorf("%s was due for %d months in a year, want 12 or 13", site.Domain, months)
		}
	}
}

// TestAQuarterHourOffsetStillFiresOnce pins Kathmandu specifically. At UTC+5:45
// the local midnight never lands on the top of a UTC hour, and an implementation
// that tested `local.Hour() == 0` would work by luck of the cadence and start
// silently missing days the moment the job ran on any other interval.
func TestAQuarterHourOffsetStillFiresOnce(t *testing.T) {
	site := ScheduledSite{SiteID: 4, Domain: "kathmandu.example", Timezone: "Asia/Kathmandu", Weekly: true}

	// Run the ticks at :50 past rather than :05, to prove the result does not
	// depend on where in the hour the job happens to run.
	start := time.Date(2026, 8, 1, 0, 50, 0, 0, time.UTC)

	fired := 0

	for hour := 0; hour < 7*24; hour++ {
		fired += len(DueAt(start.Add(time.Duration(hour)*time.Hour), []ScheduledSite{site}))
	}

	if fired != 1 {
		t.Fatalf("a UTC+5:45 site fired %d times in a week, want 1", fired)
	}
}

// TestADaylightSavingMidnightSkipDoesNotLoseTheReport is the case that testing
// the local hour gets wrong.
//
// Several zones spring forward at midnight, so on that day the local hour is
// never zero at all. Chile is one of them. Comparing the local *day* either side
// of an hour has no such gap.
func TestADaylightSavingMidnightSkipDoesNotLoseTheReport(t *testing.T) {
	location, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skipf("the tz database has no America/Santiago: %v", err)
	}

	site := ScheduledSite{SiteID: 9, Domain: "santiago.example", Timezone: "America/Santiago", Weekly: true, Monthly: true}

	// Walk a whole year of hourly ticks and confirm every local day boundary is
	// crossed exactly once, including the one the clock skips over.
	start := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	crossings := map[string]int{}

	for hour := 0; hour < 366*24; hour++ {
		at := start.Add(time.Duration(hour) * time.Hour)

		if crossedLocalMidnight(at, location) {
			crossings[at.In(location).Format("2006-01-02")]++
		}
	}

	for day, count := range crossings {
		if count != 1 {
			t.Errorf("%s was crossed %d times", day, count)
		}
	}

	if len(crossings) < 364 {
		t.Fatalf("only %d local days were crossed in a year — a day was lost", len(crossings))
	}

	// And the scheduler itself still produces a report for the week containing
	// the skipped midnight.
	fired := 0

	for hour := 0; hour < 14*24; hour++ {
		fired += len(DueAt(time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC).
			Add(time.Duration(hour)*time.Hour), []ScheduledSite{site}))
	}

	if fired < 2 {
		t.Fatalf("santiago fired %d times in a fortnight spanning the change", fired)
	}
}

// TestTheWindowIsTheCompletedLocalPeriod checks that a Monday report covers the
// week that just ended rather than the one that has just begun.
func TestTheWindowIsTheCompletedLocalPeriod(t *testing.T) {
	site := ScheduledSite{SiteID: 2, Domain: "nyc.example", Timezone: "America/New_York", Weekly: true}

	// 06:05 UTC on Monday 3 August 2026 is 02:05 local, which is the first hour
	// after New York's local midnight in daylight saving time.
	at := time.Date(2026, 8, 3, 4, 5, 0, 0, time.UTC)

	due := DueAt(at, []ScheduledSite{site})
	if len(due) != 1 {
		t.Fatalf("the site was due %d times at %s local", len(due), at.In(mustLocation(t, site.Timezone)))
	}

	report := due[0]

	if got := report.From.In(report.Location).Format("2006-01-02 15:04"); got != "2026-07-27 00:00" {
		t.Errorf("the window starts at %s, want the previous local Monday midnight", got)
	}

	if got := report.To.In(report.Location).Format("2006-01-02 15:04"); got != "2026-08-03 00:00" {
		t.Errorf("the window ends at %s, want this local Monday midnight", got)
	}

	if report.PeriodKey != "2026-W31" {
		t.Errorf("the period key is %q, want the ISO week that just finished", report.PeriodKey)
	}
}

// TestTheMonthlyWindowIsTheCompletedMonth checks the same for the 1st.
func TestTheMonthlyWindowIsTheCompletedMonth(t *testing.T) {
	site := ScheduledSite{SiteID: 3, Domain: "tokyo.example", Timezone: "Asia/Tokyo", Monthly: true}

	// 15:30 UTC on 31 August is 00:30 on 1 September in Tokyo.
	at := time.Date(2026, 8, 31, 15, 30, 0, 0, time.UTC)

	due := DueAt(at, []ScheduledSite{site})
	if len(due) != 1 {
		t.Fatalf("the site was due %d times", len(due))
	}

	if due[0].PeriodKey != "2026-08" {
		t.Errorf("the period key is %q, want 2026-08", due[0].PeriodKey)
	}

	if label := due[0].Label(); label != "August 2026" {
		t.Errorf("the label is %q, want August 2026", label)
	}
}

// TestASiteWithNoSubscriptionIsNeverDue checks the early exit, so an install
// with a thousand sites and two subscriptions does no date arithmetic for the
// other 998.
func TestASiteWithNoSubscriptionIsNeverDue(t *testing.T) {
	site := ScheduledSite{SiteID: 1, Domain: "quiet.example", Timezone: "Etc/UTC"}

	start := time.Date(2026, 8, 1, 0, 5, 0, 0, time.UTC)

	for hour := 0; hour < 40*24; hour++ {
		if due := DueAt(start.Add(time.Duration(hour)*time.Hour), []ScheduledSite{site}); len(due) != 0 {
			t.Fatalf("an unsubscribed site was due: %+v", due)
		}
	}
}

// TestAnUnloadableTimezoneIsSkippedNotDefaulted checks that a site with a bad
// zone is skipped rather than silently sent at UTC's Monday.
//
// Sending at the wrong local time is a small permanent annoyance nobody
// reports; a missing report is something somebody does.
func TestAnUnloadableTimezoneIsSkippedNotDefaulted(t *testing.T) {
	site := ScheduledSite{SiteID: 1, Domain: "broken.example", Timezone: "Mars/Olympus", Weekly: true}

	start := time.Date(2026, 8, 1, 0, 5, 0, 0, time.UTC)

	for hour := 0; hour < 14*24; hour++ {
		if due := DueAt(start.Add(time.Duration(hour)*time.Hour), []ScheduledSite{site}); len(due) != 0 {
			t.Fatalf("a site with an unloadable timezone was due: %+v", due)
		}
	}
}

// TestSitesInDifferentZonesFireAtDifferentInstants is the property cron cannot
// express, stated directly.
func TestSitesInDifferentZonesFireAtDifferentInstants(t *testing.T) {
	sites := scheduledSites()

	firstFire := map[int64]time.Time{}
	start := time.Date(2026, 8, 1, 0, 5, 0, 0, time.UTC)

	for hour := 0; hour < 14*24; hour++ {
		at := start.Add(time.Duration(hour) * time.Hour)

		for _, due := range DueAt(at, sites) {
			if due.Kind != KindWeekly {
				continue
			}

			if _, seen := firstFire[due.SiteID]; !seen {
				firstFire[due.SiteID] = at
			}
		}
	}

	if len(firstFire) != len(sites) {
		t.Fatalf("only %d of %d sites ever fired", len(firstFire), len(sites))
	}

	distinct := map[time.Time]bool{}
	for _, at := range firstFire {
		distinct[at] = true
	}

	if len(distinct) < 4 {
		t.Fatalf("five sites across five zones fired at only %d distinct instants — "+
			"the scheduler is not timezone-aware", len(distinct))
	}
}

// TestWeeklyLabelReadsAsARange checks the subject-line wording.
func TestWeeklyLabelReadsAsARange(t *testing.T) {
	location := mustLocation(t, "Etc/UTC")

	due := Due{
		Kind:     KindWeekly,
		From:     time.Date(2026, 7, 27, 0, 0, 0, 0, location),
		To:       time.Date(2026, 8, 3, 0, 0, 0, 0, location),
		Location: location,
	}

	if got := due.Label(); got != "27 July – 2 August 2026" {
		t.Fatalf("the weekly label is %q", got)
	}
}

// mustLocation loads a zone or fails the test.
func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()

	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}

	return location
}
