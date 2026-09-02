//
// v1stats.go
// The v1 compatibility shims: aggregate, timeseries, breakdown and realtime.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// The v1 endpoints exist so that an integration written against the established
// API migrates by changing one hostname. Everything they can express, v2 can
// express better — but a customer with a Looker Studio connector, a WordPress
// plugin and three cron jobs is not going to rewrite all of them to try us, and
// the whole value of matching a wire format is that they do not have to.
//
// Every one of them is the same three steps: parse the query string into a v2
// query, run it through the one engine, and reshape the answer. The reshaping
// is the only part that differs, which is deliberate — if a v1 endpoint ever
// produced a number the equivalent v2 query does not, one of them is lying.

// v1Request is everything the shims parse out of a query string, so that the
// parsing is written once and every endpoint refuses the same bad input in the
// same words.
type v1Request struct {
	Site         sites.Site
	Metrics      []string
	Range        query.DateRange
	Filters      []query.Filter
	Compare      *query.Comparison
	WithBots     bool
	WithImported bool
}

// parseV1 reads the parameters every v1 stats endpoint shares. It resolves the
// site first because the timezone every date is interpreted in belongs to the
// site, not to whoever is calling.
func (a *API) parseV1(w http.ResponseWriter, r *http.Request) (*v1Request, bool) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return nil, false
	}

	if !a.requireScope(w, key, apikeys.ScopeStatsRead) {
		return nil, false
	}
	if !a.requirePermission(w, key, teams.PermViewDashboard) {
		return nil, false
	}

	values := r.URL.Query()

	site, ok := a.resolveSite(w, key, values.Get("site_id"))
	if !ok {
		return nil, false
	}

	location, err := time.LoadLocation(site.Timezone)
	if err != nil {
		// A site whose stored timezone is not an IANA name is our bug, not the
		// caller's, but answering in UTC is far better than refusing to serve
		// their stats at all.
		location = time.UTC
	}

	request := &v1Request{Site: site}

	if request.Metrics, err = parseMetrics(values.Get("metrics")); err != nil {
		return nil, a.refuse(w, err)
	}

	if request.Range, err = parsePeriod(values, a.now(), location); err != nil {
		return nil, a.refuse(w, err)
	}

	if request.Filters, err = parseFilters(values.Get("filters")); err != nil {
		return nil, a.refuse(w, err)
	}

	if request.Compare, err = parseCompare(values); err != nil {
		return nil, a.refuse(w, err)
	}

	if request.WithBots, err = boolParam(values, "with_bots", false); err != nil {
		return nil, a.refuse(w, err)
	}

	if request.WithImported, err = boolParam(values, "with_imported", false); err != nil {
		return nil, a.refuse(w, err)
	}

	return request, true
}

// refuse answers a parameter error as a 400 and returns false, so a parse site
// reads as one line rather than four.
func (a *API) refuse(w http.ResponseWriter, err error) bool {
	var param *paramError
	if errors.As(err, &param) {
		a.fail(w, http.StatusBadRequest, param.message)
		return false
	}

	var callerError *query.Error
	if errors.As(err, &callerError) {
		a.fail(w, http.StatusBadRequest, callerError.Message)
		return false
	}

	a.internal(w, "parse parameters", err)

	return false
}

// toQuery turns a parsed v1 request into the engine's query.
//
// Exact is always set, and that is the one place this endpoint deliberately
// behaves differently from v2. The v1 response shapes carry no meta at all, so
// there is nowhere in them to say that a figure was read from a sample — and an
// estimate a caller cannot tell from an exact number is the one thing sampling
// must never produce. A very large v1 query is therefore slow rather than
// approximate; v2 is where a caller can be told and can choose.
func (v *v1Request) toQuery(dimensions []string, pagination query.Pagination) query.Query {
	return query.Query{
		SiteIDs:    []int64{v.Site.ID},
		Metrics:    v.Metrics,
		Dimensions: dimensions,
		Filters:    v.Filters,
		DateRange:  v.Range,
		Timezone:   v.Site.Timezone,
		Pagination: pagination,
		Exact:      true,
		Include: query.Include{
			Bots:        v.WithBots,
			Imports:     v.WithImported,
			Comparisons: v.Compare,
		},
	}
}

// aggregateMetric is one number in an aggregate response, with the change
// against the comparison period when one was asked for.
type aggregateMetric struct {
	Value  float64  `json:"value"`
	Change *float64 `json:"change,omitempty"`
}

// handleAggregate answers one row of totals.
func (a *API) handleAggregate(w http.ResponseWriter, r *http.Request) {
	request, ok := a.parseV1(w, r)
	if !ok {
		return
	}

	result, err := a.Query(r.Context(), request.Site, request.toQuery(nil, query.Pagination{Limit: 1}))
	if err != nil {
		a.answerQueryError(w, err)
		return
	}

	results := map[string]aggregateMetric{}

	// An empty result set is a period with no traffic, not an error: every
	// metric is zero, and returning an empty object instead would make a
	// client's chart disappear rather than flatten.
	for i, name := range request.Metrics {
		entry := aggregateMetric{}

		if len(result.Results) > 0 {
			row := result.Results[0]

			if i < len(row.Metrics) {
				entry.Value = row.Metrics[i]
			}

			if row.Comparison != nil && i < len(row.Comparison.Change) {
				entry.Change = row.Comparison.Change[i]
			}
		}

		results[name] = entry
	}

	a.write(w, http.StatusOK, map[string]any{"results": results})
}

// handleTimeseries answers one row per bucket.
func (a *API) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	request, ok := a.parseV1(w, r)
	if !ok {
		return
	}

	values := r.URL.Query()

	interval, err := parseInterval(values.Get("interval"), values.Get("period"))
	if err != nil {
		a.refuse(w, err)
		return
	}

	// The limit is the bucket ceiling rather than a page size: a timeseries is
	// read whole by every client that draws a graph, and paginating it would
	// hand them half a chart.
	result, err := a.Query(r.Context(), request.Site,
		request.toQuery([]string{interval}, query.Pagination{Limit: query.MaxLimit}))
	if err != nil {
		a.answerQueryError(w, err)
		return
	}

	rows := make([]map[string]any, 0, len(result.Results))

	for _, row := range result.Results {
		entry := map[string]any{}

		if len(row.Dimensions) > 0 {
			entry["date"] = row.Dimensions[0]
		}

		for i, name := range request.Metrics {
			if i < len(row.Metrics) {
				entry[name] = row.Metrics[i]
			}
		}

		rows = append(rows, entry)
	}

	a.write(w, http.StatusOK, map[string]any{"results": rows})
}

// handleBreakdown answers one row per value of a dimension.
//
// This is the endpoint the incumbent returns a 500 from when `page` is not a
// number, because the value goes straight into an integer parse. Here `page`,
// `limit` and every other parameter go through a parser that answers with a
// sentence, and the test suite asserts that none of them can produce a 500.
func (a *API) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	request, ok := a.parseV1(w, r)
	if !ok {
		return
	}

	values := r.URL.Query()

	property, err := parseProperty(values.Get("property"))
	if err != nil {
		a.refuse(w, err)
		return
	}

	limit, err := intParam(values, "limit", DefaultPageSize, 1, MaxPageSize)
	if err != nil {
		a.refuse(w, err)
		return
	}

	page, err := intParam(values, "page", 1, 1, 1_000_000)
	if err != nil {
		a.refuse(w, err)
		return
	}

	pagination := query.Pagination{Limit: limit, Offset: (page - 1) * limit}

	result, err := a.Query(r.Context(), request.Site, request.toQuery([]string{property}, pagination))
	if err != nil {
		a.answerQueryError(w, err)
		return
	}

	key := shortName(property)
	rows := make([]map[string]any, 0, len(result.Results))

	for _, row := range result.Results {
		entry := map[string]any{}

		if len(row.Dimensions) > 0 {
			entry[key] = row.Dimensions[0]
		}

		for i, name := range request.Metrics {
			if i < len(row.Metrics) {
				entry[name] = row.Metrics[i]
			}
		}

		rows = append(rows, entry)
	}

	a.write(w, http.StatusOK, map[string]any{"results": rows})
}

// handleRealtimeVisitors answers the number of visitors in the last half hour.
//
// The body is a bare integer, which is not a shape anybody would design — but it
// is the shape every badge, status page and shell script already parses, and
// wrapping it in an object would break all of them for no gain.
func (a *API) handleRealtimeVisitors(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeStatsRead) {
		return
	}
	if !a.requirePermission(w, key, teams.PermViewDashboard) {
		return
	}

	values := r.URL.Query()

	site, ok := a.resolveSite(w, key, values.Get("site_id"))
	if !ok {
		return
	}

	filters, err := parseFilters(values.Get("filters"))
	if err != nil {
		a.refuse(w, err)
		return
	}

	result, err := a.Query(r.Context(), site, query.Query{
		SiteIDs:    []int64{site.ID},
		Metrics:    []string{"visitors"},
		Filters:    filters,
		DateRange:  query.DateRange{Preset: query.RangeRealtime},
		Timezone:   site.Timezone,
		Pagination: query.Pagination{Limit: 1},

		// The body is a bare integer, so there is nowhere in it to admit that
		// the figure was estimated — and half an hour of traffic is cheap to
		// count exactly even on the busiest site. A shape that cannot carry the
		// caveat must not carry the estimate either.
		Exact: true,
	})
	if err != nil {
		a.answerQueryError(w, err)
		return
	}

	visitors := 0.0
	if len(result.Results) > 0 && len(result.Results[0].Metrics) > 0 {
		visitors = result.Results[0].Metrics[0]
	}

	a.write(w, http.StatusOK, int64(visitors))
}
