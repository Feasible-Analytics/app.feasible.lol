//
// executor.go
// Building the statements a query needs, running them, and merging them by group key.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// keyRestrictionLimit is how many group keys are worth naming in a follow-up
// statement. Above it the second query reads the same window unrestricted,
// which is cheaper than a bind parameter per key.
const keyRestrictionLimit = 500

// executor runs one query over one date range. The comparison period runs
// through a second executor with the same plan, which is what guarantees the
// two periods are counted exactly the same way.
type executor struct {
	engine   *Engine
	query    *Query
	plan     *plan
	resolved Resolved
	compile  compileContext
	warnings *warningSet

	spans     []offsetSpan
	pushDown  bool
	truncated bool

	// segments is what the router decided, kept so the response can name where
	// the numbers came from without asking the router a second time.
	segments []Segment

	// The money metrics share one pass and one set of exchange rates. They are
	// held here rather than recomputed per metric so that three metrics over
	// the same events can never be converted at two different rates.
	revenueDone map[int64]bool
	rates       map[string]float64
	currency    string

	// comparison marks the executor that answers the earlier period. It never
	// paginates: its rows are looked up by key, not read off a page.
	comparison bool

	// The summary read this query maps onto, worked out once. The router has
	// already decided the same thing; recomputing it here costs nothing and
	// keeps the router's signature free of a type only the reader needs.
	rollupRead    rollupRead
	rollupUsable  bool
	rollupPlanned bool

	// gaps collects what imported history could not answer, keyed by the
	// dimension that could not be answered.
	gaps map[string]ImportGap
}

// dimMode selects which session column an event-scoped page dimension maps to.
// Entry is the default and is what a bounce rate is measured over; exit exists
// for exit rate, which is the one metric that counts the other end of a visit.
type dimMode int

const (
	dimEntry dimMode = iota
	dimExit
)

// compiledDim is one group-by dimension, ready to go into a statement.
type compiledDim struct {
	dim   dimension
	sql   expr
	alias string
}

// statement is one rendered aggregate query.
type statement struct {
	table table
	alias string

	// nameOverride replaces the fact table's name, which is how a summary read
	// reuses this renderer. It is a field rather than a second renderer so
	// that the two paths can never drift in how they order their arguments.
	nameOverride string

	joins      []string
	dims       []compiledDim
	columns    []expr
	conditions []expr
	orderBy    string
	limit      int
	offset     int
	limited    bool

	// groupExtra are extra GROUP BY terms that are not select columns. Scroll
	// depth needs one: its inner query collapses to one row per session before
	// the outer query averages, and the session id is grouped on without ever
	// being returned.
	groupExtra []string
}

// render turns a statement into SQL and its arguments. The argument order
// follows the statement's own order — select list, joins, where, limit — which
// is the only ordering that stays correct as clauses are added.
func (s statement) render() (string, []any) {
	b := &builder{}

	b.add("SELECT ")

	first := true
	for _, dimension := range s.dims {
		if !first {
			b.add(", ")
		}
		first = false
		b.addExpr(dimension.sql).add(" AS " + dimension.alias)
	}

	for i, column := range s.columns {
		if !first {
			b.add(", ")
		}
		first = false
		b.addExpr(column).add(fmt.Sprintf(" AS m%d", i))
	}

	name := s.table.name()
	if s.nameOverride != "" {
		name = s.nameOverride
	}

	b.add(" FROM " + name + " " + s.alias)

	for _, join := range s.joins {
		b.add(" " + join)
	}

	where := and(s.conditions)
	b.add(" WHERE ").addExpr(where)

	if len(s.dims) > 0 || len(s.groupExtra) > 0 {
		names := make([]string, 0, len(s.dims)+len(s.groupExtra))
		for _, dimension := range s.dims {
			names = append(names, dimension.alias)
		}
		names = append(names, s.groupExtra...)

		b.add(" GROUP BY " + strings.Join(names, ", "))
	}

	if s.orderBy != "" {
		b.add(" ORDER BY " + s.orderBy)
	}

	if s.limited {
		b.add(" LIMIT ? OFFSET ?", int64(s.limit), int64(s.offset))
	}

	return b.String(), b.Args()
}

// zoneSpans returns the timezone offsets in force across this range, computing
// them once per executor because the walk that finds them is the same for every
// statement the query runs.
func (x *executor) zoneSpans() []offsetSpan {
	if x.spans == nil {
		x.spans = zoneOffsets(x.resolved.Location, x.resolved.Start, x.resolved.End)
	}

	return x.spans
}

// dimensions compiles the query's group-by dimensions for one table, together
// with the joins and extra conditions they need.
func (x *executor) dimensions(t table, mode dimMode) ([]compiledDim, []string, []expr, error) {
	var (
		compiled   []compiledDim
		joins      []string
		conditions []expr
		seenJoin   = map[string]bool{}
	)

	addJoin := func(join string) {
		if seenJoin[join] {
			return
		}
		seenJoin[join] = true
		joins = append(joins, join)
	}

	for i, d := range x.plan.Dimensions {
		alias := t.alias()

		switch {
		case d.Time:
			column := alias + "." + t.timeColumn()
			compiled = append(compiled, compiledDim{
				dim: d, alias: fmt.Sprintf("d%d", i),
				sql: bucketExpr(column, x.resolved.Interval, x.zoneSpans()),
			})

		case d.sessionScoped(x.plan.Scopes):
			// A property declared session-scoped describes the whole visit, so
			// it is read once per visit and every event of that visit carries
			// the visit's value. Reading each event's own property instead
			// would put an event that did not repeat the property into its own
			// group, and the denominator of anything measured against it would
			// count only the events that happened to mention it.
			value := sessionPropExpr(d, t.propCorrelation())

			compiled = append(compiled, compiledDim{
				dim: d, alias: fmt.Sprintf("d%d", i), sql: value,
			})

			conditions = append(conditions, expr{
				SQL: value.SQL + " IS NOT NULL", Args: value.Args,
			})

		case d.isProp():
			if t != tableEvents {
				return nil, nil, nil, invalid("%q can only be grouped at event grain — "+
					"register it as a session-scoped property if it describes the whole visit", d.Name)
			}

			// Properties live in the cold table, so a breakdown by one is the
			// single case where a query has to touch it. Events with no such
			// property are excluded rather than gathered into a null bucket:
			// a breakdown of "plan" is a list of plans.
			addJoin("JOIN event_details ed ON ed.event_id = " + alias + ".id")
			conditions = append(conditions, expr{
				SQL: "json_extract(ed.props, ?) IS NOT NULL", Args: []any{d.jsonPath()},
			})

			compiled = append(compiled, compiledDim{
				dim: d, alias: fmt.Sprintf("d%d", i),
				sql: expr{SQL: "json_extract(ed.props, ?)", Args: []any{d.jsonPath()}},
			})

		case t == tableEvents:
			if d.EventColumn != "" {
				compiled = append(compiled, compiledDim{
					dim: d, alias: fmt.Sprintf("d%d", i),
					sql: expr{SQL: x.compile.pathColumn(alias, d.EventColumn, d)},
				})
				break
			}

			// Entry and exit page exist only on sessions. The join is to a
			// fact table on an indexed integer and is many-to-one, so it adds
			// no rows; it is not a dim_* join, which is the thing that must
			// never happen before aggregating.
			addJoin("JOIN sessions sj ON sj.id = " + alias + ".session_id")
			compiled = append(compiled, compiledDim{
				dim: d, alias: fmt.Sprintf("d%d", i),
				sql: expr{SQL: x.compile.pathColumn("sj", d.SessionColumn, d)},
			})

		default:
			column, err := x.sessionColumn(d, mode)
			if err != nil {
				return nil, nil, nil, err
			}

			compiled = append(compiled, compiledDim{
				dim: d, alias: fmt.Sprintf("d%d", i),
				sql: expr{SQL: x.compile.pathColumn(alias, column, d)},
			})
		}
	}

	return compiled, joins, conditions, nil
}

// sessionPropExpr reads a session-scoped property at visit grain: the first
// value any event of the visit carried.
//
// First rather than last because a session-scoped property is declared to have
// one value per visit, so any event carrying it carries the same one — and
// where a site breaks that promise, the first value is the one that was true
// when the visit started, which is what every other visit-grain attribute on
// the row already is.
//
// It is a correlated subquery, so it costs one index probe per row rather than
// reading a column. That is the price of a property the customer declared to
// be about the visit, and it is only paid by the queries that name one.
func sessionPropExpr(d dimension, correlation string) expr {
	path := d.jsonPath()

	return expr{
		SQL: "(SELECT json_extract(ped.props, ?) FROM events pe JOIN event_details ped ON ped.event_id = pe.id" +
			" WHERE pe.session_id = " + correlation + " AND json_extract(ped.props, ?) IS NOT NULL" +
			" ORDER BY pe.timestamp, pe.id LIMIT 1)",
		Args: []any{path, path},
	}
}

// sessionColumn picks the sessions column that answers a dimension. An
// event-scoped page becomes the entry page, or the exit page for the one metric
// that measures the end of a visit; anything else event-scoped has no session
// grain answer and was already refused by the table decider.
func (x *executor) sessionColumn(d dimension, mode dimMode) (string, error) {
	if mode == dimExit {
		if d.Name == "event:page" || d.Name == "visit:exit_page" {
			return "exit_page_id", nil
		}
	}

	if d.SessionColumn != "" {
		return d.SessionColumn, nil
	}

	if d.EntryColumn != "" {
		return d.EntryColumn, nil
	}

	return "", invalid("%q cannot be grouped at visit grain", d.Name)
}

// metricsFor returns the ordinary metrics this table is counting.
func (x *executor) metricsFor(t table) []string {
	var names []string

	for _, name := range x.query.Metrics {
		if assigned, ok := x.plan.MetricTable[name]; ok && assigned == t {
			names = append(names, name)
		}
	}

	return names
}

// target says where one column of a statement's result belongs: which metric it
// feeds and which of that metric's components it is. It is explicit rather than
// positional because a composite metric collects its components from two
// different statements, and a positional scheme would silently put the second
// statement's number in the first slot.
type target struct {
	metric string
	slot   int
	column int
}

// componentsFor builds the aggregate expressions for a set of metrics, where
// each column belongs, and the aliases the ordering can name them by.
func (x *executor) componentsFor(names []string, t table) ([]expr, []target, map[string][]string) {
	var (
		columns []expr
		targets []target
	)

	aliases := map[string][]string{}

	for _, name := range names {
		definition, ok := metricByName(name)
		if !ok || definition.Components == nil {
			continue
		}

		for slot, component := range definition.Components(x.compile, t, t.alias()) {
			aliases[name] = append(aliases[name], fmt.Sprintf("m%d", len(columns)))
			targets = append(targets, target{metric: name, slot: slot, column: len(columns)})
			columns = append(columns, component)
		}
	}

	return columns, targets, aliases
}

// conditionsFor builds every condition for one table: the base window and the
// compiled filters. It also records how an event-scoped filter had to be
// re-scoped, which is what becomes a metric warning.
func (x *executor) conditionsFor(t table, r Resolved) ([]expr, error) {
	where := newWhereBuilder(t, x.compile, x.plan.Scopes, x.query.SiteIDs, r)

	conditions := where.base(x.query)

	filters, err := where.compile(x.query.Filters)
	if err != nil {
		return nil, err
	}

	if t == tableSessions {
		if where.entryScoped {
			x.warnSessionMetrics(WarnEntryScoped, entryScopeWarning)
		}
		if where.semiJoined {
			x.warnSessionMetrics(WarnSessionScoped, sessionSemiJoinWarning)
		}
	}

	return append(conditions, filters...), nil
}

// warnSessionMetrics attaches a warning to every session-scoped metric in the
// query. It is every one of them rather than the query as a whole because the
// response is read per metric: a client greys out the figure it cannot trust,
// not the whole panel.
func (x *executor) warnSessionMetrics(code, message string) {
	for _, name := range x.query.Metrics {
		definition, ok := metricByName(name)
		if !ok {
			continue
		}

		if definition.Scope == scopeSession || (definition.Scope == scopeEither && x.plan.MetricTable[name] == tableSessions) {
			x.warnings.add(name, code, message)
		}
	}
}

// execute runs every statement the query needs and returns the merged groups.
func (x *executor) execute(ctx context.Context, restrict map[int][]any) (*groupSet, error) {
	segments := x.engine.router().Route(x.query, x.resolved)

	// A range can only be answered from more than one source if every metric
	// adds up across the split. Counting distinct visitors does not, so a query
	// asking for them is read from one source over the whole range.
	//
	// A summary-backed split is the exception: those buckets carry the
	// carry-over counts that make a distinct count re-aggregate exactly, and
	// the seam between the last summarised day and today is corrected below.
	if len(segments) > 1 && !rollupBacked(segments) && !Splittable(x.query, x.plan) {
		segments = []Segment{{Range: x.resolved, Source: SourceRaw}}
	}

	// A split range is merged in memory, so the database cannot be the thing
	// that ordered and paginated it. Neither can a gap-filled time series,
	// whose empty buckets do not exist in any table, nor a query that includes
	// imported roll-ups: those are merged in memory after the fact tables have
	// been read, exactly like a split range.
	x.pushDown = x.canPushDown() && len(segments) == 1 && !timeOnly(x.query) &&
		!x.comparison && !x.query.Include.Imports
	x.segments = segments

	groups := newGroupSet()

	for _, segment := range segments {
		if segment.Source == SourceRollup {
			if err := x.rollupPass(ctx, x.plan.Primary, segment, groups, restrict, true); err != nil {
				return nil, err
			}

			continue
		}

		if err := x.primaryPass(ctx, segment.Range, groups, restrict); err != nil {
			return nil, err
		}
	}

	// Time on page is only measured for visits whose tracker reported
	// engagement, so it is averaged over a subset the reader cannot see. The
	// composites raise the same warning from their own queries; this is the one
	// ordinary metric that needs it.
	if !x.comparison {
		for _, name := range x.query.Metrics {
			if name != "time_on_page" {
				continue
			}

			if err := x.coverage(ctx, x.resolved, name, []expr{
				{SQL: "e.name_id = ?", Args: []any{x.compile.engagementNameID}},
			}); err != nil {
				return nil, err
			}
		}
	}

	keys := restrict
	if keys == nil {
		keys = x.keyRestriction(groups)
	}

	for _, segment := range segments {
		if segment.Source == SourceRollup {
			if x.plan.HasSecondary {
				if err := x.rollupPass(ctx, x.plan.Secondary, segment, groups, keys, false); err != nil {
					return nil, err
				}
			}

			continue
		}

		if x.plan.HasSecondary {
			if err := x.secondaryPass(ctx, segment.Range, groups, keys); err != nil {
				return nil, err
			}
		}

		for _, name := range x.plan.Specials {
			if err := x.specialPass(ctx, name, segment.Range, groups, keys); err != nil {
				return nil, err
			}
		}
	}

	// The last summarised day and today are two reads of the same visitors and
	// the same visits, so anything present on both sides of the boundary has
	// now been counted twice. This removes it.
	if rollupBacked(segments) && len(segments) > 1 {
		if err := x.seamPass(ctx, segments[len(segments)-1].Range, groups); err != nil {
			return nil, err
		}
	}

	// Imported history is read over the executor's own range rather than per
	// segment: the segments exist to split raw days from rolled-up ones, and an
	// imported row is already a rolled-up day. Running it here also means the
	// comparison executor runs it too, which is what stops a comparison
	// measuring native-only against a headline that included imports — the
	// mistake behind an incumbent reporting +450% where the truth was -34%.
	if err := x.importedPass(ctx, x.resolved, groups, restrict); err != nil {
		return nil, err
	}

	return groups, nil
}

// primaryPass runs the statement that defines the result set. Every other
// statement adds numbers to the groups this one found, which is what makes the
// page a single, stable result set rather than several independently paginated
// ones that disagree about which rows exist.
func (x *executor) primaryPass(ctx context.Context, r Resolved, groups *groupSet, restrict map[int][]any) error {
	t := x.plan.Primary

	dims, joins, extra, err := x.dimensions(t, dimEntry)
	if err != nil {
		return err
	}

	conditions, err := x.conditionsFor(t, r)
	if err != nil {
		return err
	}
	conditions = append(conditions, extra...)
	conditions = append(conditions, x.restrictions(dims, restrict)...)

	names := x.metricsFor(t)
	columns, targets, aliases := x.componentsFor(names, t)

	// A query whose metrics all live elsewhere still needs one aggregate here,
	// or SQLite has nothing to group and returns a row per raw record.
	if len(columns) == 0 {
		columns = append(columns, expr{SQL: "COUNT(*)"})
	}

	st := statement{
		table: t, alias: t.alias(), joins: joins,
		dims: dims, columns: columns, conditions: conditions,
	}

	switch {
	case x.pushDown:
		st.orderBy = x.orderSQL(dims, aliases)
		st.limit, st.offset, st.limited = x.query.Pagination.Limit, x.query.Pagination.Offset, true

	case len(dims) > 0:
		// Without push-down the group set is capped, and the cap keeps the
		// biggest groups: ordering by the first metric makes a truncated
		// answer the top of the list rather than an arbitrary slice of it.
		st.orderBy = fallbackOrder(dims, aliases, names)
		st.limit, st.offset, st.limited = x.engine.maxGroups()+1, 0, true
	}

	rows, err := x.scan(ctx, st, groups, targets)
	if err != nil {
		return err
	}

	if st.limited && !x.pushDown && rows > x.engine.maxGroups() {
		x.truncated = true
	}

	return nil
}

// secondaryPass adds the other fact table's metrics onto the groups the primary
// pass found. It never creates a group: the primary table defines which rows
// exist, so a page cannot gain rows between one statement and the next.
func (x *executor) secondaryPass(ctx context.Context, r Resolved, groups *groupSet, keys map[int][]any) error {
	t := x.plan.Secondary

	dims, joins, extra, err := x.dimensions(t, dimEntry)
	if err != nil {
		return err
	}

	conditions, err := x.conditionsFor(t, r)
	if err != nil {
		return err
	}
	conditions = append(conditions, extra...)
	conditions = append(conditions, x.restrictions(dims, keys)...)

	names := x.metricsFor(t)
	if len(names) == 0 {
		return nil
	}

	columns, targets, _ := x.componentsFor(names, t)

	st := statement{
		table: t, alias: t.alias(), joins: joins,
		dims: dims, columns: columns, conditions: conditions,
	}

	_, err = x.merge(ctx, st, groups, targets)

	return err
}

// restrictions narrows a follow-up statement to the group keys the primary pass
// found. It is what keeps the second query proportional to the page rather than
// to the whole table.
func (x *executor) restrictions(dims []compiledDim, keys map[int][]any) []expr {
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

// keyRestriction collects the distinct values of each dimension in the result
// so far, when there are few enough of them to be worth naming.
func (x *executor) keyRestriction(groups *groupSet) map[int][]any {
	rows := groups.list()
	if len(rows) == 0 || len(rows) > keyRestrictionLimit || len(x.plan.Dimensions) == 0 {
		return nil
	}

	keys := map[int][]any{}

	for i := range x.plan.Dimensions {
		seen := map[string]bool{}

		for _, row := range rows {
			if i >= len(row.raw) {
				continue
			}

			value := row.raw[i]
			label := valueString(value)

			if seen[label] {
				continue
			}
			seen[label] = true

			keys[i] = append(keys[i], value)
		}
	}

	return keys
}

// scan runs a statement and creates the groups it returns.
func (x *executor) scan(ctx context.Context, st statement, groups *groupSet, targets []target) (int, error) {
	sqlText, args := st.render()

	return x.readRows(ctx, sqlText, args, len(st.dims), len(st.columns), groups, targets, true)
}

// merge runs a statement and adds its numbers to groups that already exist.
func (x *executor) merge(ctx context.Context, st statement, groups *groupSet, targets []target) (int, error) {
	sqlText, args := st.render()

	return x.readRows(ctx, sqlText, args, len(st.dims), len(st.columns), groups, targets, false)
}

// readRows is the one place a statement meets the database. Components are
// added rather than assigned, so a range answered from two segments sums
// correctly and a metric fed by two statements never overwrites itself.
//
// The create flag is what makes pagination stable: only the primary statement
// may introduce a group, so a follow-up query can add numbers to the page but
// can never add rows to it.
func (x *executor) readRows(ctx context.Context, sqlText string, args []any, dims, columns int, groups *groupSet, targets []target, create bool) (int, error) {
	rows, err := x.engine.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return 0, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	count := 0

	for rows.Next() {
		raw := make([]any, dims)
		values := make([]sql.NullFloat64, columns)

		scanTo := make([]any, 0, dims+columns)
		for i := range raw {
			scanTo = append(scanTo, &raw[i])
		}
		for i := range values {
			scanTo = append(scanTo, &values[i])
		}

		if err := rows.Scan(scanTo...); err != nil {
			return 0, fmt.Errorf("query: %w", err)
		}

		count++

		key := rowKey(raw)

		row, ok := groups.rows[key]
		if !ok {
			if !create {
				continue
			}
			row = groups.upsert(key, raw)
		}

		for _, t := range targets {
			if t.column < 0 || t.column >= len(values) {
				continue
			}

			for len(row.components[t.metric]) <= t.slot {
				row.components[t.metric] = append(row.components[t.metric], 0)
			}

			row.components[t.metric][t.slot] += values[t.column].Float64
		}
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("query: %w", err)
	}

	return count, nil
}

// canPushDown reports whether the ordering can be done by the database. It can
// when every sort key is a metric counted on the primary table or a dimension
// whose stored value is already the string the caller sees — a time bucket or a
// property. An interned dimension is sorted by name rather than by id, and the
// name only exists after the ids are resolved, so those take the in-memory
// path.
func (x *executor) canPushDown() bool {
	for _, order := range x.query.OrderBy {
		if definition, ok := metricByName(order.Key); ok {
			if definition.Scope == scopeSpecial {
				return false
			}
			if x.plan.MetricTable[order.Key] != x.plan.Primary {
				return false
			}
			continue
		}

		resolved, err := resolveDimension(order.Key)
		if err != nil {
			return false
		}

		if !resolved.Time && !resolved.isProp() {
			return false
		}
	}

	return true
}

// orderSQL renders the ORDER BY for the push-down path.
func (x *executor) orderSQL(dims []compiledDim, aliases map[string][]string) string {
	var parts []string

	for _, order := range x.query.OrderBy {
		direction := " ASC"
		if order.Descending {
			direction = " DESC"
		}

		if _, ok := metricByName(order.Key); ok {
			parts = append(parts, metricOrderExpr(aliases[order.Key])+direction)
			continue
		}

		for _, dimension := range dims {
			if dimension.dim.Name == order.Key {
				parts = append(parts, dimension.alias+direction)
				break
			}
		}
	}

	// A tie broken differently on each page is how page two repeats a row from
	// page one, so the group key is always the last sort key.
	for _, dimension := range dims {
		parts = append(parts, dimension.alias+" ASC")
	}

	return strings.Join(parts, ", ")
}

// fallbackOrder is the ordering used when the group set has to be capped. It
// puts the biggest groups first so that a truncated answer is the head of the
// list rather than an arbitrary slice of it.
func fallbackOrder(dims []compiledDim, aliases map[string][]string, names []string) string {
	var parts []string

	if len(names) > 0 {
		if columns := aliases[names[0]]; len(columns) > 0 {
			parts = append(parts, metricOrderExpr(columns)+" DESC")
		}
	}

	for _, dimension := range dims {
		parts = append(parts, dimension.alias+" ASC")
	}

	return strings.Join(parts, ", ")
}

// metricOrderExpr renders a metric as something ORDER BY can use. A metric with
// one component sorts on it; a ratio sorts on the division, because sorting on
// the numerator would rank a page with two bounces out of two below one with
// three out of a thousand.
func metricOrderExpr(columns []string) string {
	switch len(columns) {
	case 0:
		return "1"
	case 1:
		return columns[0]
	default:
		return "(1.0 * " + columns[0] + " / NULLIF(" + columns[1] + ", 0))"
	}
}

// metricValues computes one group's metrics, in the order the request asked for
// them. Clamping happens here, at the last moment before a number leaves the
// engine, so there is exactly one place a nonsense rate can escape from.
func (x *executor) metricValues(row *groupRow) []float64 {
	values := make([]float64, 0, len(x.query.Metrics))

	for _, name := range x.query.Metrics {
		definition, ok := metricByName(name)
		if !ok {
			values = append(values, 0)
			continue
		}

		components := row.components[name]

		var value float64
		if definition.Combine != nil {
			value = definition.Combine(components)
		} else if len(components) > 0 {
			value = components[0]
		}

		if definition.Scaled && x.query.SampleRate > 0 && x.query.SampleRate < 1 {
			value /= x.query.SampleRate
		}

		values = append(values, round(clamp(value, definition.Percentage, definition.Signed)))
	}

	return values
}

// finalRow is one response row before it is serialised: its labels and its
// computed metrics, which is everything the ordering needs.
type finalRow struct {
	labels []string
	values []float64
}

// finalise turns the merged groups into the response rows: gap-filled where a
// graph needs it, ordered, paginated, and labelled.
func (x *executor) finalise(ctx context.Context, groups *groupSet) ([]Row, int, error) {
	if timeOnly(x.query) {
		x.fillBuckets(groups)
	}

	rows := groups.list()

	labels, err := x.resolveLabels(ctx, rows)
	if err != nil {
		return nil, 0, err
	}

	final := make([]finalRow, 0, len(rows))
	for i, row := range rows {
		final = append(final, finalRow{labels: labels[i], values: x.metricValues(row)})
	}

	total := len(final)

	if !x.pushDown {
		x.sortRows(final)

		offset := x.query.Pagination.Offset
		if offset > len(final) {
			offset = len(final)
		}

		end := offset + x.query.Pagination.Limit
		if end > len(final) {
			end = len(final)
		}

		final = final[offset:end]
	}

	if x.truncated {
		for _, name := range x.query.Metrics {
			x.warnings.add(name, WarnGroupsTruncated,
				fmt.Sprintf("more than %d groups matched, so this answer covers the largest ones only — narrow the filters or the date range", x.engine.maxGroups()))
		}
	}

	out := make([]Row, 0, len(final))
	for _, row := range final {
		out = append(out, Row{Metrics: row.values, Dimensions: row.labels})
	}

	if x.pushDown && x.query.Include.TotalRows {
		// A page cut by the database does not know how many groups it came
		// from, so it is counted again — from whichever source answered the
		// page, or the count would be a full raw scan behind a query that
		// avoided one.
		counted, err := x.countGroupsIn(ctx)
		if err != nil {
			return nil, 0, err
		}
		total = counted
	}

	return out, total, nil
}

// sortRows applies the request's ordering in memory. It is the path taken
// whenever the database could not do it: an interned dimension sorted by name,
// a gap-filled series, or an ordering by a composite metric.
//
// The group key breaks every tie, which is what stops page two from repeating a
// row from page one when two groups have identical numbers.
func (x *executor) sortRows(rows []finalRow) {
	metricIndex := map[string]int{}
	for i, name := range x.query.Metrics {
		metricIndex[name] = i
	}

	dimensionIndex := map[string]int{}
	for i, d := range x.plan.Dimensions {
		dimensionIndex[d.Name] = i
	}

	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]

		for _, order := range x.query.OrderBy {
			if index, ok := metricIndex[order.Key]; ok && index < len(left.values) && index < len(right.values) {
				if left.values[index] == right.values[index] {
					continue
				}
				if order.Descending {
					return left.values[index] > right.values[index]
				}
				return left.values[index] < right.values[index]
			}

			if index, ok := dimensionIndex[order.Key]; ok && index < len(left.labels) && index < len(right.labels) {
				if left.labels[index] == right.labels[index] {
					continue
				}
				if order.Descending {
					return left.labels[index] > right.labels[index]
				}
				return left.labels[index] < right.labels[index]
			}
		}

		return strings.Join(left.labels, keySeparator) < strings.Join(right.labels, keySeparator)
	})
}

// fillBuckets adds the empty time buckets a graph needs. Without them a quiet
// hour and a broken tracker look identical, because both are simply a missing
// point.
func (x *executor) fillBuckets(groups *groupSet) {
	for _, label := range x.resolved.Labels() {
		groups.upsert(label, []any{label})
	}
}

// resolveLabels turns interned ids into strings, one query per dimension over
// the rows that survived. This is the join that is deliberately not in the main
// statement: joining dim_* before aggregating drags a text column through the
// whole scan, where doing it afterwards touches only the rows being returned.
func (x *executor) resolveLabels(ctx context.Context, rows []*groupRow) ([][]string, error) {
	labels := make([][]string, len(rows))
	for i := range labels {
		labels[i] = make([]string, len(x.plan.Dimensions))
	}

	for index, d := range x.plan.Dimensions {
		if d.Interned == "" {
			for i, row := range rows {
				if index < len(row.raw) {
					labels[i][index] = valueString(row.raw[index])
				}
			}
			continue
		}

		ids := make([]int64, 0, len(rows))
		seen := map[int64]bool{}

		for _, row := range rows {
			if index >= len(row.raw) {
				continue
			}

			id, ok := row.raw[index].(int64)
			if !ok || seen[id] {
				continue
			}

			seen[id] = true
			ids = append(ids, id)
		}

		if len(ids) == 0 {
			continue
		}

		values, err := x.lookup(ctx, d, ids)
		if err != nil {
			return nil, err
		}

		for i, row := range rows {
			if index >= len(row.raw) {
				continue
			}

			if id, ok := row.raw[index].(int64); ok {
				labels[i][index] = values[id]
			}
		}
	}

	return labels, nil
}

// lookup reads one dimension table for a set of ids.
func (x *executor) lookup(ctx context.Context, d dimension, ids []int64) (map[int64]string, error) {
	condition := inInt64("id", ids)

	rows, err := x.engine.db.QueryContext(ctx,
		"SELECT id, value FROM "+d.Interned.Table()+" WHERE "+condition.SQL, condition.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: read %s: %w", d.Interned.Table(), err)
	}
	defer rows.Close()

	values := map[int64]string{}

	for rows.Next() {
		var (
			id    int64
			value string
		)

		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("query: read %s: %w", d.Interned.Table(), err)
		}

		values[id] = value
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read %s: %w", d.Interned.Table(), err)
	}

	return values, nil
}

// countGroups answers include.total_rows on the push-down path, where the page
// was cut by the database and the total is not simply the number of rows in
// memory.
func (x *executor) countGroups(ctx context.Context) (int, error) {
	t := x.plan.Primary

	dims, joins, extra, err := x.dimensions(t, dimEntry)
	if err != nil {
		return 0, err
	}

	conditions, err := x.conditionsFor(t, x.resolved)
	if err != nil {
		return 0, err
	}
	conditions = append(conditions, extra...)

	inner := statement{
		table: t, alias: t.alias(), joins: joins,
		dims: dims, columns: []expr{{SQL: "COUNT(*)"}}, conditions: conditions,
	}

	sqlText, args := inner.render()

	var total int
	if err := x.engine.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+sqlText+")", args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("query: count groups: %w", err)
	}

	return total, nil
}
