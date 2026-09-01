//
// propagg.go
// Summing, averaging and taking percentiles of a custom property's numeric values.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// The aggregate names. They are the complete set: a caller asking for anything
// else is told which ones exist rather than being handed a query that silently
// counts something else.
const (
	AggSum = "sum"
	AggAvg = "avg"
	AggMin = "min"
	AggMax = "max"
)

// propAggregate is one numeric aggregate over one custom property: the metric a
// name like `p95(event:props:load_ms)` resolves to.
//
// It is a family rather than twenty entries in the metric registry for the same
// reason `event:props:<key>` is a family in the dimension registry: the property
// is a parameter, and a registry cannot hold a row per property a customer might
// one day send.
type propAggregate struct {
	// Name is the wire name, exactly as the caller wrote it.
	Name string

	// Agg is the aggregate: one of the four names above, or empty for a
	// percentile.
	Agg string

	// Percentile is the fraction a percentile aggregate reports, between 0 and
	// 1, and zero for the four ordinary aggregates.
	Percentile float64

	// Dim is the property being aggregated.
	Dim dimension
}

// percentiles is the set of percentile aggregates. They are a fixed list rather
// than a parsed `p<n>` for any n because every one of them costs a window
// function over the matching rows, and a caller who can ask for p37 and p38
// separately has been handed a way to run the same expensive query a hundred
// times for one chart.
var percentiles = map[string]float64{
	"p50": 0.50,
	"p75": 0.75,
	"p90": 0.90,
	"p95": 0.95,
	"p99": 0.99,
}

// AggregateNames lists every aggregate a numeric property metric may use,
// sorted. The validation error prints it, so a caller who guessed `median` is
// told what to write instead in the same response.
func AggregateNames() []string {
	names := []string{AggSum, AggAvg, AggMin, AggMax}
	for name := range percentiles {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// parsePropAggregate reads a metric name of the form `agg(event:props:key)`,
// returning false for anything that is not one.
//
// The parenthesised form is deliberate. A suffix form — `event:props:price:sum`
// — cannot be parsed, because a property name may itself contain a colon and
// there is then no way to tell the property `price:sum` from the sum of
// `price`. Guessing there would silently aggregate the wrong column.
func parsePropAggregate(name string) (propAggregate, bool) {
	open := strings.IndexByte(name, '(')
	if open <= 0 || !strings.HasSuffix(name, ")") {
		return propAggregate{}, false
	}

	agg := strings.ToLower(name[:open])
	inner := name[open+1 : len(name)-1]

	parsed := propAggregate{Name: name}

	switch agg {
	case AggSum, AggAvg, AggMin, AggMax:
		parsed.Agg = agg
	default:
		fraction, ok := percentiles[agg]
		if !ok {
			return propAggregate{}, false
		}
		parsed.Percentile = fraction
	}

	resolved, err := resolveDimension(strings.TrimSpace(inner))
	if err != nil || !resolved.isProp() {
		// Reported as "not an aggregate" rather than as a bad property, so the
		// caller gets the metric registry's own message naming every metric
		// and the shape of this family, instead of a message about dimensions.
		return propAggregate{}, false
	}

	parsed.Dim = resolved

	return parsed, true
}

// propAggregateMetric builds the metric definition for one of these names.
//
// Every one of them is scopeSpecial: the values live in the cold event_details
// table and a percentile needs a window function over them, so all five shapes
// are answered by a query of their own and joined back on the group key — the
// same way the conversion rates and the money metrics are.
//
// All of them are signed. A custom property can hold a temperature, a discount
// or a balance change, and clamping a negative average to zero would report a
// month of refunds as a month of nothing.
func propAggregateMetric(name string) (metric, bool) {
	parsed, ok := parsePropAggregate(name)
	if !ok {
		return metric{}, false
	}

	definition := metric{
		Name:   parsed.Name,
		Scope:  scopeSpecial,
		Signed: true,

		// A total scales with the sample. An average, minimum, maximum or
		// percentile is calculated directly within selected event/session rows; it is not
		// inverse-rate expanded, but remains a population estimate.
		Scaled: parsed.Agg == AggSum,

		Combine: first,
	}

	if parsed.Agg == AggAvg {
		// Numerator and denominator rather than a stored mean, for the reason
		// every average in this package is: the mean of two groups' means is
		// not the mean of the groups unless they were the same size.
		definition.Combine = func(v []float64) float64 { return ratio(component(v, 0), component(v, 1)) }
	}

	return definition, true
}

// propAggregatePass answers one numeric property aggregate and adds it to the
// groups the primary statement found.
//
// The shape is inner-then-outer for all five aggregates, not just the
// percentiles. One shape means one place the property is read, one place the
// non-numeric values are excluded, and no chance of `avg` and `p50` disagreeing
// about which rows they were measured over.
func (x *executor) propAggregatePass(ctx context.Context, name string, r Resolved, groups *groupSet, keys map[int][]any) error {
	parsed, ok := parsePropAggregate(name)
	if !ok {
		return invalid("unknown metric %q", name)
	}

	if err := numberError(); err != nil {
		return &Error{Message: err.Error()}
	}

	inner, dims, err := x.propValueQuery(ctx, parsed, r, keys)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(dims))
	for i := range dims {
		names = append(names, fmt.Sprintf("d%d", i))
	}

	var (
		sqlText string
		targets []target
	)

	if parsed.Percentile > 0 {
		sqlText = percentileSQL(inner.SQL, names)

		// The two bind parameters the rank comparison needs come after every
		// argument the inner statement already carries, because that is the
		// order they appear in the rendered SQL.
		inner.Args = append(inner.Args, parsed.Percentile, parsed.Percentile)
		targets = []target{{metric: name, slot: 0, column: 0}}
	} else {
		sqlText, targets = aggregateSQL(inner.SQL, names, parsed.Agg, name)
	}

	if _, err := x.readRows(ctx, sqlText, inner.Args, len(dims), len(targets), groups, targets, false); err != nil {
		return err
	}

	return x.propCoverage(ctx, r, parsed)
}

// propValueQuery renders the inner statement: one row per measured thing, with
// the property's parsed number in the last column.
//
// What "one thing" means is the property's declared scope, and this is where
// that declaration earns its place. An event-scoped property describes one hit,
// so a sum adds up the hits that carried it. A session-scoped property has one
// value per visit by declaration, so the rows are collapsed to one per visit
// first — without that, a visit that viewed six pages would contribute its
// order value six times, and the total would be a multiple of the truth that
// looks entirely plausible.
func (x *executor) propValueQuery(_ context.Context, parsed propAggregate, r Resolved, keys map[int][]any) (expr, []compiledDim, error) {
	source := tableEvents
	if parsed.Dim.sessionScoped(x.plan.Scopes) {
		source = tableSessions
	}

	dims, joins, extra, err := x.dimensions(source, dimEntry)
	if err != nil {
		return expr{}, nil, err
	}

	conditions, err := x.conditionsFor(source, r)
	if err != nil {
		return expr{}, nil, err
	}

	conditions = append(conditions, extra...)
	conditions = append(conditions, x.restrictions(dims, keys)...)

	var value expr
	if source == tableSessions {
		// One sessions row is one visit, so the correlated lookup is evaluated
		// once and the visit is placed by started_at before the outer aggregate
		// groups it. Grouping event rows by time first would let one visit pay
		// into every bucket it happened to span.
		visit := sessionPropExpr(parsed.Dim, tableSessions, "s")
		value = expr{SQL: NumberFunction + "(" + visit.SQL + ")", Args: visit.Args}
	} else {
		value = propNumberExpr(parsed.Dim)
		joins = appendJoin(joins, detailsJoin("e"))
	}

	st := statement{
		table: source, alias: source.alias(), joins: joins,
		dims: dims, columns: []expr{value}, conditions: conditions,
		ungrouped: true,
	}

	sqlText, args := x.renderStatement(st)

	return expr{SQL: sqlText, Args: args}, dims, nil
}

// propNumberExpr reads one event's own value for a property, as a number.
func propNumberExpr(d dimension) expr {
	return expr{
		SQL:  NumberFunction + "(json_extract(ed.props, ?))",
		Args: []any{d.jsonPath()},
	}
}

// aggregateSQL renders the outer statement for sum, average, minimum and
// maximum. The average selects its two components rather than one, so the
// division happens in Go where a zero denominator is a decision rather than a
// NULL.
func aggregateSQL(inner string, names []string, agg, metricName string) (string, []target) {
	columns := []string{sqlAggregate(agg) + "(" + propValueColumn + ") AS a0"}
	targets := []target{{metric: metricName, slot: 0, column: 0}}

	if agg == AggAvg {
		columns = []string{
			"SUM(" + propValueColumn + ") AS a0",
			"COUNT(" + propValueColumn + ") AS a1",
		}
		targets = append(targets, target{metric: metricName, slot: 1, column: 1})
	}

	sqlText := "SELECT " + strings.Join(append(append([]string{}, names...), columns...), ", ") +
		" FROM (" + inner + ")"

	if len(names) > 0 {
		sqlText += " GROUP BY " + strings.Join(names, ", ")
	}

	return sqlText, targets
}

// sqlAggregate maps an aggregate name onto the SQL function that computes it.
func sqlAggregate(agg string) string {
	switch agg {
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	default:
		return "SUM"
	}
}

// percentileSQL renders the outer statements for a percentile: rank the values
// inside each group, then keep the one row at the requested rank.
//
// It is the nearest-rank definition — the smallest value at or above the
// fraction of the ordered set — because it always returns a value that was
// actually measured. An interpolated percentile invents a number between two
// real ones, which for a load time means reporting a duration no visitor
// experienced.
//
// The rank is selected without a ceiling function. SQLite's ceil() is a
// compile-time option no driver is obliged to enable, and `rn` is the ceiling
// of `p * n` exactly when it is the one integer in the half-open interval
// [p*n, p*n + 1) — which is two comparisons and works everywhere.
func percentileSQL(inner string, names []string) string {
	partition := ""
	if len(names) > 0 {
		partition = "PARTITION BY " + strings.Join(names, ", ") + " "
	}

	ranked := "SELECT " + strings.Join(append(append([]string{}, names...), propValueColumn,
		"ROW_NUMBER() OVER ("+partition+"ORDER BY "+propValueColumn+") AS rn",
		"COUNT(*) OVER ("+partition+") AS n"), ", ") +
		" FROM (" + inner + ") WHERE " + propValueColumn + " IS NOT NULL"

	return "SELECT " + strings.Join(append(append([]string{}, names...), propValueColumn+" AS a0"), ", ") +
		" FROM (" + ranked + ") WHERE rn >= ? * n AND rn < ? * n + 1"
}

// propValueColumn is the name the inner statement gives the parsed number. It
// is the statement renderer's own alias for the first non-dimension column, and
// naming it here rather than writing "m0" in four places is what stops the two
// drifting the day the renderer changes its scheme.
const propValueColumn = "m0"

// propCoverage says out loud what the aggregate could not measure.
//
// Two things are worth saying and neither is visible in the number itself. A
// property that holds a number on most events and a label on the rest produces
// an average over the numeric ones only, and the reader has no way to know some
// rows were left out — this is the case the incumbent has no answer for at all,
// because it has no numeric aggregation. And a property that turned out to hold
// no numbers whatsoever answers zero, which looks exactly like a real zero.
func (x *executor) propCoverage(ctx context.Context, r Resolved, parsed propAggregate) error {
	if x.comparison {
		return nil
	}

	source := tableEvents
	joins := []string{detailsJoin("e")}
	raw := expr{SQL: "json_extract(ed.props, ?)", Args: []any{parsed.Dim.jsonPath()}}
	value := propNumberExpr(parsed.Dim)

	if parsed.Dim.sessionScoped(x.plan.Scopes) {
		source = tableSessions
		joins = nil
		raw = sessionPropExpr(parsed.Dim, tableSessions, "s")
		value = expr{SQL: NumberFunction + "(" + raw.SQL + ")", Args: raw.Args}
	}

	conditions, err := x.conditionsFor(source, r)
	if err != nil {
		return err
	}

	conditions = append(conditions, expr{SQL: raw.SQL + " IS NOT NULL", Args: raw.Args})

	counts := statement{
		table: source, alias: source.alias(), joins: joins,
		columns: []expr{
			{SQL: "COUNT(" + value.SQL + ")", Args: value.Args},
			{SQL: "COUNT(*)", Args: nil},
		},
		conditions: conditions,
	}

	sqlText, args := x.renderStatement(counts)

	var numeric, total int64
	if err := x.engine.db.QueryRowContext(ctx, sqlText, args...).Scan(&numeric, &total); err != nil {
		return fmt.Errorf("query: property coverage: %w", err)
	}
	if x.sampling != nil {
		if x.sampling.PropertyCoverage == nil {
			x.sampling.PropertyCoverage = map[string]SampledPropertyCoverage{}
		}
		x.sampling.PropertyCoverage[parsed.Name] = SampledPropertyCoverage{
			ObservedValues:         total,
			ObservedNumericValues:  numeric,
			EstimatedValues:        sampledPopulationEstimate(total, x.sampling.Rate),
			EstimatedNumericValues: sampledPopulationEstimate(numeric, x.sampling.Rate),
		}
		if numeric < 100 {
			x.sampling.Sparse = true
		}
	}

	if total == 0 {
		if x.sampling != nil {
			x.warnings.add(parsed.Name, WarnNoCoverage,
				fmt.Sprintf("the selected sample observed no value of %q, so the estimate is zero with no sampled property coverage", parsed.Dim.PropKey))
			return nil
		}
		x.warnings.add(parsed.Name, WarnNoCoverage,
			fmt.Sprintf("no event in this range carried %q, so this is zero rather than unknown", parsed.Dim.PropKey))

		return nil
	}

	if numeric == 0 {
		if x.sampling != nil {
			x.warnings.add(parsed.Name, WarnNotNumeric,
				fmt.Sprintf("the selected sample observed %d values of %q and none were numeric; estimated full-range coverage is about %d values",
					total, parsed.Dim.PropKey, sampledPopulationEstimate(total, x.sampling.Rate)))
			return nil
		}
		x.warnings.add(parsed.Name, WarnNotNumeric,
			fmt.Sprintf("every one of the %d values of %q in this range is text rather than a number, so nothing could be aggregated",
				total, parsed.Dim.PropKey))

		return nil
	}

	if numeric < total {
		if x.sampling != nil {
			x.warnings.add(parsed.Name, WarnNotNumeric,
				fmt.Sprintf("the selected sample observed %d numeric values among %d values of %q; estimated full-range coverage is about %d numeric values among %d values",
					numeric, total, parsed.Dim.PropKey,
					sampledPopulationEstimate(numeric, x.sampling.Rate), sampledPopulationEstimate(total, x.sampling.Rate)))
			return nil
		}
		x.warnings.add(parsed.Name, WarnNotNumeric,
			fmt.Sprintf("measured over %d of the %d values of %q — the other %d are text and were left out rather than counted as zero",
				numeric, total, parsed.Dim.PropKey, total-numeric))
	}

	return nil
}

// sampledPopulationEstimate expands an observed property-value count by the
// selected rate. It deliberately returns an estimate separate from observed
// coverage because property sparsity is not implied by total fact-row counts.
func sampledPopulationEstimate(observed int64, rate float64) int64 {
	if observed <= 0 || rate <= 0 {
		return 0
	}

	return int64(math.Ceil(float64(observed) / rate))
}

// detailsJoin is the join that brings the cold properties table into an event
// statement. It is one function so that a breakdown by a property and an
// aggregate of one produce the identical join string and the deduplication in
// the statement builder actually deduplicates.
func detailsJoin(alias string) string {
	return "JOIN event_details ed ON ed.event_id = " + alias + ".id"
}

// appendJoin adds a join unless the statement already has it.
func appendJoin(joins []string, join string) []string {
	for _, existing := range joins {
		if existing == join {
			return joins
		}
	}

	return append(joins, join)
}
