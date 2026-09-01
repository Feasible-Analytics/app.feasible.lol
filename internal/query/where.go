//
// where.go
// Compiling filters into parameterised SQL, one operator at a time.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// EventFilterSQL compiles dashboard filters into a parameterised predicate on
// an events-table alias. Ordered reports such as funnels and journeys use it
// so their raw sequence scans select exactly the same audience as Engine.Run.
func (e *Engine) EventFilterSQL(ctx context.Context, siteIDs []int64, resolved Resolved, filters []Filter, alias string) (string, []any, error) {
	if len(filters) == 0 {
		return "", nil, nil
	}
	q := Query{SiteIDs: siteIDs, Filters: filters, Exact: true}
	compile, err := e.compileContext(ctx, &q)
	if err != nil {
		return "", nil, err
	}
	scopes, err := e.propertyScopes(ctx, siteIDs)
	if err != nil {
		return "", nil, err
	}
	builder := newWhereBuilder(tableEvents, compile, scopes, siteIDs, resolved)
	builder.alias = alias
	conditions, err := builder.compile(filters)
	if err != nil {
		return "", nil, err
	}
	compiled := and(conditions)
	return compiled.SQL, compiled.Args, nil
}

// whereBuilder compiles a query's filters for one fact table. It exists per
// table rather than once per query because the same filter means different SQL
// on each: `event:page` is a column on `events` and a translation on
// `sessions`, and the translation is the whole guard rail.
type whereBuilder struct {
	table  table
	alias  string
	ctx    compileContext
	scopes map[string]string

	sites      []int64
	rangeStart int64
	rangeEnd   int64

	// entryScoped and semiJoined record how an event-scoped filter had to be
	// expressed at session grain. They are read back by the engine and turned
	// into a metric warning, because both change what the number means.
	entryScoped bool
	semiJoined  bool
}

// newWhereBuilder builds a compiler for one table over a resolved range.
func newWhereBuilder(t table, ctx compileContext, scopes map[string]string, sites []int64, r Resolved) *whereBuilder {
	return &whereBuilder{
		table:      t,
		alias:      t.alias(),
		ctx:        ctx,
		scopes:     scopes,
		sites:      sites,
		rangeStart: r.Start.Unix(),
		rangeEnd:   r.End.Unix(),
	}
}

// base builds the conditions every query on this table carries: which sites,
// which window, and the two exclusions that would otherwise silently inflate
// every number in the product.
func (b *whereBuilder) base(q *Query) []expr {
	conditions := []expr{
		inInt64(b.alias+".site_id", b.sites),
		{
			SQL:  b.alias + "." + b.table.timeColumn() + " >= ? AND " + b.alias + "." + b.table.timeColumn() + " < ?",
			Args: []any{b.rangeStart, b.rangeEnd},
		},
	}

	// Imported data is excluded unless it is asked for, so that finishing an
	// import never changes a number nobody asked to change.
	if !q.Include.Imports {
		conditions = append(conditions, expr{SQL: b.alias + ".is_imported = 0"})
	}

	// Traffic we classified as automated is excluded by default. The events
	// are kept rather than deleted — a misclassification has to be
	// recoverable — so the exclusion has to happen here.
	if !q.Include.Bots {
		if b.table == tableSessions && b.ctx.sessionFacts {
			// Sampled membership constrains bot status in the same seek. Exact
			// queries use one primary-key session fact rather than scanning every
			// event in a potentially giant session.
			if !(q.SampleRate > 0 && q.SampleRate < 1) {
				conditions = append(conditions, expr{SQL: "(SELECT sf.is_bot FROM session_sampling sf WHERE sf.session_id = " + b.alias + ".id) = 0"})
			}
		} else {
			conditions = append(conditions, b.botExclusion())
		}
	}

	if q.SampleRate > 0 && q.SampleRate < 1 {
		// Each fact table has its own row bucket. Event-grain work samples event
		// rows, while visit-grain work samples complete rows from sessions; no
		// selected visitor can make either scan unbounded.
		conditions = append(conditions, sampleCondition(
			b.table, b.alias, b.sites, b.rangeStart, b.rangeEnd, q.SampleRate, !q.Include.Bots,
		))
	}

	return conditions
}

// botExclusion keeps automated traffic out. On events it is a column; on
// sessions there is none in the pre-0011 compatibility schema, so that schema
// uses a correlated existence check. The integrated schema never reaches that
// branch because bot status is materialized in session_sampling.
func (b *whereBuilder) botExclusion() expr {
	if b.table == tableEvents {
		return expr{SQL: b.alias + ".bot_reason_id = 0"}
	}

	return expr{SQL: "NOT EXISTS (SELECT 1 FROM events be WHERE be.site_id = " + b.alias + ".site_id AND be.session_id = " + b.alias + ".id AND be.bot_reason_id <> 0)"}
}

// compile turns the query's filters into conditions. They AND together:
// multiple filters narrow, and it is the values inside one filter that widen.
func (b *whereBuilder) compile(filters []Filter) ([]expr, error) {
	conditions := make([]expr, 0, len(filters))

	for _, filter := range filters {
		condition, err := b.one(filter)
		if err != nil {
			return nil, err
		}

		conditions = append(conditions, condition)
	}

	return conditions, nil
}

// one compiles a single filter.
func (b *whereBuilder) one(f Filter) (expr, error) {
	if f.Operator == OpHasDone {
		return b.hasDone(f)
	}
	if f.Dimension == "event:goal" {
		return b.goal(f)
	}

	resolved, err := resolveDimension(f.Dimension)
	if err != nil {
		return expr{}, err
	}

	if resolved.isProp() {
		return b.prop(f, resolved)
	}

	return b.interned(f, resolved)
}

// goal compiles configured goal ids into their exact event predicates.
func (b *whereBuilder) goal(f Filter) (expr, error) {
	var alternatives []expr
	alias := b.alias
	if b.table == tableSessions {
		alias = "e2"
	}
	for _, raw := range f.Values {
		id, _ := strconv.ParseInt(raw, 10, 64)
		definition, err := b.goalDefinition(id, alias)
		if err != nil {
			return expr{}, err
		}
		alternatives = append(alternatives, definition)
	}
	condition := or(alternatives)
	if b.table == tableSessions {
		condition = b.visitsWith(condition)
	}
	return negate(condition, f.Negated()), nil
}

// goalDefinition reads and compiles one site-owned definition against the
// builder's event alias.
func (b *whereBuilder) goalDefinition(id int64, alias string) (expr, error) {
	var kind, page, event string
	var depth int
	sites := inInt64("site_id", b.sites)
	args := append([]any{id}, sites.Args...)
	err := b.ctx.db.QueryRowContext(b.ctx.context,
		"SELECT kind, page_pattern, event_name, scroll_depth FROM goals WHERE id = ? AND "+sites.SQL,
		args...).Scan(&kind, &page, &event, &depth)
	if errors.Is(err, sql.ErrNoRows) {
		return expr{}, invalid("goal %d does not exist on the requested site", id)
	}
	if err != nil {
		return expr{}, fmt.Errorf("query: read goal %d: %w", id, err)
	}

	var condition expr
	switch kind {
	case "page":
		path := expr{SQL: alias + ".pathname_id IN (SELECT id FROM dim_pathname WHERE value = ?)", Args: []any{page}}
		if strings.Contains(page, "*") {
			path = expr{SQL: alias + ".pathname_id IN (SELECT id FROM dim_pathname WHERE " + MatchFunction + "(?, value, 1))", Args: []any{goalPattern(page)}}
		}
		condition = and([]expr{{SQL: alias + ".name_id = ?", Args: []any{b.ctx.pageviewNameID}}, path})
	case "event":
		names := []any{event}
		if event == "Form: Submission" {
			names = append(names, "Form: Submit")
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
		condition = expr{SQL: alias + ".name_id IN (SELECT id FROM dim_event_name WHERE value IN (" + placeholders + "))", Args: names}
	case "scroll":
		condition = expr{SQL: alias + ".scroll_depth >= ? AND " + alias + ".scroll_depth <= 100", Args: []any{depth}}
		if page != "" {
			condition = and([]expr{condition, {SQL: alias + ".pathname_id IN (SELECT id FROM dim_pathname WHERE " + MatchFunction + "(?, value, 1))", Args: []any{goalPattern(page)}}})
		}
	default:
		return expr{}, invalid("goal %d has unsupported kind %q", id, kind)
	}

	rows, err := b.ctx.db.QueryContext(b.ctx.context,
		"SELECT name, value FROM goal_properties WHERE goal_id = ? ORDER BY id", id)
	if err != nil {
		return expr{}, fmt.Errorf("query: read goal %d properties: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	parts := []expr{condition}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return expr{}, fmt.Errorf("query: read goal %d properties: %w", id, err)
		}
		parts = append(parts, expr{
			SQL:  "EXISTS (SELECT 1 FROM event_details gp WHERE gp.event_id = " + alias + ".id AND json_extract(gp.props, ?) = ?)",
			Args: []any{`$."` + name + `"`, value},
		})
	}
	if err := rows.Err(); err != nil {
		return expr{}, fmt.Errorf("query: read goal %d properties: %w", id, err)
	}
	return and(parts), nil
}

// goalPattern converts stored star syntax into an anchored regular expression.
func goalPattern(pattern string) string {
	var out strings.Builder
	out.WriteByte('^')
	for i := 0; i < len(pattern); {
		if pattern[i] != '*' {
			next := strings.IndexByte(pattern[i:], '*')
			if next < 0 {
				out.WriteString(regexp.QuoteMeta(pattern[i:]))
				break
			}
			out.WriteString(regexp.QuoteMeta(pattern[i : i+next]))
			i += next
			continue
		}
		if strings.HasPrefix(pattern[i:], "**") {
			out.WriteString(".*")
			i += 2
		} else {
			out.WriteString("[^/]*")
			i++
		}
	}
	out.WriteByte('$')
	return out.String()
}

// interned compiles a filter on a dimension whose values live in a dim_* table.
// The predicate is pushed into a subquery against that table rather than joined
// in: the dimension tables are small and hot, the fact tables are not, and
// joining before aggregating would drag the string column through every row of
// the scan.
func (b *whereBuilder) interned(f Filter, d dimension) (expr, error) {
	predicate, err := b.values(expr{SQL: "dv.value"}, f)
	if err != nil {
		return expr{}, err
	}

	subquery := expr{
		SQL:  "SELECT dv.id FROM " + d.Interned.Table() + " dv WHERE " + predicate.SQL,
		Args: predicate.Args,
	}

	column, wrap, err := b.column(d)
	if err != nil {
		return expr{}, err
	}

	inner := expr{SQL: column + " IN (" + subquery.SQL + ")", Args: subquery.Args}

	return negate(wrap(inner), f.Negated()), nil
}

// negate wraps a compiled condition when the operator excludes rather than
// includes. It is applied to the whole condition, after any scope translation,
// which is the only placement that is correct: "the session has an event that
// is not a signup" and "the session has no signup" are different sets, and
// pushing the NOT inside the translation quietly produces the first when the
// caller asked for the second.
func negate(condition expr, negated bool) expr {
	if !negated {
		return condition
	}

	return expr{SQL: "NOT (" + condition.SQL + ")", Args: condition.Args}
}

// column resolves which column on this table carries a dimension, and returns
// the wrapper that lifts a predicate written against another table into this
// one. The wrapper is where every scope translation happens, and it is one
// function so that no caller can accidentally compile a session filter against
// an event column.
func (b *whereBuilder) column(d dimension) (string, func(expr) expr, error) {
	identity := func(e expr) expr { return e }

	if d.EventColumn == "" && d.SessionColumn == "" && d.EntryColumn == "" && d.EntryEventColumn == "" {
		return "", nil, invalid("%q cannot be filtered", d.Name)
	}

	if b.table == tableEvents {
		if d.EventColumn != "" {
			return b.ctx.pathColumn(b.alias, d.EventColumn, d), identity, nil
		}

		// Entry and exit page exist only on sessions, so an event query asking
		// about them selects the sessions first and matches events to them.
		return b.ctx.pathColumn("s2", d.SessionColumn, d), b.throughSessions, nil
	}

	if d.SessionColumn != "" {
		return b.ctx.pathColumn(b.alias, d.SessionColumn, d), identity, nil
	}

	// An event-scoped dimension being asked at session grain. A page has an
	// entry analogue and is scoped to entrances; anything else selects whole
	// visits that contain a matching event, which is a different question and
	// is reported as one.
	if d.EntryColumn != "" {
		b.entryScoped = true

		return b.ctx.pathColumn(b.alias, d.EntryColumn, d), identity, nil
	}

	if d.EntryEventColumn != "" {
		b.entryScoped = true

		return sessionEntryEventColumn(b.alias, d.EntryEventColumn, dimEntry, b.ctx.sessionFacts), identity, nil
	}

	b.semiJoined = true

	return b.ctx.pathColumn("e2", d.EventColumn, d), b.throughEvents, nil
}

// throughSessions lifts a predicate written against `sessions` into a query on
// `events`.
func (b *whereBuilder) throughSessions(inner expr) expr {
	sites := inInt64("s2.site_id", b.sites)

	sql := "EXISTS (SELECT 1 FROM sessions s2 WHERE s2.id = " + b.alias + ".session_id AND " + sites.SQL + " AND " + inner.SQL + ")"

	return expr{SQL: sql, Args: append(append([]any{}, sites.Args...), inner.Args...)}
}

// throughEvents lifts a predicate written against `events` into a query on
// `sessions`. The window is applied to the inner events too: without it a goal
// reached last year would select a session from today, and with it the lookup
// uses the same index every other query on the table does.
func (b *whereBuilder) throughEvents(inner expr) expr {
	sites := inInt64("e2.site_id", b.sites)
	conditions := []expr{
		{SQL: "e2.session_id = " + b.alias + ".id"},
		sites,
		{SQL: "e2.timestamp >= ? AND e2.timestamp < ?", Args: []any{b.rangeStart, b.rangeEnd}},
	}

	conditions = append(conditions, inner)

	where := and(conditions)

	return expr{SQL: "EXISTS (SELECT 1 FROM events e2 WHERE " + where.SQL + ")", Args: where.Args}
}

// prop compiles a filter on a custom property. Properties live in the cold
// event_details table, which is why this is a subquery rather than a column:
// the split exists so that a scan which never looks at props does not drag a
// JSON blob off disk with every row.
func (b *whereBuilder) prop(f Filter, d dimension) (expr, error) {
	// A property declared session-scoped describes the visit, so filtering on
	// it selects whole visits on either table. Matching only the events that
	// repeated the property would answer "the hits that mentioned the variant"
	// where the caller asked for "the visits in it".
	if d.sessionScoped(b.scopes) {
		alias := b.alias
		wrap := func(condition expr) expr { return condition }
		if b.table == tableEvents {
			alias = "s2"
			wrap = b.throughSessions
		}

		value := expr{SQL: "json_extract(" + alias + ".entry_props, ?)", Args: []any{d.jsonPath()}}
		predicate, err := b.values(value, f)
		if err != nil {
			return expr{}, err
		}

		return negate(wrap(predicate), f.Negated()), nil
	}

	if b.table == tableEvents {
		condition, err := b.eventProperty(f, d, b.alias)
		if err != nil {
			return expr{}, err
		}

		return negate(condition, f.Negated()), nil
	}

	// At session grain a property filter selects whole visits containing a
	// matching event. For an event-scoped property that is a re-scoping and is
	// reported as one; for a property declared session-scoped it is not — the
	// property describes the visit, so selecting visits by it is exactly what
	// the caller asked for, and a warning there would be noise that trains
	// people to ignore the real ones.
	if !d.sessionScoped(b.scopes) {
		b.semiJoined = true
	}

	inner, err := b.eventProperty(f, d, "e2")
	if err != nil {
		return expr{}, err
	}

	return negate(b.throughEvents(inner), f.Negated()), nil
}

// eventProperty tests one event's cold property through its primary-key row.
// Correlating details to an event selected by the sampled fact-table index is
// what keeps a property filter bounded; selecting matching event ids from all
// of event_details first would scan the unsampled cold table before SQLite had
// a chance to apply the event-row buckets.
func (b *whereBuilder) eventProperty(f Filter, d dimension, eventAlias string) (expr, error) {
	value := expr{SQL: "json_extract(ep.props, ?)", Args: []any{d.jsonPath()}}

	predicate, err := b.values(value, f)
	if err != nil {
		return expr{}, err
	}

	return expr{
		SQL:  "EXISTS (SELECT 1 FROM event_details ep WHERE ep.event_id = " + eventAlias + ".id AND " + predicate.SQL + ")",
		Args: predicate.Args,
	}, nil
}

// hasDone selects whole visits by something that happened inside them. It is
// the escape hatch that makes an event-scoped question composable with a
// session-scoped metric: "the visits in which somebody signed up" is a
// well-defined set of visits, where "the bounce rate of a signup event" is not.
func (b *whereBuilder) hasDone(f Filter) (expr, error) {
	if f.Child == nil {
		return expr{}, invalid("has_done needs an inner filter")
	}

	inner := &whereBuilder{
		table:      tableEvents,
		alias:      "e2",
		ctx:        b.ctx,
		scopes:     b.scopes,
		sites:      b.sites,
		rangeStart: b.rangeStart,
		rangeEnd:   b.rangeEnd,
	}

	condition, err := inner.one(*f.Child)
	if err != nil {
		return expr{}, err
	}

	return b.visitsWith(condition), nil
}

// visitsWith selects the visits containing an event that matches a condition,
// expressed against whichever table is being read. It is one function because
// two features need exactly this set — a has_done filter and a session-scoped
// property — and two spellings of "the visits that did this" would eventually
// disagree about one of them.
func (b *whereBuilder) visitsWith(condition expr) expr {
	sites := inInt64("e2.site_id", b.sites)

	column := b.alias + ".id"
	if b.table == tableEvents {
		column = b.alias + ".session_id"
	}

	// The window is applied to the inner events too: without it a match from
	// last year could select a visit from today. Sampled queries that reach this
	// complete-session membership operation are refused before compilation;
	// row-sampling this inner set would change its meaning.
	conditions := []expr{
		sites,
		{SQL: "e2.timestamp >= ? AND e2.timestamp < ?", Args: []any{b.rangeStart, b.rangeEnd}},
	}
	conditions = append(conditions, condition)

	where := and(conditions)

	return expr{SQL: column + " IN (SELECT e2.session_id FROM events e2 WHERE " + where.SQL + ")", Args: where.Args}
}

// values builds the positive predicate for one filter's value list against a
// value expression. The values OR together: one filter widens, several filters
// narrow, and that asymmetry is the whole filter grammar.
//
// Negation is applied by the caller to the membership test rather than here, so
// that "is" and "is_not" are guaranteed to be exact complements instead of two
// predicates somebody has to keep in step.
func (b *whereBuilder) values(value expr, f Filter) (expr, error) {
	values := append([]string(nil), f.Values...)
	if f.CaseInsensitive {
		for i := range values {
			values[i] = asciiLower(values[i])
		}
	}

	encoded, err := json.Marshal(values)
	if err != nil {
		return expr{}, invalid("filter on %q could not be encoded", f.Dimension)
	}

	caseSensitive := !f.CaseInsensitive
	list := string(encoded)

	switch f.Operator {
	case OpIs, OpIsNot:
		if caseSensitive {
			return expr{
				SQL:  value.SQL + " IN (SELECT fv.value FROM json_each(?) fv)",
				Args: append(append([]any{}, value.Args...), list),
			}, nil
		}

		// Both sides are folded the same ASCII-only way. Folding one side with
		// Go's Unicode-aware rules and the other with SQLite's ASCII-only
		// lower() would silently fail to match any non-ASCII value.
		return expr{
			SQL:  "lower(" + value.SQL + ") IN (SELECT fv.value FROM json_each(?) fv)",
			Args: append(append([]any{}, value.Args...), list),
		}, nil

	case OpContains, OpContainsNot:
		if caseSensitive {
			return expr{
				SQL:  "EXISTS (SELECT 1 FROM json_each(?) fv WHERE instr(" + value.SQL + ", fv.value) > 0)",
				Args: append([]any{list}, value.Args...),
			}, nil
		}

		return expr{
			SQL:  "EXISTS (SELECT 1 FROM json_each(?) fv WHERE instr(lower(" + value.SQL + "), fv.value) > 0)",
			Args: append([]any{list}, value.Args...),
		}, nil

	case OpMatches, OpMatchesNot:
		if err := matcherError(); err != nil {
			return expr{}, &Error{Message: err.Error()}
		}

		sensitive := int64(0)
		if caseSensitive {
			sensitive = 1
		}

		args := []any{list}
		args = append(args, value.Args...)
		args = append(args, sensitive)

		return expr{
			SQL:  "EXISTS (SELECT 1 FROM json_each(?) fv WHERE " + MatchFunction + "(fv.value, " + value.SQL + ", ?))",
			Args: args,
		}, nil

	default:
		return expr{}, invalid("unknown filter operator %q", f.Operator)
	}
}
