//
// special.go
// The composite metrics: a second query, joined back on the group key.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// specialPass answers one composite metric and adds it to the groups the
// primary statement found.
//
// Every one of these is a second query rather than a window function over the
// first. A window function would have to carry the whole filtered row set
// through to the end of the query, and each new composite would change the
// shape of the main statement; a second query with the same WHERE clause
// changes nothing, which is what makes filters compose with composites at all.
func (x *executor) specialPass(ctx context.Context, name string, r Resolved, groups *groupSet, keys map[int][]any) error {
	switch name {
	case "scroll_depth":
		return x.scrollDepth(ctx, r, groups, keys)
	case "exit_rate":
		return x.exitRate(ctx, r, groups, keys)
	case "conversion_rate":
		return x.conversionRate(ctx, r, groups, keys, name, true)
	case "group_conversion_rate":
		return x.conversionRate(ctx, r, groups, keys, name, false)
	case "total_revenue", "average_revenue", "revenue_per_visitor":
		return x.revenue(ctx, r, groups, keys)
	}

	// Everything else that reached the composite list is a numeric property
	// aggregate, because those are the only other metrics the planner routes
	// here. A name that is neither is refused by that pass rather than
	// silently answering zero.
	return x.propAggregatePass(ctx, name, r, groups, keys)
}

// scrollDepth averages how far down the page people got.
//
// The measurement is per session, not per event: a visitor sends several
// engagement pings as they scroll, and averaging the pings would weight a
// visitor who scrolled slowly more heavily than one who scrolled straight to
// the bottom. So the inner query collapses to the deepest point each session
// reached and the outer one averages those.
//
// Depths above 100 are excluded because 255 is the "never reported" marker: a
// page too short to scroll has no scroll depth, and counting it as 255% would
// be a number nobody could explain.
func (x *executor) scrollDepth(ctx context.Context, r Resolved, groups *groupSet, keys map[int][]any) error {
	dims, joins, extra, err := x.dimensions(tableEvents, dimEntry)
	if err != nil {
		return err
	}

	conditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return err
	}

	conditions = append(conditions, extra...)
	conditions = append(conditions, x.restrictions(dims, keys)...)
	conditions = append(conditions,
		expr{SQL: "e.name_id = ?", Args: []any{x.compile.engagementNameID}},
		expr{SQL: "e.scroll_depth <= 100"},
	)

	inner := statement{
		table: tableEvents, alias: tableEvents.alias(), joins: joins,
		dims: dims, columns: []expr{{SQL: "MAX(e.scroll_depth)"}},
		conditions: conditions, groupExtra: []string{"e.session_id"},
	}

	innerSQL, args := inner.render()

	names := make([]string, 0, len(dims))
	for i := range dims {
		names = append(names, fmt.Sprintf("d%d", i))
	}

	outer := "SELECT " + strings.Join(append(append([]string{}, names...), "AVG(m0) AS m0"), ", ") +
		" FROM (" + innerSQL + ")"

	if len(names) > 0 {
		outer += " GROUP BY " + strings.Join(names, ", ")
	}

	if _, err := x.readRows(ctx, outer, args, len(dims), 1, groups,
		[]target{{metric: "scroll_depth", slot: 0, column: 0}}, false); err != nil {
		return err
	}

	return x.coverage(ctx, r, "scroll_depth", []expr{
		{SQL: "e.name_id = ?", Args: []any{x.compile.engagementNameID}},
		{SQL: "e.scroll_depth <= 100"},
	})
}

// exitRate is the share of a page's views that ended the visit.
//
// The denominator is the page's pageviews, not its entrances. Measuring against
// entrances answers "of the visits that started here, how many left from here",
// which is a different and much smaller question, and it is the reading that
// makes an exit rate look impossibly high on any page people navigate to from
// inside the site.
func (x *executor) exitRate(ctx context.Context, r Resolved, groups *groupSet, keys map[int][]any) error {
	exitDims, _, _, err := x.dimensions(tableSessions, dimExit)
	if err != nil {
		return err
	}

	sessionConditions, err := x.conditionsFor(tableSessions, r)
	if err != nil {
		return err
	}
	sessionConditions = append(sessionConditions, x.restrictions(exitDims, keys)...)

	exits := statement{
		table: tableSessions, alias: tableSessions.alias(),
		dims: exitDims, columns: []expr{{SQL: "COUNT(*)"}}, conditions: sessionConditions,
	}

	if _, err := x.merge(ctx, exits, groups, []target{{metric: "exit_rate", slot: 0, column: 0}}); err != nil {
		return err
	}

	viewDims, joins, extra, err := x.dimensions(tableEvents, dimEntry)
	if err != nil {
		return err
	}

	eventConditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return err
	}
	eventConditions = append(eventConditions, extra...)
	eventConditions = append(eventConditions, x.restrictions(viewDims, keys)...)

	views := statement{
		table: tableEvents, alias: tableEvents.alias(), joins: joins,
		dims: viewDims,
		columns: []expr{{
			SQL:  "SUM(CASE WHEN e.name_id = ? THEN 1 ELSE 0 END)",
			Args: []any{x.compile.pageviewNameID},
		}},
		conditions: eventConditions,
	}

	_, err = x.merge(ctx, views, groups, []target{{metric: "exit_rate", slot: 1, column: 0}})

	return err
}

// conversionRate divides the visitors who matched the goal by the visitors who
// could have.
//
// The denominator query is the same query with the goal stripped out — both the
// goal filters and any breakdown by goal — because a conversion rate measured
// against its own numerator is always 100%. Stripping and re-running is what
// makes "signups per source" mean signups divided by everybody from that
// source, which is the only reading of the number that is any use.
//
// The global flag chooses between the two rates, and it is the entire
// difference between them: global divides by every visitor in the period,
// grouped divides inside each row's own group. Both are correct answers, to
// different questions, and a report that reaches for the wrong one shows a
// number that looks perfectly reasonable and answers something nobody asked.
func (x *executor) conversionRate(ctx context.Context, r Resolved, groups *groupSet, keys map[int][]any, name string, global bool) error {
	dims, joins, extra, err := x.dimensions(tableEvents, dimEntry)
	if err != nil {
		return err
	}

	conditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return err
	}
	conditions = append(conditions, extra...)
	conditions = append(conditions, x.restrictions(dims, keys)...)

	converted := statement{
		table: tableEvents, alias: tableEvents.alias(), joins: joins,
		dims: dims, columns: []expr{{SQL: "COUNT(DISTINCT e.user_id)"}}, conditions: conditions,
	}

	if _, err := x.merge(ctx, converted, groups, []target{{metric: name, slot: 0, column: 0}}); err != nil {
		return err
	}

	return x.visitorDenominator(ctx, r, groups, name, 1, global)
}

// visitorDenominator counts everybody who could have converted and writes the
// count into one slot of a metric.
//
// Grouped, it keeps the dimensions that survive the goal being stripped out, so
// each row divides by its own group. Global, it keeps none of them, and every
// row divides by the same number: the visitors in the period.
func (x *executor) visitorDenominator(ctx context.Context, r Resolved, groups *groupSet, name string, slot int, global bool) error {
	kept := make([]int, 0, len(x.plan.Dimensions))
	var keptDims []dimension

	if !global {
		for i, d := range x.plan.Dimensions {
			if isGoalDimension(d, x.plan.Scopes) {
				continue
			}

			kept = append(kept, i)
			keptDims = append(keptDims, d)
		}
	}

	base := *x.query
	base.Filters = nil
	for _, filter := range x.query.Filters {
		if isGoalFilter(filter, x.plan.Scopes) {
			continue
		}
		base.Filters = append(base.Filters, filter)
	}

	// A second executor over the query with its goal removed, so the dimension
	// compiler and the filter compiler behave exactly as they do for the real
	// one.
	without := &executor{
		engine:   x.engine,
		query:    &base,
		plan:     &plan{Primary: tableEvents, MetricTable: x.plan.MetricTable, Dimensions: keptDims, Scopes: x.plan.Scopes},
		resolved: r,
		compile:  x.compile,
		warnings: x.warnings,
		spans:    x.spans,
	}

	dims, joins, extra, err := without.dimensions(tableEvents, dimEntry)
	if err != nil {
		return err
	}

	conditions, err := without.conditionsFor(tableEvents, r)
	if err != nil {
		return err
	}
	conditions = append(conditions, extra...)

	st := statement{
		table: tableEvents, alias: tableEvents.alias(), joins: joins,
		dims: dims, columns: []expr{{SQL: "COUNT(DISTINCT e.user_id)"}}, conditions: conditions,
	}

	sqlText, args := st.render()

	rows, err := x.engine.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return fmt.Errorf("query: conversion denominator: %w", err)
	}
	defer rows.Close()

	totals := map[string]float64{}

	for rows.Next() {
		raw := make([]any, len(dims))
		var value sql.NullFloat64

		scanTo := make([]any, 0, len(raw)+1)
		for i := range raw {
			scanTo = append(scanTo, &raw[i])
		}
		scanTo = append(scanTo, &value)

		if err := rows.Scan(scanTo...); err != nil {
			return fmt.Errorf("query: conversion denominator: %w", err)
		}

		totals[rowKey(raw)] = value.Float64
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: conversion denominator: %w", err)
	}

	for _, row := range groups.list() {
		key := subKey(row.raw, kept)

		for len(row.components[name]) <= slot {
			row.components[name] = append(row.components[name], 0)
		}

		// Assigned rather than added: a denominator counts the same set however
		// many metrics ask for it, and adding would double the divisor the
		// moment two of them share one.
		row.components[name][slot] = totals[key]
	}

	return nil
}

// coverage warns when a metric was measured over less of the data than the
// reader will assume. Scroll depth and time on page only exist for visits whose
// tracker reported engagement, so a site that upgraded its script last week has
// a scroll depth for last week and nothing before it — and an average over
// "whatever we happened to have" is exactly the sort of number that gets quoted
// in a meeting.
func (x *executor) coverage(ctx context.Context, r Resolved, metric string, extra []expr) error {
	total, err := x.countSessions(ctx, r, []expr{{SQL: "e.name_id = ?", Args: []any{x.compile.pageviewNameID}}})
	if err != nil {
		return err
	}

	measured, err := x.countSessions(ctx, r, extra)
	if err != nil {
		return err
	}

	if measured == 0 {
		x.warnings.add(metric, WarnNoCoverage,
			"no visit in this range reported the measurement this metric needs, so it is zero rather than unknown")

		return nil
	}

	if total > 0 && measured < total {
		x.warnings.add(metric, WarnPartialBucket,
			fmt.Sprintf("measured over %d of %d visits (%.0f%%) — the rest did not report it", measured, total, 100*float64(measured)/float64(total)))
	}

	return nil
}

// countSessions counts the distinct visits matching the query's filters plus an
// extra condition. It is the shape both halves of a coverage check take.
func (x *executor) countSessions(ctx context.Context, r Resolved, extra []expr) (int64, error) {
	conditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return 0, err
	}
	conditions = append(conditions, extra...)

	st := statement{
		table: tableEvents, alias: tableEvents.alias(),
		columns:    []expr{{SQL: "COUNT(DISTINCT e.session_id)"}},
		conditions: conditions,
	}

	sqlText, args := st.render()

	var count sql.NullInt64
	if err := x.engine.db.QueryRowContext(ctx, sqlText, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("query: coverage: %w", err)
	}

	return count.Int64, nil
}

// subKey renders the part of a group key that survives goal-stripping.
func subKey(raw []any, indices []int) string {
	parts := make([]string, 0, len(indices))

	for _, index := range indices {
		if index < len(raw) {
			parts = append(parts, valueString(raw[index]))
		}
	}

	return strings.Join(parts, keySeparator)
}

// isGoalFilter reports whether a filter selects a conversion rather than
// narrowing the audience.
func isGoalFilter(f Filter, scopes map[string]string) bool {
	if f.Operator == OpHasDone {
		return true
	}

	resolved, err := resolveDimension(f.Dimension)
	if err != nil {
		return false
	}

	return isGoalDimension(resolved, scopes)
}

// isGoalDimension reports whether a dimension describes what somebody did
// rather than who they are.
//
// This is where a declared property scope earns its place. An event-scoped
// property is part of the conversion — "Purchase, plan growth" — so it is
// stripped from the denominator and the rate divides by everybody. A
// session-scoped one describes the visitor, the A/B variant they were put in,
// so it stays and the rate divides by the visitors who were in that variant.
// With no declaration every property took the first reading, which makes a
// variant's conversion rate a share of the whole audience rather than of the
// variant — the one wrong number an A/B test cannot survive.
func isGoalDimension(d dimension, scopes map[string]string) bool {
	if d.isProp() {
		return d.scopeOf(scopes) == propScopeEvent
	}

	return d.Name == "event:name"
}

// hasGoal reports whether a query names a conversion at all.
func hasGoal(q *Query, scopes map[string]string) bool {
	for _, filter := range q.Filters {
		if isGoalFilter(filter, scopes) {
			return true
		}
	}

	for _, name := range q.Dimensions {
		resolved, err := resolveDimension(name)
		if err == nil && isGoalDimension(resolved, scopes) {
			return true
		}
	}

	return false
}
