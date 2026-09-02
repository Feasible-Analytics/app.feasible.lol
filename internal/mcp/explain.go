//
// explain.go
// explain_traffic_change: the comparisons an analyst would run, ranked.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// This is the tool worth getting right. Wrapping a query API is a morning's
// work and gets an assistant that can fetch numbers; the reason anybody
// connects analytics to an assistant at all is to ask why something moved, and
// answering that means running the comparisons a person would run and then
// saying which of them actually accounts for the change.
//
// What it does, in order:
//
//  1. Measures the headline metrics over the period and the one before it.
//  2. For each dimension an analyst would check, pulls the top groups in *both*
//     periods and merges them. Pulling only the current period's top groups is
//     the mistake that makes an automated version of this useless: a source that
//     stopped entirely has no rows in the current period at all, and it is
//     usually the answer.
//  3. Ranks every group by how much of the total change it accounts for.
//  4. Names the patterns that mean something specific — one source carrying the
//     whole move, a page that vanished, a change spread so evenly across
//     everything that it is probably measurement rather than audience.

// explainDimensions are the breakdowns to sweep, in the order a person would
// look at them: where traffic came from first, then what it landed on, then who
// and what it came from.
var explainDimensions = []string{
	"visit:source",
	"visit:channel",
	"visit:utm_campaign",
	"event:page",
	"visit:country",
	"visit:device",
	"visit:browser",
	"visit:os",
	"visit:referrer",
}

// headlineMetrics are reported whatever the driving metric is, because a fall in
// visitors with a flat bounce rate is a different story from one where the
// bounce rate doubled, and the second story is the one worth telling.
var headlineMetrics = []string{"visitors", "visits", "pageviews", "bounce_rate", "visit_duration"}

// groupsPerDimension is how many groups are pulled from each period. Twenty-five
// is past the point where a group is large enough to explain a change on its
// own, and small enough that eighteen breakdowns stay fast.
const groupsPerDimension = 25

// moversReported is how many groups per dimension come back in the answer.
const moversReported = 6

// concentratedShare is the share of the total change one group has to carry
// before it is called the cause rather than a contributor.
const concentratedShare = 45

// broadShare is the share below which no single group explains anything, which
// is what makes a change look like measurement rather than audience.
const broadShare = 20

// explainArgs is what the tool takes.
type explainArgs struct {
	SiteID    string          `json:"site_id"`
	DateRange query.DateRange `json:"date_range"`
	Compare   string          `json:"compare"`
	Metric    string          `json:"metric"`
	Filters   []query.Filter  `json:"filters"`
}

// movement is one group's contribution to the change.
type movement struct {
	Value    string  `json:"value"`
	Current  float64 `json:"current"`
	Previous float64 `json:"previous"`
	Delta    float64 `json:"delta"`

	// SharePct is how much of the overall change this group accounts for.
	// It can exceed 100 when groups move in opposite directions, which is
	// itself informative: a group carrying 180% of a small net fall means
	// something else was growing at the same time.
	SharePct float64 `json:"share_of_change_pct"`

	// BounceRate and VisitDuration describe the traffic's quality now. They are
	// carried because a group that arrived with a 100% bounce rate and no time
	// on site is not an audience, it is a bot or a broken redirect.
	BounceRate    float64 `json:"bounce_rate,omitempty"`
	VisitDuration float64 `json:"visit_duration_seconds,omitempty"`
}

// driver is one dimension's contribution.
type driver struct {
	Dimension string `json:"dimension"`

	// ExplainsPct is how much of the total change the reported movers account
	// for between them. A dimension that explains 90% is the one to look at; one
	// that explains 15% is noise wearing a name.
	ExplainsPct float64 `json:"explains_pct"`

	// Pattern is concentrated, broad or mixed — the shape of the change within
	// this dimension, which is what separates "one campaign stopped" from
	// "everything fell a little".
	Pattern string     `json:"pattern"`
	Movers  []movement `json:"movers"`

	// Note records why a dimension could not be measured, rather than dropping
	// it silently. A breakdown that is missing with no explanation is worse than
	// one that is missing with one.
	Note string `json:"note,omitempty"`
}

// explanation is the whole answer.
type explanation struct {
	SiteID string `json:"site_id"`
	Metric string `json:"metric"`

	Period     []string `json:"period"`
	Comparison []string `json:"comparison_period"`
	Mode       string   `json:"comparison_mode"`

	Current   float64  `json:"current"`
	Previous  float64  `json:"previous"`
	Delta     float64  `json:"delta"`
	ChangePct *float64 `json:"change_pct"`

	Headline map[string]headlineChange `json:"headline"`
	Drivers  []driver                  `json:"drivers"`
	Findings []string                  `json:"findings"`
	Summary  string                    `json:"summary"`
}

// headlineChange is one headline metric over both periods.
type headlineChange struct {
	Current   float64  `json:"current"`
	Previous  float64  `json:"previous"`
	ChangePct *float64 `json:"change_pct"`
}

// explainTrafficChangeTool defines the tool.
func (s *Server) explainTrafficChangeTool() *Tool {
	return &Tool{
		Name:  "explain_traffic_change",
		Scope: apikeys.ScopeStatsRead,
		Title: "Explain a traffic change",

		// One call is one headline query plus two breakdowns per dimension,
		// every one exact. It is charged as that many requests so the hourly
		// limit means the same thing here as it does for query_stats.
		Cost: 1 + 2*len(explainDimensions),
		Description: "Work out why a site's traffic moved. Measures the period against the one before " +
			"it, then breaks the change down by source, channel, campaign, page, country, device, " +
			"browser and OS, and ranks what actually accounts for the difference — including things " +
			"that stopped entirely, which never appear in a plain breakdown of the current period. " +
			"Use this instead of a series of query_stats calls whenever the question is \"why\".",
		ReadOnly: true,
		InputSchema: object(map[string]any{
			"site_id":    siteArg(),
			"date_range": dateRangeArg(),
			"compare":    compareArg(),
			"metric": map[string]any{
				"type":        "string",
				"description": "Which number to explain. Defaults to visitors.",
				"enum":        []string{"visitors", "visits", "pageviews", "events"},
			},
			"filters": filtersArg(),
		}, "site_id"),
		Handler: s.explainTrafficChange,
	}
}

// explainTrafficChange runs the whole investigation.
func (s *Server) explainTrafficChange(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
	args := &explainArgs{}
	if err := decodeArgs(raw, args); err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	site, err := s.API.SiteFor(key, args.SiteID)
	if err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	metric := args.Metric
	if metric == "" {
		metric = "visitors"
	}

	if err := query.ValidMetric(metric); err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	mode := args.Compare
	if mode == "" {
		mode = query.ComparePreviousPeriod
	}

	if args.DateRange.Preset == "" && args.DateRange.Start.IsZero() {
		args.DateRange.Preset = query.RangeLast7Days
	}

	// "All time" has no period before it, so there is nothing to compare and
	// nothing to explain. Saying so beats running twenty queries to produce a
	// table of zeroes.
	if args.DateRange.Preset == query.RangeAll {
		return toolFailure("date_range \"all\" has no earlier period to compare against — pick a bounded range such as 7d or 28d"), nil
	}

	current, previous, err := resolvePeriods(s.API, site, args.DateRange, mode)
	if err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	answer, err := s.investigate(ctx, site, metric, mode, args.Filters, args.DateRange, current, previous)
	if err != nil {
		return queryFailure(err)
	}

	return &toolResult{
		Content:           []content{text(answer.Summary)},
		StructuredContent: answer,
	}, nil
}

// resolvePeriods works out both windows against one clock.
//
// Both are resolved here rather than left to the engine because the second
// breakdown has to be asked for as an explicit range: the engine's comparison
// support attaches previous values to the *current* period's rows, which is
// exactly the thing that hides a group that stopped.
func resolvePeriods(api *publicapi.API, site sites.Site, dateRange query.DateRange, mode string) (query.Resolved, query.Resolved, error) {
	location := publicapi.Location(site)

	current, err := dateRange.Resolve(api.Clock(), location, time.Time{})
	if err != nil {
		return query.Resolved{}, query.Resolved{}, err
	}

	previous, err := current.Compare(&query.Comparison{Mode: mode})
	if err != nil {
		return query.Resolved{}, query.Resolved{}, err
	}

	return current, previous, nil
}

// asRange turns a resolved window back into a request for the same window. The
// bounds are written as local wall-clock times because that is how the engine
// reads a custom range, and the resolved values are already in the site's zone.
func asRange(resolved query.Resolved) query.DateRange {
	start := resolved.Start.In(resolved.Location)
	end := resolved.End.In(resolved.Location)

	return query.DateRange{
		Preset: query.RangeCustom,
		Start:  time.Date(start.Year(), start.Month(), start.Day(), start.Hour(), start.Minute(), start.Second(), 0, time.UTC),
		End:    time.Date(end.Year(), end.Month(), end.Day(), end.Hour(), end.Minute(), end.Second(), 0, time.UTC),
	}
}

// investigate runs the queries and assembles the answer.
func (s *Server) investigate(ctx context.Context, site sites.Site, metric, mode string,
	filters []query.Filter, requested query.DateRange, current, previous query.Resolved) (*explanation, error) {

	answer := &explanation{
		SiteID:     site.Domain,
		Metric:     metric,
		Mode:       mode,
		Period:     []string{current.Start.Format(time.RFC3339), current.End.Format(time.RFC3339)},
		Comparison: []string{previous.Start.Format(time.RFC3339), previous.End.Format(time.RFC3339)},
		Headline:   map[string]headlineChange{},
	}

	// The headline uses the engine's own comparison rather than two queries, so
	// that a period still in progress is compared against the same elapsed time.
	// At four in the afternoon, "against yesterday" has to mean sixteen hours
	// against sixteen hours, and that arithmetic belongs in one place. The
	// composed answer has no query meta of its own, so every contributing query
	// is exact rather than letting an estimate become indistinguishable inside
	// the explanation.
	headline, err := s.API.Query(ctx, site, query.Query{
		SiteIDs:   []int64{site.ID},
		Metrics:   headlineMetrics,
		Filters:   filters,
		DateRange: requested,
		Timezone:  site.Timezone,
		Exact:     true,
		Include:   query.Include{Comparisons: &query.Comparison{Mode: mode}},
	})
	if err != nil {
		return nil, err
	}

	for i, name := range headlineMetrics {
		entry := headlineChange{}

		if len(headline.Results) > 0 {
			row := headline.Results[0]

			if i < len(row.Metrics) {
				entry.Current = row.Metrics[i]
			}
			if row.Comparison != nil {
				if i < len(row.Comparison.Metrics) {
					entry.Previous = row.Comparison.Metrics[i]
				}
				if i < len(row.Comparison.Change) {
					entry.ChangePct = row.Comparison.Change[i]
				}
			}
		}

		answer.Headline[name] = entry
	}

	driving := answer.Headline[metric]
	if _, tracked := answer.Headline[metric]; !tracked {
		// A driving metric outside the headline set still needs its own totals,
		// which is one extra query rather than a special case in the loop above.
		driving, err = s.total(ctx, site, metric, filters, requested, mode)
		if err != nil {
			return nil, err
		}
		answer.Headline[metric] = driving
	}

	answer.Current = driving.Current
	answer.Previous = driving.Previous
	answer.Delta = driving.Current - driving.Previous
	answer.ChangePct = driving.ChangePct

	for _, dimension := range explainDimensions {
		answer.Drivers = append(answer.Drivers, s.driverFor(ctx, site, dimension, metric, filters, current, previous, answer.Delta))
	}

	// The dimension that explains the most goes first, because that is the one
	// a person would read and everything below it is corroboration.
	sort.SliceStable(answer.Drivers, func(i, j int) bool {
		return answer.Drivers[i].ExplainsPct > answer.Drivers[j].ExplainsPct
	})

	answer.Findings = findings(answer, current)
	answer.Summary = narrate(answer)

	return answer, nil
}

// total measures one metric over both periods. It stays exact because its
// result is folded into an explanation shape that has nowhere to carry query
// sampling metadata.
func (s *Server) total(ctx context.Context, site sites.Site, metric string,
	filters []query.Filter, requested query.DateRange, mode string) (headlineChange, error) {

	result, err := s.API.Query(ctx, site, query.Query{
		SiteIDs:   []int64{site.ID},
		Metrics:   []string{metric},
		Filters:   filters,
		DateRange: requested,
		Timezone:  site.Timezone,
		Exact:     true,
		Include:   query.Include{Comparisons: &query.Comparison{Mode: mode}},
	})
	if err != nil {
		return headlineChange{}, err
	}

	entry := headlineChange{}

	if len(result.Results) > 0 {
		row := result.Results[0]

		if len(row.Metrics) > 0 {
			entry.Current = row.Metrics[0]
		}
		if row.Comparison != nil {
			if len(row.Comparison.Metrics) > 0 {
				entry.Previous = row.Comparison.Metrics[0]
			}
			if len(row.Comparison.Change) > 0 {
				entry.ChangePct = row.Comparison.Change[0]
			}
		}
	}

	return entry, nil
}

// driverFor breaks the change down along one dimension.
func (s *Server) driverFor(ctx context.Context, site sites.Site, dimension, metric string,
	filters []query.Filter, current, previous query.Resolved, totalDelta float64) driver {

	result := driver{Dimension: dimension}

	now, quality, err := s.breakdown(ctx, site, dimension, metric, filters, asRange(current))
	if err != nil {
		// One dimension that cannot be measured must not take the whole answer
		// down. Some combinations of metric and dimension genuinely do not
		// compose, and the honest response is to say which one and carry on.
		result.Note = "could not be measured: " + shortReason(err)
		return result
	}

	before, _, err := s.breakdown(ctx, site, dimension, metric, filters, asRange(previous))
	if err != nil {
		result.Note = "the earlier period could not be measured: " + shortReason(err)
		return result
	}

	movers := attribute(now, before, quality, totalDelta)

	if len(movers) > moversReported {
		movers = movers[:moversReported]
	}

	result.Movers = movers
	result.ExplainsPct = explains(movers)
	result.Pattern = patternOf(movers, now, before)

	// A dimension whose every value is unset explains nothing, however large its
	// numbers look. Without this it would score a perfect 100% — one group
	// trivially accounts for the whole change — and sort above the dimension
	// that actually names the cause, which is the difference between an answer
	// and a page of noise.
	if allUnset(movers) {
		result.ExplainsPct = 0
		result.Pattern = "unset"
		result.Note = "every visit has this dimension unset, so it cannot account for anything"
		result.Movers = nil
	}

	return result
}

// unsetLabel is what an interned empty value is shown as. It is a real value in
// the data — "not set" is an ordinary id rather than a NULL — but it is never an
// explanation for anything.
const unsetLabel = "(not set)"

// informative reports whether a group is worth naming in a sentence. Telling
// somebody that their unset country accounts for the whole change is true and
// useless.
func informative(mover movement) bool {
	return mover.Value != unsetLabel && mover.Value != ""
}

// allUnset reports whether a dimension has nothing but unset values.
func allUnset(movers []movement) bool {
	for _, mover := range movers {
		if informative(mover) {
			return false
		}
	}

	return true
}

// breakdown pulls one dimension's groups for one window, with the quality
// metrics where the engine can produce them. Each read is exact because these
// values are merged into a custom explanation that cannot expose query meta.
func (s *Server) breakdown(ctx context.Context, site sites.Site, dimension, metric string,
	filters []query.Filter, window query.DateRange) (map[string]float64, map[string][2]float64, error) {

	run := func(metrics []string) (*query.Result, error) {
		return s.API.Query(ctx, site, query.Query{
			SiteIDs:    []int64{site.ID},
			Metrics:    metrics,
			Dimensions: []string{dimension},
			Filters:    filters,
			DateRange:  window,
			Timezone:   site.Timezone,
			OrderBy:    []query.Order{{Key: metric, Descending: true}},
			Pagination: query.Pagination{Limit: groupsPerDimension},
			Exact:      true,
		})
	}

	// Bounce rate and visit duration are asked for first and dropped on
	// refusal, rather than asked for only where they are known to work. The
	// engine decides which combinations compose, and encoding a second copy of
	// that decision here is how the two get out of step.
	withQuality := true

	result, err := run([]string{metric, "bounce_rate", "visit_duration"})
	if err != nil {
		withQuality = false

		result, err = run([]string{metric})
		if err != nil {
			return nil, nil, err
		}
	}

	values := map[string]float64{}
	quality := map[string][2]float64{}

	for _, row := range result.Results {
		if len(row.Dimensions) == 0 || len(row.Metrics) == 0 {
			continue
		}

		label := row.Dimensions[0]
		if label == "" {
			label = "(not set)"
		}

		values[label] = row.Metrics[0]

		if withQuality && len(row.Metrics) >= 3 {
			quality[label] = [2]float64{row.Metrics[1], row.Metrics[2]}
		}
	}

	return values, quality, nil
}

// shortReason renders an engine error for the answer.
func shortReason(err error) string {
	var callerError *query.Error
	if asQueryError(err, &callerError) {
		return callerError.Message
	}

	return "the query failed"
}

// attribute merges the two periods and ranks the groups by how much of the
// total change each one accounts for.
//
// It is a pure function of two maps and a number, which is the point: the
// arithmetic that decides what an assistant tells somebody about their business
// is the part that has to be provably right, and it can be tested here without
// a database, a clock or a query engine.
func attribute(current, previous map[string]float64, quality map[string][2]float64, totalDelta float64) []movement {
	seen := map[string]bool{}
	movers := []movement{}

	add := func(value string) {
		if seen[value] {
			return
		}
		seen[value] = true

		mover := movement{
			Value:    value,
			Current:  current[value],
			Previous: previous[value],
		}
		mover.Delta = mover.Current - mover.Previous

		if measures, ok := quality[value]; ok {
			mover.BounceRate = measures[0]
			mover.VisitDuration = measures[1]
		}

		movers = append(movers, mover)
	}

	for _, value := range sortedKeys(current) {
		add(value)
	}
	for _, value := range sortedKeys(previous) {
		add(value)
	}

	// The denominator is the net change when there is one. When the total barely
	// moved, shares against it would be meaningless or infinite, so the
	// denominator becomes the gross movement instead — which is the right
	// question anyway: a flat total hiding a churned mix is worth saying.
	denominator := math.Abs(totalDelta)
	if denominator < 1 {
		denominator = 0
		for _, mover := range movers {
			denominator += math.Abs(mover.Delta)
		}
	}

	for i := range movers {
		if denominator > 0 {
			movers[i].SharePct = round1(100 * movers[i].Delta / denominator)
		}
	}

	// Biggest absolute movement first, whichever direction it moved in. Sorting
	// by the signed value would bury the group that fell hardest underneath
	// every group that grew slightly.
	sort.SliceStable(movers, func(i, j int) bool {
		return math.Abs(movers[i].Delta) > math.Abs(movers[j].Delta)
	})

	// Groups that did not move are not movers. Keeping them pads the answer
	// with rows whose only content is "this stayed the same".
	trimmed := movers[:0]
	for _, mover := range movers {
		if mover.Delta != 0 {
			trimmed = append(trimmed, mover)
		}
	}

	return trimmed
}

// explains is how much of the change the listed movers account for between
// them, as a percentage of the total movement.
func explains(movers []movement) float64 {
	total := 0.0
	for _, mover := range movers {
		total += math.Abs(mover.SharePct)
	}

	return round1(total)
}

// patternOf names the shape of a change within one dimension.
func patternOf(movers []movement, current, previous map[string]float64) string {
	if len(movers) == 0 {
		return "flat"
	}

	top := math.Abs(movers[0].SharePct)

	switch {
	case top >= concentratedShare:
		return "concentrated"
	case top < broadShare && len(current)+len(previous) >= 8:
		return "broad"
	default:
		return "mixed"
	}
}

// findings names the patterns that mean something specific.
//
// Each one is a sentence a person would say out loud, and each is only emitted
// when the numbers actually support it. A tool that always produces five
// confident findings teaches people to ignore all five.
func findings(answer *explanation, current query.Resolved) []string {
	var notes []string

	if current.IncludesNow() {
		notes = append(notes,
			"The current period is still running, so it is measured against the same elapsed time in the earlier period rather than against a whole one.")
	}

	if math.Abs(answer.Delta) < 1 {
		notes = append(notes, fmt.Sprintf(
			"%s barely moved: %s now against %s before. Anything below is a change in the mix rather than in the total.",
			answer.Metric, number(answer.Current), number(answer.Previous)))
	}

	for _, entry := range answer.Drivers {
		if len(entry.Movers) == 0 {
			continue
		}

		top := entry.Movers[0]

		if informative(top) && math.Abs(top.SharePct) >= concentratedShare && math.Abs(answer.Delta) >= 1 {
			notes = append(notes, fmt.Sprintf(
				"%s %q accounts for %.0f%% of the change on its own (%s to %s).",
				label(entry.Dimension), top.Value, math.Abs(top.SharePct), number(top.Previous), number(top.Current)))
		}

		// A group that went to nothing is the single most useful thing this tool
		// can find, and it is invisible to any breakdown of the current period
		// alone — which is why both periods are pulled.
		for _, mover := range entry.Movers {
			if !informative(mover) {
				continue
			}

			if mover.Previous > 0 && mover.Current == 0 && math.Abs(mover.SharePct) >= 10 {
				notes = append(notes, fmt.Sprintf(
					"%s %q stopped entirely — it was %s before and is zero now.",
					label(entry.Dimension), mover.Value, number(mover.Previous)))
			}

			if mover.Previous == 0 && mover.Current > 0 && math.Abs(mover.SharePct) >= 20 {
				notes = append(notes, fmt.Sprintf(
					"%s %q is new — nothing before, %s now.",
					label(entry.Dimension), mover.Value, number(mover.Current)))
			}

			// Traffic that bounces immediately and stays for no time is not an
			// audience. Saying so is the difference between a celebrated spike
			// and a scraper somebody should block.
			if mover.Delta > 0 && mover.BounceRate >= 95 && mover.VisitDuration <= 2 && math.Abs(mover.SharePct) >= 15 {
				notes = append(notes, fmt.Sprintf(
					"The extra traffic from %s %q bounces at %.0f%% and stays %.0f seconds, which looks automated rather than human.",
					label(entry.Dimension), mover.Value, mover.BounceRate, mover.VisitDuration))
			}
		}
	}

	// A change spread evenly across every page is usually not an audience
	// change at all. It is the shape a broken snippet, a failed deploy or a
	// consent-banner change makes, and it is worth saying before somebody
	// spends a day looking for a marketing explanation that is not there.
	for _, entry := range answer.Drivers {
		if entry.Dimension == "event:page" && entry.Pattern == "broad" && math.Abs(answer.Delta) >= 1 {
			notes = append(notes,
				"The change is spread evenly across pages with no single page responsible, which looks more like a tracking or deployment change than a change in the audience. Check whether the script was deployed or a consent banner changed on that date.")
		}
	}

	if bounce, ok := answer.Headline["bounce_rate"]; ok && bounce.ChangePct != nil && math.Abs(*bounce.ChangePct) >= 15 {
		direction := "rose"
		if *bounce.ChangePct < 0 {
			direction = "fell"
		}

		notes = append(notes, fmt.Sprintf(
			"Bounce rate %s from %.1f%% to %.1f%%, so the traffic that did arrive behaved differently, not just less of it.",
			direction, bounce.Previous, bounce.Current))
	}

	if len(notes) == 0 {
		notes = append(notes, "No single dimension accounts for much of this change; it is spread thinly across everything measured.")
	}

	return notes
}

// narrate writes the paragraph a person reads first.
func narrate(answer *explanation) string {
	var out strings.Builder

	direction := "rose"
	if answer.Delta < 0 {
		direction = "fell"
	}
	if math.Abs(answer.Delta) < 1 {
		direction = "was flat"
	}

	change := ""
	if answer.ChangePct != nil {
		change = fmt.Sprintf(" (%+.1f%%)", *answer.ChangePct)
	}

	fmt.Fprintf(&out, "%s on %s %s from %s to %s%s, comparing %s..%s against %s..%s.\n\n",
		titleCase(strings.ReplaceAll(answer.Metric, "_", " ")),
		answer.SiteID, direction, number(answer.Previous), number(answer.Current), change,
		day(answer.Period[0]), day(answer.Period[1]), day(answer.Comparison[0]), day(answer.Comparison[1]))

	out.WriteString("What accounts for it:\n")
	for _, note := range answer.Findings {
		out.WriteString("  • " + note + "\n")
	}

	out.WriteString("\nBiggest movers by dimension:\n")

	for _, entry := range answer.Drivers {
		if entry.Note != "" {
			fmt.Fprintf(&out, "  %s: %s\n", entry.Dimension, entry.Note)
			continue
		}

		if len(entry.Movers) == 0 {
			continue
		}

		fmt.Fprintf(&out, "  %s (%s, explains %.0f%%):\n", entry.Dimension, entry.Pattern, entry.ExplainsPct)

		for _, mover := range entry.Movers {
			fmt.Fprintf(&out, "    %-28s %s → %s  (%+.0f, %+.0f%% of the change)\n",
				truncate(mover.Value, 28), number(mover.Previous), number(mover.Current), mover.Delta, mover.SharePct)
		}
	}

	return out.String()
}

// label turns a dimension name into the noun a sentence can use.
func label(dimension string) string {
	switch dimension {
	case "visit:source":
		return "source"
	case "visit:channel":
		return "channel"
	case "visit:utm_campaign":
		return "campaign"
	case "event:page":
		return "page"
	case "visit:country":
		return "country"
	case "visit:device":
		return "device"
	case "visit:browser":
		return "browser"
	case "visit:os":
		return "operating system"
	case "visit:referrer":
		return "referrer"
	}

	return dimension
}

// number renders a metric value without trailing noise.
func number(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%.0f", value)
	}

	return fmt.Sprintf("%.1f", value)
}

// day trims an RFC 3339 timestamp to the date, which is what a sentence about a
// week of traffic wants.
func day(timestamp string) string {
	if len(timestamp) >= 10 {
		return timestamp[:10]
	}

	return timestamp
}

// truncate bounds a label so the table stays aligned.
func truncate(value string, width int) string {
	if len(value) <= width {
		return value
	}

	return value[:width-1] + "…"
}

// round1 trims a percentage to one decimal place.
func round1(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}

	return math.Round(value*10) / 10
}

// titleCase capitalises each word of a metric name for the opening sentence.
// Metric names are ASCII identifiers, so the first byte of each word is the
// whole rule.
func titleCase(value string) string {
	words := strings.Fields(value)
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}

	return strings.Join(words, " ")
}
