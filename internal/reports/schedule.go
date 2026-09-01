//
// schedule.go
// "Which sites just crossed their local Monday 00:00?" — the only sane way to schedule this.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package reports sends the numbers to people who will not open the dashboard:
// a weekly and a monthly summary per site, and spike and drop alerts when
// something changes fast enough to be worth an interruption.
//
// Scheduling is done by a worker, not by cron, and this file is why.
//
// A weekly report is due at the *site's* local Monday 00:00. Sites in one
// install span every timezone there is, so there is no single instant at which
// "it is Monday" is true. Cron can only fire at instants; expressing "Monday
// morning for each of nine hundred sites in forty zones" as crontab entries
// means one entry per zone, kept in step with a customer-editable setting, and
// silently wrong the moment somebody changes a site's timezone.
//
// So the job runs hourly and asks each site a question instead: has your local
// clock crossed midnight in the last hour, and is it now Monday? That is a
// property of the site, evaluated at run time, and it is correct for every
// offset — including the quarter-hour ones — and across daylight saving.
package reports

import (
	"fmt"
	"time"
)

// The kinds of scheduled report. They are the same strings the schema's CHECK
// constraint holds, so a typo fails at the database rather than producing a
// subscription nothing ever matches.
const (
	KindWeekly  = "weekly"
	KindMonthly = "monthly"
)

// ScheduledSite is what the scheduler needs to know about one site. It is a
// struct of its own rather than the sites package's type because scheduling
// cares about three fields and nothing else, and a scheduler that took the
// routing type would need a routing table to be tested.
type ScheduledSite struct {
	SiteID   int64
	Domain   string
	Timezone string

	// Weekly and Monthly are whether a subscription of each kind exists and is
	// enabled. A site with neither is skipped before any date arithmetic runs.
	Weekly  bool
	Monthly bool
}

// Due is one report that should be sent now.
type Due struct {
	SiteID int64
	Domain string
	Kind   string

	// PeriodKey names the completed period this report covers — "2026-W35" or
	// "2026-08" — in the site's own local calendar. It is the idempotency key:
	// the delivery ledger has a unique index on it, so two processes both
	// deciding that Monday has arrived produce one email rather than two.
	PeriodKey string

	// From and To bound the period as instants. They are the site's local
	// midnights, which is what makes the totals in the email agree with the
	// totals somebody sees on the dashboard when they click through.
	From time.Time
	To   time.Time

	// Location is the site's zone, carried so the renderer can label the dates
	// without loading the zone a second time.
	Location *time.Location
}

// Label renders the period the way it is written in a subject line.
func (d Due) Label() string {
	if d.Kind == KindMonthly {
		return d.From.In(d.Location).Format("January 2006")
	}

	from := d.From.In(d.Location)
	to := d.To.In(d.Location).AddDate(0, 0, -1)

	if from.Year() == to.Year() {
		return from.Format("2 January") + " – " + to.Format("2 January 2006")
	}

	return from.Format("2 January 2006") + " – " + to.Format("2 January 2006")
}

// DueAt returns every report that should be sent at this instant.
//
// It is a pure function over the site list and the clock. That is deliberate:
// the hardest thing about scheduled email is being sure it fires once and at
// the right local time, and the only way to be sure is to be able to run a
// year of hourly ticks through it in a test in a millisecond.
//
// A site whose timezone cannot be loaded is skipped rather than defaulted to
// UTC. Sending somebody's weekly report at the wrong local time is a small,
// permanent annoyance nobody reports; skipping it produces a missing report,
// which somebody does.
func DueAt(now time.Time, sites []ScheduledSite) []Due {
	var due []Due

	for _, site := range sites {
		if !site.Weekly && !site.Monthly {
			continue
		}

		location, err := time.LoadLocation(site.Timezone)
		if err != nil {
			continue
		}

		if !crossedLocalMidnight(now, location) {
			continue
		}

		local := now.In(location)
		midnight := startOfLocalDay(local)

		if site.Weekly && local.Weekday() == time.Monday {
			from := midnight.AddDate(0, 0, -7)

			due = append(due, Due{
				SiteID:    site.SiteID,
				Domain:    site.Domain,
				Kind:      KindWeekly,
				PeriodKey: weekKey(from),
				From:      from,
				To:        midnight,
				Location:  location,
			})
		}

		if site.Monthly && local.Day() == 1 {
			from := midnight.AddDate(0, -1, 0)

			due = append(due, Due{
				SiteID:    site.SiteID,
				Domain:    site.Domain,
				Kind:      KindMonthly,
				PeriodKey: from.Format("2006-01"),
				From:      from,
				To:        midnight,
				Location:  location,
			})
		}
	}

	return due
}

// crossedLocalMidnight reports whether a site's local day changed in the hour
// leading up to now.
//
// This is the whole trick, and it is written as a day comparison rather than as
// `local.Hour() == 0` for two reasons.
//
// Quarter-hour offsets. In a zone at UTC+5:45 the local hour is zero for a
// window that does not line up with the top of a UTC hour, so an hourly job
// testing the hour still works — but only by luck of the cadence, and a job
// that later ran every ninety minutes would silently start missing days.
//
// Daylight saving. Several zones spring forward at midnight, so on one day a
// year the local hour is never zero at all. Testing the hour loses that week's
// report for every site in those zones, once a year, with nothing reporting it.
//
// Comparing the day either side of an hour has neither failure: a day boundary
// is crossed exactly once per local day whatever the offset does.
func crossedLocalMidnight(now time.Time, location *time.Location) bool {
	local := now.In(location)
	previous := now.Add(-time.Hour).In(location)

	return local.Year() != previous.Year() || local.YearDay() != previous.YearDay()
}

// startOfLocalDay is the site's local midnight for the day now falls in.
func startOfLocalDay(local time.Time) time.Time {
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
}

// weekKey names an ISO week. ISO rather than "the week containing 1 January" so
// that the week spanning a new year has one key rather than two, which is what
// keeps the delivery ledger's uniqueness meaningful over a year boundary.
func weekKey(start time.Time) string {
	year, week := start.ISOWeek()

	return fmt.Sprintf("%04d-W%02d", year, week)
}
