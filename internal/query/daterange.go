//
// daterange.go
// Turning a date range and an IANA timezone into absolute bounds and buckets.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"encoding/json"
	"fmt"
	"time"
)

// The date range presets. They are the ones a dashboard offers, and they are
// resolved server-side rather than by the client so that "the last 7 days"
// means the same thing to the graph, the table and an export taken a second
// apart.
const (
	RangeDay          = "day"
	RangeLast7Days    = "7d"
	RangeLast28Days   = "28d"
	RangeLast91Days   = "91d"
	RangeMonth        = "month"
	RangeLastMonth    = "last_month"
	RangeYear         = "year"
	RangeLast12Months = "12mo"
	RangeAll          = "all"
	RangeCustom       = "custom"
	RangeRealtime     = "realtime"
	RangeLast5Minutes = "5m"
	RangeLast24Hours  = "24h"
)

// RealtimeWindow is how far back "realtime" reaches. Thirty minutes is the
// session timeout, so it is exactly the window in which a visitor still counts
// as being on the site.
const RealtimeWindow = 30 * time.Minute

// CurrentWindow is how far back "who is on the site right now" reaches. It is a
// server-resolved preset rather than a pair of bounds the client computes,
// because a realtime screen shows this figure beside a thirty-minute graph and
// the two have to be cut by the same clock: a rate built from one window's
// numerator and another's denominator is how a conversion rate comes out over
// 100%.
const CurrentWindow = 5 * time.Minute

// dateLayout and dateTimeLayout are the two forms a custom bound may take. A
// bare date is read as local midnight in the site's timezone, which is what
// somebody typing "2026-08-01" into a date picker means.
const (
	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02 15:04:05"
)

// DateRange is a preset name or an explicit pair of bounds. The bounds are held
// as wall-clock times with no location, because the location belongs to the
// site and is not known until the query is resolved — reading "2026-08-01" as
// UTC and then converting would move the boundary by the site's offset and
// silently shift a whole day of traffic.
type DateRange struct {
	Preset string

	// Start and End are the custom bounds as written. End is inclusive on the
	// wire — a date picker's "to 31 August" means through the end of the 31st —
	// and is turned into an exclusive bound during resolution.
	Start time.Time
	End   time.Time

	// DateOnly records that the bounds were written as bare dates, so the
	// inclusive end covers the whole final day rather than one instant of it.
	DateOnly bool
}

// Resolved is a date range with every ambiguity removed: absolute bounds, the
// timezone they were computed in, the bucket width, and the clock that resolved
// them. It is what the SQL is built from and what the response echoes back.
type Resolved struct {
	Preset   string
	Location *time.Location

	// Start is inclusive and End is exclusive. Half-open is the only bound
	// pairing that composes: with an inclusive end, adjacent ranges either
	// overlap by one second or leave a gap, and both are wrong.
	Start time.Time
	End   time.Time

	// Now is the clock this was resolved against. It is kept because "is this
	// bucket still in progress" and "how much of this period has elapsed" are
	// both questions about the same instant, and re-reading the clock later
	// would answer them about a different one.
	Now time.Time

	// Interval is the bucket width: minute, hour, day, week or month.
	Interval string
}

// validate checks the range before anything is resolved.
func (d DateRange) validate() error {
	switch d.Preset {
	case RangeDay, RangeLast7Days, RangeLast28Days, RangeLast91Days,
		RangeMonth, RangeLastMonth, RangeYear, RangeLast12Months,
		RangeAll, RangeRealtime, RangeLast5Minutes, RangeLast24Hours:
		return nil

	case RangeCustom:
		if d.Start.IsZero() || d.End.IsZero() {
			return invalid("a custom date range needs both a start and an end")
		}
		if d.End.Before(d.Start) {
			return invalid("the date range ends before it starts")
		}
		return nil

	default:
		return invalid("unknown date_range %q — use day, 7d, 28d, 91d, month, last_month, year, 12mo, all, realtime, 5m, 24h, or a pair of dates", d.Preset)
	}
}

// NeedsEarliest reports whether resolving this range requires knowing when the
// site's first event was. Only "all" does, and asking is what keeps every other
// query from paying for a MIN() over the whole table.
func (d DateRange) NeedsEarliest() bool {
	return d.Preset == RangeAll
}

// Resolve turns the range into absolute bounds in the site's timezone. The
// earliest argument is the site's first event and is only read for "all"; a
// zero value there means the site has no data yet, which resolves to today
// rather than to the beginning of the epoch.
func (d DateRange) Resolve(now time.Time, loc *time.Location, earliest time.Time) (Resolved, error) {
	if loc == nil {
		loc = time.UTC
	}

	now = now.In(loc)
	resolved := Resolved{Preset: d.Preset, Location: loc, Now: now}

	today := startOfDay(now, loc)
	tomorrow := today.AddDate(0, 0, 1)

	switch d.Preset {
	case RangeDay:
		resolved.Start, resolved.End = today, tomorrow

	case RangeLast7Days:
		resolved.Start, resolved.End = today.AddDate(0, 0, -6), tomorrow

	case RangeLast28Days:
		resolved.Start, resolved.End = today.AddDate(0, 0, -27), tomorrow

	case RangeLast91Days:
		resolved.Start, resolved.End = today.AddDate(0, 0, -90), tomorrow

	case RangeMonth:
		resolved.Start = startOfMonth(now, loc)
		resolved.End = resolved.Start.AddDate(0, 1, 0)

	case RangeLastMonth:
		resolved.End = startOfMonth(now, loc)
		resolved.Start = resolved.End.AddDate(0, -1, 0)

	case RangeYear:
		resolved.Start = time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
		resolved.End = resolved.Start.AddDate(1, 0, 0)

	case RangeLast12Months:
		resolved.Start = startOfMonth(now, loc).AddDate(0, -11, 0)
		resolved.End = startOfMonth(now, loc).AddDate(0, 1, 0)

	case RangeAll:
		resolved.Start = today
		if !earliest.IsZero() {
			resolved.Start = startOfDay(earliest.In(loc), loc)
		}
		resolved.End = tomorrow

	case RangeRealtime:
		// Truncated to the second and pushed one second past now, so the event
		// that arrived this instant is inside the range rather than one tick
		// outside it.
		resolved.Start = now.Add(-RealtimeWindow).Truncate(time.Second)
		resolved.End = now.Truncate(time.Second).Add(time.Second)

	case RangeLast5Minutes:
		resolved.Start = now.Add(-CurrentWindow).Truncate(time.Second)
		resolved.End = now.Truncate(time.Second).Add(time.Second)

	case RangeLast24Hours:
		resolved.Start = now.Add(-24 * time.Hour).Truncate(time.Second)
		resolved.End = now.Truncate(time.Second).Add(time.Second)

	case RangeCustom:
		resolved.Start = inLocation(d.Start, loc)
		resolved.End = inLocation(d.End, loc)

		// A bare date names a whole day, so the inclusive end becomes the start
		// of the next one. Without this, "1 August to 1 August" is an empty
		// range rather than one day.
		if d.DateOnly {
			resolved.End = startOfDay(resolved.End, loc).AddDate(0, 0, 1)
		}

	default:
		return Resolved{}, invalid("unknown date_range %q", d.Preset)
	}

	resolved.Interval = chooseInterval(resolved)

	return resolved, nil
}

// chooseInterval picks the bucket width a range implies. The thresholds are
// about how many points a graph can usefully draw: a year of hourly buckets is
// nine thousand points nobody can read, and a week of monthly buckets is one.
func chooseInterval(r Resolved) string {
	// Both live windows are cut into minutes rather than by span. Five minutes
	// falls under every threshold below and would otherwise come out as one
	// hour bucket — a single point, which is not a graph.
	if r.Preset == RangeRealtime || r.Preset == RangeLast5Minutes {
		return IntervalMinute
	}

	span := r.End.Sub(r.Start)

	switch {
	case span <= 48*time.Hour:
		return IntervalHour
	case span <= 100*24*time.Hour:
		return IntervalDay
	case span <= 400*24*time.Hour:
		return IntervalWeek
	default:
		return IntervalMonth
	}
}

// WithInterval returns the range bucketed at an explicit width, which is what
// an explicit time:day or time:month dimension asks for.
func (r Resolved) WithInterval(interval string) Resolved {
	if interval != "" {
		r.Interval = interval
	}

	return r
}

// IncludesNow reports whether the range is still running. It is the question
// behind both the in-progress bucket and the like-for-like comparison: a period
// that has not finished must never be compared against a complete one.
func (r Resolved) IncludesNow() bool {
	return !r.Now.Before(r.Start) && r.Now.Before(r.End)
}

// Elapsed is how much of the range has actually happened. For a finished period
// that is its whole length; for today at four in the afternoon it is sixteen
// hours, and comparing sixteen hours against yesterday's twenty-four is the
// single most common way a comparison lies.
func (r Resolved) Elapsed() time.Duration {
	if r.Now.After(r.End) {
		return r.End.Sub(r.Start)
	}

	if r.Now.Before(r.Start) {
		return 0
	}

	return r.Now.Sub(r.Start)
}

// Complete reports whether the whole range is in the past. It is the question
// the roll-up router will ask once roll-up tables exist: a finished day can be
// read from a summary, an unfinished one cannot.
func (r Resolved) Complete() bool {
	return !r.End.After(r.Now)
}

// Buckets lists the local start of every bucket in the range, including the
// ones with no data. A graph that only receives the buckets that had traffic
// cannot tell a quiet Tuesday from a Tuesday the tracker was broken, so the
// complete list is generated here rather than inferred from the result rows.
func (r Resolved) Buckets() []time.Time {
	var buckets []time.Time

	for at := bucketStart(r.Start, r.Interval, r.Location); at.Before(r.End); at = nextBucket(at, r.Interval, r.Location) {
		buckets = append(buckets, at)

		// A pathological range cannot be allowed to allocate without bound.
		if len(buckets) > maxBuckets {
			break
		}
	}

	return buckets
}

// maxBuckets caps the label list. The widest sensible request — a year of daily
// buckets — is 366, and anything past this is a range and an interval that
// should not have been combined.
const maxBuckets = 2000

// Labels renders the bucket list as the strings the SQL produces, so that a row
// coming back from the database can be matched to its label by string equality
// rather than by re-deriving the bucket in two places.
func (r Resolved) Labels() []string {
	buckets := r.Buckets()
	labels := make([]string, 0, len(buckets))

	for _, at := range buckets {
		labels = append(labels, bucketLabel(at, r.Interval))
	}

	return labels
}

// PresentIndex is the bucket that is still filling up, or nil when the range is
// entirely in the past. The graph dashes that bucket: without it, the last
// point of every live chart looks like a cliff, and somebody asks why traffic
// collapsed this morning.
func (r Resolved) PresentIndex() *int {
	if !r.IncludesNow() {
		return nil
	}

	present := bucketLabel(bucketStart(r.Now, r.Interval, r.Location), r.Interval)

	for i, label := range r.Labels() {
		if label == present {
			index := i
			return &index
		}
	}

	return nil
}

// Compare resolves the period this range is measured against. The comparison
// is truncated to the same elapsed time as the primary range whenever the
// primary is still running, which is what makes "vs yesterday" at four in the
// afternoon compare sixteen hours against sixteen hours.
func (r Resolved) Compare(c *Comparison) (Resolved, error) {
	if c == nil {
		return Resolved{}, invalid("no comparison was requested")
	}

	comparison := Resolved{
		Preset:   r.Preset,
		Location: r.Location,
		Now:      r.Now,
		Interval: r.Interval,
	}

	switch c.Mode {
	case ComparePreviousPeriod:
		shift := previousPeriodShift(r)
		comparison.Start, comparison.End = shift(r.Start), shift(r.End)

	case CompareYearOverYear:
		comparison.Start = r.Start.AddDate(-1, 0, 0)
		comparison.End = r.End.AddDate(-1, 0, 0)

	case CompareCustom:
		resolved, err := c.DateRange.Resolve(r.Now, r.Location, time.Time{})
		if err != nil {
			return Resolved{}, err
		}
		comparison.Start, comparison.End = resolved.Start, resolved.End

		return comparison, nil

	default:
		return Resolved{}, invalid("unknown comparison mode %q", c.Mode)
	}

	// A period that is still running is compared against the same amount of
	// elapsed time, not against a whole earlier period. At four in the
	// afternoon, "vs yesterday" means sixteen hours against sixteen hours.
	if r.IncludesNow() {
		comparison.End = comparison.Start.Add(r.Elapsed())
	}

	return comparison, nil
}

// previousPeriodShift returns the function that moves a range back by one of
// itself. Whole calendar months move by months and whole days move by days, so
// that a comparison lands on the boundary a reader expects rather than however
// many hours the period happened to contain.
func previousPeriodShift(r Resolved) func(time.Time) time.Time {
	if months, whole := wholeMonths(r); whole {
		return func(at time.Time) time.Time { return at.AddDate(0, -months, 0) }
	}

	if days, whole := wholeDays(r); whole {
		return func(at time.Time) time.Time { return at.AddDate(0, 0, -days) }
	}

	length := r.End.Sub(r.Start)

	return func(at time.Time) time.Time { return at.Add(-length) }
}

// wholeMonths reports whether a range covers a whole number of calendar months,
// and how many.
func wholeMonths(r Resolved) (int, bool) {
	if !r.Start.Equal(startOfMonth(r.Start, r.Location)) || !r.End.Equal(startOfMonth(r.End, r.Location)) {
		return 0, false
	}

	months := (r.End.Year()-r.Start.Year())*12 + int(r.End.Month()) - int(r.Start.Month())
	if months < 1 {
		return 0, false
	}

	return months, true
}

// wholeDays reports whether a range covers a whole number of local days, and
// how many.
func wholeDays(r Resolved) (int, bool) {
	if !r.Start.Equal(startOfDay(r.Start, r.Location)) || !r.End.Equal(startOfDay(r.End, r.Location)) {
		return 0, false
	}

	days := 0
	for at := r.Start; at.Before(r.End); at = at.AddDate(0, 0, 1) {
		days++

		if days > maxBuckets {
			return 0, false
		}
	}

	return days, true
}

// startOfDay is local midnight. It is built from the calendar fields rather
// than by truncating, because truncation works in absolute time and a day is
// not always twenty-four hours long.
func startOfDay(at time.Time, loc *time.Location) time.Time {
	at = at.In(loc)

	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc)
}

// startOfMonth is local midnight on the first.
func startOfMonth(at time.Time, loc *time.Location) time.Time {
	at = at.In(loc)

	return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, loc)
}

// startOfWeek is local midnight on the Monday of this week. Monday because
// every calendar in the product's markets starts there and because a week that
// starts on Sunday puts the weekend on both ends of the chart.
func startOfWeek(at time.Time, loc *time.Location) time.Time {
	at = startOfDay(at, loc)

	offset := (int(at.Weekday()) + 6) % 7

	return at.AddDate(0, 0, -offset)
}

// bucketStart snaps a time back to the start of its bucket.
func bucketStart(at time.Time, interval string, loc *time.Location) time.Time {
	at = at.In(loc)

	switch interval {
	case IntervalMinute:
		return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), 0, 0, loc)
	case IntervalHour:
		return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, loc)
	case IntervalWeek:
		return startOfWeek(at, loc)
	case IntervalMonth:
		return startOfMonth(at, loc)
	default:
		return startOfDay(at, loc)
	}
}

// nextBucket steps one bucket forward. Days, weeks and months step by calendar
// arithmetic so a daylight saving change does not shift every later bucket by
// an hour.
func nextBucket(at time.Time, interval string, loc *time.Location) time.Time {
	switch interval {
	case IntervalMinute:
		return at.Add(time.Minute)
	case IntervalHour:
		return at.Add(time.Hour)
	case IntervalWeek:
		return startOfDay(at.AddDate(0, 0, 7), loc)
	case IntervalMonth:
		return at.AddDate(0, 1, 0)
	default:
		return startOfDay(at.AddDate(0, 0, 1), loc)
	}
}

// bucketLabel renders a bucket exactly as the SQL renders it. The two have to
// agree character for character, because the join between a result row and its
// label is a string comparison.
func bucketLabel(at time.Time, interval string) string {
	switch interval {
	case IntervalMinute:
		return at.Format("2006-01-02 15:04:00")
	case IntervalHour:
		return at.Format("2006-01-02 15:00:00")
	case IntervalMonth:
		return at.Format("2006-01")
	default:
		return at.Format(dateLayout)
	}
}

// inLocation reinterprets a wall-clock time as being in the site's timezone.
// The parsed bounds carry no location, so this is where "2026-08-01 00:00" the
// text becomes an instant.
func inLocation(at time.Time, loc *time.Location) time.Time {
	return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), 0, loc)
}

// UnmarshalJSON reads either a preset string or a pair of bounds.
func (d *DateRange) UnmarshalJSON(data []byte) error {
	var preset string
	if err := json.Unmarshal(data, &preset); err == nil {
		d.Preset = preset
		return nil
	}

	var bounds []string
	if err := json.Unmarshal(data, &bounds); err != nil {
		return invalid("date_range must be a preset name or a pair of dates")
	}

	if len(bounds) != 2 {
		return invalid("a custom date_range needs exactly two dates")
	}

	start, startDateOnly, err := parseBound(bounds[0])
	if err != nil {
		return err
	}

	end, endDateOnly, err := parseBound(bounds[1])
	if err != nil {
		return err
	}

	d.Preset = RangeCustom
	d.Start, d.End = start, end
	d.DateOnly = startDateOnly && endDateOnly

	return nil
}

// parseBound reads one custom bound, accepting a date, a local datetime or an
// RFC 3339 timestamp.
func parseBound(value string) (time.Time, bool, error) {
	if at, err := time.Parse(dateLayout, value); err == nil {
		return at, true, nil
	}

	if at, err := time.Parse(dateTimeLayout, value); err == nil {
		return at, false, nil
	}

	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at.UTC(), false, nil
	}

	return time.Time{}, false, invalid("%q is not a date — use 2026-08-01 or 2026-08-01 13:00:00", value)
}

// MarshalJSON writes the preset name, or the pair of bounds for a custom range.
func (d DateRange) MarshalJSON() ([]byte, error) {
	if d.Preset != RangeCustom {
		return json.Marshal(d.Preset)
	}

	layout := dateTimeLayout
	if d.DateOnly {
		layout = dateLayout
	}

	return json.Marshal([]string{d.Start.Format(layout), d.End.Format(layout)})
}

// String renders a resolved range for an error message or a log line.
func (r Resolved) String() string {
	return fmt.Sprintf("%s..%s (%s, %s buckets)",
		r.Start.Format(time.RFC3339), r.End.Format(time.RFC3339), r.Location, r.Interval)
}
