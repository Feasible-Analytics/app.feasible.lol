//
// sampling.go
// Answering a query that is too big to answer exactly, and saying so.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// DefaultSampleThreshold is how many fact-table row visits a query may be
// estimated to perform before it is answered from a sample instead.
const DefaultSampleThreshold = 10_000_000

// SampleBuckets is how many stable fact-row buckets the SQL predicate uses.
// It is exported because request validation and predicate compilation must use
// the identical precision or the response can claim a rate the query did not
// actually execute.
//
// A power of two lets the materialized stratum allocator use an exact ten-bit
// permutation. Fact ids are deliberately absent from query-time selection;
// every aligned run of 1,024 site/day writes contains each bucket once even
// when imported ids are sparse or signed. Higher ordinal blocks are folded
// into those bits so periodic deletion patterns do not erase one bucket.
const SampleBuckets = 1024

// MinSampleRate is the finest sample the query can express: one row bucket.
const MinSampleRate = 1.0 / SampleBuckets

// sampleLadderBuckets is the stable set of bucket counts automatic sampling
// may choose, coarsest first. A ladder stops a dashboard's rate wobbling with
// every newly arrived event, while integer counts make every disclosed rate
// exactly representable by the SQL predicate.
var sampleLadderBuckets = []int{1024, 512, 204, 102, 51, 20, 10, 5, 2, 1}

// Sampling reasons.
const (
	// SampledOnRequest is a rate the caller asked for.
	SampledOnRequest = "requested"

	// SampledAutomatically is a rate the engine chose because the query was
	// estimated to read more rows than the threshold allows.
	SampledAutomatically = "automatic"
)

// Sampling is the response's account of an answer read from part of the data.
type Sampling struct {
	// Rate is the fraction of event and session fact rows read. It is the same number as
	// meta.sample_rate, repeated here so a client rendering this object does not
	// have to join two pieces of metadata.
	Rate float64 `json:"rate"`

	// Reason is requested or automatic.
	Reason string `json:"reason"`

	// EstimatedRows is the event plus session row-work in the sampled raw plan
	// before the chosen rate is applied. The fact fields split it by table, and
	// the period fields split it between the primary and comparison windows.
	// Repeated passes count repeatedly: an aggregate and its coverage check are
	// two scans, not one row set.
	EstimatedRows              int64 `json:"estimated_rows,omitempty"`
	EstimatedEventRows         int64 `json:"estimated_event_rows,omitempty"`
	EstimatedSessionRows       int64 `json:"estimated_session_rows,omitempty"`
	EstimatedPrimaryRows       int64 `json:"estimated_primary_rows,omitempty"`
	EstimatedComparisonRows    int64 `json:"estimated_comparison_rows,omitempty"`
	ExpectedSampledEventRows   int64 `json:"expected_sampled_event_rows,omitempty"`
	ExpectedSampledSessionRows int64 `json:"expected_sampled_session_rows,omitempty"`
	Threshold                  int64 `json:"threshold,omitempty"`

	// EventMetrics, SessionMetrics and MixedMetrics disclose the independent
	// row grains behind each estimate. Sparse warns that at least one used
	// grain is expected to contribute fewer than one hundred fact-row reads;
	// ZeroResult reports that every returned sampled metric was zero. No
	// confidence interval is claimed because row values may be highly skewed.
	EventMetrics   []string `json:"event_metrics"`
	SessionMetrics []string `json:"session_metrics"`
	MixedMetrics   []string `json:"mixed_metrics"`
	Sparse         bool     `json:"sparse"`
	ZeroResult     bool     `json:"zero_result"`
	Uncertainty    string   `json:"uncertainty"`

	// ScaledMetrics are additive totals expanded by the inverse sample rate.
	// DirectMetrics are rates, averages, extrema and percentiles calculated
	// directly within selected fact rows. Both sets remain estimates.
	ScaledMetrics []string `json:"scaled_metrics"`
	DirectMetrics []string `json:"direct_metrics"`

	// PropertyCoverage reports values actually observed in the selected fact
	// rows separately from inverse-rate estimates of the matching population.
	// Keeping both numbers prevents a sparse sample from being described as an
	// exact full-range property census.
	PropertyCoverage map[string]SampledPropertyCoverage `json:"property_coverage,omitempty"`
}

// SampledPropertyCoverage describes the evidence behind one sampled numeric
// property aggregate. Observed counts are exact only for selected fact rows;
// estimated counts apply the disclosed sample rate to that evidence.
type SampledPropertyCoverage struct {
	ObservedValues         int64 `json:"observed_values"`
	ObservedNumericValues  int64 `json:"observed_numeric_values"`
	EstimatedValues        int64 `json:"estimated_values"`
	EstimatedNumericValues int64 `json:"estimated_numeric_values"`
}

// scanPasses counts full-range equivalents over each fact table.
type scanPasses struct {
	Events   int64
	Sessions int64
}

// scanEstimate is estimated row-work after daily traffic, passes and a date
// span have been combined.
type scanEstimate struct {
	Events   int64
	Sessions int64
}

// total returns both fact-table estimates as one budget number.
func (s scanEstimate) total() int64 {
	return saturatingAdd(s.Events, s.Sessions)
}

// add combines two independently estimated periods without integer overflow.
func (s scanEstimate) add(other scanEstimate) scanEstimate {
	return scanEstimate{
		Events:   saturatingAdd(s.Events, other.Events),
		Sessions: saturatingAdd(s.Sessions, other.Sessions),
	}
}

// sampleThreshold returns the configured ceiling, or the default.
func (e *Engine) sampleThreshold() int64 {
	if e.SampleThreshold == 0 {
		return DefaultSampleThreshold
	}

	return e.SampleThreshold
}

// decideSampling settles the one rate both the primary and comparison periods
// run at, and returns the disclosure that describes the resulting answer.
func (e *Engine) decideSampling(ctx context.Context, q *Query, primary Resolved, comparison *Resolved, blueprint *plan) (*Sampling, error) {
	if q.Exact {
		// Exact is authoritative even if a generic client left a stale rate in
		// the request. Resetting the field matters because the WHERE compiler
		// reads it later.
		q.SampleRate = 1
		return nil, nil
	}

	if q.SampleRate < 1 {
		if err := validateBoundedSampling(q, blueprint); err != nil {
			return nil, err
		}

		primaryRows, known, err := e.factRowsUpper(ctx, q.SiteIDs, primary)
		if err != nil {
			return nil, err
		}
		primaryEstimate := scanEstimate{}
		comparisonEstimate := scanEstimate{}
		if known {
			primaryEstimate = estimateFactWork(primaryRows, plannedScanPasses(q, blueprint, false))
			if comparison != nil {
				comparisonRows, comparisonKnown, err := e.factRowsUpper(ctx, q.SiteIDs, *comparison)
				if err != nil {
					return nil, err
				}
				known = comparisonKnown
				if known {
					comparisonEstimate = estimateFactWork(comparisonRows, plannedScanPasses(q, blueprint, true))
				}
			}
		}
		if !known {
			return nil, requiresExact("sampling is not available until the materialized sampling schema is integrated; set exact to true")
		}

		fullEstimate := primaryEstimate.add(comparisonEstimate)
		threshold := e.sampleThreshold()
		if known && threshold >= 0 && float64(fullEstimate.total())*q.SampleRate > float64(threshold) {
			return nil, requiresExact(
				"the requested sample is estimated to read %d fact rows after applying rate %g, above the %d-row budget; use a lower sample rate, narrow the query, or set exact to true",
				sampledEstimate(fullEstimate.total(), q.SampleRate), q.SampleRate, threshold)
		}

		return samplingMeta(q, blueprint, q.SampleRate, SampledOnRequest, fullEstimate,
			primaryEstimate.total(), comparisonEstimate.total(), threshold), nil
	}

	threshold := e.sampleThreshold()
	if threshold < 0 {
		return nil, nil
	}

	primaryPasses := plannedScanPasses(q, blueprint, false)
	comparisonPasses := scanPasses{}
	if comparison != nil {
		comparisonPasses = plannedScanPasses(q, blueprint, true)
	}

	primarySegments := e.router().Route(q, primary)
	primaryHasRaw := rawSpanDays(primarySegments) > 0
	comparisonHasRaw := false
	var comparisonSegments []Segment
	if comparison != nil {
		comparisonSegments = e.router().Route(q, *comparison)
		comparisonHasRaw = rawSpanDays(comparisonSegments) > 0
	}

	// A query answered wholly from summaries is already on the cheaper path.
	// Raw portions of a split still count: today can be large even when the
	// preceding twenty-seven days came from daily roll-ups.
	if !primaryHasRaw && !comparisonHasRaw {
		return nil, nil
	}

	primaryRows, known, err := e.rawFactRowsUpper(ctx, q.SiteIDs, primarySegments)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, nil
	}

	primaryEstimate := estimateFactWork(primaryRows, primaryPasses)
	primarySeam, seamKnown, err := e.seamScanEstimate(ctx, q, blueprint, primary, primarySegments)
	if err != nil {
		return nil, err
	}
	if !seamKnown {
		return nil, nil
	}
	primaryEstimate = primaryEstimate.add(primarySeam)
	comparisonEstimate := scanEstimate{}
	if comparison != nil {
		comparisonRows, comparisonKnown, err := e.rawFactRowsUpper(ctx, q.SiteIDs, comparisonSegments)
		if err != nil {
			return nil, err
		}
		if !comparisonKnown {
			return nil, nil
		}
		comparisonEstimate = estimateFactWork(comparisonRows, comparisonPasses)
		comparisonSeam, comparisonSeamKnown, err := e.seamScanEstimate(ctx, q, blueprint, *comparison, comparisonSegments)
		if err != nil {
			return nil, err
		}
		if !comparisonSeamKnown {
			return nil, nil
		}
		comparisonEstimate = comparisonEstimate.add(comparisonSeam)
	}
	if primaryEstimate.add(comparisonEstimate).total() <= threshold {
		return nil, nil
	}

	if err := validateBoundedSampling(q, blueprint); err != nil {
		return nil, err
	}
	boundedQuery := *q
	boundedQuery.SampleRate = MinSampleRate
	primaryPasses = plannedScanPasses(&boundedQuery, blueprint, false)
	comparisonPasses = scanPasses{}
	if comparison != nil {
		comparisonPasses = plannedScanPasses(&boundedQuery, blueprint, true)
	}

	// Sampling disables roll-up reads because summary rows already contain
	// every visitor. Once either period triggers sampling, both periods run at
	// one coherent rate over raw facts, so calculate that rate from both full
	// windows rather than only the raw slice that crossed the threshold.
	primaryRows, known, err = e.factRowsUpper(ctx, q.SiteIDs, primary)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, nil
	}
	primaryEstimate = estimateFactWork(primaryRows, primaryPasses)
	comparisonEstimate = scanEstimate{}
	if comparison != nil {
		comparisonRows, comparisonKnown, err := e.factRowsUpper(ctx, q.SiteIDs, *comparison)
		if err != nil {
			return nil, err
		}
		if !comparisonKnown {
			return nil, nil
		}
		comparisonEstimate = estimateFactWork(comparisonRows, comparisonPasses)
	}
	fullEstimate := primaryEstimate.add(comparisonEstimate)

	rate := ladderRate(float64(threshold) / float64(fullEstimate.total()))
	if rate >= 1 {
		// The exact summary/raw split can cost more than a full raw read when a
		// short range needs a boundary correction. Sampling itself disables the
		// split, so use the highest real sample rung when that rare shape was the
		// work that crossed the budget.
		rate = float64(sampleLadderBuckets[1]) / SampleBuckets
	}
	if float64(fullEstimate.total())*rate > float64(threshold) {
		return nil, requiresExact(
			"this query is estimated to read %d fact rows across its metrics and comparison, which exceeds the %d-row budget even at the minimum sample rate; narrow the date range or metrics, or set exact to true to run it without the budget",
			fullEstimate.total(), threshold)
	}

	q.SampleRate = rate

	return samplingMeta(q, blueprint, rate, SampledAutomatically, fullEstimate,
		primaryEstimate.total(), comparisonEstimate.total(), threshold), nil
}

// samplingMeta builds the disclosure shared by requested and automatic
// samples. Metric names stay in request order so clients can present the same
// order they already use for result columns.
func samplingMeta(q *Query, blueprint *plan, rate float64, reason string, estimate scanEstimate, primary, comparison, threshold int64) *Sampling {
	meta := &Sampling{
		Rate:                       rate,
		Reason:                     reason,
		EstimatedRows:              estimate.total(),
		EstimatedEventRows:         estimate.Events,
		EstimatedSessionRows:       estimate.Sessions,
		EstimatedPrimaryRows:       primary,
		EstimatedComparisonRows:    comparison,
		Threshold:                  threshold,
		ExpectedSampledEventRows:   sampledEstimate(estimate.Events, rate),
		ExpectedSampledSessionRows: sampledEstimate(estimate.Sessions, rate),
		Uncertainty:                "sampling error is not quantified; skewed values can differ materially from the full population",
	}

	var sparseEventGrain, sparseSessionGrain bool
	for _, name := range q.Metrics {
		_, propertyMetric := parsePropAggregate(name)
		switch metricSampleGrain(name, blueprint) {
		case "session":
			meta.SessionMetrics = append(meta.SessionMetrics, name)
			if !propertyMetric {
				sparseSessionGrain = true
			}
		case "mixed":
			meta.MixedMetrics = append(meta.MixedMetrics, name)
			sparseEventGrain = true
			sparseSessionGrain = true
		default:
			meta.EventMetrics = append(meta.EventMetrics, name)
			if !propertyMetric {
				sparseEventGrain = true
			}
		}

		definition, ok := metricByName(name)
		if ok && definition.Scaled {
			meta.ScaledMetrics = append(meta.ScaledMetrics, name)
			continue
		}

		meta.DirectMetrics = append(meta.DirectMetrics, name)
	}

	if meta.ScaledMetrics == nil {
		meta.ScaledMetrics = []string{}
	}
	if meta.DirectMetrics == nil {
		meta.DirectMetrics = []string{}
	}
	if meta.EventMetrics == nil {
		meta.EventMetrics = []string{}
	}
	if meta.SessionMetrics == nil {
		meta.SessionMetrics = []string{}
	}
	if meta.MixedMetrics == nil {
		meta.MixedMetrics = []string{}
	}

	meta.Sparse = sparseExpected(meta.ExpectedSampledEventRows, sparseEventGrain) ||
		sparseExpected(meta.ExpectedSampledSessionRows, sparseSessionGrain)

	return meta
}

// sampledEstimate applies a rate to estimated full-row work without rounding a
// nonzero expectation down to zero.
func sampledEstimate(rows int64, rate float64) int64 {
	if rows <= 0 {
		return 0
	}

	return int64(math.Ceil(float64(rows) * rate))
}

// sparseExpected reports a small expected sample on a grain the query uses.
// Unknown estimates are not called sparse; an actually all-zero response is
// disclosed separately after execution.
func sparseExpected(rows int64, used bool) bool {
	return used && rows > 0 && rows < 100
}

// sampledResultIsZero distinguishes an all-zero sampled response from an exact
// zero without claiming that no matching population rows exist outside the
// selected buckets.
func sampledResultIsZero(rows []Row) bool {
	for _, row := range rows {
		for _, value := range row.Metrics {
			if value != 0 {
				return false
			}
		}
		if row.Comparison == nil {
			continue
		}
		for _, value := range row.Comparison.Metrics {
			if value != 0 {
				return false
			}
		}
	}

	return true
}

// metricSampleGrain names the fact-row population from which one metric is
// estimated. Composite exit rate uses independently sampled session exits and
// event pageviews, while a session-scoped numeric property uses sessions.
func metricSampleGrain(name string, blueprint *plan) string {
	if name == "exit_rate" {
		return "mixed"
	}

	if aggregate, ok := parsePropAggregate(name); ok {
		if aggregate.Dim.sessionScoped(blueprint.Scopes) {
			return "session"
		}
		return "event"
	}

	if fact, ok := blueprint.MetricTable[name]; ok && fact == tableSessions {
		return "session"
	}

	return "event"
}

// validateBoundedSampling refuses operations whose meaning depends on all
// events for a visitor or session. Row-grain sampling can bound ordinary fact
// aggregates, but inverse-scaling a distinct visitor or a distinct session
// observed through events is frequency-biased, and row-sampling a has_done set
// changes which complete sessions qualify.
func validateBoundedSampling(q *Query, blueprint *plan) error {
	for _, name := range q.Metrics {
		switch name {
		case "visitors", "time_on_page", "scroll_depth", "conversion_rate", "group_conversion_rate", "revenue_per_visitor":
			return requiresExact("cannot sample %q with bounded fact-row sampling because it requires complete visitor or session event membership; set exact to true or remove that metric", name)
		case "visits":
			if blueprint.MetricTable[name] == tableEvents {
				return requiresExact("cannot sample visits from event-grain rows because a session's inclusion depends on its event count; use a visit-grain breakdown, set exact to true, or remove that metric")
			}
		}
	}

	for _, filter := range q.Filters {
		if filter.Operator == OpHasDone {
			return requiresExact("cannot sample has_done with bounded fact-row sampling because it requires complete event membership for each session; set exact to true or remove that filter")
		}
	}

	if !planUsesSessions(blueprint) {
		return nil
	}

	for _, filter := range q.Filters {
		dimension, err := resolveDimension(filter.Dimension)
		if err != nil {
			continue
		}
		if dimension.isProp() {
			if dimension.sessionScoped(blueprint.Scopes) {
				continue
			}
			return requiresExact("cannot sample session-grain work filtered by %q because it requires complete session event membership; set exact to true or remove that filter", filter.Dimension)
		}
		if dimension.eventOnly() && dimension.EntryColumn == "" && dimension.EntryEventColumn == "" {
			return requiresExact("cannot sample session-grain work filtered by %q because it requires complete session event membership; set exact to true or remove that filter", filter.Dimension)
		}
	}

	for _, dimension := range blueprint.Dimensions {
		if dimension.EntryEventColumn != "" {
			if !containsMetric(q.Metrics, "exit_rate") {
				continue
			}
			return requiresExact("cannot sample session-grain work grouped by %q because it requires reading events inside each session; set exact to true or remove that dimension", dimension.Name)
		}
	}

	return nil
}

// planUsesSessions reports whether any ordinary or composite metric opens the
// sessions fact table.
func planUsesSessions(blueprint *plan) bool {
	if blueprint.Primary == tableSessions || blueprint.HasSecondary && blueprint.Secondary == tableSessions {
		return true
	}

	for _, name := range blueprint.Specials {
		if name == "exit_rate" {
			return true
		}
		if aggregate, ok := parsePropAggregate(name); ok && aggregate.Dim.sessionScoped(blueprint.Scopes) {
			return true
		}
	}

	return false
}

// ladderRate snaps a target fraction down to a rate the bucket ladder offers.
func ladderRate(target float64) float64 {
	chosen := MinSampleRate

	for _, buckets := range sampleLadderBuckets {
		rate := float64(buckets) / SampleBuckets
		if rate <= target {
			chosen = rate
			break
		}
	}

	return chosen
}

// sampleCondition returns a primary-key membership test driven by the narrow,
// materialized sampling table. Every selected bucket is an equality term in an
// IN set, which lets SQLite constrain site, bucket, bot status and time before
// it fetches a fact row. Fact ids never participate in bucket selection.
func sampleCondition(t table, alias string, sites []int64, start, end int64, rate float64, excludeBots bool) expr {
	count := int(math.Round(rate * SampleBuckets))
	if count <= 0 || count >= SampleBuckets {
		return expr{}
	}

	var buckets strings.Builder
	for i := 0; i < count; i++ {
		if i > 0 {
			buckets.WriteByte(',')
		}
		// Selecting from the low end makes reducing a sample remove buckets
		// without changing the membership of the ones that remain. The values are
		// generated integers, so literals avoid SQLite's bind-variable ceiling.
		fmt.Fprint(&buckets, i)
	}

	sampleTable := "event_sampling"
	index := "event_sampling_seek"
	idColumn := "event_id"
	timeColumn := "timestamp"
	sampleAlias := "esample"
	if t == tableSessions {
		sampleTable = "session_sampling"
		index = "session_sampling_seek"
		idColumn = "session_id"
		timeColumn = "started_at"
		sampleAlias = "ssample"
	}

	siteCondition := inInt64(sampleAlias+".site_id", sites)
	conditions := []expr{
		siteCondition,
		{SQL: sampleAlias + ".bucket IN (" + buckets.String() + ")"},
	}
	if t == tableSessions && excludeBots {
		conditions = append(conditions, expr{SQL: sampleAlias + ".is_bot = 0"})
	}
	conditions = append(conditions, expr{
		SQL:  sampleAlias + "." + timeColumn + " >= ? AND " + sampleAlias + "." + timeColumn + " < ?",
		Args: []any{start, end},
	})
	where := and(conditions)

	return expr{
		SQL: alias + ".id IN (SELECT " + sampleAlias + "." + idColumn + " FROM " + sampleTable + " " +
			sampleAlias + " INDEXED BY " + index + " WHERE " + where.SQL + ")",
		Args: where.Args,
	}
}

// rawSpanDays returns the fractional days actually routed to raw facts.
func rawSpanDays(segments []Segment) float64 {
	var days float64

	for _, segment := range segments {
		if segment.Source != SourceRaw {
			continue
		}
		days += spanDays(segment.Range)
	}

	return days
}

// seamScanEstimate charges every raw carry-over statement issued where a
// summary range meets its current raw segment. Each carried metric component
// is a separate statement over the previous day plus the raw segment, and
// nested session predicates are charged by the same rules as ordinary passes.
func (e *Engine) seamScanEstimate(ctx context.Context, q *Query, blueprint *plan, resolved Resolved, segments []Segment) (scanEstimate, bool, error) {
	if len(segments) <= 1 || !rollupBacked(segments) || segments[len(segments)-1].Source != SourceRaw {
		return scanEstimate{}, true, nil
	}

	read, ok := planRollupRead(q, resolved)
	if !ok || read.perBucket {
		return scanEstimate{}, true, nil
	}

	partial := segments[len(segments)-1].Range
	previous, _, needed := seamCorrectionWindow(resolved, partial, read)
	if !needed {
		return scanEstimate{}, true, nil
	}

	passes := scanPasses{}
	for _, name := range q.Metrics {
		t := blueprint.MetricTable[name]
		components, ok := rollupComponents(name, t)
		if !ok {
			continue
		}

		for _, component := range components {
			if component.carried == "" {
				continue
			}
			if t == tableSessions {
				passes.Sessions++
			} else {
				passes.Events++
			}
		}
	}

	passes = nestedScanPasses(q, blueprint, passes)
	window := partial
	window.Start = previous.Start

	rows, known, err := e.factRowsUpper(ctx, q.SiteIDs, window)
	if err != nil || !known {
		return scanEstimate{}, known, err
	}

	return estimateFactWork(rows, passes), true, nil
}

// spanDays returns one range's fractional day count.
func spanDays(r Resolved) float64 {
	span := r.End.Sub(r.Start)
	if span <= 0 {
		return 0
	}

	return span.Hours() / 24
}

// plannedScanPasses counts the statements the executor issues against raw fact
// tables for one period. It mirrors executor.execute: primary and secondary,
// coverage work, each special pass, shared revenue work, and total-row work.
func plannedScanPasses(q *Query, blueprint *plan, comparison bool) scanPasses {
	passes := scanPasses{}
	add := func(t table, count int64) {
		if t == tableSessions {
			passes.Sessions += count
			return
		}
		passes.Events += count
	}

	add(blueprint.Primary, 1)
	if blueprint.HasSecondary {
		add(blueprint.Secondary, 1)
	}

	if !comparison && containsMetric(q.Metrics, "time_on_page") {
		add(tableEvents, 2)
	}

	revenuePlanned := false
	for _, name := range blueprint.Specials {
		switch name {
		case "scroll_depth":
			add(tableEvents, 3)
		case "exit_rate":
			add(tableSessions, 1)
			add(tableEvents, 1)
		case "conversion_rate", "group_conversion_rate":
			add(tableEvents, 2)
		case "total_revenue", "average_revenue", "revenue_per_visitor":
			if revenuePlanned {
				continue
			}
			revenuePlanned = true
			if q.Currency == "" {
				add(tableEvents, 1)
			}
			add(tableEvents, 2) // aggregate plus missing-rate coverage
			if containsMetric(q.Metrics, "revenue_per_visitor") {
				add(tableEvents, 1)
			}
		default:
			aggregate, ok := parsePropAggregate(name)
			if !ok {
				continue
			}
			source := tableEvents
			if aggregate.Dim.sessionScoped(blueprint.Scopes) {
				source = tableSessions
			}
			add(source, 1)
			if !comparison {
				add(source, 1)
			}
		}
	}

	if !comparison && q.Include.TotalRows {
		add(blueprint.Primary, 1)
	}

	return nestedScanPasses(q, blueprint, passes)
}

// nestedScanPasses accounts for indexed event probes inside a fact-table
// statement. Sampled complete-membership scans are refused; session-property
// reads use sessions.entry_props and therefore add no event pass.
func nestedScanPasses(q *Query, blueprint *plan, passes scanPasses) scanPasses {
	eventOuter, sessionOuter := passes.Events, passes.Sessions

	// Integrated session bot status is one materialized column lookup. A legacy
	// version-10 database has no sampling counts, so its compatibility event
	// probe never participates in an automatic estimate before maintenance.

	for _, filter := range q.Filters {
		if filter.Operator == OpHasDone {
			passes.Events = saturatingAdd(passes.Events, saturatingAdd(eventOuter, sessionOuter))
			continue
		}

		d, err := resolveDimension(filter.Dimension)
		if err != nil {
			continue
		}
		if d.eventOnly() && d.EntryColumn == "" && d.EntryEventColumn == "" {
			passes.Events = saturatingAdd(passes.Events, sessionOuter)
		}
	}

	for _, d := range blueprint.Dimensions {
		if d.EntryEventColumn != "" {
			if !containsMetric(q.Metrics, "exit_rate") {
				continue
			}
			passes.Events = saturatingAdd(passes.Events, sessionOuter)
		}
	}

	return passes
}

// containsMetric reports whether a requested metric list contains a name.
func containsMetric(metrics []string, wanted string) bool {
	for _, name := range metrics {
		if name == wanted {
			return true
		}
	}

	return false
}

// factRowsUpper reads exact trigger-maintained counts for every UTC day touched
// by a range. Boundary days are included in full, making this a conservative
// upper bound for partial ranges and ensuring a current spike cannot be diluted
// by historical traffic.
func (e *Engine) factRowsUpper(ctx context.Context, siteIDs []int64, r Resolved) (scanEstimate, bool, error) {
	if !r.End.After(r.Start) {
		return scanEstimate{}, true, nil
	}

	sites := inInt64("site_id", siteIDs)
	startDay := utcDay(r.Start.Unix())
	endDay := utcDay(r.End.Add(-time.Nanosecond).Unix())
	args := append(append([]any{}, sites.Args...), startDay, endDay)

	var events, sessions sql.NullInt64
	err := e.db.QueryRowContext(ctx,
		"SELECT SUM(event_rows), SUM(session_rows) FROM sampling_daily_counts WHERE "+sites.SQL+
			" AND day >= ? AND day <= ?", args...).Scan(&events, &sessions)
	if err != nil {
		// A version-10 database opened for maintenance or compatibility remains
		// exact until migration 0011 installs the sampling schema.
		if strings.Contains(err.Error(), "no such table: sampling_daily_counts") {
			return scanEstimate{}, false, nil
		}
		return scanEstimate{}, false, fmt.Errorf("query: estimate scan: %w", err)
	}

	return scanEstimate{Events: events.Int64, Sessions: sessions.Int64}, true, nil
}

// rawFactRowsUpper adds the bounded daily populations touched by raw segments.
func (e *Engine) rawFactRowsUpper(ctx context.Context, siteIDs []int64, segments []Segment) (scanEstimate, bool, error) {
	var total scanEstimate
	for _, segment := range segments {
		if segment.Source != SourceRaw {
			continue
		}
		rows, known, err := e.factRowsUpper(ctx, siteIDs, segment.Range)
		if err != nil || !known {
			return scanEstimate{}, known, err
		}
		total = total.add(rows)
	}

	return total, true, nil
}

// utcDay returns the UTC midnight containing one Unix timestamp, including
// timestamps before 1970 where integer division would truncate the wrong way.
func utcDay(timestamp int64) int64 {
	at := time.Unix(timestamp, 0).UTC()
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

// estimateFactWork multiplies a range-aware fact population by statement pass
// counts without allowing overflow to turn expensive work into a small number.
func estimateFactWork(rows scanEstimate, passes scanPasses) scanEstimate {
	return scanEstimate{
		Events:   saturatingMultiply(rows.Events, passes.Events),
		Sessions: saturatingMultiply(rows.Sessions, passes.Sessions),
	}
}

// saturatingMultiply combines non-negative counts without integer overflow.
func saturatingMultiply(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left > math.MaxInt64/right {
		return math.MaxInt64
	}
	return left * right
}

// saturatingAdd combines non-negative estimates without integer overflow.
func saturatingAdd(left, right int64) int64 {
	if right > math.MaxInt64-left {
		return math.MaxInt64
	}

	return left + right
}

// sampleWarning is attached to every metric in a sampled answer. It says
// whether inverse-rate scaling was mathematically appropriate rather than
// claiming every metric was scaled or that an unscaled distribution is
// unchanged by observing fewer visitors.
func sampleWarning(sampling *Sampling, scaled bool) string {
	method := "calculated directly within selected fact rows; it was not scaled and may differ materially from the full-population rate, average, extremum or percentile"
	if scaled {
		method = "expanded by the inverse sample rate because it is an additive total; it remains an estimate"
	}

	prefix := fmt.Sprintf("read from %g%% deterministic buckets at each metric's event/session row grain and %s", sampling.Rate*100, method)
	if sampling.Reason != SampledAutomatically {
		return prefix
	}

	return fmt.Sprintf("%s. Before applying that rate, the sampled raw plan represents about %d fact-row reads (%d event and %d session); ask again with exact set to true for the slow, exact answer",
		prefix, sampling.EstimatedRows, sampling.EstimatedEventRows, sampling.EstimatedSessionRows)
}
