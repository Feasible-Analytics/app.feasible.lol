//
// rollup.go
// The seam where pre-aggregated days will be read instead of raw events.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

// Source names where one slice of a query's date range is read from.
type Source int

const (
	// SourceRaw is the events and sessions tables.
	SourceRaw Source = iota

	// SourceRollup is a pre-aggregated summary table. Nothing produces one
	// yet; the constant exists so that the router, the engine and the tests
	// already speak in terms of a source rather than assuming raw.
	SourceRollup
)

// String renders a source for a log line or a test failure.
func (s Source) String() string {
	if s == SourceRollup {
		return "rollup"
	}

	return "raw"
}

// Segment is one contiguous slice of a query's date range and where it is read
// from. A query over the last 28 days will eventually be 27 complete days out
// of a summary table and today out of the raw events, which is two segments
// whose results are added together.
type Segment struct {
	Range  Resolved
	Source Source
}

// Router decides which source answers each slice of a range.
//
// It is an interface with one trivial implementation today because the shape of
// the change is the expensive part, not the roll-up tables themselves.
// Retrofitting "this range is really two ranges from two sources" into an
// engine that assumes one query is a rewrite of everything downstream of it;
// having the engine already loop over segments and add them up means the
// roll-up milestone is a new Router and nothing else.
type Router interface {
	// Route splits a resolved range into the segments that answer it. It
	// returns at least one segment.
	Route(q *Query, r Resolved) []Segment
}

// RawRouter reads everything from the raw tables. It is the only router today,
// and it stays the right answer for any query a roll-up cannot serve — a
// filtered query, or one grouped by a dimension the summary does not carry.
type RawRouter struct{}

// Route answers with the whole range, read raw.
func (RawRouter) Route(_ *Query, r Resolved) []Segment {
	return []Segment{{Range: r, Source: SourceRaw}}
}

// SplitAtToday cuts a range into the part that is finished and the part that is
// still running. It is the split a roll-up router makes — complete days come
// from the summary, the day in progress cannot — and it lives here rather than
// in the future router so that the boundary arithmetic is written and tested
// once, against the same timezone handling everything else uses.
func SplitAtToday(r Resolved) (complete Resolved, partial Resolved, split bool) {
	if !r.IncludesNow() {
		return r, Resolved{}, false
	}

	boundary := startOfDay(r.Now, r.Location)

	// A range that starts today has no complete part, and one that has not
	// reached today has no partial part. Neither is a split.
	if !boundary.After(r.Start) {
		return r, Resolved{}, false
	}

	complete = r
	complete.End = boundary

	partial = r
	partial.Start = boundary

	return complete, partial, true
}

// Splittable reports whether a query's metrics can be answered from more than
// one segment and added together. Counting rows adds up across two time slices;
// counting distinct visitors does not, because the same person can appear in
// both, and adding those two counts would invent visitors who do not exist.
//
// It is the check a roll-up router has to make before it splits a range, and it
// is why the roll-up milestone will need visitor sketches rather than counts.
func Splittable(q *Query, p *plan) bool {
	for _, name := range q.Metrics {
		definition, ok := metricByName(name)
		if !ok {
			return false
		}

		if !definition.additive(p.MetricTable[name]) {
			return false
		}
	}

	return true
}
