//
// table.go
// Deciding which fact table answers a query, and refusing the combinations that cannot be answered.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import "strings"

// table names one of the two fact tables.
type table int

const (
	tableEvents table = iota
	tableSessions
)

// name is the table's name in SQL.
func (t table) name() string {
	if t == tableSessions {
		return "sessions"
	}

	return "events"
}

// alias is the short name every expression in this package qualifies its
// columns with. Qualifying is not optional: a session-only dimension pulls the
// sessions table into an events query, and two tables in one statement both
// have a site_id.
func (t table) alias() string {
	if t == tableSessions {
		return "s"
	}

	return "e"
}

// timeColumn is the column the date range is applied to. A session is placed in
// time by when it started, not by when it was last seen: a visit that began at
// 23:58 belongs to that day even though it ended on the next one, and moving it
// would make yesterday's total change after midnight.
func (t table) timeColumn() string {
	if t == tableSessions {
		return "started_at"
	}

	return "timestamp"
}

// plan is the compiler's decision about how a query is answered: which table
// produces the paginated result set, which one contributes the rest, and what
// the answer will mean when a session-scoped metric had to be re-scoped to
// compose with an event-scoped breakdown.
type plan struct {
	// Primary is the table the group keys, the ordering and the page come
	// from. Everything else is joined back onto its rows.
	Primary table

	// Secondary is the other fact table, used when the query mixes event- and
	// session-scoped metrics. HasSecondary says whether it is used at all.
	Secondary    table
	HasSecondary bool

	// MetricTable says which table each ordinary metric is counted on.
	MetricTable map[string]table

	// Specials are the composite metrics, each answered by a query of its own
	// and joined back on the group key.
	Specials []string

	// SessionsEntryScoped records that the session-grain half of this query was
	// narrowed to the visits that *entered* on the matching page rather than
	// the visits that merely touched it. It is the only honest way to put a
	// bounce rate beside a page, and it changes what the number means, so it is
	// reported to the caller rather than assumed.
	SessionsEntryScoped bool

	// Dimensions are the resolved group-by dimensions, in request order.
	Dimensions []dimension
}

// Session-scoped metrics that are ratios over the whole visit. These are the
// three that cannot be re-scoped to an event without changing what they mean:
// a bounce is a property of a visit, and half a visit does not bounce.
func isSessionRatio(name string) bool {
	switch name {
	case "bounce_rate", "visit_duration", "views_per_visit":
		return true
	}

	return false
}

// decide is the table decider. It reads the requested metrics and dimensions
// and returns the plan, or a caller-facing error for a combination that has no
// correct answer.
//
// The error branch is the point of this function. A session-scoped metric under
// an event-scoped breakdown is the shape behind a long tail of wrong numbers in
// this category of product: the metric silently starts describing the whole
// visit of anybody who matched, which is a different question from the one on
// the screen. Where there is a correctly-scoped answer we return it and say so;
// where there is not, we refuse.
func decide(q *Query) (*plan, error) {
	p := &plan{MetricTable: map[string]table{}}

	for _, name := range q.Dimensions {
		resolved, err := resolveDimension(name)
		if err != nil {
			return nil, err
		}

		p.Dimensions = append(p.Dimensions, resolved)
	}

	var needsEvents, needsSessions bool

	for _, name := range q.Metrics {
		definition, ok := metricByName(name)
		if !ok {
			return nil, invalid("unknown metric %q", name)
		}

		switch definition.Scope {
		case scopeEvent:
			needsEvents = true
		case scopeSession:
			needsSessions = true
		case scopeSpecial:
			p.Specials = append(p.Specials, name)
		case scopeEither:
			// Decided below, once it is known which table is being read
			// anyway. Counting a visitor on a table the query already scans is
			// free; opening the second table for it is not.
		}
	}

	// Every composite is anchored on events: scroll depth and conversion rate
	// are counted there, and exit rate divides by pageviews. Reading them from
	// a plan whose group keys came from the sessions table would join an entry
	// page against a viewed page and quietly return zeros.
	if len(p.Specials) > 0 || (!needsEvents && !needsSessions) {
		needsEvents = true
	}

	if err := checkConversionGoal(q, p); err != nil {
		return nil, err
	}

	if err := checkDimensionScopes(p, needsSessions); err != nil {
		return nil, err
	}

	// A breakdown by entry or exit page can only be grouped on the sessions
	// table, so that table leads even when the metrics are event-scoped.
	for _, resolved := range p.Dimensions {
		if resolved.sessionOnly() {
			needsSessions = true
		}
	}

	switch {
	case needsSessions && !needsEvents:
		p.Primary = tableSessions
	default:
		p.Primary = tableEvents
	}

	if needsEvents && needsSessions {
		p.HasSecondary = true
		p.Secondary = tableSessions
		if p.Primary == tableSessions {
			p.Secondary = tableEvents
		}
	}

	for _, name := range q.Metrics {
		definition, _ := metricByName(name)

		switch definition.Scope {
		case scopeEvent:
			p.MetricTable[name] = tableEvents
		case scopeSession:
			p.MetricTable[name] = tableSessions
		case scopeEither:
			p.MetricTable[name] = p.Primary
		}
	}

	p.SessionsEntryScoped = needsSessions && entryScopeRequired(q, p)

	return p, nil
}

// checkDimensionScopes refuses a breakdown that has no correct answer. An
// event-scoped dimension with no session analogue — a custom event name, a
// property, a page title — cannot carry a bounce rate, because there is no
// definition of "the visits that this event name describes".
func checkDimensionScopes(p *plan, needsSessions bool) error {
	if !needsSessions {
		return nil
	}

	for _, resolved := range p.Dimensions {
		if !resolved.eventOnly() || resolved.EntryColumn != "" {
			continue
		}

		return invalid(
			"%q is an event-scoped dimension and cannot break down a session-scoped metric — "+
				"visits, bounce rate, visit duration and views per visit describe a whole visit, "+
				"so there is no correct value for them per %s. Ask for them without that dimension, "+
				"or use a has_done filter to select the visits that contain a matching event.",
			resolved.Name, strings.TrimPrefix(resolved.Name, "event:"))
	}

	return nil
}

// entryScopeRequired reports whether the session half of this query had to be
// narrowed to entry pages. It is true whenever an event-scoped page constraint
// — a breakdown or a filter — has to be expressed at session grain, which is
// exactly when the incumbent silently answers a different question.
func entryScopeRequired(q *Query, p *plan) bool {
	for _, resolved := range p.Dimensions {
		if resolved.eventOnly() && resolved.EntryColumn != "" {
			return true
		}
	}

	for _, filter := range q.Filters {
		if filter.Operator == OpHasDone {
			continue
		}

		resolved, err := resolveDimension(filter.Dimension)
		if err != nil {
			continue
		}

		if resolved.eventOnly() && resolved.EntryColumn != "" {
			return true
		}
	}

	return false
}

// entryScopeWarning is the sentence attached to every session-scoped metric in
// a query that had to be entry-scoped. It names the change rather than hinting
// at it, because a bounce rate measured over entrances and one measured over
// visits that touched a page are different numbers, and the reader cannot tell
// which one they are looking at from the number alone.
const entryScopeWarning = "computed over the visits that entered on the matching page, not every visit that touched it — " +
	"a bounce rate, visit duration or views per visit describes a whole visit, so it is scoped to entrances"

// sessionSemiJoinWarning is attached when an event-scoped filter with no entry
// analogue had to select whole sessions.
const sessionSemiJoinWarning = "computed over whole visits that contain a matching event — " +
	"this metric describes a visit, so the event filter selects visits rather than narrowing the metric"

// checkConversionGoal refuses a conversion rate with nothing to convert.
// Without a goal the numerator and the denominator are the same set and every
// row reads 100%, which is not a smaller answer than the right one — it is a
// number that looks like a triumph and means nothing.
func checkConversionGoal(q *Query, p *plan) error {
	for _, name := range p.Specials {
		if name != "conversion_rate" {
			continue
		}

		if !hasGoal(q) {
			return invalid("conversion_rate needs a goal to measure — add a has_done filter, " +
				"a filter on event:name or event:props:<key>, or break down by event:name")
		}
	}

	return nil
}
