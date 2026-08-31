//
// rollup_test.go
// The roll-up seam: routing, splitting, and what may not be split.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"
)

// rawSplitter cuts a range at the start of today and reads both halves raw. It
// is what any router that is not the roll-up router looks like, and the engine
// has to refuse to use it for a metric that does not add up across the cut.
type rawSplitter struct{}

// Route splits at today when there is a complete part to split off.
func (rawSplitter) Route(_ *Query, r Resolved) []Segment {
	complete, partial, split := SplitAtToday(r)
	if !split {
		return []Segment{{Range: r, Source: SourceRaw}}
	}

	return []Segment{
		{Range: complete, Source: SourceRaw},
		{Range: partial, Source: SourceRaw},
	}
}

// fixedState is a coverage reader that answers whatever a test tells it to.
type fixedState struct {
	coverage RollupCoverage
	found    bool
}

// Coverage answers the fixed reading.
func (s fixedState) Coverage(context.Context, int64, Grain) (RollupCoverage, bool) {
	return s.coverage, s.found
}

// coveringRouter is a roll-up router that believes everything is built, so a
// test can check what the router refuses without building anything.
func coveringRouter(timezone string) *RollupRouter {
	return &RollupRouter{State: fixedState{
		found:    true,
		coverage: RollupCoverage{Timezone: timezone, From: 0, Through: math.MaxInt32},
	}}
}

// TestRawRouterAnswersTheWholeRange checks the default.
func TestRawRouterAnswersTheWholeRange(t *testing.T) {
	resolved, err := DateRange{Preset: RangeLast7Days}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	segments := RawRouter{}.Route(&Query{}, resolved)

	if len(segments) != 1 || segments[0].Source != SourceRaw {
		t.Fatalf("the raw router should answer with one raw segment, got %+v", segments)
	}
}

// TestSplitAtTodaySeparatesTheFinishedDays checks the boundary arithmetic the
// roll-up router depends on.
func TestSplitAtTodaySeparatesTheFinishedDays(t *testing.T) {
	resolved, err := DateRange{Preset: RangeLast7Days}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	complete, partial, split := SplitAtToday(resolved)
	if !split {
		t.Fatal("a range that reaches today has a finished part and an unfinished one")
	}

	if got := complete.End.Format(time.RFC3339); got != "2026-08-30T00:00:00Z" {
		t.Errorf("the finished part ends at %s, want 2026-08-30T00:00:00Z", got)
	}

	if got := partial.Start.Format(time.RFC3339); got != "2026-08-30T00:00:00Z" {
		t.Errorf("the unfinished part starts at %s, want 2026-08-30T00:00:00Z", got)
	}

	past, err := DateRange{
		Preset: RangeCustom, DateOnly: true,
		Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC),
	}.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, split := SplitAtToday(past); split {
		t.Error("a range entirely in the past has nothing to split off")
	}
}

// TestAdditiveMetricsSurviveASplitRange checks that the seam works: a query
// answered from two segments has to produce the same number as one answered
// from one.
func TestAdditiveMetricsSurviveASplitRange(t *testing.T) {
	engine := newEngine(t)
	engine.Router = rawSplitter{}

	result := run(t, engine, baseQuery("pageviews", "visits"))

	closeTo(t, "pageviews across two segments", result.Results[0].Metrics[0], 7)
}

// TestDistinctCountsAreNotSplit is the reason Splittable exists. Visitor A
// appears on both days, so adding the two halves would invent a fourth visitor
// who does not exist — so the engine collapses the split instead.
func TestDistinctCountsAreNotSplit(t *testing.T) {
	engine := newEngine(t)
	engine.Router = rawSplitter{}

	result := run(t, engine, baseQuery("visitors"))

	closeTo(t, "visitors across a split range", result.Results[0].Metrics[0], 3)

	if len(result.Meta.Sources) != 1 || result.Meta.Sources[0] != "raw" {
		t.Errorf("meta.sources = %v, want just raw", result.Meta.Sources)
	}
}

// TestSplittableKnowsWhichMetricsAddUp pins the rule itself.
func TestSplittableKnowsWhichMetricsAddUp(t *testing.T) {
	cases := map[string]bool{
		"pageviews":       true,
		"events":          true,
		"bounce_rate":     true,
		"visit_duration":  true,
		"views_per_visit": true,
		"visitors":        false,
		"time_on_page":    false,
		"scroll_depth":    false,
	}

	for name, want := range cases {
		q := Query{SiteIDs: []int64{1}, Metrics: []string{name}}
		q.Normalise()

		decided, err := decide(&q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if got := Splittable(&q, decided); got != want {
			t.Errorf("Splittable(%s) = %v, want %v", name, got, want)
		}
	}
}

// TestTheRouterRefusesEverythingItCannotAnswerExactly is the guard rail on the
// whole feature. Each of these is a query whose answer from a summary would
// differ from its answer from raw, and every one of them has to come back as a
// single raw segment.
func TestTheRouterRefusesEverythingItCannotAnswerExactly(t *testing.T) {
	router := coveringRouter("UTC")

	cases := []struct {
		name   string
		query  Query
		rollup bool
		why    string
	}{
		{
			name:   "the headline numbers",
			query:  Query{SiteIDs: []int64{1}, Metrics: []string{"visitors", "visits", "pageviews", "bounce_rate"}},
			rollup: true,
		},
		{
			name:   "a page breakdown",
			query:  Query{SiteIDs: []int64{1}, Metrics: []string{"visitors", "pageviews"}, Dimensions: []string{"event:page"}},
			rollup: true,
		},
		{
			name:   "a daily graph",
			query:  Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, Dimensions: []string{"time:day"}},
			rollup: true,
		},
		{
			name: "a filtered report",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"},
				Filters: []Filter{{Operator: OpIs, Dimension: "visit:country", Values: []string{"US"}}}},
			why: "a summary has already collapsed the rows a filter would narrow",
		},
		{
			name:  "two sites at once",
			query: Query{SiteIDs: []int64{1, 2}, Metrics: []string{"visitors"}},
			why:   "one visitor on two sites of an account would be added twice",
		},
		{
			name:  "a sampled report",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, SampleRate: 0.1},
			why:   "the summary counted every visitor, not a tenth of them",
		},
		{
			name:  "bots included",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, Include: Include{Bots: true}},
			why:   "the summary excluded automated traffic when it was built",
		},
		{
			name:  "time on page",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"time_on_page"}},
			why:   "it is averaged over the visits that reported engagement, which is a distinct count of visits",
		},
		{
			name:  "a composite",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"exit_rate"}, Dimensions: []string{"event:page"}},
			why:   "a composite needs a second query of its own shape against raw rows",
		},
		{
			name:  "a custom property breakdown",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, Dimensions: []string{"event:props:plan"}},
			why:   "properties live in the cold table and are not summarised",
		},
		{
			name:  "realtime",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, DateRange: DateRange{Preset: RangeRealtime}},
			why:   "a half-hour window of raw rows is already fast",
		},
		{
			name:  "hourly with no time dimension",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visitors"}, DateRange: DateRange{Preset: RangeLast24Hours}},
			why:   "hourly buckets are only ever correct read one at a time",
		},
	}

	for _, test := range cases {
		q := test.query
		if q.DateRange.Preset == "" && q.DateRange.Start.IsZero() {
			q.DateRange = DateRange{Preset: RangeLast28Days}
		}
		q.Normalise()

		resolved, err := q.DateRange.Resolve(fixtureNow, time.UTC, fixtureNow.AddDate(0, -6, 0))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}

		segments := router.Route(&q, resolved)

		if got := rollupBacked(segments); got != test.rollup {
			t.Errorf("%s: routed to %s, want rollup=%v — %s", test.name, rollupExplain(segments), test.rollup, test.why)
		}
	}
}

// TestTheRouterRefusesAnotherTimezone is its own test because it is the one
// refusal that depends on how the buckets were cut rather than on the query.
// A summary built on Los Angeles days cannot answer a question about UTC days:
// the two disagree about which events belong to which day.
func TestTheRouterRefusesAnotherTimezone(t *testing.T) {
	router := coveringRouter("America/Los_Angeles")

	q := Query{SiteIDs: []int64{1}, Metrics: []string{"pageviews"}, Timezone: "UTC"}
	q.Normalise()

	resolved, err := q.DateRange.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if segments := router.Route(&q, resolved); rollupBacked(segments) {
		t.Errorf("a summary cut on Los Angeles days answered a query about UTC days: %s", rollupExplain(segments))
	}
}

// TestTheRouterRefusesWhatIsNotBuiltYet checks the coverage window. A report
// that read zeros out of buckets nobody has built would show a customer a week
// in which nothing happened.
func TestTheRouterRefusesWhatIsNotBuiltYet(t *testing.T) {
	q := Query{SiteIDs: []int64{1}, Metrics: []string{"pageviews"}, Timezone: "UTC"}
	q.Normalise()

	resolved, err := q.DateRange.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	nothing := &RollupRouter{State: fixedState{found: false}}
	if segments := nothing.Route(&q, resolved); rollupBacked(segments) {
		t.Errorf("a site with nothing built was routed to a summary: %s", rollupExplain(segments))
	}

	// Built, but only from yesterday — the twenty-eight day range reaches back
	// further than that.
	partial := &RollupRouter{State: fixedState{found: true, coverage: RollupCoverage{
		Timezone: "UTC",
		From:     RollupLocalUnix(startOfDay(fixtureNow, time.UTC).AddDate(0, 0, -1), time.UTC),
		Through:  RollupLocalUnix(startOfDay(fixtureNow, time.UTC), time.UTC),
	}}}

	if segments := partial.Route(&q, resolved); rollupBacked(segments) {
		t.Errorf("a range reaching past the covered window was routed to a summary: %s", rollupExplain(segments))
	}
}

// TestEveryRollupDimensionHasATableAndAColumn is a structural check against the
// migration. A registry entry naming a column that does not exist fails at the
// first build rather than at compile time, which is a very long way from where
// the mistake was made.
func TestEveryRollupDimensionHasATableAndAColumn(t *testing.T) {
	_, account := newEngineWithAccount(t)

	tables := map[string]bool{}
	for _, name := range RollupTables() {
		tables[name] = true

		var count int
		if err := account.Reader().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&count); err != nil {
			t.Errorf("roll-up table %s is in the registry but not in the schema: %v", name, err)
		}
	}

	seen := map[int]string{}

	for _, dimension := range RollupDims() {
		if !tables[dimension.Table] {
			t.Errorf("dimension %d names table %s, which is not in the registry", dimension.Code, dimension.Table)
		}

		if other, ok := seen[dimension.Code]; ok {
			t.Errorf("dimensions %s and %s share code %d", other, dimension.Name, dimension.Code)
		}
		seen[dimension.Code] = dimension.Name

		if dimension.Total {
			continue
		}

		if dimension.EventColumn != "" {
			assertColumn(t, account.Reader(), "events", dimension.EventColumn)
		}

		if dimension.SessionColumn != "" {
			assertColumn(t, account.Reader(), "sessions", dimension.SessionColumn)
		}

		// Every dimension a caller can name must resolve, or the router would
		// look up a keying that the reader cannot compile.
		if dimension.Name != entryHostnameDimension {
			if _, err := resolveDimension(dimension.Name); err != nil {
				t.Errorf("roll-up dimension %q is not a query dimension: %v", dimension.Name, err)
			}
		}
	}
}

// assertColumn checks that a fact table really has a column the registry names.
func assertColumn(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()

	var found int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&found); err != nil {
		t.Fatal(err)
	}

	if found != 1 {
		t.Errorf("the roll-up registry names %s.%s, which the schema does not have", table, column)
	}
}
