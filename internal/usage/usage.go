//
// usage.go
// What we bill for, what counts towards it, and where the ladder's rungs are.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package usage counts the billable volume of an account and turns the plan's
// limit into a sales conversation rather than a wall.
//
// The rules that matter are in one place because getting any of them wrong
// changes somebody's bill:
//
//   - The billable unit is a pageview or a custom event, summed across every
//     site on the account, per calendar month in UTC.
//   - Engagement events do not count. They are the tracker's own heartbeat —
//     time on page and scroll depth — and billing for them would charge people
//     for a feature they never asked for and cannot turn off.
//   - Pageview goals do not count. A goal is a saved query over pageviews that
//     already counted; counting the goal too would bill the same event twice.
//   - Going over is not a payment failure and never touches the deletion clock.
package usage

import (
	"fmt"
	"time"
)

// MonthlyLimit is the plan's included volume. It is a hard limit in the sense
// that it is what the price buys, and a soft one in the sense that exceeding it
// never drops a customer's data — we would rather have the conversation.
const MonthlyLimit = 1_000_000

// The rungs of the ladder, as absolute counts rather than percentages so the
// number in the email and the number in the comparison are the same value.
//
// The 70% rung is the important one. It reaches somebody while they still have
// a month of runway, which is the difference between "let us talk about a plan
// that fits" and "your dashboard is locked".
const (
	WarnThreshold    = 700_000
	NearThreshold    = 850_000
	ReachedThreshold = MonthlyLimit
)

// ReplyWindow is how long an account has to answer the second-month email
// before the dashboard locks. Two weeks, named explicitly in the message, and
// nothing happens before it elapses.
const ReplyWindow = 14 * 24 * time.Hour

// Counts is one account's billable volume for one period, split by what
// produced it. The split is kept rather than a single total so a customer
// asking "why is my number that" gets an answer.
type Counts struct {
	Pageviews    int64
	CustomEvents int64
}

// Billable is the number the limit applies to.
func (c Counts) Billable() int64 {
	return c.Pageviews + c.CustomEvents
}

// Add merges another set of counts in, which is what the in-memory recorder
// does between flushes.
func (c *Counts) Add(other Counts) {
	c.Pageviews += other.Pageviews
	c.CustomEvents += other.CustomEvents
}

// Period is the calendar month a timestamp belongs to, as 'YYYY-MM' in UTC. It
// is UTC rather than the site's timezone on purpose: an account can hold sites
// in a dozen timezones, and a bill has to be one number rather than a dozen
// overlapping months.
func Period(at time.Time) string {
	return at.UTC().Format("2006-01")
}

// PreviousPeriod is the month before a given one. The two-consecutive-months
// rule needs it, and doing the arithmetic on a parsed time rather than on the
// string is what makes January work.
func PreviousPeriod(period string) (string, error) {
	at, err := time.Parse("2006-01", period)
	if err != nil {
		return "", fmt.Errorf("usage: %q is not a period: %w", period, err)
	}

	return Period(at.AddDate(0, -1, 0)), nil
}

// Level is how far up the ladder an account has climbed this month.
type Level string

// The levels. The empty value means below every rung, so the zero value of a
// Level is "nothing to say", which is the common case.
const (
	LevelOK      Level = ""
	LevelWarn    Level = "warn"
	LevelNear    Level = "near"
	LevelReached Level = "reached"
)

// LevelFor maps a billable count onto the highest rung it has passed. It is a
// pure function so the in-app meter, the email job and the tests all agree
// about where the boundaries are.
func LevelFor(billable int64) Level {
	switch {
	case billable >= ReachedThreshold:
		return LevelReached
	case billable >= NearThreshold:
		return LevelNear
	case billable >= WarnThreshold:
		return LevelWarn
	default:
		return LevelOK
	}
}

// Reached lists every rung a count has passed, lowest first. A month that goes
// from nothing to a million between two sweeps still owes the customer all
// three emails — skipping to the last one would mean the 70% conversation, the
// one that is actually useful, never happens.
func Reached(billable int64) []Level {
	var levels []Level

	for _, candidate := range []struct {
		level     Level
		threshold int64
	}{
		{LevelWarn, WarnThreshold},
		{LevelNear, NearThreshold},
		{LevelReached, ReachedThreshold},
	} {
		if billable >= candidate.threshold {
			levels = append(levels, candidate.level)
		}
	}

	return levels
}

// Projection estimates where a month ends up, given how far into it we are. The
// 85% email quotes it, because "you are at 850,000 on the 20th" means much less
// than "at this rate you finish the month around 1.3 million".
//
// It returns zero for a month that has barely started, since a projection from
// two hours of data is noise dressed up as a number.
func Projection(billable int64, now time.Time) int64 {
	// The month is read in UTC because that is the month being billed. Taking
	// the calendar fields from whatever zone the caller's clock carries would
	// pick a different month from the one the elapsed time is measured against,
	// and near a month boundary the two disagree.
	now = now.UTC()

	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := start.AddDate(0, 1, 0)

	elapsed := now.Sub(start)
	total := next.Sub(start)

	if elapsed < 24*time.Hour || elapsed >= total {
		return 0
	}

	return int64(float64(billable) * (float64(total) / float64(elapsed)))
}

// Percent is the share of the limit used, for the in-app meter. It is capped at
// nothing: a customer at 140% should see 140%, because rounding their overage
// down to a full bar is the kind of small dishonesty that costs trust.
func Percent(billable int64) int {
	return int(float64(billable) / float64(MonthlyLimit) * 100)
}
