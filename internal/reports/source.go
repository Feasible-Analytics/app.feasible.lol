//
// source.go
// Turning one site's traffic into the numbers a report and an alert are made of.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// TopN is how many rows each list in a report carries. Five is the number that
// fits on a phone screen without scrolling, and a report nobody scrolls is a
// report people keep reading.
const TopN = 5

// SiteRef is everything the stats source needs to find a site's data.
type SiteRef struct {
	SiteID    int64
	AccountID int64
	Domain    string
	Timezone  string
}

// Location loads the site's zone, falling back to UTC. A report that renders a
// date is better with the wrong offset than not at all; the *scheduler* refuses
// to fire on an unloadable zone, so by the time anything reaches here the zone
// has already been proven to load.
func (s SiteRef) Location() *time.Location {
	location, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}

	return location
}

// Snapshot is a period's numbers, already shaped for a template.
type Snapshot struct {
	Figures    []Figure
	TopPages   []Entry
	TopSources []Entry
	Countries  []Entry

	// Visitors is the raw unique-visitor count, kept alongside the formatted
	// figures so the job can tell a quiet week from a broken tracker without
	// parsing its own output back.
	Visitors int
}

// StatsSource answers the three questions reports and alerts ask. It is an
// interface so the job logic can be tested against fixed numbers rather than
// against a database somebody has to seed first — the interesting bugs in a
// notifier are in when it fires, not in how it counts.
type StatsSource interface {
	// Period is the totals and top lists for a closed window.
	Period(ctx context.Context, site SiteRef, from, to time.Time) (Snapshot, error)

	// CurrentVisitors is how many people are on the site right now, which is
	// what a spike alert is thresholded against.
	CurrentVisitors(ctx context.Context, site SiteRef) (int, error)

	// VisitorsInLastHours is unique visitors over a rolling window, which is
	// what a drop alert is thresholded against.
	VisitorsInLastHours(ctx context.Context, site SiteRef, hours int) (int, error)
}

// QuerySource is the real StatsSource, reading through the same query engine
// the dashboard runs on.
//
// It goes through the engine rather than writing its own SQL for the reason the
// engine exists at all: the moment there are two ways to count a visitor there
// are two answers to "how many visitors did I have", and an email that disagrees
// with the dashboard it links to is worse than no email.
type QuerySource struct {
	Accounts *accounts.Manager

	// Now is the clock the rolling windows are measured from.
	Now func() time.Time

	// SampleThreshold is passed to each query engine. Reports and alerts force
	// exact execution regardless, but keeping the configured ceiling available
	// makes that invariant testable against an intentionally small population.
	SampleThreshold int64
}

// NewQuerySource builds a source over the account manager.
func NewQuerySource(manager *accounts.Manager) *QuerySource {
	return &QuerySource{Accounts: manager, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the injected clock, falling back to the real one.
func (q *QuerySource) now() time.Time {
	if q.Now == nil {
		return time.Now().UTC()
	}

	return q.Now().UTC()
}

// engine opens the site's account database and builds an engine over it.
func (q *QuerySource) engine(ctx context.Context, site SiteRef) (*query.Engine, error) {
	account, err := q.Accounts.Open(ctx, site.AccountID)
	if err != nil {
		return nil, fmt.Errorf("reports: open account %d: %w", site.AccountID, err)
	}

	engine := query.New(account.Reader())
	engine.Now = q.now
	engine.SampleThreshold = q.SampleThreshold

	return engine, nil
}

// Period builds the whole snapshot for a closed window.
//
// The four queries are separate calls rather than one because the engine
// answers one result set per request by design — a breakdown paginated per
// metric is how a multi-metric report ends up with page two of pages beside
// page one of sources.
func (q *QuerySource) Period(ctx context.Context, site SiteRef, from, to time.Time) (Snapshot, error) {
	engine, err := q.engine(ctx, site)
	if err != nil {
		return Snapshot{}, err
	}

	location := site.Location()

	// The bounds arrive as instants at the site's local midnight. The engine
	// takes a wall-clock range plus a timezone, and End is inclusive on the
	// wire, so the exclusive upper bound becomes the last whole local day.
	dateRange := query.DateRange{
		Preset:   query.RangeCustom,
		Start:    stripLocation(from.In(location)),
		End:      stripLocation(to.In(location).AddDate(0, 0, -1)),
		DateOnly: true,
	}

	totals, err := engine.Run(ctx, query.Query{
		SiteIDs:   []int64{site.SiteID},
		Metrics:   []string{"visitors", "visits", "pageviews", "bounce_rate", "visit_duration"},
		DateRange: dateRange,
		Timezone:  site.Timezone,
		Include:   query.Include{Comparisons: &query.Comparison{Mode: query.ComparePreviousPeriod}},
		Exact:     true,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("reports: totals for %s: %w", site.Domain, err)
	}

	snapshot := Snapshot{Figures: figuresFrom(totals)}

	if len(totals.Results) > 0 && len(totals.Results[0].Metrics) > 0 {
		snapshot.Visitors = int(math.Round(totals.Results[0].Metrics[0]))
	}

	snapshot.TopPages, err = q.top(ctx, engine, site, dateRange, "event:page", "pageviews")
	if err != nil {
		return Snapshot{}, err
	}

	snapshot.TopSources, err = q.top(ctx, engine, site, dateRange, "visit:source", "visitors")
	if err != nil {
		return Snapshot{}, err
	}

	snapshot.Countries, err = q.top(ctx, engine, site, dateRange, "visit:country", "visitors")
	if err != nil {
		return Snapshot{}, err
	}

	return snapshot, nil
}

// top runs one breakdown and formats it.
func (q *QuerySource) top(ctx context.Context, engine *query.Engine, site SiteRef,
	dateRange query.DateRange, dimension, metric string) ([]Entry, error) {
	result, err := engine.Run(ctx, query.Query{
		SiteIDs:    []int64{site.SiteID},
		Metrics:    []string{metric},
		Dimensions: []string{dimension},
		DateRange:  dateRange,
		Timezone:   site.Timezone,
		OrderBy:    []query.Order{{Key: metric, Descending: true}},
		Pagination: query.Pagination{Limit: TopN},
		Exact:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("reports: %s for %s: %w", dimension, site.Domain, err)
	}

	entries := make([]Entry, 0, len(result.Results))

	for _, row := range result.Results {
		label := ""
		if len(row.Dimensions) > 0 {
			label = row.Dimensions[0]
		}

		if strings.TrimSpace(label) == "" {
			label = "(not set)"
		}

		value := 0.0
		if len(row.Metrics) > 0 {
			value = row.Metrics[0]
		}

		entries = append(entries, Entry{Label: label, Value: formatCount(value)})
	}

	return entries, nil
}

// CurrentVisitors counts who is on the site now. Realtime is the engine's own
// thirty-minute window, which is the session timeout — so "current" means the
// same thing here as it does on the live pill on the dashboard.
func (q *QuerySource) CurrentVisitors(ctx context.Context, site SiteRef) (int, error) {
	engine, err := q.engine(ctx, site)
	if err != nil {
		return 0, err
	}

	result, err := engine.Run(ctx, query.Query{
		SiteIDs:   []int64{site.SiteID},
		Metrics:   []string{"visitors"},
		DateRange: query.DateRange{Preset: query.RangeRealtime},
		Timezone:  site.Timezone,
		Exact:     true,
	})
	if err != nil {
		return 0, fmt.Errorf("reports: current visitors for %s: %w", site.Domain, err)
	}

	return firstMetric(result), nil
}

// VisitorsInLastHours counts unique visitors over a rolling window.
func (q *QuerySource) VisitorsInLastHours(ctx context.Context, site SiteRef, hours int) (int, error) {
	engine, err := q.engine(ctx, site)
	if err != nil {
		return 0, err
	}

	location := site.Location()
	now := q.now().In(location)

	result, err := engine.Run(ctx, query.Query{
		SiteIDs: []int64{site.SiteID},
		Metrics: []string{"visitors"},
		DateRange: query.DateRange{
			Preset: query.RangeCustom,
			Start:  stripLocation(now.Add(-time.Duration(hours) * time.Hour)),
			End:    stripLocation(now),
		},
		Timezone: site.Timezone,
		Exact:    true,
	})
	if err != nil {
		return 0, fmt.Errorf("reports: rolling visitors for %s: %w", site.Domain, err)
	}

	return firstMetric(result), nil
}

// firstMetric reads the single number an aggregate query returns, treating an
// empty result as zero. Empty is the honest answer for a site with no traffic,
// and it is exactly the case a drop alert exists to catch.
func firstMetric(result *query.Result) int {
	if result == nil || len(result.Results) == 0 || len(result.Results[0].Metrics) == 0 {
		return 0
	}

	return int(math.Round(result.Results[0].Metrics[0]))
}

// stripLocation drops the zone from a local time, leaving the wall clock. The
// engine's custom range holds wall-clock bounds with no location on purpose —
// the location belongs to the site — so handing it a zoned time would apply the
// offset twice.
func stripLocation(local time.Time) time.Time {
	return time.Date(local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
}

// figureLabels names the five metrics a report carries, in the order they are
// asked for. It is a slice rather than a map so the report's column order is
// the query's metric order, which is the contract the engine returns rows on.
var figureLabels = []string{"Unique visitors", "Visits", "Pageviews", "Bounce rate", "Visit duration"}

// figuresFrom formats the totals row and its comparison.
func figuresFrom(result *query.Result) []Figure {
	figures := make([]Figure, 0, len(figureLabels))

	if result == nil || len(result.Results) == 0 {
		for _, label := range figureLabels {
			figures = append(figures, Figure{Label: label, Value: "0", Direction: "flat"})
		}

		return figures
	}

	row := result.Results[0]

	for i, label := range figureLabels {
		if i >= len(row.Metrics) {
			break
		}

		figure := Figure{Label: label, Value: formatMetric(i, row.Metrics[i]), Direction: "flat"}

		if row.Comparison != nil && i < len(row.Comparison.Change) && row.Comparison.Change[i] != nil {
			change := *row.Comparison.Change[i]
			figure.Change = formatChange(change)
			figure.Direction = directionOf(change)
		}

		figures = append(figures, figure)
	}

	return figures
}

// formatMetric renders one metric by its position, because the three shapes —
// a count, a percentage and a duration — read as nonsense in each other's form.
func formatMetric(index int, value float64) string {
	switch index {
	case 3:
		return strconv.FormatFloat(math.Round(value), 'f', 0, 64) + "%"
	case 4:
		return formatDuration(value)
	}

	return formatCount(value)
}

// formatCount renders a whole number with thousands separators. It is written
// here rather than pulled from a formatting library because it is nine lines
// and the alternative is a dependency in a binary whose pitch is that it has
// almost none.
func formatCount(value float64) string {
	digits := strconv.FormatInt(int64(math.Round(value)), 10)

	negative := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")

	var out strings.Builder

	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}

		out.WriteRune(digit)
	}

	if negative {
		return "-" + out.String()
	}

	return out.String()
}

// formatDuration renders seconds as minutes and seconds.
func formatDuration(seconds float64) string {
	total := int(math.Round(seconds))
	if total < 60 {
		return strconv.Itoa(total) + "s"
	}

	return fmt.Sprintf("%dm %ds", total/60, total%60)
}

// formatChange renders a percentage difference with an explicit sign, using a
// real minus sign rather than a hyphen so the figure lines up under a
// proportional font in a mail client.
func formatChange(change float64) string {
	rounded := math.Round(change)

	if rounded > 0 {
		return "+" + strconv.FormatFloat(rounded, 'f', 0, 64) + "%"
	}

	if rounded < 0 {
		return "−" + strconv.FormatFloat(-rounded, 'f', 0, 64) + "%"
	}

	return "no change"
}

// directionOf classifies a change so the template can colour it without
// re-parsing the string above.
func directionOf(change float64) string {
	switch {
	case math.Round(change) > 0:
		return "up"
	case math.Round(change) < 0:
		return "down"
	}

	return "flat"
}
