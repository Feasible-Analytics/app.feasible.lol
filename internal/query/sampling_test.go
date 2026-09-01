//
// sampling_test.go
// A sampled answer has to say so, and a caller has to be able to refuse one.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// TestGeneratedSetsDoNotConsumeSQLiteVariables covers both sources of large
// trusted sets: 1023 selected buckets and 32 filters with 1,000 values each.
// The generated bucket literals add no binds, and each validated filter list
// occupies one JSON bind under the bundled modernc SQLite build.
func TestGeneratedSetsDoNotConsumeSQLiteVariables(t *testing.T) {
	_, account := newEngineWithAccount(t)

	for _, rate := range []float64{1023.0 / 1024, 0.5} {
		condition := sampleCondition(tableEvents, "e", []int64{1}, at(24, 0, 0), at(31, 0, 0), rate, false)
		if len(condition.Args) != 3 {
			t.Fatalf("rate %g generated %d population binds, want three site/time binds and no bucket binds", rate, len(condition.Args))
		}
	}

	values := make([]string, MaxFilterValues)
	for i := range values {
		values[i] = fmt.Sprintf("value-%04d", i)
	}

	q := baseQuery("pageviews")
	q.Include.Bots = true
	q.Include.Imports = true
	q.Filters = make([]Filter, MaxFilters)
	for i := range q.Filters {
		q.Filters[i] = Filter{Operator: OpIs, Dimension: "event:name", Values: values}
	}
	q.Normalise()
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}

	for _, rate := range []float64{1, 1023.0 / 1024, 0.5} {
		q.SampleRate = rate
		builder := newWhereBuilder(tableEvents, compileContext{sampleRate: rate}, nil, q.SiteIDs,
			Resolved{Start: time.Unix(at(24, 0, 0), 0), End: time.Unix(at(31, 0, 0), 0)})
		conditions := builder.base(&q)
		filters, err := builder.compile(q.Filters)
		if err != nil {
			t.Fatal(err)
		}
		where := and(append(conditions, filters...))
		if len(where.Args) > MaxFilters+6 {
			t.Fatalf("rate %g compiled %d binds, want at most %d", rate, len(where.Args), MaxFilters+6)
		}
		_ = explainPlan(t, account, "SELECT COUNT(*) FROM events e WHERE "+where.SQL, where.Args)
	}

	hasDone := Filter{Operator: OpHasDone, Child: &Filter{
		Operator: OpIs, Dimension: "event:name", Values: values,
	}}
	exact := baseQuery("pageviews")
	exact.Include.Bots = true
	exact.Include.Imports = true
	builder := newWhereBuilder(tableEvents, compileContext{sampleRate: 1}, nil, exact.SiteIDs,
		Resolved{Start: time.Unix(at(24, 0, 0), 0), End: time.Unix(at(31, 0, 0), 0)})
	condition, err := builder.hasDone(hasDone)
	if err != nil {
		t.Fatal(err)
	}
	if len(condition.Args) > 4 {
		t.Fatalf("nested selector compiled %d binds for 1,000 values", len(condition.Args))
	}
	_ = explainPlan(t, account, "SELECT COUNT(*) FROM events e WHERE "+condition.SQL, condition.Args)
}

// TestAdversarialGiantVisitorAndSessionStayRowBounded proves the property the
// old visitor-hash sample could not provide. The production-speed run uses two
// million events from one visitor and session; race instrumentation uses a
// smaller but still multi-bucket population because modernc's translated
// SQLite backfill otherwise takes more than thirty minutes. Both variants
// select only one of every 1024 event facts, while a visit metric reads the
// single sampled sessions row and never opens events.
func TestAdversarialGiantVisitorAndSessionStayRowBounded(t *testing.T) {
	ctx := context.Background()
	account := newAccountThrough(t, 10)

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, is_bounce, entry_props)
		VALUES (1024, 1, 42, ?, ?, 1, '{"plan":"pro"}')`, at(29, 0, 0), at(29, 1, 0)); err != nil {
		t.Fatal(err)
	}
	giantEvents := int64(2_000_000)
	queryBound := 2 * time.Second
	if raceInstrumented() {
		giantEvents = 65_536
		queryBound = 10 * time.Second
		t.Log("race instrumentation: using 65,536 events; the normal matrix proves the two-million-event bound")
	}
	if _, err := account.Writer().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id)
		SELECT n, 1, ?, 0, 42, 1024 FROM seq`, giantEvents, at(29, 0, 0)); err != nil {
		t.Fatal(err)
	}

	// Backfill after the large fixture is present so the measured query time does
	// not include two million trigger-maintenance writes.
	installSamplingSchema(t, account.Writer())
	if _, err := account.Writer().ExecContext(ctx,
		"UPDATE session_sampling SET bucket = 0 WHERE session_id = 1024"); err != nil {
		t.Fatal(err)
	}
	engine := New(account.Reader())
	engine.Now = func() time.Time { return fixtureNow }

	exactSQL := "SELECT SUM(e.id), COUNT(*) FROM events e WHERE e.site_id = ? AND e.timestamp >= ? AND e.timestamp < ?"
	args := []any{int64(1), at(28, 0, 0), at(30, 0, 0)}
	started := time.Now()
	var exactSum, exactRows int64
	if err := account.Reader().QueryRowContext(ctx, exactSQL, args...).Scan(&exactSum, &exactRows); err != nil {
		t.Fatal(err)
	}
	exactElapsed := time.Since(started)

	condition := sampleCondition(tableEvents, "e", []int64{1}, at(28, 0, 0), at(30, 0, 0), MinSampleRate, false)
	sampledSQL := strings.Replace(exactSQL, "events e", "events e NOT INDEXED", 1) + " AND " + condition.SQL
	eventPlan := explainPlan(t, account, sampledSQL,
		append(append([]any{}, args...), condition.Args...))
	t.Logf("giant-session event sampled plan: %s", strings.ReplaceAll(eventPlan, "\n", " | "))
	if !strings.Contains(eventPlan, "INDEX event_sampling_seek") ||
		!strings.Contains(eventPlan, "INTEGER PRIMARY KEY (rowid=?)") {
		t.Fatalf("sampled event query is not membership-driven:\n%s", eventPlan)
	}
	started = time.Now()
	var sampledSum, sampledRows int64
	sampleArgs := append(append([]any{}, args...), condition.Args...)
	if err := account.Reader().QueryRowContext(ctx, sampledSQL, sampleArgs...).Scan(&sampledSum, &sampledRows); err != nil {
		t.Fatal(err)
	}
	sampledElapsed := time.Since(started)

	wantRows := float64(giantEvents) / SampleBuckets
	if exactRows != giantEvents || math.Abs(float64(sampledRows)-wantRows) > 1 {
		t.Fatalf("exact/sample rows = %d/%d, want %d and within one row of %.3f", exactRows, sampledRows, giantEvents, wantRows)
	}
	if sampledElapsed > queryBound {
		t.Fatalf("one-bucket event scan took %s for %d selected rows", sampledElapsed, sampledRows)
	}
	t.Logf("%d-event visitor: exact scan %s; sampled %d rows in %s", exactRows, exactElapsed, sampledRows, sampledElapsed)

	q := baseQuery("events")
	q.Include.Bots = true
	q.Include.Imports = true
	q.SampleRate = MinSampleRate
	result := run(t, engine, q)
	estimate := result.Results[0].Metrics[0]
	if estimate < float64(giantEvents)*0.99 || estimate > float64(giantEvents)*1.01 {
		t.Fatalf("row-sampled event estimate = %g for %d rows", estimate, giantEvents)
	}
	if result.Meta.Sampling.ZeroResult {
		t.Fatal("a nonzero sampled row set was disclosed as zero")
	}

	visitors := baseQuery("visitors")
	visitors.SampleRate = MinSampleRate
	if _, err := engine.Run(ctx, visitors); err == nil || !strings.Contains(err.Error(), "complete visitor") {
		t.Fatalf("sparse visitor sample error = %v", err)
	}
	visitors.Exact = true
	if exact := run(t, engine, visitors).Results[0].Metrics[0]; exact != 1 {
		t.Fatalf("exact sparse visitors = %g, want 1", exact)
	}

	visits := baseQuery("bounce_rate")
	visits.Include.Imports = true
	visits.SampleRate = MinSampleRate
	visits.Filters = []Filter{{Operator: OpIs, Dimension: "event:page_title", Values: []string{""}}}
	started = time.Now()
	visitResult := run(t, engine, visits)
	if elapsed := time.Since(started); elapsed > queryBound {
		t.Fatalf("giant-session visit query took %s", elapsed)
	}
	closeTo(t, "giant session bounce rate", visitResult.Results[0].Metrics[0], 100)

	builder := newWhereBuilder(tableSessions, compileContext{sampleRate: MinSampleRate, sessionFacts: true}, nil, []int64{1},
		Resolved{Start: time.Unix(at(28, 0, 0), 0), End: time.Unix(at(30, 0, 0), 0)})
	visitConditions := builder.base(&visits)
	visitFilters, err := builder.compile(visits.Filters)
	if err != nil {
		t.Fatal(err)
	}
	visitWhere := and(append(visitConditions, visitFilters...))
	visitPlan := explainPlan(t, account, "SELECT SUM(s.is_bounce), COUNT(*) FROM sessions s NOT INDEXED WHERE "+visitWhere.SQL, visitWhere.Args)
	if !strings.Contains(visitPlan, "INDEX session_sampling_seek") ||
		!strings.Contains(visitPlan, "INTEGER PRIMARY KEY (rowid=?)") || strings.Contains(visitPlan, "events") {
		t.Fatalf("giant-session plan left sessions grain:\n%s", visitPlan)
	}

	if _, err := account.Writer().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1024)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id)
		SELECT ? + n, 2, ?, 0, 42, 2048 FROM seq`, giantEvents, at(29, 0, 0)); err != nil {
		t.Fatal(err)
	}
	condition = sampleCondition(tableEvents, "e", []int64{1, 2}, at(28, 0, 0), at(30, 0, 0), MinSampleRate, false)
	multiSQL := "SELECT COUNT(*) FROM events e WHERE e.site_id IN (?, ?) AND e.timestamp >= ? AND e.timestamp < ? AND " + condition.SQL
	multiArgs := []any{1, 2, at(28, 0, 0), at(30, 0, 0)}
	multiArgs = append(multiArgs, condition.Args...)
	var multiRows int64
	if err := account.Reader().QueryRowContext(ctx, multiSQL, multiArgs...).Scan(&multiRows); err != nil {
		t.Fatal(err)
	}
	if bound := (giantEvents+1024)/SampleBuckets + 2; multiRows > bound {
		t.Fatalf("multi-site sample selected %d rows, above concrete bound %d", multiRows, bound)
	}
}

// raceInstrumented reports whether this test binary was compiled with -race.
// The Go tool records that flag in build settings, which is more reliable than
// an environment variable and leaves ordinary test binaries at the full
// adversarial fixture size.
func raceInstrumented() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}

	for _, setting := range info.Settings {
		if setting.Key == "-race" {
			return setting.Value == "true"
		}
	}

	return false
}

// TestRequestedSamplingIsBlockedBeforeSchemaIntegration keeps an M9
// version-10 database predictable before its explicit 0011 maintenance step. A
// caller can request exact work, but a sample must fail with the actionable
// recovery code before SQL references sampling membership tables.
func TestRequestedSamplingIsBlockedBeforeSchemaIntegration(t *testing.T) {
	account := newAccountThrough(t, 10)
	engine := New(account.Reader())
	engine.Now = func() time.Time { return fixtureNow }

	q := baseQuery("pageviews")
	q.SampleRate = 0.5
	_, err := engine.Run(context.Background(), q)
	var queryError *Error
	if !errors.As(err, &queryError) || queryError.Code != "sampling_requires_exact" {
		t.Fatalf("pre-integration requested sample error = %v", err)
	}

	exact := baseQuery("pageviews")
	exact.Normalise()
	resolved, err := exact.DateRange.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	titleSQL, titleArgs, err := pageTitleEnrichmentQuery([]int64{1}, []int64{1}, resolved, &exact, -1)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := account.Reader().QueryContext(context.Background(), titleSQL, titleArgs...)
	if err != nil {
		t.Fatalf("pre-integration exact title lookup required migration 0011: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestMaterializedSamplingDefeatsIDPatternsAndPeriodicDeletes reproduces the
// low-bit failure with explicit sparse signed import ids. Old `id & 1023`
// selection expands an exact 1,024 rows to 1,048,576; the site/day ordinal
// strata select exactly one. A second site and periodic deletes remain within
// one bucket's error without changing query-time indexability.
func TestMaterializedSamplingDefeatsIDPatternsAndPeriodicDeletes(t *testing.T) {
	_, account := newUnseededSamplingEngine(t)
	ctx := context.Background()

	if _, err := account.Writer().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1024)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, is_imported)
		SELECT CASE WHEN (n & 1) = 0 THEN n * 1048576 ELSE n * -1048576 END,
		       1, ?, 0, n, n, 1
		FROM seq`, at(29, 0, 0)); err != nil {
		t.Fatal(err)
	}

	var exact, oldSelected int64
	if err := account.Reader().QueryRowContext(ctx,
		"SELECT COUNT(*), COUNT(*) FILTER (WHERE (id & 1023) = 0) FROM events").Scan(&exact, &oldSelected); err != nil {
		t.Fatal(err)
	}
	if exact != 1024 || oldSelected*SampleBuckets != 1_048_576 {
		t.Fatalf("old low-bit fixture exact/estimate = %d/%d, want 1024/1048576", exact, oldSelected*SampleBuckets)
	}

	selectedRows := func(sites []int64) int64 {
		t.Helper()
		condition := sampleCondition(tableEvents, "e", sites, at(28, 0, 0), at(30, 0, 0), MinSampleRate, false)
		var selected int64
		if err := account.Reader().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM events e WHERE "+condition.SQL, condition.Args...).Scan(&selected); err != nil {
			t.Fatal(err)
		}
		return selected
	}

	if selected := selectedRows([]int64{1}); selected*SampleBuckets != exact {
		t.Fatalf("materialized signed-id estimate = %d for exact %d", selected*SampleBuckets, exact)
	}

	if _, err := account.Writer().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1024)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, is_imported)
		SELECT CASE WHEN (n & 1) = 0 THEN (n + 2000) * 1048576 ELSE (n + 2000) * -1048576 END,
		       2, ?, 0, n, n, 1
		FROM seq`, at(29, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(ctx,
		"DELETE FROM events WHERE (abs(id) / 1048576) % 257 = 0"); err != nil {
		t.Fatal(err)
	}
	if err := account.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&exact); err != nil {
		t.Fatal(err)
	}
	selected := selectedRows([]int64{1, 2})
	errorRows := selected*SampleBuckets - exact
	if errorRows < 0 {
		errorRows = -errorRows
	}
	if errorRows > SampleBuckets {
		t.Fatalf("multi-site deleted estimate/exact/error = %d/%d/%d", selected*SampleBuckets, exact, errorRows)
	}
	t.Logf("id pathology: old estimate 1048576 for 1024; materialized estimate %d for %d after imports/deletes", selected*SampleBuckets, exact)
}

// TestSkewedNumericStatisticsAreDirectAndDisclosed keeps extrema and
// percentiles from being inverse-scaled. The only sampled row has value one;
// an extreme billion-value row in another bucket changes the exact population
// but must never turn the sampled max into 1024.
func TestSkewedNumericStatisticsAreDirectAndDisclosed(t *testing.T) {
	engine, account := newUnseededSamplingEngine(t)
	ctx := context.Background()

	if _, err := account.Writer().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1024)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, has_details)
		SELECT n, 1, ?, 0, n, n, 1 FROM seq;
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1024)
		INSERT INTO event_details (event_id, props)
		SELECT n, json_object('value', CASE WHEN n = 1023 THEN 1000000000 ELSE 1 END) FROM seq;`, at(29, 0, 0)); err != nil {
		t.Fatal(err)
	}
	writeRollupTotalsWithVisits(t, account, 1, 1024, 0)

	q := baseQuery(
		"sum(event:props:value)",
		"avg(event:props:value)",
		"min(event:props:value)",
		"max(event:props:value)",
		"p95(event:props:value)",
	)
	q.Include.Bots = true
	q.Include.Imports = true
	q.SampleRate = MinSampleRate

	result := run(t, engine, q)
	metrics := result.Results[0].Metrics
	if metrics[0] != 1024 {
		t.Fatalf("sampled additive sum = %g, want 1024", metrics[0])
	}
	for i, name := range []string{"avg", "min", "max", "p95"} {
		if metrics[i+1] != 1 {
			t.Fatalf("sampled %s = %g, want direct value 1", name, metrics[i+1])
		}
	}
	if !result.Meta.Sampling.Sparse || result.Meta.Sampling.Uncertainty == "" {
		t.Fatalf("skewed sparse sample disclosure = %+v", result.Meta.Sampling)
	}
	coverage := result.Meta.Sampling.PropertyCoverage["avg(event:props:value)"]
	if coverage.ObservedValues != 1 || coverage.ObservedNumericValues != 1 ||
		coverage.EstimatedValues != 1024 || coverage.EstimatedNumericValues != 1024 {
		t.Fatalf("sampled property coverage = %+v, want one observed and 1024 estimated", coverage)
	}
	if strings.Join(result.Meta.Sampling.DirectMetrics, ",") !=
		"avg(event:props:value),min(event:props:value),max(event:props:value),p95(event:props:value)" {
		t.Fatalf("direct metrics = %v", result.Meta.Sampling.DirectMetrics)
	}
}

// TestAnEmptySelectedBucketIsDisclosed distinguishes a zero sampled response
// from proof that the full population is zero.
func TestAnEmptySelectedBucketIsDisclosed(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.SampleRate = MinSampleRate
	result := run(t, engine, q)
	if !result.Meta.Sampling.ZeroResult {
		t.Fatalf("empty selected bucket disclosure = %+v", result.Meta.Sampling)
	}
}

// TestSampledZeroDisclosureIncludesComparison prevents an empty current period
// from hiding nonzero values returned for the comparison period.
func TestSampledZeroDisclosureIncludesComparison(t *testing.T) {
	rows := []Row{{
		Metrics: []float64{0},
		Comparison: &ComparisonRow{
			Metrics: []float64{1},
		},
	}}
	if sampledResultIsZero(rows) {
		t.Fatal("a nonzero sampled comparison was disclosed as an all-zero sample")
	}
}

// newUnseededSamplingEngine opens a migrated account without the shared query
// fixture, so adversarial ids and row counts are exact and reproducible.
func newUnseededSamplingEngine(t *testing.T) (*Engine, *accounts.Account) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.CloseAll() })

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	engine := New(account.Reader())
	engine.Now = func() time.Time { return fixtureNow }

	return engine, account
}

// TestLadderRateSnapsDown pins the rate chooser. Down rather than to the
// nearest, so the sampled scan lands under the threshold rather than a little
// over the ceiling it was chosen to stay beneath.
func TestLadderRateSnapsDown(t *testing.T) {
	cases := map[float64]float64{
		2.0:      1,
		1.0:      1,
		0.9:      0.5,
		0.5:      0.5,
		0.3:      204.0 / SampleBuckets,
		0.11:     102.0 / SampleBuckets,
		0.001:    1.0 / SampleBuckets,
		0.000001: MinSampleRate,
	}

	for target, want := range cases {
		if got := ladderRate(target); got != want {
			t.Errorf("ladderRate(%g) = %g, want %g", target, got, want)
		}
	}
}

// TestAnUnsampledAnswerSaysNothingAboutSampling is the other half of the
// contract: meta.sampling is absent exactly when the numbers are exact, so a
// client can branch on its presence alone.
func TestAnUnsampledAnswerSaysNothingAboutSampling(t *testing.T) {
	engine := newEngine(t)

	result := run(t, engine, baseQuery("visitors"))

	if result.Meta.Sampling != nil {
		t.Fatalf("an exact answer reported sampling: %+v", result.Meta.Sampling)
	}

	if result.Meta.SampleRate != 1 {
		t.Fatalf("sample rate = %v, want 1", result.Meta.SampleRate)
	}

	if _, ok := result.Meta.MetricWarnings["visitors"]; ok {
		t.Fatal("an exact answer carries a sampling warning")
	}
}

// TestARequestedSampleIsLabelledAsRequested covers the rate a caller named. It
// is still labelled — a number read from part of the fact rows is an estimate
// however it came to be one — but it is not attributed to us.
func TestARequestedSampleIsLabelledAsRequested(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.SampleRate = 0.5

	result := run(t, engine, q)

	if result.Meta.Sampling == nil {
		t.Fatal("a sampled answer reported no sampling")
	}

	if result.Meta.Sampling.Reason != SampledOnRequest {
		t.Fatalf("reason = %q, want %q", result.Meta.Sampling.Reason, SampledOnRequest)
	}

	if result.Meta.Sampling.Rate != 0.5 || result.Meta.SampleRate != 0.5 {
		t.Fatalf("rate = %v / %v, want 0.5", result.Meta.Sampling.Rate, result.Meta.SampleRate)
	}

	if warning := result.Meta.MetricWarnings["pageviews"]; warning.Code != WarnSampled {
		t.Fatalf("warning code = %q, want %q", warning.Code, WarnSampled)
	}
}

// TestABigQueryIsSampledAndSaysWhy is the whole feature.
//
// The site's daily rate says this range holds far more events than the
// threshold allows, and nothing of it is summarised, so the engine picks a
// rate, labels every metric, and names both the estimate and the ceiling so a
// caller can see how far over the line the question was.
func TestABigQueryIsSampledAndSaysWhy(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)

	engine.SampleThreshold = 10_000

	q := baseQuery("pageviews")

	result := run(t, engine, q)

	sampling := result.Meta.Sampling
	if sampling == nil {
		t.Fatal("a query far over the threshold was answered exactly")
	}

	if sampling.Reason != SampledAutomatically {
		t.Fatalf("reason = %q, want %q", sampling.Reason, SampledAutomatically)
	}

	if sampling.Threshold != 10_000 {
		t.Fatalf("threshold = %d, want 10000", sampling.Threshold)
	}

	if sampling.EstimatedRows < 1_000_000 {
		t.Fatalf("estimated rows = %d, want at least a million", sampling.EstimatedRows)
	}

	if sampling.Rate >= 1 || sampling.Rate != result.Meta.SampleRate {
		t.Fatalf("rate = %v, meta rate = %v", sampling.Rate, result.Meta.SampleRate)
	}

	// Every metric, not one, and every warning names the escape hatch.
	for _, name := range q.Metrics {
		warning, ok := result.Meta.MetricWarnings[name]
		if !ok || warning.Code != WarnSampled {
			t.Fatalf("%q carries warning %+v, want a sampled one", name, warning)
		}

		if !strings.Contains(warning.Warning, "exact") {
			t.Fatalf("the warning does not name the escape hatch: %q", warning.Warning)
		}
	}
}

// TestExactRefusesAutomaticSampling is the escape hatch. The same query that
// was sampled above is answered exactly when the caller says so, and the
// response carries nothing that would make the figure look like an estimate.
func TestExactRefusesAutomaticSampling(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)

	engine.SampleThreshold = 1_000_000

	q := baseQuery("visitors")
	q.Exact = true

	result := run(t, engine, q)

	if result.Meta.Sampling != nil {
		t.Fatalf("exact was ignored: %+v", result.Meta.Sampling)
	}

	if result.Meta.SampleRate != 1 {
		t.Fatalf("sample rate = %v, want 1", result.Meta.SampleRate)
	}

	if !result.Query.Exact {
		t.Fatal("the echoed query does not show that exactness was asked for")
	}
}

// TestExactOverridesARequestedSample pins the precedence of the two controls.
// Clients often build requests by layering an exact toggle over saved query
// settings, so leaving a stale sample_rate in that request must not make the
// supposedly exact SQL keep its sampling predicate.
func TestExactOverridesARequestedSample(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.SampleRate = 0.5
	q.Exact = true

	result := run(t, engine, q)

	if result.Meta.Sampling != nil || result.Meta.SampleRate != 1 {
		t.Fatalf("exact request ran at rate %v with sampling %+v", result.Meta.SampleRate, result.Meta.Sampling)
	}

	if result.Results[0].Metrics[0] != 7 {
		t.Fatalf("pageviews = %v, want all 7", result.Results[0].Metrics[0])
	}
}

// TestASubDayRangeUsesACurrentDayUpperBound proves a million-row current spike
// cannot be averaged down or prorated away. A thirty-minute report charges the
// full trigger-maintained boundary-day population as its conservative bound.
func TestASubDayRangeUsesACurrentDayUpperBound(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)

	engine.SampleThreshold = 100_000

	q := baseQuery("pageviews")
	q.DateRange = DateRange{Preset: RangeRealtime}

	result := run(t, engine, q)

	if result.Meta.Sampling == nil || result.Meta.Sampling.EstimatedEventRows < 1_000_000 {
		t.Fatalf("current-day spike was hidden by range averaging: %+v", result.Meta.Sampling)
	}
}

// TestASummarisedQueryIsNeverSampled is the case sampling would make worse
// rather than better. The additive summary rows plus one current raw day fit
// the budget, so sampling must not push the full range back onto raw facts.
func TestASummarisedQueryIsNeverSampled(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)
	writeRollupWindow(t, account)

	engine.SampleThreshold = 1_000_000

	result := run(t, engine, baseQuery("pageviews"))

	if result.Meta.Sampling != nil {
		t.Fatalf("a summarisable query was sampled: %+v", result.Meta.Sampling)
	}
}

// TestSummarySeamCorrectionsCountTowardTheBudget covers raw work beyond the
// obvious current-day pass. Distinct visitors need a second statement over the
// preceding and current days; that independently crosses this ceiling and then
// receives the explicit complete-membership refusal required by row sampling.
func TestSummarySeamCorrectionsCountTowardTheBudget(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)
	writeRollupWindow(t, account)
	engine.SampleThreshold = 1_500_000

	_, err := engine.Run(context.Background(), baseQuery("visitors", "pageviews"))
	if err == nil || !strings.Contains(err.Error(), "complete visitor") {
		t.Fatalf("error = %v, want visitor-membership refusal after seam work crossed the budget", err)
	}
}

// TestAFilteredQueryIsSampledEvenWhenTheRangeIsSummarised is the shape
// sampling exists for. A filter narrows rows the summary has already collapsed,
// so there is nothing left to filter and the whole range goes back to raw
// events however completely it was summarised.
func TestAFilteredQueryIsSampledEvenWhenTheRangeIsSummarised(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)
	writeRollupWindow(t, account)

	engine.SampleThreshold = 10_000

	q := baseQuery("pageviews")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "visit:country", Values: []string{"US"}}}

	if result := run(t, engine, q); result.Meta.Sampling == nil {
		t.Fatal("a filtered query over a summarised range was answered exactly")
	}
}

// TestSamplingUsesFactCountsWithoutRollups proves automatic protection no
// longer depends on a historical summary average. Fresh raw rows alone carry
// a trigger-maintained upper bound and cross a one-row budget.
func TestSamplingUsesFactCountsWithoutRollups(t *testing.T) {
	engine := newEngine(t)
	engine.SampleThreshold = 1

	if result := run(t, engine, baseQuery("pageviews")); result.Meta.Sampling == nil {
		t.Fatal("raw facts without rollups were not sampled")
	}
}

// TestANegativeThresholdTurnsSamplingOff covers the operator who would rather
// wait than estimate.
func TestANegativeThresholdTurnsSamplingOff(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)

	engine.SampleThreshold = -1

	if result := run(t, engine, baseQuery("visitors")); result.Meta.Sampling != nil {
		t.Fatalf("sampling ran with the threshold off: %+v", result.Meta.Sampling)
	}
}

// TestSessionQueriesAreEstimatedAtSessionGrain checks that a visit-only report
// is sized by sessions rather than by the much larger events count stored in
// the same summary row.
func TestSessionQueriesAreEstimatedAtSessionGrain(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotalsWithVisits(t, account, 1, 1_000_000, 10)
	engine.SampleThreshold = 100

	q := baseQuery("bounce_rate")
	q.Include.Bots = true
	result := run(t, engine, q)
	if result.Meta.Sampling != nil {
		t.Fatalf("ten daily visits were estimated from event rows: %+v", result.Meta.Sampling)
	}
}

// TestMixedQueriesEstimateBothFactTables checks that a plan with event and
// session passes includes both sets of rows in its work estimate.
func TestMixedQueriesEstimateBothFactTables(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotalsWithVisits(t, account, 1, 100, 20)
	engine.SampleThreshold = 800

	result := run(t, engine, baseQuery("pageviews", "bounce_rate"))
	if result.Meta.Sampling == nil {
		t.Fatal("the sessions pass was omitted from the mixed-plan estimate")
	}

	if result.Meta.Sampling.EstimatedRows != 840 {
		t.Fatalf("estimated rows = %d, want 840 after precomputed sampled bot facts", result.Meta.Sampling.EstimatedRows)
	}
}

// TestNumericMetricsCostTheirAggregateAndCoveragePasses checks the cold-value
// aggregate and the independent data-quality count. Omitting either one would
// halve the estimated work of the most expensive extensible metric family.
func TestNumericMetricsCostTheirAggregateAndCoveragePasses(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 100)
	engine.SampleThreshold = 1000

	result := run(t, engine, baseQuery("avg(event:props:price)"))
	if result.Meta.Sampling == nil {
		t.Fatal("numeric aggregate and coverage scans did not trigger sampling")
	}
	if result.Meta.Sampling.EstimatedEventRows != 2100 {
		t.Fatalf("event work = %d, want 2100 for three seven-day passes", result.Meta.Sampling.EstimatedEventRows)
	}
}

// TestEveryRepeatedMetricPassIsCosted pins the scan multiplier for the real
// executor paths: primary, coverage, composite, shared revenue, numeric
// property, total-row and nested session bot work. Comparison omits only the
// current-period-only coverage and pagination count statements.
func TestEveryRepeatedMetricPassIsCosted(t *testing.T) {
	q := baseQuery(
		"time_on_page",
		"scroll_depth",
		"exit_rate",
		"conversion_rate",
		"total_revenue",
		"revenue_per_visitor",
		"avg(event:props:price)",
	)
	q.Include.TotalRows = true
	q.Filters = []Filter{{Operator: OpIs, Dimension: "event:name", Values: []string{"Signup"}}}

	blueprint, err := decide(&q)
	if err != nil {
		t.Fatal(err)
	}

	if got := plannedScanPasses(&q, blueprint, false); got != (scanPasses{Events: 17, Sessions: 1}) {
		t.Fatalf("primary passes = %+v, want 17 event and 1 session with materialized bot facts", got)
	}
	if got := plannedScanPasses(&q, blueprint, true); got != (scanPasses{Events: 13, Sessions: 1}) {
		t.Fatalf("comparison passes = %+v, want 13 event and 1 session with materialized bot facts", got)
	}
}

// TestMaximumMetricFanoutStillFitsTheBudget proves the request's maximum
// metric count cannot multiply special and coverage passes beyond the selected
// rate's ceiling.
func TestMaximumMetricFanoutStillFitsTheBudget(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 100)
	engine.SampleThreshold = 100

	metrics := make([]string, MaxMetrics)
	for i := range metrics {
		metrics[i] = "avg(event:props:value" + string(rune('a'+i)) + ")"
	}

	result := run(t, engine, baseQuery(metrics...))
	sampling := result.Meta.Sampling
	if sampling == nil {
		t.Fatal("maximum metric fanout did not trigger sampling")
	}
	if float64(sampling.EstimatedRows)*sampling.Rate > float64(sampling.Threshold) {
		t.Fatalf("sampled work %g exceeds threshold %d", float64(sampling.EstimatedRows)*sampling.Rate, sampling.Threshold)
	}
}

// TestLongCustomComparisonChoosesOneSafeRate checks the period that used to be
// invisible to automatic sampling. The one-day primary fits exactly, while a
// thirty-day comparison independently crosses the ceiling; both are answered
// with the single rate disclosed in metadata.
func TestLongCustomComparisonChoosesOneSafeRate(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 100)
	for day := 1; day <= 30; day++ {
		bucket := time.Date(2026, time.July, day, 0, 0, 0, 0, time.UTC).Unix()
		if _, err := account.Writer().ExecContext(context.Background(), `
			INSERT INTO sampling_daily_counts (site_id, day, event_rows, session_rows)
			VALUES (1, ?, 100, 0)
			ON CONFLICT(site_id, day) DO UPDATE SET event_rows = 100`, bucket); err != nil {
			t.Fatal(err)
		}
	}
	engine.SampleThreshold = 1000

	q := baseQuery("pageviews")
	q.DateRange = DateRange{Preset: RangeDay}
	q.Include.Comparisons = &Comparison{
		Mode: CompareCustom,
		DateRange: DateRange{
			Preset:   RangeCustom,
			Start:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			End:      time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
			DateOnly: true,
		},
	}

	result := run(t, engine, q)
	sampling := result.Meta.Sampling
	if sampling == nil || sampling.Reason != SampledAutomatically {
		t.Fatalf("long comparison did not trigger automatic sampling: %+v", sampling)
	}
	if sampling.EstimatedComparisonRows <= sampling.EstimatedPrimaryRows {
		t.Fatalf("comparison work %d does not exceed primary work %d",
			sampling.EstimatedComparisonRows, sampling.EstimatedPrimaryRows)
	}
	if result.Query.SampleRate != sampling.Rate || result.Meta.SampleRate != sampling.Rate {
		t.Fatalf("incoherent rates: query=%g meta=%g sampling=%g",
			result.Query.SampleRate, result.Meta.SampleRate, sampling.Rate)
	}
}

// TestImpossibleAutomaticSampleIsRefused pins the hard ceiling. A bounded
// sample cannot promise a budget smaller than its single indexed row
// bucket, so the engine must refuse rather than execute an unexpectedly large
// query or disclose a rate that does not fit.
func TestImpossibleAutomaticSampleIsRefused(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)
	engine.SampleThreshold = 1000

	_, err := engine.Run(context.Background(), baseQuery("pageviews"))
	if err == nil || !strings.Contains(err.Error(), "minimum sample rate") {
		t.Fatalf("error = %v, want minimum-rate budget refusal", err)
	}
}

// TestSamplingDisclosureSeparatesScaledAndDirectMetrics prevents a sampled
// average, range or percentile from being presented as unchanged. Only
// additive totals are inverse-rate expanded; all direct sample statistics are
// still estimates of the full population.
func TestSamplingDisclosureSeparatesScaledAndDirectMetrics(t *testing.T) {
	engine, account := newEngineWithAccount(t)
	writeCheckouts(t, account)

	q := baseQuery("pageviews", "avg(event:props:price)", "min(event:props:price)", "p95(event:props:price)")
	q.SampleRate = 0.5

	result := run(t, engine, q)
	sampling := result.Meta.Sampling
	if strings.Join(sampling.ScaledMetrics, ",") != "pageviews" {
		t.Fatalf("scaled metrics = %v, want pageviews", sampling.ScaledMetrics)
	}
	if strings.Join(sampling.EventMetrics, ",") != strings.Join(q.Metrics, ",") {
		t.Fatalf("event-grain metrics = %v, want %v", sampling.EventMetrics, q.Metrics)
	}
	if strings.Join(sampling.DirectMetrics, ",") != "avg(event:props:price),min(event:props:price),p95(event:props:price)" {
		t.Fatalf("direct metrics = %v", sampling.DirectMetrics)
	}
	if warning := result.Meta.MetricWarnings["pageviews"].Warning; !strings.Contains(warning, "expanded by the inverse sample rate") {
		t.Fatalf("scaled warning = %q", warning)
	}
	if warning := result.Meta.MetricWarnings["avg(event:props:price)"].Warning; !strings.Contains(warning, "not scaled") || !strings.Contains(warning, "may differ materially") {
		t.Fatalf("direct warning = %q", warning)
	}
}

// TestSampledFactQueriesUseTheBucketIndexes asks SQLite's planner rather than
// inferring from SQL text. Both facts must constrain site, expression bucket
// and time through migration 0011; otherwise sampling still walks the exact
// range and discards rows afterward.
func TestSampledFactQueriesUseTheBucketIndexes(t *testing.T) {
	_, account := newEngineWithAccount(t)

	q := baseQuery("pageviews")
	q.Include.Bots = true
	q.Include.Imports = true
	q.SampleRate = 1.0 / 16
	range_ := Resolved{Start: time.Unix(at(24, 0, 0), 0), End: time.Unix(at(31, 0, 0), 0)}

	for _, fact := range []table{tableEvents, tableSessions} {
		builder := newWhereBuilder(fact, compileContext{sampleRate: q.SampleRate}, nil, q.SiteIDs, range_)
		where := and(builder.base(&q))
		plan := explainPlan(t, account, "SELECT SUM("+fact.alias()+".id) FROM "+fact.name()+" "+fact.alias()+" NOT INDEXED WHERE "+where.SQL, where.Args)
		t.Logf("%s sampled plan: %s", fact.name(), strings.ReplaceAll(plan, "\n", " | "))
		want := "INDEX event_sampling_seek"
		if fact == tableSessions {
			want = "INDEX session_sampling_seek"
		}
		if !strings.Contains(plan, want) {
			t.Fatalf("%s plan does not use indexed buckets:\n%s", fact.name(), plan)
		}
		if !strings.Contains(plan, "bucket=?") || !strings.Contains(plan, fact.timeColumn()+">?") {
			t.Fatalf("%s plan does not constrain bucket and time:\n%s", fact.name(), plan)
		}
		if !strings.Contains(plan, "INTEGER PRIMARY KEY (rowid=?)") {
			t.Fatalf("%s sample did not drive primary-key fact fetches:\n%s", fact.name(), plan)
		}
	}
}

// TestSampledVisitSelectorsKeepTheirMeaningAndTheirBound refuses has_done and
// answers declared session properties from sessions.entry_props. The first
// fundamentally needs complete event membership; the second has a bounded
// precomputed visit representation and must not open events at all.
func TestSampledVisitSelectorsKeepTheirMeaningAndTheirBound(t *testing.T) {
	engine, account := newEngineWithAccount(t)
	declareProperty(t, account, "plan", propScopeSession)

	hasDone := baseQuery("pageviews")
	hasDone.Filters = []Filter{{
		Operator: OpHasDone,
		Child:    &Filter{Operator: OpIs, Dimension: "event:name", Values: []string{"Signup"}},
	}}
	hasDone.SampleRate = 0.5
	if _, err := engine.Run(context.Background(), hasDone); err == nil || !strings.Contains(err.Error(), "complete event membership") {
		t.Fatalf("sampled has_done error = %v", err)
	}

	var selectedSession int64
	if err := account.Reader().QueryRowContext(context.Background(), `
		SELECT session_id FROM session_sampling
		WHERE site_id = 1 AND bucket < 512
		ORDER BY session_id LIMIT 1`).Scan(&selectedSession); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(context.Background(),
		`UPDATE sessions SET entry_props = '{"plan":"pro"}' WHERE id = ?`, selectedSession); err != nil {
		t.Fatal(err)
	}

	q := baseQuery("visits")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "event:props:plan", Values: []string{"pro"}}}
	q.Include.Bots = true
	q.Include.Imports = true
	q.SampleRate = 0.5

	result := run(t, engine, q)
	closeTo(t, "session property visits", result.Results[0].Metrics[0], 2)

	builder := newWhereBuilder(tableSessions, compileContext{sampleRate: q.SampleRate},
		map[string]string{"plan": propScopeSession}, q.SiteIDs,
		Resolved{Start: time.Unix(at(24, 0, 0), 0), End: time.Unix(at(31, 0, 0), 0)})
	conditions := builder.base(&q)
	filtered, err := builder.compile(q.Filters)
	if err != nil {
		t.Fatal(err)
	}
	where := and(append(conditions, filtered...))
	plan := explainPlan(t, account, "SELECT SUM(s.id) FROM sessions s WHERE "+where.SQL, where.Args)
	t.Logf("session-property sampled plan: %s", strings.ReplaceAll(plan, "\n", " | "))
	if !strings.Contains(plan, "INDEX session_sampling_seek") || strings.Contains(plan, "events") {
		t.Fatalf("session-property sample did not stay on bounded session rows:\n%s", plan)
	}
}

// explainPlan returns SQLite's readable plan for one query.
func explainPlan(t *testing.T, account *accounts.Account, query string, args []any) string {
	t.Helper()

	rows, err := account.Reader().QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return strings.Join(details, "\n")
}

// BenchmarkIndexedRowSampling compares exact and sampled row-grain paths
// over a fixture large enough that wall time and selected index entries are
// both visible. The aggregate touches every selected id, preventing COUNT's
// metadata optimizations from hiding the scan.
func BenchmarkIndexedRowSampling(b *testing.B) {
	account := newAccountThrough(b, 10)

	const fixtureRows = 524_288
	if _, err := account.Writer().ExecContext(context.Background(), `
		WITH RECURSIVE seq(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?
		)
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at)
		SELECT n, 1, n, ?, ? FROM seq`, fixtureRows, at(29, 0, 0), at(29, 0, 0)); err != nil {
		b.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(context.Background(), `
		WITH RECURSIVE seq(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?
		)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id)
		SELECT n, 1, ?, 0, 7, 7 FROM seq`, fixtureRows, at(29, 0, 0)); err != nil {
		b.Fatal(err)
	}
	installSamplingSchema(b, account.Writer())

	for _, fact := range []table{tableEvents, tableSessions} {
		for _, sample := range []struct {
			name string
			rate float64
			rows int64
		}{
			{name: "exact", rate: 1, rows: fixtureRows},
			{name: "sample_1_of_1024", rate: 1.0 / 1024, rows: fixtureRows / 1024},
		} {
			b.Run(fact.name()+"/"+sample.name, func(b *testing.B) {
				condition := sampleCondition(fact, fact.alias(), []int64{1}, at(28, 0, 0), at(30, 0, 0), sample.rate, false)
				where := fact.alias() + ".site_id = ? AND " + fact.alias() + "." + fact.timeColumn() + " >= ? AND " + fact.alias() + "." + fact.timeColumn() + " < ?"
				args := []any{1, at(28, 0, 0), at(30, 0, 0)}
				if condition.SQL != "" {
					where += " AND " + condition.SQL
					args = append(args, condition.Args...)
				}
				statement := "SELECT SUM(" + fact.alias() + ".id), COUNT(*) FROM " + fact.name() + " " + fact.alias() + " WHERE " + where
				if sample.rate < 1 {
					statement = strings.Replace(statement, " WHERE ", " NOT INDEXED WHERE ", 1)
				}

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var sum, count int64
					if err := account.Reader().QueryRowContext(context.Background(), statement, args...).Scan(&sum, &count); err != nil {
						b.Fatal(err)
					}
					if count != sample.rows || sum <= 0 {
						b.Fatalf("selected %d rows with sum %d, want %d rows", count, sum, sample.rows)
					}
				}
				b.ReportMetric(float64(sample.rows), "selected_rows/op")
			})
		}
	}
}

// TestMultiSiteEstimateAddsEachSitesDailyRate checks that averaging across all
// site-day rows does not divide a two-site report by two. Each site contributes
// seven hundred rows, so the combined estimate crosses the one-thousand limit.
func TestMultiSiteEstimateAddsEachSitesDailyRate(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotalsWithVisits(t, account, 1, 100, 10)
	writeRollupTotalsWithVisits(t, account, 2, 100, 10)
	engine.SampleThreshold = 1000

	q := baseQuery("pageviews")
	q.SiteIDs = []int64{1, 2}

	result := run(t, engine, q)
	if result.Meta.Sampling == nil {
		t.Fatal("the combined two-site scan was averaged down to one site's size")
	}

	if result.Meta.Sampling.EstimatedRows != 1400 {
		t.Fatalf("estimated rows = %d, want 1400", result.Meta.Sampling.EstimatedRows)
	}
}

// writeRollupWindow records that the summary has actually been built over the
// fixture's window, which is what makes the router read it. The totals rows
// alone only say how big the site is; this is the separate claim that they can
// be trusted to answer a report.
func writeRollupWindow(t *testing.T, account *accounts.Account) {
	t.Helper()

	if _, err := account.Writer().ExecContext(context.Background(), `
		INSERT INTO rollup_state (site_id, grain, timezone, covered_from, covered_through, built_at)
		VALUES (1, 0, 'UTC', ?, ?, 0)`, at(1, 0, 0), at(31, 0, 0)); err != nil {
		t.Fatal(err)
	}
}

// writeRollupTotals writes one whole-site summary row per day of the fixture's
// window, carrying the event count the estimator reads.
func writeRollupTotals(t *testing.T, account *accounts.Account, perDay int64) {
	t.Helper()
	writeRollupTotalsWithVisits(t, account, 1, perDay, 0)
}

// writeRollupTotalsWithVisits writes one site's event and visit rates for the
// estimator tests. Keeping both columns in the same fixture makes a test fail
// if the estimator reads the wrong grain rather than if the fixture is absent.
func writeRollupTotalsWithVisits(t *testing.T, account *accounts.Account, siteID, events, visits int64) {
	t.Helper()

	for day := 24; day <= 30; day++ {
		bucket := at(day, 0, 0)

		if _, err := account.Writer().ExecContext(context.Background(), `
				INSERT INTO rollup_visitors (site_id, grain, bucket, dimension, value_id, events, visits)
				VALUES (?, 0, ?, 0, 0, ?, ?)`, siteID, bucket, events, visits); err != nil {
			t.Fatal(err)
		}
		if _, err := account.Writer().ExecContext(context.Background(), `
				INSERT INTO sampling_daily_counts (site_id, day, event_rows, session_rows)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(site_id, day) DO UPDATE
				SET event_rows = excluded.event_rows, session_rows = excluded.session_rows`,
			siteID, bucket, events, visits); err != nil {
			t.Fatal(err)
		}
	}
}
