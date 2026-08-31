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
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// TestLadderRateSnapsDown pins the rate chooser. Down rather than to the
// nearest, so the sampled scan lands under the threshold rather than a little
// over the ceiling it was chosen to stay beneath.
func TestLadderRateSnapsDown(t *testing.T) {
	cases := map[float64]float64{
		2.0:      1,
		1.0:      1,
		0.9:      0.5,
		0.5:      0.5,
		0.3:      0.2,
		0.11:     0.1,
		0.001:    0.001,
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
// is still labelled — a number read from a tenth of the visitors is an estimate
// however it came to be one — but it is not attributed to us.
func TestARequestedSampleIsLabelledAsRequested(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("visitors")
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

	if warning := result.Meta.MetricWarnings["visitors"]; warning.Code != WarnSampled {
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

	engine.SampleThreshold = 1000

	q := baseQuery("visitors", "pageviews")

	result := run(t, engine, q)

	sampling := result.Meta.Sampling
	if sampling == nil {
		t.Fatal("a query far over the threshold was answered exactly")
	}

	if sampling.Reason != SampledAutomatically {
		t.Fatalf("reason = %q, want %q", sampling.Reason, SampledAutomatically)
	}

	if sampling.Threshold != 1000 {
		t.Fatalf("threshold = %d, want 1000", sampling.Threshold)
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

	engine.SampleThreshold = 1000

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

// TestASummarisedQueryIsNeverSampled is the case sampling would make worse
// rather than better. A range the roll-up tables have actually built reads a
// few dozen pre-aggregated rows however much traffic sits behind it, and
// sampling it would push it back onto the raw tables it was avoiding.
func TestASummarisedQueryIsNeverSampled(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeRollupTotals(t, account, 1_000_000)
	writeRollupWindow(t, account)

	engine.SampleThreshold = 1000

	result := run(t, engine, baseQuery("visitors"))

	if result.Meta.Sampling != nil {
		t.Fatalf("a summarisable query was sampled: %+v", result.Meta.Sampling)
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

	engine.SampleThreshold = 1000

	q := baseQuery("visitors")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "visit:country", Values: []string{"US"}}}

	if result := run(t, engine, q); result.Meta.Sampling == nil {
		t.Fatal("a filtered query over a summarised range was answered exactly")
	}
}

// TestSamplingIsRefusedWithoutAnEstimate covers the site whose roll-ups have
// never been built. We do not know how big it is, so we do not guess: it is
// answered exactly, which is slow rather than wrong.
func TestSamplingIsRefusedWithoutAnEstimate(t *testing.T) {
	engine := newEngine(t)
	engine.SampleThreshold = 1

	if result := run(t, engine, baseQuery("visitors")); result.Meta.Sampling != nil {
		t.Fatalf("a site with no summary was sampled: %+v", result.Meta.Sampling)
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

	for day := 24; day <= 30; day++ {
		bucket := at(day, 0, 0)

		if _, err := account.Writer().ExecContext(context.Background(), `
			INSERT INTO rollup_visitors (site_id, grain, bucket, dimension, value_id, events)
			VALUES (1, 0, ?, 0, 0, ?)`, bucket, perDay); err != nil {
			t.Fatal(err)
		}
	}
}
