//
// sql.go
// The small builder every statement in this package is assembled with.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"math"
	"strings"
	"time"
)

// builder accumulates SQL and its bind arguments together. Keeping them in one
// place is the whole trick: a fragment and its parameters can never be appended
// in a different order, which is the bug that turns a parameterised query into
// a mis-bound one that returns confidently wrong numbers.
type builder struct {
	sql  strings.Builder
	args []any
}

// add appends a fragment and its arguments.
func (b *builder) add(sql string, args ...any) *builder {
	b.sql.WriteString(sql)
	b.args = append(b.args, args...)

	return b
}

// addExpr appends a prepared expression.
func (b *builder) addExpr(e expr) *builder {
	return b.add(e.SQL, e.Args...)
}

// String is the finished statement.
func (b *builder) String() string {
	return b.sql.String()
}

// Args are the finished statement's bind parameters, in order.
func (b *builder) Args() []any {
	return b.args
}

// and joins conditions with AND. An empty set is the always-true condition
// rather than an empty string, so a caller never has to decide whether to write
// the word WHERE.
func and(conditions []expr) expr {
	if len(conditions) == 0 {
		return expr{SQL: "1 = 1"}
	}

	parts := make([]string, 0, len(conditions))
	var args []any

	for _, condition := range conditions {
		parts = append(parts, condition.SQL)
		args = append(args, condition.Args...)
	}

	return expr{SQL: strings.Join(parts, " AND "), Args: args}
}

// or joins conditions with OR, parenthesised so it cannot bind loosely inside
// an AND chain. Multiple values inside one filter OR together, which is the
// only place this is used.
func or(conditions []expr) expr {
	if len(conditions) == 0 {
		return expr{SQL: "1 = 0"}
	}

	if len(conditions) == 1 {
		return conditions[0]
	}

	parts := make([]string, 0, len(conditions))
	var args []any

	for _, condition := range conditions {
		parts = append(parts, condition.SQL)
		args = append(args, condition.Args...)
	}

	return expr{SQL: "(" + strings.Join(parts, " OR ") + ")", Args: args}
}

// placeholders renders n bind markers. It is the only way a list ever reaches a
// statement in this package: the values themselves stay in the argument slice.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}

	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// inInt64 builds "column IN (?,?,?)" for a list of ids.
func inInt64(column string, values []int64) expr {
	args := make([]any, 0, len(values))
	for _, value := range values {
		args = append(args, value)
	}

	return expr{SQL: column + " IN (" + placeholders(len(values)) + ")", Args: args}
}

// offsetSpan is one stretch of wall-clock time during which a timezone's offset
// from UTC does not change. A zone has one of these per daylight saving
// transition, which is at most a handful inside any range a dashboard asks for.
type offsetSpan struct {
	// Until is the first instant this span does not cover, as unix seconds.
	Until int64

	// Offset is seconds east of UTC.
	Offset int
}

// zoneOffsets works out the offsets in force across a range. Timestamps are
// stored as UTC integers and buckets are local days, so the conversion has to
// happen inside the query — and SQLite has no timezone database, so the offsets
// are computed here and travel as bind parameters.
//
// Computing one offset for the whole range would be wrong twice a year: every
// event on one side of a daylight saving change would land in the wrong local
// day, and a dashboard would show a 23-hour Sunday and a missing hour of
// traffic that nobody could account for.
func zoneOffsets(loc *time.Location, from, to time.Time) []offsetSpan {
	if loc == nil {
		loc = time.UTC
	}

	// A margin either side, so an event exactly on the boundary is covered by a
	// span rather than by the fallback.
	start := from.Add(-24 * time.Hour)
	end := to.Add(24 * time.Hour)

	_, offset := start.In(loc).Zone()

	var spans []offsetSpan
	previous := start

	for at := start; at.Before(end); at = at.Add(24 * time.Hour) {
		_, current := at.In(loc).Zone()

		if current != offset {
			transition := findTransition(loc, previous, at, offset)
			spans = append(spans, offsetSpan{Until: transition.Unix(), Offset: offset})
			offset = current
		}

		previous = at

		// A range wide enough to blow this bound is a range whose bucket width
		// would already have been widened to months, where a single offset is
		// close enough to never move a bucket.
		if len(spans) > maxOffsetSpans {
			return []offsetSpan{{Until: math.MaxInt64, Offset: offset}}
		}
	}

	return append(spans, offsetSpan{Until: math.MaxInt64, Offset: offset})
}

// maxOffsetSpans bounds the CASE expression the offsets compile to.
const maxOffsetSpans = 128

// findTransition binary-searches for the second a zone's offset changed. To the
// second rather than to the hour because a bucket boundary is exact, and being
// an hour out puts real events in the wrong day.
func findTransition(loc *time.Location, low, high time.Time, before int) time.Time {
	for high.Sub(low) > time.Second {
		middle := low.Add(high.Sub(low) / 2)

		if _, offset := middle.In(loc).Zone(); offset == before {
			low = middle
		} else {
			high = middle
		}
	}

	return high
}

// localExpr renders a UTC timestamp column as local seconds. One span compiles
// to an addition; several compile to a CASE, which is the cheapest thing SQLite
// can evaluate per row that is still correct across a daylight saving change.
func localExpr(column string, spans []offsetSpan) expr {
	if len(spans) == 1 {
		return expr{SQL: "(" + column + " + ?)", Args: []any{int64(spans[0].Offset)}}
	}

	var (
		sql  strings.Builder
		args []any
	)

	sql.WriteString("(" + column + " + CASE")

	for _, span := range spans[:len(spans)-1] {
		sql.WriteString(" WHEN " + column + " < ? THEN ?")
		args = append(args, span.Until, int64(span.Offset))
	}

	sql.WriteString(" ELSE ? END)")
	args = append(args, int64(spans[len(spans)-1].Offset))

	return expr{SQL: sql.String(), Args: args}
}

// bucketExpr renders the local bucket label for a timestamp column. The strings
// it produces have to match bucketLabel exactly, because that equality is the
// join between a result row and the label list a graph draws its axis from.
func bucketExpr(column, interval string, spans []offsetSpan) expr {
	local := localExpr(column, spans)

	switch interval {
	case IntervalMinute:
		return expr{SQL: "strftime('%Y-%m-%d %H:%M:00', " + local.SQL + ", 'unixepoch')", Args: local.Args}

	case IntervalHour:
		return expr{SQL: "strftime('%Y-%m-%d %H:00:00', " + local.SQL + ", 'unixepoch')", Args: local.Args}

	case IntervalWeek:
		// 'weekday 0' moves to the coming Sunday, staying put if it is already
		// Sunday; six days back from there is the Monday that starts the week.
		return expr{SQL: "date(" + local.SQL + ", 'unixepoch', 'weekday 0', '-6 days')", Args: local.Args}

	case IntervalMonth:
		return expr{SQL: "strftime('%Y-%m', " + local.SQL + ", 'unixepoch')", Args: local.Args}

	default:
		return expr{SQL: "date(" + local.SQL + ", 'unixepoch')", Args: local.Args}
	}
}

// asciiLower folds a string the way SQLite's lower() does. Go's strings.ToLower
// is Unicode-aware and SQLite's is not, so folding a filter value in Go and
// comparing it against a column folded in SQLite would disagree on any
// non-ASCII letter — matching nothing, with no error.
func asciiLower(value string) string {
	var out strings.Builder
	out.Grow(len(value))

	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out.WriteByte(c)
	}

	return out.String()
}
