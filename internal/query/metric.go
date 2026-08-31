//
// metric.go
// Every metric the product can count, defined once, in one place.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"math"
	"sort"
)

// scope says which fact table a metric is counted on. It is the single most
// important property of a metric in this package: an event-scoped metric counts
// hits and a session-scoped one counts visits, and the two do not mix without
// somebody deciding what the mixture means.
type scope int

const (
	// scopeEvent is counted on `events`, one row per hit.
	scopeEvent scope = iota

	// scopeSession is counted on `sessions`, one row per visit.
	scopeSession

	// scopeEither can be counted on whichever table the query is already
	// reading, because it means the same thing on both.
	scopeEither

	// scopeSpecial is a composite that needs a query of its own, joined back
	// on the group key. Keeping composites as a second query rather than a
	// window function is what keeps this compiler small and is what makes
	// filters compose: the second query carries the same WHERE clause.
	scopeSpecial
)

// expr is one piece of parameterised SQL and the arguments that go with it.
// Everything this package builds is an expr, which is what makes "never
// concatenate a value into SQL" a property of the type rather than a rule
// people have to remember.
type expr struct {
	SQL  string
	Args []any
}

// compileContext carries what every expression builder needs and nobody should
// look up twice: the interned ids of the two event names the metric
// definitions key off, and the sampling rate.
type compileContext struct {
	// pageviewNameID and engagementNameID are the dim_event_name ids for
	// "pageview" and "engagement". They are -1 on a database that has never
	// seen one, which matches no row rather than matching id 0 — the empty
	// string — and quietly counting every event with no name.
	pageviewNameID   int64
	engagementNameID int64

	sampleRate float64
}

// metric is one number a query can ask for.
type metric struct {
	Name  string
	Scope scope

	// Components are the aggregate expressions this metric needs from the
	// group. They are summed and counted separately and divided in Go rather
	// than in SQL so that a zero denominator is an explicit decision here
	// instead of a NULL that becomes a zero somewhere downstream.
	Components func(c compileContext, t table, alias string) []expr

	// Combine turns the component values into the metric. Splitting it this
	// way is what makes a derived rate testable without a database.
	Combine func(values []float64) float64

	// Percentage marks a metric that must land between 0 and 100. Anything
	// outside that is a bug — an overflow, a denominator counted over the wrong
	// set — and clamping it is how a bug stays a small wrong number instead of
	// a bounce rate of four billion percent on somebody's dashboard.
	Percentage bool

	// Scaled marks a count that sampling divides. A rate is unaffected by
	// sampling; a total is not, and scaling it back up is the only thing that
	// makes a sampled total comparable with an unsampled one.
	Scaled bool
}

// metrics is the registry. The definitions here are the product: if one of them
// is wrong, every screen that shows it is wrong, and no amount of correct code
// elsewhere helps.
var metrics = map[string]metric{
	// A visitor is a fingerprint, not a person. It counts the same on either
	// table because every session's events carry its user id.
	"visitors": {
		Name: "visitors", Scope: scopeEither, Scaled: true,
		Components: func(_ compileContext, _ table, alias string) []expr {
			return []expr{{SQL: "COUNT(DISTINCT " + alias + ".user_id)"}}
		},
		Combine: first,
	},

	// One row of `sessions` is one visit.
	"visits": {
		Name: "visits", Scope: scopeEither, Scaled: true,
		Components: func(_ compileContext, t table, alias string) []expr {
			// On sessions a visit is a row. On events it is a distinct session
			// id, which is what makes "visits to this page" answerable without
			// dragging the session table into an event-scoped breakdown.
			if t == tableSessions {
				return []expr{{SQL: "COUNT(*)"}}
			}

			return []expr{{SQL: "COUNT(DISTINCT " + alias + ".session_id)"}}
		},
		Combine: first,
	},

	"pageviews": {
		Name: "pageviews", Scope: scopeEvent, Scaled: true,
		Components: func(c compileContext, _ table, alias string) []expr {
			return []expr{{
				SQL:  "SUM(CASE WHEN " + alias + ".name_id = ? THEN 1 ELSE 0 END)",
				Args: []any{c.pageviewNameID},
			}}
		},
		Combine: first,
	},

	// Engagement pings are not events anybody asked for — they exist to carry
	// time on page and scroll depth — so they are the one name excluded here.
	"events": {
		Name: "events", Scope: scopeEvent, Scaled: true,
		Components: func(c compileContext, _ table, alias string) []expr {
			return []expr{{
				SQL:  "SUM(CASE WHEN " + alias + ".name_id <> ? THEN 1 ELSE 0 END)",
				Args: []any{c.engagementNameID},
			}}
		},
		Combine: first,
	},

	"bounce_rate": {
		Name: "bounce_rate", Scope: scopeSession, Percentage: true,
		Components: func(_ compileContext, _ table, alias string) []expr {
			return []expr{
				{SQL: "SUM(" + alias + ".is_bounce)"},
				{SQL: "COUNT(*)"},
			}
		},
		Combine: func(v []float64) float64 { return 100 * ratio(component(v, 0), component(v, 1)) },
	},

	// Bounces are included as zero rather than excluded. Excluding them makes
	// the average describe only the visits that stayed, which is a different
	// question and always a larger number — and it is the difference behind
	// most "your visit duration does not match" support tickets.
	"visit_duration": {
		Name: "visit_duration", Scope: scopeSession,
		Components: func(_ compileContext, _ table, alias string) []expr {
			return []expr{
				{SQL: "SUM(" + alias + ".duration)"},
				{SQL: "COUNT(*)"},
			}
		},
		Combine: func(v []float64) float64 { return ratio(component(v, 0), component(v, 1)) },
	},

	"views_per_visit": {
		Name: "views_per_visit", Scope: scopeSession,
		Components: func(_ compileContext, _ table, alias string) []expr {
			return []expr{
				{SQL: "SUM(" + alias + ".pageviews)"},
				{SQL: "COUNT(*)"},
			}
		},
		Combine: func(v []float64) float64 { return ratio(component(v, 0), component(v, 1)) },
	},

	// Engagement time only ever lands on an engagement ping, so summing it
	// across the group and dividing by the sessions that reported one gives
	// seconds per session. Dividing by all sessions instead would report a time
	// on page for visits whose tracker never sent a ping at all.
	"time_on_page": {
		Name: "time_on_page", Scope: scopeEvent,
		Components: func(c compileContext, _ table, alias string) []expr {
			return []expr{
				{SQL: "SUM(" + alias + ".engagement_time)"},
				{
					SQL:  "COUNT(DISTINCT CASE WHEN " + alias + ".name_id = ? THEN " + alias + ".session_id END)",
					Args: []any{c.engagementNameID},
				},
			}
		},
		Combine: func(v []float64) float64 { return ratio(component(v, 0)/1000, component(v, 1)) },
	},

	// The three composites. Each is a second query joined back on the group
	// key; see special.go for what each one actually counts. Their components
	// are filled in by those queries rather than by a select list here, which
	// is why they have no Components function.
	"scroll_depth": {
		Name: "scroll_depth", Scope: scopeSpecial,
		Combine: first,
	},

	// The denominator is the page's pageviews rather than its entrances —
	// measuring exits against entrances answers a different question and makes
	// every internally-linked page look like a dead end.
	"exit_rate": {
		Name: "exit_rate", Scope: scopeSpecial, Percentage: true,
		Combine: func(v []float64) float64 { return 100 * ratio(component(v, 0), component(v, 1)) },
	},

	"conversion_rate": {
		Name: "conversion_rate", Scope: scopeSpecial, Percentage: true,
		Combine: func(v []float64) float64 { return 100 * ratio(component(v, 0), component(v, 1)) },
	},
}

// additive reports whether a metric's components can be summed across two
// disjoint slices of time. It is the question a roll-up router has to ask
// before it answers half a range from a summary table and half from raw
// events: counting rows adds up, counting distinct visitors does not, because
// the same visitor can appear in both halves.
func (m metric) additive(t table) bool {
	switch m.Name {
	case "visitors", "time_on_page", "scroll_depth", "exit_rate", "conversion_rate":
		return false
	case "visits":
		// A visit is a row on `sessions` and a distinct session id on
		// `events`, and only the first of those adds up.
		return t == tableSessions
	}

	return true
}

// metricByName looks one up.
func metricByName(name string) (metric, bool) {
	found, ok := metrics[name]

	return found, ok
}

// MetricNames lists every metric, sorted. The validation error prints it, so a
// caller who mistyped one is told what the alternatives are in the same
// response rather than in the documentation.
func MetricNames() []string {
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// first returns the single component of a metric that has one.
func first(values []float64) float64 {
	return component(values, 0)
}

// component reads one of a metric's parts, answering zero for a part that no
// statement produced. A group can legitimately be missing a component — a page
// that nobody ever entered on has no session row to count — and a metric must
// answer zero there rather than panic on a short slice.
func component(values []float64, index int) float64 {
	if index < 0 || index >= len(values) {
		return 0
	}

	return values[index]
}

// ratio divides and answers zero rather than NaN or an infinity for an empty
// denominator. A group with no rows is not an error and not an unknown: nobody
// did the thing, so the rate is zero, and returning NaN here would travel all
// the way to a JSON encoder that cannot represent it.
func ratio(numerator, denominator float64) float64 {
	if denominator == 0 {
		return 0
	}

	value := numerator / denominator
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}

	return value
}

// clamp holds a derived rate inside the range it can possibly take. A
// percentage outside 0 to 100 is a bug rather than a data point, and the
// difference between showing a slightly wrong number and showing 4,294,967,271%
// is the difference between a support ticket and a screenshot on the internet.
func clamp(value float64, percentage bool) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}

	if !percentage {
		if value < 0 {
			return 0
		}
		return value
	}

	if value < 0 {
		return 0
	}

	if value > 100 {
		return 100
	}

	return value
}

// round trims floating-point noise from a metric before it is serialised. A
// bounce rate of 66.66666666666667 is not more accurate than 66.67, and the
// extra digits are pure noise in every response body and every test assertion.
func round(value float64) float64 {
	rounded := math.Round(value*1000) / 1000

	// Negative zero serialises as "-0", which looks like a bug to anybody
	// reading a response body.
	if rounded == 0 {
		return 0
	}

	return rounded
}
