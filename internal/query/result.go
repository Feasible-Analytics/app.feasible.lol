//
// result.go
// The response shape, including the resolved query echoed back to the caller.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import "time"

// Result is one answered query.
type Result struct {
	Results []Row `json:"results"`
	Meta    Meta  `json:"meta"`

	// Query is the query as it actually ran, with every default filled in and
	// every date resolved to an instant. It is echoed back because date maths
	// is the single biggest source of "your numbers are wrong" reports: a
	// client that can read back the exact window it was given can tell a
	// disagreement about data from a disagreement about which days were in it.
	Query ResolvedQuery `json:"query"`
}

// Row is one group. Metrics and Dimensions are positional and line up with the
// request's own lists, which is what lets a client index into them without
// looking up names on every row.
type Row struct {
	Metrics    []float64 `json:"metrics"`
	Dimensions []string  `json:"dimensions"`

	Comparison *ComparisonRow `json:"comparison,omitempty"`
}

// ComparisonRow is the same group over the comparison period.
type ComparisonRow struct {
	Metrics []float64 `json:"metrics"`

	// Change is the percentage difference per metric, or null where the
	// earlier value was zero. Null rather than zero or infinity: there is no
	// meaningful percentage change from nothing, and reporting one as a number
	// puts a fabricated figure on a dashboard.
	Change []*float64 `json:"change"`
}

// Warning explains that a metric does not mean the obvious thing. Making a
// partially-covered or re-scoped metric announce itself is the difference
// between a number somebody can trust and a number somebody has to verify.
type Warning struct {
	Code    string `json:"code"`
	Warning string `json:"warning"`
}

// Warning codes. They are stable strings so a client can react to one — greying
// out a figure, showing a footnote — without parsing English.
const (
	WarnEntryScoped     = "entry_scoped"
	WarnSessionScoped   = "session_scoped"
	WarnPartialBucket   = "partial_bucket"
	WarnGroupsTruncated = "groups_truncated"
	WarnSampled         = "sampled"
	WarnNoCoverage      = "no_coverage"
)

// Meta is everything about the answer that is not a number in a row.
type Meta struct {
	// TimeLabels is every bucket in the range, including the empty ones. A
	// graph handed only the buckets that had traffic cannot tell a quiet day
	// from a day the tracker was broken.
	TimeLabels []string `json:"time_labels,omitempty"`

	// PresentIndex is the bucket that is still filling up, or null when the
	// range has finished. It is always present in the response: the graph
	// dashes that bucket, and without it every live chart ends in a cliff.
	PresentIndex *int `json:"present_index"`

	// MetricWarnings is keyed by metric name.
	MetricWarnings map[string]Warning `json:"metric_warnings,omitempty"`

	// TotalRows is how many groups exist before pagination, when it was asked
	// for.
	TotalRows *int `json:"total_rows,omitempty"`

	// Interval is the bucket width the range was rendered at.
	Interval string `json:"interval"`

	// SampleRate is the fraction of visitors actually read.
	SampleRate float64 `json:"sample_rate"`

	// Sources names where the data came from — raw tables today, and a mix of
	// raw and roll-up once summaries exist.
	Sources []string `json:"sources"`

	// ComparisonDateRange is the resolved comparison window, when one was
	// asked for. It is resolved server-side and echoed for the same reason the
	// main range is.
	ComparisonDateRange []string `json:"comparison_date_range,omitempty"`
}

// ResolvedQuery is the request after normalisation, with the date range turned
// into two instants.
type ResolvedQuery struct {
	SiteIDs    []int64    `json:"site_ids"`
	Metrics    []string   `json:"metrics"`
	Dimensions []string   `json:"dimensions"`
	Filters    []Filter   `json:"filters"`
	DateRange  []string   `json:"date_range"`
	Preset     string     `json:"date_range_preset"`
	Timezone   string     `json:"timezone"`
	OrderBy    []Order    `json:"order_by"`
	Pagination Pagination `json:"pagination"`
	Include    Include    `json:"include"`
	SampleRate float64    `json:"sample_rate"`
}

// resolvedQuery builds the echo. The bounds are rendered in the site's timezone
// with its offset attached, so that a reader can see both the instant and the
// day it belonged to without doing the conversion themselves.
func resolvedQuery(q *Query, r Resolved) ResolvedQuery {
	filters := q.Filters
	if filters == nil {
		filters = []Filter{}
	}

	dimensions := q.Dimensions
	if dimensions == nil {
		dimensions = []string{}
	}

	return ResolvedQuery{
		SiteIDs:    q.SiteIDs,
		Metrics:    q.Metrics,
		Dimensions: dimensions,
		Filters:    filters,
		DateRange: []string{
			r.Start.In(r.Location).Format(time.RFC3339),
			r.End.In(r.Location).Format(time.RFC3339),
		},
		Preset:     r.Preset,
		Timezone:   q.Timezone,
		OrderBy:    q.OrderBy,
		Pagination: q.Pagination,
		Include:    q.Include,
		SampleRate: q.SampleRate,
	}
}

// warningSet collects metric warnings without letting one overwrite another
// silently. First writer wins, because the first warning recorded for a metric
// is the one raised by the decision that changed its meaning most.
type warningSet struct {
	items map[string]Warning
}

// add records a warning against a metric.
func (w *warningSet) add(metric, code, message string) {
	if w.items == nil {
		w.items = map[string]Warning{}
	}

	if _, exists := w.items[metric]; exists {
		return
	}

	w.items[metric] = Warning{Code: code, Warning: message}
}

// all returns the collected warnings, or nil when there are none so the field
// stays out of the response.
func (w *warningSet) all() map[string]Warning {
	if len(w.items) == 0 {
		return nil
	}

	return w.items
}

// change is the percentage difference between two values, or nil when the
// earlier one was zero.
func change(current, previous float64) *float64 {
	if previous == 0 {
		return nil
	}

	value := round(100 * (current - previous) / previous)

	return &value
}
