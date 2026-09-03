//
// report.go
// The goals report: unique conversions, total conversions, and the rate.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// ReportRequest asks for a site's goals over a period.
type ReportRequest struct {
	SiteID int64

	// DateRange and Timezone are the same range the rest of the dashboard is
	// showing. The timezone is the site's, so a day boundary here is the same
	// boundary every other number on the screen used.
	DateRange query.DateRange
	Timezone  string

	// Filters narrow both the population that could convert and each goal's
	// matching events. Dashboard filters therefore describe one population
	// consistently from the headline visitors through the conversion rate.
	Filters []query.Filter

	// Exact carries the dashboard's explicit refusal of sampled answers through
	// every underlying query used to assemble the report.
	Exact bool

	// Currency converts every goal's money into one currency so the totals can
	// be added up. Empty means each goal reports in its own currency and
	// nothing is converted, which needs no exchange rate and is never wrong.
	Currency string

	// IncludeEmptyAutomatic keeps the goals we created ourselves in the answer
	// even when nothing matched them. The report hides them by default: an
	// automatic 404 goal costs nothing precisely because it does not appear
	// until 404 events actually arrive, and a settings screen is the only
	// place somebody wants to see the empty ones.
	IncludeEmptyAutomatic bool
}

// ReportRow is one goal's line.
type ReportRow struct {
	Goal Goal `json:"goal"`

	// Label is the goal's display name or its readable fallback. Sending the
	// package's own label prevents every client from reimplementing that rule.
	Label string `json:"label"`

	// UniqueConversions counts each visitor at most once. Clicking a tracked
	// button in two visits is still one unique conversion and every matching
	// event remains visible in TotalConversions.
	UniqueConversions int64 `json:"unique_conversions"`

	// TotalConversions counts every matching event.
	TotalConversions int64 `json:"total_conversions"`

	// ConvertedVisitors is how many distinct visitors converted at least once.
	// It is the numerator of the rate below, and it is reported beside the
	// rate so the rate is never a number nobody can reconstruct.
	ConvertedVisitors int64 `json:"converted_visitors"`

	// ConversionRate is converted visitors over every visitor in the period,
	// as a percentage. The divisor is the whole period rather than anything
	// narrower — that is what makes it comparable between goals.
	ConversionRate float64 `json:"conversion_rate"`

	// Revenue is the money this goal carried, in minor units, and the currency
	// those units are in. Zero for a goal that is not revenue-bearing.
	Revenue         int64  `json:"revenue"`
	AverageRevenue  int64  `json:"average_revenue"`
	RevenuePerVisit int64  `json:"revenue_per_visitor"`
	Currency        string `json:"currency,omitempty"`

	// From is the instant this goal's counting actually started, which is the
	// later of the report range and the goal's creation. It is returned rather
	// than implied because a goal created halfway through the period reports a
	// smaller number than its neighbours for a reason the reader cannot
	// otherwise see.
	From time.Time `json:"from"`

	// Partial marks a row whose window was cut short by the goal's creation
	// time.
	Partial bool `json:"partial"`
}

// ReportResult is the whole report.
type ReportResult struct {
	Rows []ReportRow `json:"rows"`

	// Visitors and Visits are the period's totals: the divisor every rate on
	// the report used, returned so the reader can check the arithmetic.
	Visitors int64 `json:"visitors"`
	Visits   int64 `json:"visits"`

	// From and To are the resolved period.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Report answers the goals report for one site.
//
// Every number here comes back through the query compiler, one query per goal,
// rather than from aggregate SQL written for this screen. That costs a handful
// of small queries and buys the only thing that matters: the conversions on
// this page are counted by exactly the same code as the visitors on the graph
// beside it, so the two can never disagree.
func Report(ctx context.Context, db *sql.DB, engine *query.Engine, req ReportRequest) (*ReportResult, error) {
	list, err := List(ctx, db, req.SiteID)
	if err != nil {
		return nil, err
	}

	resolved, err := resolveRange(ctx, db, engine, req.SiteID, req.DateRange, req.Timezone)
	if err != nil {
		return nil, err
	}

	full := NewWindow(resolved.Start, resolved.End)

	result := &ReportResult{Rows: []ReportRow{}, From: resolved.Start, To: resolved.End}

	// The divisor is read once per distinct window. Most goals share the whole
	// period, so this is one query however many goals a site has, and a goal
	// created mid-period pays for one more.
	totals := map[Window]periodTotals{}

	period, err := periodFor(ctx, engine, req, full, resolved.Location, totals)
	if err != nil {
		return nil, err
	}

	result.Visitors, result.Visits = period.Visitors, period.Visits

	for _, goal := range list {
		window := full.clampTo(goal.CreatedAt)

		row := ReportRow{
			Goal:    goal,
			Label:   goal.Label(),
			From:    time.Unix(window.Start, 0).In(resolved.Location),
			Partial: window.Start > full.Start,
		}

		if !window.Empty() {
			if err := countGoal(ctx, engine, req, goal, window, resolved.Location, &row); err != nil {
				return nil, err
			}

			divisor, err := periodFor(ctx, engine, req, window, resolved.Location, totals)
			if err != nil {
				return nil, err
			}

			row.ConversionRate = percentage(row.ConvertedVisitors, divisor.Visitors)

			if row.Revenue != 0 && divisor.Visitors > 0 {
				row.RevenuePerVisit = row.Revenue / divisor.Visitors
			}
		}

		// An automatic goal with nothing to show stays out of the report. That
		// is what makes creating one on every new site free: a site that never
		// serves a 404 never sees a 404 goal.
		if goal.IsAutomatic && row.TotalConversions == 0 && !req.IncludeEmptyAutomatic {
			continue
		}

		result.Rows = append(result.Rows, row)
	}

	return result, nil
}

// periodTotals is the denominator every rate on the report divides by.
type periodTotals struct {
	Visitors int64
	Visits   int64
}

// periodFor counts everybody who could have converted in a window, caching by
// window so that goals sharing a period share one query.
func periodFor(ctx context.Context, engine *query.Engine, req ReportRequest, window Window, loc *time.Location, cache map[Window]periodTotals) (periodTotals, error) {
	if cached, ok := cache[window]; ok {
		return cached, nil
	}

	q := query.Query{
		SiteIDs:   []int64{req.SiteID},
		Metrics:   []string{"visitors", "visits"},
		Filters:   append([]query.Filter(nil), req.Filters...),
		DateRange: customRange(window, loc),
		Timezone:  req.Timezone,
		Exact:     req.Exact,
		Include:   query.Include{Imports: true},
	}

	result, err := engine.Run(ctx, q)
	if err != nil {
		return periodTotals{}, err
	}

	var totals periodTotals

	if len(result.Results) > 0 {
		totals.Visitors = int64(result.Results[0].Metrics[0])
		totals.Visits = int64(result.Results[0].Metrics[1])
	}

	cache[window] = totals

	return totals, nil
}

// countGoal fills in one row's conversions, and its money when the goal
// carries any.
func countGoal(ctx context.Context, engine *query.Engine, req ReportRequest, goal Goal, window Window, loc *time.Location, row *ReportRow) error {
	if goal.Kind == KindScroll {
		return countScrollGoal(ctx, engine, req, goal, window, loc, row)
	}
	filters, err := goal.Filters()
	if err != nil {
		return err
	}
	filters = append(filters, req.Filters...)

	// A visitor is the unit of a unique conversion. The same person converting
	// in two sessions is one unique conversion, while every matching event is
	// still included in total conversions.
	metrics := []string{"visitors", "events"}

	if goal.IsRevenue {
		metrics = append(metrics, "total_revenue", "average_revenue")
	}

	q := query.Query{
		SiteIDs:   []int64{req.SiteID},
		Metrics:   metrics,
		Filters:   filters,
		DateRange: customRange(window, loc),
		Timezone:  req.Timezone,
		Currency:  reportCurrency(req, goal),
		Exact:     req.Exact,
		Include:   query.Include{Imports: true},
	}

	result, err := engine.Run(ctx, q)
	if err != nil {
		return err
	}

	if len(result.Results) == 0 {
		return nil
	}

	values := result.Results[0].Metrics

	row.UniqueConversions = int64(values[0])
	row.TotalConversions = int64(values[1])
	row.ConvertedVisitors = row.UniqueConversions

	if goal.IsRevenue && len(values) >= 4 {
		row.Revenue = int64(values[2])
		row.AverageRevenue = int64(values[3])
		row.Currency = reportCurrency(req, goal)
	}

	return nil
}

// countScrollGoal counts visitors whose engagement measurement reached the
// configured threshold. Scroll depth is a numeric fact column rather than a
// query dimension, so this small ordered-report escape hatch still compiles
// the dashboard's filters through the canonical query filter compiler.
func countScrollGoal(ctx context.Context, engine *query.Engine, req ReportRequest, goal Goal, window Window, loc *time.Location, row *ReportRow) error {
	predicate, err := goalPredicate(ctx, engine.Database(), goal, -1)
	if err != nil {
		return err
	}
	resolved := query.Resolved{
		Preset: query.RangeCustom, Location: loc,
		Start: time.Unix(window.Start, 0).In(loc), End: time.Unix(window.End, 0).In(loc),
	}
	filterSQL, filterArgs, err := engine.EventFilterSQL(ctx, []int64{req.SiteID}, resolved, req.Filters, "e")
	if err != nil {
		return err
	}
	where, baseArgs := baseConditions("e", req.SiteID, window)
	statement := "SELECT COUNT(DISTINCT e.user_id), COUNT(*) FROM events e WHERE " + where + " AND (" + predicate.SQL + ")"
	args := append(baseArgs, predicate.Args...)
	if filterSQL != "" {
		statement += " AND (" + filterSQL + ")"
		args = append(args, filterArgs...)
	}
	if err := engine.Database().QueryRowContext(ctx, statement, args...).Scan(&row.UniqueConversions, &row.TotalConversions); err != nil {
		return fmt.Errorf("goals: count scroll goal: %w", err)
	}
	row.ConvertedVisitors = row.UniqueConversions
	return nil
}

// reportCurrency picks the currency one goal's money is reported in. The
// request wins when it names one, because a report that adds four goals
// together has to add them in one currency; otherwise the goal reports in its
// own, which needs no exchange rate and cannot be stale.
func reportCurrency(req ReportRequest, goal Goal) string {
	if req.Currency != "" {
		return req.Currency
	}

	return goal.Currency
}

// customRange renders a window as the explicit date range the compiler takes.
//
// The bounds are written in the site's timezone rather than in UTC, because
// the compiler reinterprets a custom range's wall-clock components in that
// timezone. Handing it a UTC wall clock would move both ends by the site's
// offset, which is a whole day of traffic in the wrong report.
func customRange(window Window, loc *time.Location) query.DateRange {
	return query.DateRange{
		Preset: query.RangeCustom,
		Start:  time.Unix(window.Start, 0).In(loc),
		End:    time.Unix(window.End, 0).In(loc),
	}
}

// resolveRange turns the request's date range into absolute bounds, using the
// same resolver the compiler uses so that a goals report and the graph above
// it cover exactly the same seconds.
func resolveRange(ctx context.Context, db *sql.DB, engine *query.Engine, siteID int64, dateRange query.DateRange, timezone string) (query.Resolved, error) {
	if timezone == "" {
		timezone = "UTC"
	}

	location, err := time.LoadLocation(timezone)
	if err != nil {
		return query.Resolved{}, invalid("unknown timezone %q — use an IANA name such as America/Los_Angeles", timezone)
	}

	if dateRange.Preset == "" && dateRange.Start.IsZero() {
		dateRange.Preset = query.RangeLast28Days
	}

	var earliest time.Time

	if dateRange.NeedsEarliest() {
		found, err := earliestEvent(ctx, db, siteID)
		if err != nil {
			return query.Resolved{}, err
		}
		earliest = found
	}

	now := time.Now().UTC()
	if engine != nil && engine.Now != nil {
		now = engine.Now()
	}

	return dateRange.Resolve(now.In(location), location, earliest)
}

// earliestEvent finds a site's first native or imported event, which is where
// “all time” starts. A site with neither answers the zero time, which the range
// resolver turns into today rather than into 1970.
func earliestEvent(ctx context.Context, db *sql.DB, siteID int64) (time.Time, error) {
	var earliest sql.NullInt64

	if err := db.QueryRowContext(ctx, `
		SELECT MIN(timestamp) FROM (
			SELECT timestamp FROM events WHERE site_id = ?
			UNION ALL
			SELECT timestamp FROM imported_rollups WHERE site_id = ?
		)`, siteID, siteID).Scan(&earliest); err != nil {
		return time.Time{}, fmt.Errorf("goals: first event: %w", err)
	}

	if !earliest.Valid {
		return time.Time{}, nil
	}

	return time.Unix(earliest.Int64, 0).UTC(), nil
}

// percentage divides and answers zero for an empty denominator. Nobody
// converted out of nobody is zero per cent, not an error and not a NaN that
// would travel to a JSON encoder which cannot represent it.
func percentage(numerator, denominator int64) float64 {
	if denominator <= 0 {
		return 0
	}

	value := 100 * float64(numerator) / float64(denominator)

	return float64(int64(value*1000+0.5)) / 1000
}
