//
// params.go
// Parsing every query-string parameter into something, or into a sentence.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// Pagination defaults. A hundred is the page size everything in this API
// returns when nobody asks for one, and the ceiling exists because a page size
// nobody bounds is a way for one request to pull an account's whole path table
// into memory.
const (
	DefaultPageSize = 100
	MaxPageSize     = 1000
)

// paramError is a caller's mistake in a query-string parameter. It exists as a
// type so a handler can answer every one of them with the same 400, which is
// the whole promise of this file: no parameter reaches a parser that can panic
// or a database that can only answer with a 500.
type paramError struct {
	message string
}

// Error renders the message the caller reads.
func (e *paramError) Error() string {
	return e.message
}

// badParam builds a parameter error.
func badParam(format string, args ...any) error {
	return &paramError{message: fmt.Sprintf(format, args...)}
}

// intParam reads a bounded integer.
//
// This is the function the incumbent does not have. Their breakdown endpoint
// passes `page` straight into an integer parse, so `page=foo` raises inside
// their framework and comes back as a 500 — a caller cannot tell a typo from an
// outage, and neither can their support desk.
func intParam(values url.Values, name string, fallback, low, high int) (int, error) {
	raw := strings.TrimSpace(values.Get(name))
	if raw == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, badParam("%s must be a whole number, not %q", name, raw)
	}

	if parsed < low || parsed > high {
		return 0, badParam("%s must be between %d and %d, not %d", name, low, high, parsed)
	}

	return parsed, nil
}

// boolParam reads a flag, accepting the spellings people actually type.
func boolParam(values url.Values, name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(values.Get(name))
	if raw == "" {
		return fallback, nil
	}

	switch strings.ToLower(raw) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}

	return false, badParam("%s must be true or false, not %q", name, raw)
}

// parseMetrics reads the comma-separated metric list, defaulting to visitors,
// and refuses an unknown name with the list of known ones attached. Telling
// somebody "unknown metric" without saying what the alternatives are costs them
// a round trip to the documentation for a typo.
func parseMetrics(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{"visitors"}, nil
	}

	var metrics []string
	seen := map[string]bool{}

	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		if err := query.ValidMetric(name); err != nil {
			return nil, &paramError{message: err.Error()}
		}

		if seen[name] {
			return nil, badParam("metric %q is listed twice", name)
		}
		seen[name] = true

		metrics = append(metrics, name)
	}

	if len(metrics) == 0 {
		return nil, badParam("metrics is empty — name at least one, for example metrics=visitors,pageviews")
	}

	return metrics, nil
}

// parseProperty reads the dimension a breakdown groups by.
func parseProperty(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badParam("property is required — for example property=visit:source")
	}

	if err := query.ValidDimension(raw); err != nil {
		return "", &paramError{message: err.Error()}
	}

	return raw, nil
}

// shortName is the key a v1 breakdown row uses for its dimension value. The
// established shape drops the scope prefix — `visit:source` comes back as
// `source` — and a client reading `row["source"]` is the reason we match it
// rather than returning the fully-qualified name we prefer internally.
func shortName(dimension string) string {
	if index := strings.LastIndex(dimension, ":"); index >= 0 {
		return dimension[index+1:]
	}

	return dimension
}

// The v1 interval names, which are not the v2 dimension names. `date` meaning
// "one row per day" is the one that surprises people, and it is exactly why the
// translation lives in one function rather than in three handlers.
var v1Intervals = map[string]string{
	"minute": "time:minute",
	"hour":   "time:hour",
	"date":   "time:day",
	"day":    "time:day",
	"week":   "time:week",
	"month":  "time:month",
}

// parseInterval reads the timeseries bucket width, defaulting to the one the
// period implies.
func parseInterval(raw, period string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultInterval(period), nil
	}

	dimension, ok := v1Intervals[strings.ToLower(raw)]
	if !ok {
		return "", badParam("interval must be one of minute, hour, date, week or month, not %q", raw)
	}

	return dimension, nil
}

// defaultInterval is the bucket width a period is drawn at when nobody asked.
// The thresholds are about how many points a graph can usefully carry: a year
// of daily buckets is 365 points nobody reads, and one day of daily buckets is
// a single bar.
func defaultInterval(period string) string {
	switch period {
	case "day", "realtime":
		return "time:hour"
	case "6mo", "12mo", "year":
		return "time:month"
	default:
		return "time:day"
	}
}

// v1Periods is the set the compatibility endpoints accept. It is a fixed set
// rather than a pass-through to the engine's presets because an integration
// written against the established API sends exactly these, and an unknown one
// has to come back naming the alternatives.
var v1Periods = map[string]bool{
	"day": true, "7d": true, "30d": true, "month": true,
	"6mo": true, "12mo": true, "custom": true, "all": true,
	"realtime": true, "year": true,
}

// parsePeriod turns a v1 period and its optional `date` parameter into an
// absolute range.
//
// Most of these become explicit bounds rather than one of the engine's presets,
// because the two vocabularies do not line up: `30d` has no preset here and
// resolving it as the nearest one — 28 days — would quietly change every number
// a migrating customer compares against their old dashboard.
func parsePeriod(values url.Values, now time.Time, loc *time.Location) (query.DateRange, error) {
	period := strings.TrimSpace(values.Get("period"))
	if period == "" {
		period = "30d"
	}

	if !v1Periods[period] {
		return query.DateRange{}, badParam(
			"period must be one of day, 7d, 30d, month, 6mo, 12mo, year, all, realtime or custom, not %q", period)
	}

	raw := strings.TrimSpace(values.Get("date"))

	// The anchor is the day the period is measured back from. It defaults to
	// today, and `date` moves it, which is how the established API asks for
	// "the seven days ending on the 14th" without a custom range.
	anchor := startOfLocalDay(now.In(loc), loc)

	if raw != "" && period != "custom" {
		parsed, err := parseDate(raw, loc)
		if err != nil {
			return query.DateRange{}, err
		}
		anchor = parsed
	}

	switch period {
	case "realtime":
		return query.DateRange{Preset: query.RangeRealtime}, nil

	case "all":
		return query.DateRange{Preset: query.RangeAll}, nil

	case "day":
		return dateOnlyRange(anchor, anchor), nil

	case "7d":
		return dateOnlyRange(anchor.AddDate(0, 0, -6), anchor), nil

	case "30d":
		return dateOnlyRange(anchor.AddDate(0, 0, -29), anchor), nil

	case "month":
		start := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, loc)
		return dateOnlyRange(start, start.AddDate(0, 1, -1)), nil

	case "6mo":
		start := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -5, 0)
		return dateOnlyRange(start, anchor), nil

	case "12mo":
		start := time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -11, 0)
		return dateOnlyRange(start, anchor), nil

	case "year":
		start := time.Date(anchor.Year(), 1, 1, 0, 0, 0, 0, loc)
		return dateOnlyRange(start, anchor), nil

	case "custom":
		return parseCustomRange(raw, loc)
	}

	return query.DateRange{}, badParam("period %q cannot be resolved", period)
}

// parseCustomRange reads `date=YYYY-MM-DD,YYYY-MM-DD`.
func parseCustomRange(raw string, loc *time.Location) (query.DateRange, error) {
	if raw == "" {
		return query.DateRange{}, badParam("period=custom needs date=YYYY-MM-DD,YYYY-MM-DD")
	}

	from, to, found := strings.Cut(raw, ",")
	if !found {
		return query.DateRange{}, badParam("period=custom needs two dates, as date=YYYY-MM-DD,YYYY-MM-DD")
	}

	start, err := parseDate(strings.TrimSpace(from), loc)
	if err != nil {
		return query.DateRange{}, err
	}

	end, err := parseDate(strings.TrimSpace(to), loc)
	if err != nil {
		return query.DateRange{}, err
	}

	if end.Before(start) {
		return query.DateRange{}, badParam("the date range ends before it starts")
	}

	return dateOnlyRange(start, end), nil
}

// dateOnlyRange builds an inclusive whole-day range. DateOnly is set so the
// engine reads the end as "through the end of that day" rather than as one
// instant at midnight, which is what a date picker's "to 31 August" means.
func dateOnlyRange(start, end time.Time) query.DateRange {
	return query.DateRange{
		Preset:   query.RangeCustom,
		Start:    time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC),
		End:      time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC),
		DateOnly: true,
	}
}

// parseDate reads one YYYY-MM-DD.
func parseDate(raw string, loc *time.Location) (time.Time, error) {
	parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil {
		return time.Time{}, badParam("date must be written as YYYY-MM-DD, not %q", raw)
	}

	return parsed, nil
}

// startOfLocalDay is local midnight, built from the calendar fields rather than
// by truncating, because truncation works in absolute time and a day is not
// always twenty-four hours long.
func startOfLocalDay(at time.Time, loc *time.Location) time.Time {
	at = at.In(loc)

	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc)
}

// parseCompare reads the comparison request. The empty string means none, which
// is why this returns a pointer rather than a value.
func parseCompare(values url.Values) (*query.Comparison, error) {
	raw := strings.TrimSpace(values.Get("compare"))
	if raw == "" {
		return nil, nil
	}

	switch raw {
	case "previous_period":
		return &query.Comparison{Mode: query.ComparePreviousPeriod}, nil
	case "year_over_year":
		return &query.Comparison{Mode: query.CompareYearOverYear}, nil
	}

	return nil, badParam("compare must be previous_period or year_over_year, not %q", raw)
}

// The v1 filter operators, longest first so that `!=` is matched before `=` and
// `!~` before `~`. Getting that order wrong turns "is not" into "is" against a
// value that starts with `=`, which is a filter that silently means the
// opposite of what it says.
var v1Operators = []struct {
	token    string
	operator string
}{
	{"!=", query.OpIsNot},
	{"==", query.OpIs},
	{"!~", query.OpContainsNot},
	{"~", query.OpContains},
}

// goalDimension is the v1 name for "a session that converted". We have no goal
// registry to look the name up in yet, so it is read as the event name or the
// page path it almost always is — see parseFilter.
const goalDimension = "event:goal"

// parseFilters reads the v1 filter string: predicates joined by `;`, each one a
// dimension, an operator and one or more values joined by `|`.
//
// It is parsed by hand rather than with a regular expression because values
// contain the separators — a URL with a semicolon in it is ordinary — and the
// backslash escape that makes those values expressible is exactly the thing a
// regular expression would get wrong.
func parseFilters(raw string) ([]query.Filter, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var filters []query.Filter

	for _, clause := range splitEscaped(raw, ';') {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}

		filter, err := parseFilter(clause)
		if err != nil {
			return nil, err
		}

		filters = append(filters, filter)
	}

	if len(filters) == 0 {
		return nil, badParam("filters is empty — write it as filters=visit:source==Google;visit:country==US")
	}

	return filters, nil
}

// parseFilter reads one predicate.
func parseFilter(clause string) (query.Filter, error) {
	for _, candidate := range v1Operators {
		dimension, values, found := cutEscaped(clause, candidate.token)
		if !found {
			continue
		}

		dimension = strings.TrimSpace(dimension)
		if dimension == "" {
			return query.Filter{}, badParam("filter %q names no dimension", clause)
		}

		parts := splitEscaped(values, '|')
		for i, part := range parts {
			parts[i] = unescape(part)
		}

		if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
			return query.Filter{}, badParam("filter on %q has no value", dimension)
		}

		if dimension == goalDimension {
			return goalFilter(parts, candidate.operator)
		}

		if err := query.ValidDimension(dimension); err != nil {
			return query.Filter{}, &paramError{message: err.Error()}
		}

		// A wildcard turns an equality into a pattern match. Doing it here
		// rather than in the engine keeps the engine's operator set small and
		// keeps the translation visible in one place.
		if candidate.operator == query.OpIs || candidate.operator == query.OpIsNot {
			if anyWildcard(parts) {
				return wildcardFilter(dimension, parts, candidate.operator == query.OpIsNot)
			}
		}

		return query.Filter{Operator: candidate.operator, Dimension: dimension, Values: parts}, nil
	}

	return query.Filter{}, badParam(
		"filter %q is not a predicate — write it as visit:source==Google, visit:source!=Google, event:page~/blog or event:page!~/blog", clause)
}

// goalFilter reads `event:goal==Signup`.
//
// There is no goal registry to resolve the name against yet, so the value is
// read as what a goal almost always is: a path when it starts with a slash and
// a custom event name otherwise. That is a guess, but it is the guess that makes
// a migrating customer's existing filter string keep working, and the
// alternative — refusing every goal filter — makes the shim useless to exactly
// the people it is for.
func goalFilter(values []string, operator string) (query.Filter, error) {
	if operator != query.OpIs {
		return query.Filter{}, badParam("event:goal supports == only")
	}

	dimension := "event:name"
	for _, value := range values {
		if strings.HasPrefix(value, "/") {
			dimension = "event:page"
			break
		}
	}

	return query.Filter{
		Operator: query.OpHasDone,
		Child:    &query.Filter{Operator: query.OpIs, Dimension: dimension, Values: values},
	}, nil
}

// anyWildcard reports whether a value list uses glob syntax.
func anyWildcard(values []string) bool {
	for _, value := range values {
		if strings.Contains(value, "*") {
			return true
		}
	}

	return false
}

// wildcardFilter turns globbed values into an anchored regular expression.
//
// `*` matches within one path segment and `**` matches across segments, which
// is the established meaning: without the distinction, `/blog/*` on a path
// filter would match every page under every subdirectory and quietly widen
// every migrated filter.
func wildcardFilter(dimension string, values []string, negated bool) (query.Filter, error) {
	patterns := make([]string, 0, len(values))

	for _, value := range values {
		patterns = append(patterns, "^"+globToRegexp(value)+"$")
	}

	operator := query.OpMatches
	if negated {
		operator = query.OpMatchesNot
	}

	for _, pattern := range patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return query.Filter{}, badParam("filter on %q could not be turned into a pattern: %v", dimension, err)
		}
	}

	return query.Filter{Operator: operator, Dimension: dimension, Values: patterns}, nil
}

// globToRegexp translates one glob.
func globToRegexp(value string) string {
	var out strings.Builder

	for i := 0; i < len(value); i++ {
		if value[i] != '*' {
			out.WriteString(regexp.QuoteMeta(string(value[i])))
			continue
		}

		if i+1 < len(value) && value[i+1] == '*' {
			out.WriteString(".*")
			i++
			continue
		}

		out.WriteString("[^/]*")
	}

	return out.String()
}

// splitEscaped splits on a separator, honouring backslash escapes so a value
// may contain the separator itself.
func splitEscaped(value string, separator byte) []string {
	var (
		parts   []string
		current strings.Builder
	)

	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			current.WriteByte(value[i])
			current.WriteByte(value[i+1])
			i++
			continue
		}

		if value[i] == separator {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}

		current.WriteByte(value[i])
	}

	parts = append(parts, current.String())

	return parts
}

// cutEscaped finds the first unescaped occurrence of a token and splits there.
func cutEscaped(value, token string) (string, string, bool) {
	for i := 0; i+len(token) <= len(value); i++ {
		if value[i] == '\\' {
			i++
			continue
		}

		if value[i:i+len(token)] == token {
			return value[:i], value[i+len(token):], true
		}
	}

	return "", "", false
}

// unescape removes the backslashes that protected separators.
func unescape(value string) string {
	var out strings.Builder

	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			out.WriteByte(value[i+1])
			i++
			continue
		}

		out.WriteByte(value[i])
	}

	return out.String()
}
