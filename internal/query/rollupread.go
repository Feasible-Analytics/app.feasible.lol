//
// rollupread.go
// Answering a segment out of the summary tables instead of the raw ones.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// rollupPlan is the executor's cached decision about how the summary answers
// this query. It is computed once because the router already worked it out and
// the answer cannot change inside one query.
func (x *executor) rollupPlan() (rollupRead, bool) {
	if !x.rollupPlanned {
		x.rollupRead, x.rollupUsable = planRollupRead(x.query, x.resolved)
		x.rollupPlanned = true
	}

	return x.rollupRead, x.rollupUsable
}

// rollupDimFor picks the summary keying for one fact table.
func (r rollupRead) dimFor(t table) RollupDim {
	if t == tableSessions {
		return r.sessionDim
	}

	return r.eventDim
}

// rollupPass answers one fact table's share of a segment out of the summary.
//
// It mirrors the raw pass exactly — same group keys, same component order, same
// ordering and pagination — because the two are added together into one result
// set, and a summary row that keyed itself differently would land beside the
// raw rows rather than on top of them.
func (x *executor) rollupPass(ctx context.Context, t table, segment Segment, groups *groupSet, restrict map[int][]any, create bool) error {
	read, ok := x.rollupPlan()
	if !ok {
		return fmt.Errorf("query: a segment was routed to the roll-up tables that they cannot answer")
	}

	dimension := read.dimFor(t)

	names := x.metricsFor(t)
	if len(names) == 0 && !create {
		return nil
	}

	dims, err := x.rollupDimensions(read)
	if err != nil {
		return err
	}

	columns, targets, aliases, err := x.rollupColumns(names, t, read)
	if err != nil {
		return err
	}

	// A statement with no aggregate of its own still needs one, or SQLite has
	// nothing to group and returns a row per summary row.
	if len(columns) == 0 {
		columns = append(columns, expr{SQL: "COUNT(*)"})
	}

	from, through := rollupWindow(segment.Range)

	conditions := []expr{{
		SQL: "r.site_id = ? AND r.grain = ? AND r.dimension = ? AND r.bucket >= ? AND r.bucket < ?",
		Args: []any{
			x.query.SiteIDs[0], int64(segment.Grain), int64(dimension.Code), from, through,
		},
	}}

	conditions = append(conditions, x.rollupRestrictions(dims, restrict)...)

	st := statement{
		alias: "r", nameOverride: dimension.Table,
		dims: dims, columns: columns, conditions: conditions,
	}

	switch {
	// Only the statement that defines the result set may be ordered and cut.
	// A follow-up read adds numbers to the page the primary found; giving it a
	// page of its own would drop the rows whose numbers it was fetching.
	case x.pushDown && create:
		st.orderBy = x.orderSQL(dims, aliases)
		st.limit, st.offset, st.limited = x.query.Pagination.Limit, x.query.Pagination.Offset, true

	case len(dims) > 0 && create:
		st.orderBy = fallbackOrder(dims, aliases, names)
		st.limit, st.offset, st.limited = x.engine.maxGroups()+1, 0, true
	}

	sqlText, args := x.renderStatement(st)

	rows, err := x.readRows(ctx, sqlText, args, len(dims), len(columns), groups, targets, create)
	if err != nil {
		return err
	}

	if st.limited && !x.pushDown && rows > x.engine.maxGroups() {
		x.truncated = true
	}

	return nil
}

// rollupDimensions compiles the group-by list for a summary read. The time
// bucket is rendered from the `bucket` column with no timezone conversion,
// because that column already holds local seconds — and it goes through the
// same label builder the raw path uses, so the two produce identical strings.
func (x *executor) rollupDimensions(read rollupRead) ([]compiledDim, error) {
	compiled := make([]compiledDim, 0, len(x.plan.Dimensions))

	for i, d := range x.plan.Dimensions {
		alias := fmt.Sprintf("d%d", i)

		if d.Time {
			compiled = append(compiled, compiledDim{
				dim: d, alias: alias,
				sql: bucketExpr("r.bucket", x.resolved.Interval, utcSpans),
			})

			continue
		}

		if i != read.keyIndex {
			return nil, fmt.Errorf("query: dimension %q is not keyed in the roll-up tables", d.Name)
		}

		compiled = append(compiled, compiledDim{dim: d, alias: alias, sql: expr{SQL: "r.value_id"}})
	}

	return compiled, nil
}

// rollupColumns builds the aggregate expressions for a set of metrics against
// the summary columns, where each one belongs, and the aliases the ordering can
// name them by.
func (x *executor) rollupColumns(names []string, t table, read rollupRead) ([]expr, []target, map[string][]string, error) {
	var (
		columns []expr
		targets []target
	)

	aliases := map[string][]string{}
	firsts := x.rollupGroupFirsts(read)

	for _, name := range names {
		components, ok := rollupComponents(name, t)
		if !ok {
			return nil, nil, nil, fmt.Errorf("query: %q cannot be read from the roll-up tables", name)
		}

		for slot, component := range components {
			aliases[name] = append(aliases[name], fmt.Sprintf("m%d", len(columns)))
			targets = append(targets, target{metric: name, slot: slot, column: len(columns)})
			columns = append(columns, rollupColumnExpr(component, "r", read.perBucket, firsts))
		}
	}

	return columns, targets, aliases, nil
}

// rollupGroupFirsts lists the bucket values that begin an output group.
//
// Adding a range of buckets double-counts anything present in two of them, and
// subtracting each bucket's carry-over undoes exactly that — except for the
// first bucket of a group, whose carry-over belongs to whatever came before the
// group and is not in the sum at all. A query with no time dimension is one
// group over the whole range, so it has exactly one such bucket.
func (x *executor) rollupGroupFirsts(read rollupRead) []any {
	return rollupGroupFirsts(x.resolved, read)
}

// rollupGroupFirsts calculates the bucket values that begin an output group.
// It is shared with sampling's seam-cost estimator so the estimator skips a
// correction in exactly the same cases as the executor.
func rollupGroupFirsts(resolved Resolved, read rollupRead) []any {
	if read.perBucket {
		return nil
	}

	location := resolved.Location

	if read.timeIndex < 0 {
		start := RollupBucketStart(resolved.Start, read.grain, location)

		return []any{RollupLocalUnix(start, location)}
	}

	buckets := resolved.Buckets()
	firsts := make([]any, 0, len(buckets))

	for _, at := range buckets {
		start := at
		if start.Before(resolved.Start) {
			start = resolved.Start
		}

		firsts = append(firsts, RollupLocalUnix(RollupBucketStart(start, read.grain, location), location))
	}

	return firsts
}

// rollupRestrictions narrows a follow-up summary read to the group keys the
// primary pass found, the same way the raw path does.
func (x *executor) rollupRestrictions(dims []compiledDim, keys map[int][]any) []expr {
	if len(keys) == 0 {
		return nil
	}

	var conditions []expr

	for i, dimension := range dims {
		values, ok := keys[i]
		if !ok || len(values) == 0 {
			continue
		}

		conditions = append(conditions, expr{
			SQL:  dimension.sql.SQL + " IN (" + placeholders(len(values)) + ")",
			Args: append(append([]any{}, dimension.sql.Args...), values...),
		})
	}

	return conditions
}

// seamPass removes the entities that today's raw rows and yesterday's summary
// rows both counted.
//
// The summary and the raw tables meet at local midnight, and a distinct count
// does not add up across that line: a visitor id lives for a whole UTC day and
// a visit lasts as long as somebody keeps clicking, so either can be present on
// both sides and be counted twice. Inside the summary the `_carried` columns
// already correct this; today has no summary row, so the same correction is
// computed here from raw rows over two days — which is a small, indexed read,
// not the scan the whole query is avoiding.
//
// It does nothing when every output group is a single day, which is every graph
// drawn at a daily interval: today is then a group of its own and there is
// nothing to double count.
func (x *executor) seamPass(ctx context.Context, partial Resolved, groups *groupSet) error {
	read, ok := x.rollupPlan()
	if !ok || read.perBucket {
		return nil
	}

	previous, label, needed := seamCorrectionWindow(x.resolved, partial, read)
	if !needed {
		return nil
	}

	for _, name := range x.query.Metrics {
		t := x.plan.MetricTable[name]

		components, ok := rollupComponents(name, t)
		if !ok {
			continue
		}

		for slot, component := range components {
			if component.carried == "" {
				continue
			}

			if err := x.seamCorrection(ctx, name, slot, t, read, partial, previous, label, groups); err != nil {
				return err
			}
		}
	}

	return nil
}

// seamCorrection counts, per group, the entities that both sides of the
// boundary saw, and subtracts them.
//
// It is one pass over two days of rows and a single grouping: the entities are
// gathered once over yesterday and today together, and the HAVING keeps only
// the ones that appear on both sides. The obvious shape — the two days as
// separate sets, joined — reads the same rows but makes SQLite compare every
// row of one against every row of the other, because there is no index on a
// subquery's output.
//
// The count is selected negative so it travels through the same accumulator
// every other component does, rather than needing a second code path that
// subtracts.
func (x *executor) seamCorrection(ctx context.Context, name string, slot int, t table, read rollupRead, today, previous Resolved, label string, groups *groupSet) error {
	entity := t.alias() + ".user_id"
	if name == "visits" {
		entity = t.alias() + ".session_id"
	}

	dimension := read.dimFor(t)

	group := "0"
	if !dimension.Total {
		if t == tableSessions {
			group = dimension.SessionGroupSQL()
		} else {
			group = dimension.EventGroupSQL()
		}
	}

	// The two days as one window. Which side a row is on becomes a flag, and
	// the grouping keeps the entities that have both.
	window := today
	window.Start = previous.Start

	conditions, err := x.conditionsFor(t, window)
	if err != nil {
		return err
	}

	side := t.alias() + "." + t.timeColumn() + " >= ?"
	boundary := today.Start.Unix()

	b := &builder{}
	b.add("SELECT ")

	for i, d := range x.plan.Dimensions {
		if i > 0 {
			b.add(", ")
		}

		if d.Time {
			b.add("?", label)
		} else {
			b.add("c.v")
		}

		b.add(fmt.Sprintf(" AS d%d", i))
	}

	if len(x.plan.Dimensions) > 0 {
		b.add(", ")
	}

	b.add("-COUNT(*) AS m0 FROM (")
	b.add("SELECT " + group + " AS v, " + entity + " AS entity")
	b.add(" FROM " + t.name() + " " + t.alias() + " WHERE ").addExpr(and(conditions))
	b.add(" GROUP BY v, entity")
	b.add(" HAVING MIN(CASE WHEN "+side+" THEN 1 ELSE 0 END) = 0", boundary)
	b.add(" AND MAX(CASE WHEN "+side+" THEN 1 ELSE 0 END) = 1", boundary)
	b.add(") c GROUP BY c.v")

	_, err = x.readRows(ctx, b.String(), b.Args(), len(x.plan.Dimensions), 1, groups,
		[]target{{metric: name, slot: slot, column: 0}}, false)

	return err
}

// seamCorrectionWindow returns the preceding complete day scanned beside the
// current raw segment. It is the single source of truth for whether a carry
// correction runs, which keeps execution and sampling-cost decisions aligned.
func seamCorrectionWindow(resolved, partial Resolved, read rollupRead) (Resolved, string, bool) {
	location := resolved.Location
	todayBucket := RollupLocalUnix(RollupBucketStart(partial.Start, read.grain, location), location)

	for _, first := range rollupGroupFirsts(resolved, read) {
		if value, ok := first.(int64); ok && value == todayBucket {
			return Resolved{}, "", false
		}
	}

	previous := partial
	previous.End = partial.Start
	previous.Start = startOfDay(partial.Start.Add(-time.Second), location)

	label := bucketLabel(bucketStart(partial.Start, resolved.Interval, location), resolved.Interval)

	return previous, label, true
}

// countGroupsIn answers include.total_rows from whichever source produced the
// page. It exists because the push-down path is the one place a page is cut by
// the database, and counting the groups off the raw tables behind a query that
// read a summary would put the whole scan back.
func (x *executor) countGroupsIn(ctx context.Context) (int, error) {
	if len(x.segments) != 1 || x.segments[0].Source != SourceRollup {
		return x.countGroups(ctx)
	}

	read, ok := x.rollupPlan()
	if !ok {
		return x.countGroups(ctx)
	}

	segment := x.segments[0]
	dimension := read.dimFor(x.plan.Primary)

	dims, err := x.rollupDimensions(read)
	if err != nil {
		return 0, err
	}

	from, through := rollupWindow(segment.Range)

	inner := statement{
		alias: "r", nameOverride: dimension.Table,
		dims: dims, columns: []expr{{SQL: "COUNT(*)"}},
		conditions: []expr{{
			SQL: "r.site_id = ? AND r.grain = ? AND r.dimension = ? AND r.bucket >= ? AND r.bucket < ?",
			Args: []any{
				x.query.SiteIDs[0], int64(segment.Grain), int64(dimension.Code), from, through,
			},
		}},
	}

	sqlText, args := x.renderStatement(inner)

	var total int
	if err := x.engine.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+sqlText+")", args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("query: count roll-up groups: %w", err)
	}

	return total, nil
}

// rollupBacked reports whether a split was made by the roll-up router, which is
// the only router allowed to cut a range that carries a distinct count.
func rollupBacked(segments []Segment) bool {
	for _, segment := range segments {
		if segment.Source == SourceRollup {
			return true
		}
	}

	return false
}

// rollupExplain renders the routing decision for a log line or a test failure.
func rollupExplain(segments []Segment) string {
	parts := make([]string, 0, len(segments))

	for _, segment := range segments {
		part := segment.Source.String()
		if segment.Source == SourceRollup {
			part += "/" + segment.Grain.String()
		}

		parts = append(parts, part+" "+segment.Range.String())
	}

	return strings.Join(parts, " + ")
}
