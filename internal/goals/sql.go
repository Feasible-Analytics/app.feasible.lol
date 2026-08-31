//
// sql.go
// The scaffolding the two reports that cannot go through the compiler share.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Window is the resolved period a report covers: unix seconds, half-open.
// Half-open is the only pairing that composes — with an inclusive end,
// adjacent windows either overlap by a second or leave a gap.
type Window struct {
	Start int64
	End   int64
}

// NewWindow builds a window from two instants, which is how a caller that
// already resolved a date range through the query compiler hands one over.
func NewWindow(start, end time.Time) Window {
	return Window{Start: start.Unix(), End: end.Unix()}
}

// clampTo moves the start of a window forward to an instant, which is how a
// goal's creation time is applied. Goals do not backfill, so every conversion
// query starts at the later of the report range and the goal's creation.
func (w Window) clampTo(at int64) Window {
	if at > w.Start {
		w.Start = at
	}

	return w
}

// Empty reports a window that cannot contain anything, which happens whenever
// a goal was created after the report range ended. Answering zero without
// running the query is both faster and clearer than a query that cannot match.
func (w Window) Empty() bool {
	return w.End <= w.Start
}

// baseConditions is the WHERE clause every raw report in this package starts
// from. It is one function rather than a line in each report because the two
// exclusions in it — imported data and traffic we classified as automated —
// are the difference between a funnel and a funnel with somebody's crawler in
// it, and a report that forgot one would be wrong in a way nobody would spot.
//
// It deliberately matches what the query compiler's own base clause does, so a
// funnel step and the same goal on the goals report count the same events.
func baseConditions(alias string, siteID int64, window Window) (string, []any) {
	conditions := []string{
		alias + ".site_id = ?",
		alias + ".timestamp >= ?",
		alias + ".timestamp < ?",
		alias + ".is_imported = 0",
		alias + ".bot_reason_id = 0",
	}

	return strings.Join(conditions, " AND "), []any{siteID, window.Start, window.End}
}

// eventNameID reads the interned id of an event name, answering -1 for a name
// this account has never recorded. Minus one rather than zero: id 0 is the
// empty string in every dimension table, so a missing name matching it would
// quietly select every event that has no name at all.
func eventNameID(ctx context.Context, db *sql.DB, name string) (int64, error) {
	var id int64

	err := db.QueryRowContext(ctx, "SELECT id FROM dim_event_name WHERE value = ?", name).Scan(&id)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return -1, nil
	case err != nil:
		return 0, fmt.Errorf("goals: read event name: %w", err)
	}

	return id, nil
}

// jsonPath renders the path json_extract reads one property by. The key
// travels as a bind parameter rather than as SQL text, and the characters that
// would need escaping inside it are refused when a property is registered.
func jsonPath(name string) string {
	return `$."` + name + `"`
}
