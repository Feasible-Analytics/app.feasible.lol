//
// query.go
// The one request struct every report in the product is compiled from.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package query compiles a single Query struct into parameterised SQL against
// one account's analytics database. Every report in the product — the
// dashboard, the public API, an export — goes through it, because the moment
// there are two ways to count a visitor there are two answers to "how many
// visitors did I have", and no way to tell which one is wrong.
//
// There is no ORM. database/sql and a small builder is the right level for a
// compiler whose whole job is to render aggregate SQL: an ORM would hide the
// one thing this package has to get exactly right, which is the shape of the
// GROUP BY. Nothing here ever concatenates a value into SQL. Column and table
// names come from constants in this package's registries; everything a caller
// supplied travels as a bind parameter.
package query

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The filter operators. They are the complete set: a caller asking for anything
// else gets a 400 naming what it sent, rather than a query that silently
// matches nothing.
const (
	OpIs          = "is"
	OpIsNot       = "is_not"
	OpContains    = "contains"
	OpContainsNot = "contains_not"
	OpMatches     = "matches"
	OpMatchesNot  = "matches_not"
	OpHasDone     = "has_done"
)

// Limits on what one request may ask for. They exist because every one of them
// is a way for a single request to cost the whole box: a filter with fifty
// thousand values, a breakdown across six dimensions, or a page size that pulls
// an account's entire path table into memory.
const (
	MaxLimit        = 10000
	DefaultLimit    = 100
	MaxDimensions   = 5
	MaxFilters      = 32
	MaxFilterValues = 1000

	// MaxGroups caps how many groups are pulled back when the ordering cannot
	// be pushed into SQL — see engine.go. It is a bound on memory, not a
	// bound on the answer, and hitting it is reported in meta.metric_warnings
	// rather than silently truncating.
	MaxGroups = 10000
)

// Query is one request. It is deliberately a plain struct with no methods that
// touch a database: the same value is built by the HTTP handler, echoed back to
// the client in the response, and used by a test to assert a number, and none
// of those three should need a connection.
type Query struct {
	// SiteIDs is which sites to read. It is a list rather than one id because
	// a rolled-up view across an account's sites is the same query with a
	// wider IN clause, and retrofitting that later means touching every
	// builder in the package.
	SiteIDs []int64 `json:"site_ids"`

	// Metrics is what to count, in the order the response returns them. The
	// order is part of the contract: a client reads results[i].metrics[j]
	// positionally, so re-ordering them server-side would silently relabel
	// every number on a dashboard.
	Metrics []string `json:"metrics"`

	// Dimensions is what to group by. Empty means one aggregate row.
	Dimensions []string `json:"dimensions,omitempty"`

	// Filters AND together. Multiple values inside one filter OR together.
	Filters []Filter `json:"filters,omitempty"`

	DateRange DateRange `json:"date_range"`

	// Timezone is an IANA name. It is the site's, not the viewer's: a day
	// boundary has to be the same one the site owner sees in every other
	// report, or yesterday's total changes depending on who is looking.
	Timezone string `json:"timezone,omitempty"`

	OrderBy    []Order    `json:"order_by,omitempty"`
	Pagination Pagination `json:"pagination,omitempty"`
	Include    Include    `json:"include,omitempty"`

	// Currency is the ISO 4217 code the money metrics are reported in.
	// Everything is converted into it at the stored exchange rate. Empty means
	// "the currency the data is already in", which is resolved from the range
	// and refused when the range holds more than one — adding two currencies
	// together is a number nobody could ever reconcile.
	Currency string `json:"currency,omitempty"`

	// SampleRate is the fraction of visitors to read, between 0 and 1. One
	// means no sampling and is the default. Sampling picks visitors rather
	// than rows so that a sampled session is whole: sampling rows would break
	// every session-scoped metric, because half a visit is not a visit.
	//
	// Leaving it at one does not guarantee an exact answer: a query estimated
	// to read more rows than the engine's threshold is sampled on the caller's
	// behalf, and says so in meta.sampling. Exact is how a caller opts out.
	SampleRate float64 `json:"sample_rate,omitempty"`

	// Exact refuses automatic sampling, however large the range. It is the
	// escape hatch for the answer that has to be right rather than quick — an
	// invoice, a reconciliation, a figure going into a report — and it is a
	// separate field rather than sample_rate: 1 because those two are
	// indistinguishable on the wire, and defaulting to exactness would put
	// every dashboard back on the slow path.
	Exact bool `json:"exact,omitempty"`
}

// Filter is one predicate. The wire form is the array shape the ecosystem
// already uses — ["is", "visit:country", ["US"], {"case_sensitive": true}] —
// because matching an established query-API shape is what lets somebody migrate
// to us by changing one hostname.
type Filter struct {
	Operator  string
	Dimension string
	Values    []string

	// CaseInsensitive is stored inverted from the wire's case_sensitive so that
	// the zero value is the default. A filter built in Go with a struct literal
	// is then case-sensitive, which is what everybody expects "is" to mean;
	// with the field the other way round, forgetting it would quietly widen
	// every filter in the codebase.
	CaseInsensitive bool

	// Child is the inner filter of a has_done. A goal filter selects whole
	// sessions by something that happened inside them, so its payload is
	// another filter rather than a dimension and a list of values.
	Child *Filter
}

// Order is one sort key: a metric or dimension name and a direction.
type Order struct {
	Key        string
	Descending bool
}

// Pagination is one page of the single result set. It is one result set on
// purpose: paginating each metric's sub-query independently is how a
// multi-metric breakdown ends up returning page two with rows from page one and
// half its columns null.
type Pagination struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// Include is the optional extras a caller can ask for. Everything here costs
// an extra query or an extra scan, which is why none of it is unconditional.
type Include struct {
	// Imports includes data brought in from another analytics product. It
	// defaults to false so that a number never changes the day an import
	// finishes without anybody asking for it.
	Imports bool `json:"imports,omitempty"`

	// Bots includes traffic we classified as automated. It defaults to false
	// because bot traffic in a dashboard is simply a wrong number; the events
	// are kept rather than deleted so that a misclassification is recoverable.
	Bots bool `json:"bots,omitempty"`

	// TimeLabels asks for the full bucket list even when the query has no time
	// dimension. With a time dimension they are emitted regardless: a graph
	// cannot render a gap as a gap without knowing which buckets exist.
	TimeLabels bool `json:"time_labels,omitempty"`

	// TotalRows asks how many groups the query has before pagination.
	TotalRows bool `json:"total_rows,omitempty"`

	Comparisons *Comparison `json:"comparisons,omitempty"`
}

// Comparison asks for the same query over an earlier period. It exists as a
// first-class part of the request rather than two calls from the client so that
// the two periods are resolved by the same clock and the same timezone — two
// round trips either side of midnight compare the wrong days.
type Comparison struct {
	// Mode is previous_period, year_over_year or custom.
	Mode string `json:"mode"`

	// DateRange is the explicit range for mode custom.
	DateRange DateRange `json:"date_range,omitempty"`
}

// Comparison modes.
const (
	ComparePreviousPeriod = "previous_period"
	CompareYearOverYear   = "year_over_year"
	CompareCustom         = "custom"
)

// Error is a caller's mistake rather than ours, and it carries the message the
// caller reads. Every path into this package returns one of these for bad
// input, which is what lets the HTTP layer answer 400 with something useful
// instead of turning a bad page number into a 500.
type Error struct {
	Message string
}

// Error renders the message. It is the message the API returns verbatim, so it
// is written for the person holding the failing request, not for a log.
func (e *Error) Error() string {
	return e.Message
}

// invalid builds a caller-facing validation error.
func invalid(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// Normalise fills in the defaults a caller left out. It runs before Validate
// and before the query is echoed back, so the echo shows what actually ran
// rather than what was typed — which is the entire point of echoing it.
func (q *Query) Normalise() {
	if q.Timezone == "" {
		q.Timezone = "UTC"
	}

	if q.Pagination.Limit == 0 {
		q.Pagination.Limit = DefaultLimit
	}

	if q.SampleRate == 0 {
		q.SampleRate = 1
	}

	q.Currency = strings.ToUpper(strings.TrimSpace(q.Currency))

	if q.DateRange.Preset == "" && q.DateRange.Start.IsZero() {
		q.DateRange.Preset = RangeLast28Days
	}

	if len(q.OrderBy) == 0 {
		q.OrderBy = defaultOrder(q)
	}
}

// defaultOrder picks the sort a caller who did not ask for one wants. A time
// series reads forwards in time; everything else reads biggest first, which is
// what every breakdown table in every analytics product shows.
func defaultOrder(q *Query) []Order {
	for _, name := range q.Dimensions {
		if isTimeDimension(name) {
			return []Order{{Key: name, Descending: false}}
		}
	}

	if len(q.Metrics) > 0 {
		return []Order{{Key: q.Metrics[0], Descending: true}}
	}

	return nil
}

// Validate rejects everything this package cannot answer, with a message naming
// the field. It is exhaustive on purpose: an unvalidated parameter reaching the
// SQL layer is how `page=foo` becomes a 500 instead of a 400, and a 500 is a
// page somebody has to read our logs to explain.
func (q *Query) Validate() error {
	if len(q.SiteIDs) == 0 {
		return invalid("at least one site is required")
	}

	if len(q.Metrics) == 0 {
		return invalid("at least one metric is required")
	}

	seenMetric := map[string]bool{}
	for _, name := range q.Metrics {
		if _, ok := metricByName(name); !ok {
			return invalid("unknown metric %q — known metrics are %s, plus <aggregate>(event:props:<key>) where <aggregate> is one of %s",
				name, strings.Join(MetricNames(), ", "), strings.Join(AggregateNames(), ", "))
		}
		if seenMetric[name] {
			return invalid("metric %q is listed twice", name)
		}
		seenMetric[name] = true
	}

	if len(q.Dimensions) > MaxDimensions {
		return invalid("a query may group by at most %d dimensions, not %d", MaxDimensions, len(q.Dimensions))
	}

	seenDimension := map[string]bool{}
	for _, name := range q.Dimensions {
		if _, err := resolveDimension(name); err != nil {
			return err
		}
		if seenDimension[name] {
			return invalid("dimension %q is listed twice", name)
		}
		seenDimension[name] = true
	}

	if len(q.Filters) > MaxFilters {
		return invalid("a query may carry at most %d filters, not %d", MaxFilters, len(q.Filters))
	}

	for _, filter := range q.Filters {
		if err := filter.validate(false); err != nil {
			return err
		}
	}

	if _, err := time.LoadLocation(q.Timezone); err != nil {
		return invalid("unknown timezone %q — use an IANA name such as America/Los_Angeles", q.Timezone)
	}

	if err := q.DateRange.validate(); err != nil {
		return err
	}

	if q.Pagination.Limit < 1 || q.Pagination.Limit > MaxLimit {
		return invalid("pagination limit must be between 1 and %d, not %d", MaxLimit, q.Pagination.Limit)
	}

	if q.Pagination.Offset < 0 {
		return invalid("pagination offset cannot be negative")
	}

	for _, order := range q.OrderBy {
		if !seenMetric[order.Key] && !seenDimension[order.Key] {
			return invalid("cannot order by %q — order_by may only name a metric or dimension the query asked for", order.Key)
		}
	}

	if err := validateCurrency(q.Currency); err != nil {
		return err
	}

	if q.SampleRate <= 0 || q.SampleRate > 1 {
		return invalid("sample_rate must be greater than 0 and at most 1, not %g", q.SampleRate)
	}

	if q.Include.Comparisons != nil {
		if err := q.Include.Comparisons.validate(); err != nil {
			return err
		}
	}

	return nil
}

// validateCurrency refuses anything that is not an ISO 4217 alphabetic code.
// Empty is allowed and means the compiler resolves it from the data; a typo is
// not, because a typo would match no stored rate and report every revenue
// figure as zero.
func validateCurrency(code string) error {
	if code == "" {
		return nil
	}

	if len(code) != 3 {
		return invalid("currency must be a three-letter code such as USD, not %q", code)
	}

	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return invalid("currency must be three uppercase letters such as USD, not %q", code)
		}
	}

	return nil
}

// validate checks one comparison request.
func (c *Comparison) validate() error {
	switch c.Mode {
	case ComparePreviousPeriod, CompareYearOverYear:
		return nil
	case CompareCustom:
		if c.DateRange.Preset != RangeCustom {
			return invalid("comparison mode custom needs an explicit date_range")
		}
		return c.DateRange.validate()
	default:
		return invalid("unknown comparison mode %q — use previous_period, year_over_year or custom", c.Mode)
	}
}

// validate checks one filter. The nested flag is what stops has_done filters
// containing each other: a goal inside a goal has no meaning, and allowing it
// would let one request build an arbitrarily deep pile of correlated
// subqueries.
func (f *Filter) validate(nested bool) error {
	switch f.Operator {
	case OpHasDone:
		if nested {
			return invalid("has_done cannot contain another has_done")
		}
		if f.Child == nil {
			return invalid("has_done needs an inner filter, for example [\"has_done\", [\"is\", \"event:name\", [\"Signup\"]]]")
		}
		if len(f.Values) > 0 || f.Dimension != "" {
			return invalid("has_done takes an inner filter, not a dimension and values")
		}
		return f.Child.validate(true)

	case OpIs, OpIsNot, OpContains, OpContainsNot, OpMatches, OpMatchesNot:
	default:
		return invalid("unknown filter operator %q", f.Operator)
	}

	dimension, err := resolveDimension(f.Dimension)
	if err != nil {
		return err
	}

	if dimension.Time {
		return invalid("%q cannot be filtered — narrow the date range instead", f.Dimension)
	}

	if len(f.Values) == 0 {
		return invalid("filter on %q needs at least one value", f.Dimension)
	}

	if len(f.Values) > MaxFilterValues {
		return invalid("filter on %q carries %d values, more than the %d allowed", f.Dimension, len(f.Values), MaxFilterValues)
	}

	if f.Operator == OpMatches || f.Operator == OpMatchesNot {
		for _, value := range f.Values {
			if _, err := regexp.Compile(value); err != nil {
				return invalid("filter on %q has an invalid regular expression %q: %v", f.Dimension, value, err)
			}
		}
	}

	return nil
}

// Negated reports whether this operator excludes rather than includes. The
// compiler builds the positive predicate once and wraps it, so "is" and
// "is_not" can never drift apart.
func (f *Filter) Negated() bool {
	switch f.Operator {
	case OpIsNot, OpContainsNot, OpMatchesNot:
		return true
	}

	return false
}

// UnmarshalJSON reads the array wire form. It is hand-written because the shape
// is positional — operator, dimension, values, modifiers — and because a
// has_done carries a nested filter where the others carry a dimension.
func (f *Filter) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return invalid("a filter must be an array, for example [\"is\", \"visit:country\", [\"US\"]]")
	}

	if len(raw) < 2 {
		return invalid("a filter needs at least an operator and a dimension")
	}

	if err := json.Unmarshal(raw[0], &f.Operator); err != nil {
		return invalid("a filter's first element must be an operator string")
	}

	if f.Operator == OpHasDone {
		child := &Filter{}
		if err := child.UnmarshalJSON(raw[1]); err != nil {
			return err
		}
		f.Child = child

		return nil
	}

	if err := json.Unmarshal(raw[1], &f.Dimension); err != nil {
		return invalid("a filter's second element must be a dimension name")
	}

	if len(raw) < 3 {
		return invalid("filter on %q needs a list of values", f.Dimension)
	}

	if err := json.Unmarshal(raw[2], &f.Values); err != nil {
		return invalid("filter on %q needs its values as an array of strings", f.Dimension)
	}

	if len(raw) > 3 {
		var modifiers struct {
			CaseSensitive *bool `json:"case_sensitive"`
		}
		if err := json.Unmarshal(raw[3], &modifiers); err != nil {
			return invalid("filter on %q has modifiers that are not an object", f.Dimension)
		}
		if modifiers.CaseSensitive != nil {
			f.CaseInsensitive = !*modifiers.CaseSensitive
		}
	}

	return nil
}

// MarshalJSON writes the array wire form back. The modifier object is always
// written, even when it holds the default, because the echoed query exists to
// remove ambiguity and an absent flag is exactly the ambiguity it removes.
func (f Filter) MarshalJSON() ([]byte, error) {
	if f.Operator == OpHasDone {
		return json.Marshal([]any{f.Operator, f.Child})
	}

	values := f.Values
	if values == nil {
		values = []string{}
	}

	return json.Marshal([]any{
		f.Operator,
		f.Dimension,
		values,
		map[string]any{"case_sensitive": !f.CaseInsensitive},
	})
}

// UnmarshalJSON reads ["visitors", "desc"].
func (o *Order) UnmarshalJSON(data []byte) error {
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) != 2 {
		return invalid("order_by entries look like [\"visitors\", \"desc\"]")
	}

	o.Key = raw[0]

	switch strings.ToLower(raw[1]) {
	case "desc":
		o.Descending = true
	case "asc":
		o.Descending = false
	default:
		return invalid("order direction must be asc or desc, not %q", raw[1])
	}

	return nil
}

// MarshalJSON writes ["visitors", "desc"].
func (o Order) MarshalJSON() ([]byte, error) {
	direction := "asc"
	if o.Descending {
		direction = "desc"
	}

	return json.Marshal([]string{o.Key, direction})
}
