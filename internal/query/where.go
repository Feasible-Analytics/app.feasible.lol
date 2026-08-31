//
// where.go
// Compiling filters into parameterised SQL, one operator at a time.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

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
		conditions = append(conditions, b.botExclusion())
	}

	if q.SampleRate > 0 && q.SampleRate < 1 {
		// Sampling picks visitors, not rows. Sampling rows would cut sessions
		// in half, and half a visit bounces, lasts no time and views one page.
		conditions = append(conditions, expr{
			SQL:  "(ABS(" + b.alias + ".user_id) % 1000) < ?",
			Args: []any{int64(q.SampleRate * 1000)},
		})
	}

	return conditions
}

// botExclusion keeps automated traffic out. On events it is a column; on
// sessions there is none, so it is a correlated existence check through the
// session's own events — one index probe per session row, which is affordable
// because the session table in a range is far smaller than the event table.
func (b *whereBuilder) botExclusion() expr {
	if b.table == tableEvents {
		return expr{SQL: b.alias + ".bot_reason_id = 0"}
	}

	return expr{SQL: "NOT EXISTS (SELECT 1 FROM events be WHERE be.session_id = " + b.alias + ".id AND be.bot_reason_id <> 0)"}
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

	resolved, err := resolveDimension(f.Dimension)
	if err != nil {
		return expr{}, err
	}

	if resolved.isProp() {
		return b.prop(f, resolved)
	}

	return b.interned(f, resolved)
}

// interned compiles a filter on a dimension whose values live in a dim_* table.
// The predicate is pushed into a subquery against that table rather than joined
// in: the dimension tables are small and hot, the fact tables are not, and
// joining before aggregating would drag the string column through every row of
// the scan.
func (b *whereBuilder) interned(f Filter, d dimension) (expr, error) {
	predicate, err := b.values(expr{SQL: "value"}, f)
	if err != nil {
		return expr{}, err
	}

	subquery := expr{
		SQL:  "SELECT id FROM " + d.Interned.Table() + " WHERE " + predicate.SQL,
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

	if d.EventColumn == "" && d.SessionColumn == "" && d.EntryColumn == "" {
		return "", nil, invalid("%q cannot be filtered", d.Name)
	}

	if b.table == tableEvents {
		if d.EventColumn != "" {
			return b.alias + "." + d.EventColumn, identity, nil
		}

		// Entry and exit page exist only on sessions, so an event query asking
		// about them selects the sessions first and matches events to them.
		return "s2." + d.SessionColumn, b.throughSessions, nil
	}

	if d.SessionColumn != "" {
		return b.alias + "." + d.SessionColumn, identity, nil
	}

	// An event-scoped dimension being asked at session grain. A page has an
	// entry analogue and is scoped to entrances; anything else selects whole
	// visits that contain a matching event, which is a different question and
	// is reported as one.
	if d.EntryColumn != "" {
		b.entryScoped = true

		return b.alias + "." + d.EntryColumn, identity, nil
	}

	b.semiJoined = true

	return "e2." + d.EventColumn, b.throughEvents, nil
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

	sql := "EXISTS (SELECT 1 FROM events e2 WHERE e2.session_id = " + b.alias + ".id AND " + sites.SQL +
		" AND e2.timestamp >= ? AND e2.timestamp < ? AND " + inner.SQL + ")"

	args := append([]any{}, sites.Args...)
	args = append(args, b.rangeStart, b.rangeEnd)
	args = append(args, inner.Args...)

	return expr{SQL: sql, Args: args}
}

// prop compiles a filter on a custom property. Properties live in the cold
// event_details table, which is why this is a subquery rather than a column:
// the split exists so that a scan which never looks at props does not drag a
// JSON blob off disk with every row.
func (b *whereBuilder) prop(f Filter, d dimension) (expr, error) {
	value := expr{SQL: "json_extract(ed.props, ?)", Args: []any{d.jsonPath()}}

	predicate, err := b.values(value, f)
	if err != nil {
		return expr{}, err
	}

	details := expr{
		SQL:  "SELECT ed.event_id FROM event_details ed WHERE " + predicate.SQL,
		Args: predicate.Args,
	}

	// A property declared session-scoped describes the visit, so filtering on
	// it selects whole visits on either table. Matching only the events that
	// repeated the property would answer "the hits that mentioned the variant"
	// where the caller asked for "the visits in it".
	if d.sessionScoped(b.scopes) {
		inner := expr{SQL: "e2.id IN (" + details.SQL + ")", Args: details.Args}

		return negate(b.visitsWith(inner), f.Negated()), nil
	}

	if b.table == tableEvents {
		return negate(expr{SQL: b.alias + ".id IN (" + details.SQL + ")", Args: details.Args}, f.Negated()), nil
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

	inner := expr{SQL: "e2.id IN (" + details.SQL + ")", Args: details.Args}

	return negate(b.throughEvents(inner), f.Negated()), nil
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
	// last year would select a visit from today, and with it the lookup uses
	// the same index every other query on the table does.
	sql := column + " IN (SELECT e2.session_id FROM events e2 WHERE " + sites.SQL +
		" AND e2.timestamp >= ? AND e2.timestamp < ? AND " + condition.SQL + ")"

	args := append([]any{}, sites.Args...)
	args = append(args, b.rangeStart, b.rangeEnd)
	args = append(args, condition.Args...)

	return expr{SQL: sql, Args: args}
}

// values builds the positive predicate for one filter's value list against a
// value expression. The values OR together: one filter widens, several filters
// narrow, and that asymmetry is the whole filter grammar.
//
// Negation is applied by the caller to the membership test rather than here, so
// that "is" and "is_not" are guaranteed to be exact complements instead of two
// predicates somebody has to keep in step.
func (b *whereBuilder) values(value expr, f Filter) (expr, error) {
	predicates := make([]expr, 0, len(f.Values))

	for _, raw := range f.Values {
		predicate, err := b.predicate(value, f.Operator, raw, !f.CaseInsensitive)
		if err != nil {
			return expr{}, err
		}

		predicates = append(predicates, predicate)
	}

	return or(predicates), nil
}

// predicate compiles one operator against one value.
func (b *whereBuilder) predicate(value expr, operator, raw string, caseSensitive bool) (expr, error) {
	switch operator {
	case OpIs, OpIsNot:
		if caseSensitive {
			return expr{SQL: value.SQL + " = ?", Args: append(append([]any{}, value.Args...), raw)}, nil
		}

		// Both sides are folded the same ASCII-only way. Folding one side with
		// Go's Unicode-aware rules and the other with SQLite's ASCII-only
		// lower() would silently fail to match any non-ASCII value.
		return expr{
			SQL:  "lower(" + value.SQL + ") = ?",
			Args: append(append([]any{}, value.Args...), asciiLower(raw)),
		}, nil

	case OpContains, OpContainsNot:
		if caseSensitive {
			return expr{SQL: "instr(" + value.SQL + ", ?) > 0", Args: append(append([]any{}, value.Args...), raw)}, nil
		}

		return expr{
			SQL:  "instr(lower(" + value.SQL + "), ?) > 0",
			Args: append(append([]any{}, value.Args...), asciiLower(raw)),
		}, nil

	case OpMatches, OpMatchesNot:
		if err := matcherError(); err != nil {
			return expr{}, &Error{Message: err.Error()}
		}

		sensitive := int64(0)
		if caseSensitive {
			sensitive = 1
		}

		args := []any{raw}
		args = append(args, value.Args...)
		args = append(args, sensitive)

		return expr{SQL: MatchFunction + "(?, " + value.SQL + ", ?)", Args: args}, nil

	default:
		return expr{}, invalid("unknown filter operator %q", operator)
	}
}
