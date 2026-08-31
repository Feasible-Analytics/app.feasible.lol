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
	"time"
)

// DefaultSampleThreshold is how many event rows a query may be estimated to
// read before it is answered from a sample instead.
//
// Ten million is where an exact answer stops being worth its cost: a scan of
// that many rows is several seconds of a dashboard sitting on a spinner, and
// the estimate that decides it is a floor rather than a ceiling — see
// estimateScan — so a query that crosses this line is reading rather more than
// ten million rows in practice.
const DefaultSampleThreshold = 10_000_000

// MinSampleRate is the finest sample the query can express.
//
// It is not a policy number. Sampling picks visitors with `user_id % 1000`, so
// a thousandth is the smallest fraction that predicate can name; anything
// below it rounds to zero and the query matches nothing at all. A range too
// big to answer at one visitor in a thousand is answered at one in a thousand
// and labelled, rather than being refused.
const MinSampleRate = 0.001

// sampleLadder is the set of rates automatic sampling may choose from, coarsest
// first.
//
// It is a ladder rather than the exact ratio the estimate implies because a
// dashboard is refreshed over and over, and a rate computed freshly each time
// would move a little with every new event — so the same figure would wobble
// between refreshes for no reason a reader could see. Snapping to a step means
// the rate only changes when the data has genuinely changed size.
var sampleLadder = []float64{1, 0.5, 0.2, 0.1, 0.05, 0.02, 0.01, 0.005, 0.002, MinSampleRate}

// Sampling reasons.
const (
	// SampledOnRequest is a rate the caller asked for.
	SampledOnRequest = "requested"

	// SampledAutomatically is a rate the engine chose because the query was
	// estimated to read more rows than the threshold allows.
	SampledAutomatically = "automatic"
)

// Sampling is the response's account of an answer that was read from part of
// the data.
//
// It is a struct of its own rather than one more number in meta because the
// rate alone does not tell a caller what to do about it. Knowing that the
// engine chose the rate, roughly how much data was behind the question, and
// which ceiling it crossed is what lets a client decide between showing the
// estimate and asking again for the exact answer — and it is what makes
// "exact" a real option rather than a flag nobody knows exists.
type Sampling struct {
	// Rate is the fraction of visitors read. It is the same number as
	// meta.sample_rate, repeated here so that a client which reads this object
	// never has to look anywhere else to render the caveat.
	Rate float64 `json:"rate"`

	// Reason is requested or automatic.
	Reason string `json:"reason"`

	// EstimatedRows is roughly how many event rows an exact answer would have
	// read, and Threshold is the ceiling it crossed. Both are omitted for a
	// rate the caller asked for, because no estimate was made.
	EstimatedRows int64 `json:"estimated_rows,omitempty"`
	Threshold     int64 `json:"threshold,omitempty"`
}

// sampleThreshold returns the configured ceiling, or the default.
func (e *Engine) sampleThreshold() int64 {
	if e.SampleThreshold == 0 {
		return DefaultSampleThreshold
	}

	return e.SampleThreshold
}

// decideSampling settles the rate one query runs at and returns what the
// response has to say about it.
//
// The order of the three branches is the order of precedence, and it is
// deliberate. A caller who asked for exactness gets it however big the range
// is; a caller who named a rate gets that rate; and only a caller who said
// nothing is sampled on their behalf. Nothing here ever samples a query that
// asked not to be.
func (e *Engine) decideSampling(ctx context.Context, q *Query, r Resolved) (*Sampling, error) {
	if q.SampleRate < 1 {
		return &Sampling{Rate: q.SampleRate, Reason: SampledOnRequest}, nil
	}

	if q.Exact {
		return nil, nil
	}

	threshold := e.sampleThreshold()
	if threshold < 0 {
		return nil, nil
	}

	// A range with any summarised part in it is left alone, and that is a
	// trade rather than a technicality. Sampling is all or nothing for a query:
	// a sampled query cannot read the summary at all, because the summary
	// counted every visitor rather than one in ten. So sampling a report that
	// was reading twenty-seven days out of the summary and one day raw would
	// push all twenty-eight days back onto the raw tables to save part of one —
	// slower and less exact at the same time.
	days := rawSpanDays(e.router().Route(q, r))
	if days == 0 {
		return nil, nil
	}

	estimate, known, err := e.estimateScan(ctx, q, days)
	if err != nil {
		return nil, err
	}

	if !known || estimate <= threshold {
		return nil, nil
	}

	rate := ladderRate(float64(threshold) / float64(estimate))
	if rate >= 1 {
		return nil, nil
	}

	q.SampleRate = rate

	return &Sampling{
		Rate:          rate,
		Reason:        SampledAutomatically,
		EstimatedRows: estimate,
		Threshold:     threshold,
	}, nil
}

// ladderRate snaps a target fraction down to a rate the ladder offers. Down
// rather than to the nearest, so the sampled scan lands under the threshold
// rather than a little over it.
func ladderRate(target float64) float64 {
	chosen := MinSampleRate

	for _, rate := range sampleLadder {
		if rate <= target {
			chosen = rate
			break
		}
	}

	return chosen
}

// rawSpanDays is how many days of the range are read out of the raw tables,
// rounded up — and zero as soon as any part of it is answered from the summary,
// which is the signal that this query is already on the fast path and must be
// left there.
//
// Days rather than seconds because that is the grain the estimate is built on,
// and rounding up because half a day of a busy site is still a scan.
func rawSpanDays(segments []Segment) int64 {
	var days int64

	for _, segment := range segments {
		if segment.Source != SourceRaw {
			return 0
		}

		span := segment.Range.End.Sub(segment.Range.Start)
		if span <= 0 {
			continue
		}

		days += int64((span + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
	}

	return days
}

// estimateScan is roughly how many event rows an exact answer would read.
//
// It multiplies the site's own daily event rate — taken from the summary
// tables, which already hold one row per day — by the number of days that have
// to be read raw. Counting the rows a query would scan by scanning them is
// exactly the cost sampling exists to avoid, so the estimate has to come from
// somewhere that is already aggregated.
//
// The daily rate is read over every bucket the site has rather than over this
// query's range, and that is deliberate: the ranges most worth sampling are the
// ones reaching further back than the summary covers, where a rate measured
// inside the range would have nothing to measure.
//
// Two things make the number a floor rather than an exact count, and a floor is
// the right direction to be wrong in — under-estimating means answering exactly
// when we could have sampled, which is merely slow, where over-estimating means
// estimating when we could have been exact, which is worse.
//
//   - The `events` column excludes engagement pings, and a scan reads those
//     too. A site whose visitors scroll sends one or two per pageview.
//   - A daily average flattens a launch, a campaign or a weekend.
//
// The boolean is false when the summary holds nothing at all, which is the
// honest answer for a site whose roll-ups have never been built: we do not know
// how big it is, so we do not sample it.
func (e *Engine) estimateScan(ctx context.Context, q *Query, days int64) (int64, bool, error) {
	sites := inInt64("site_id", q.SiteIDs)

	var (
		total   sql.NullInt64
		buckets sql.NullInt64
	)

	err := e.db.QueryRowContext(ctx,
		"SELECT SUM(events), COUNT(*) FROM rollup_visitors WHERE "+sites.SQL+" AND grain = ? AND dimension = ?",
		append(append([]any{}, sites.Args...), int64(GrainDay), int64(rollupCodeTotal))...,
	).Scan(&total, &buckets)
	if err != nil {
		return 0, false, fmt.Errorf("query: estimate scan: %w", err)
	}

	if !total.Valid || buckets.Int64 == 0 {
		return 0, false, nil
	}

	return total.Int64 / buckets.Int64 * days, true, nil
}

// sampleWarning is the sentence attached to every metric of a sampled answer.
//
// A sampled figure that reads like an exact one is worse than a slow exact one,
// because somebody will make a decision on it, so every metric carries the
// caveat rather than the response carrying it once. An automatic sample also
// says how to get the exact answer: an escape hatch nobody is told about is not
// an escape hatch.
func sampleWarning(sampling *Sampling) string {
	if sampling.Reason == SampledAutomatically {
		return fmt.Sprintf(
			"an exact answer would have read about %d events, so this was read from %g%% of visitors and scaled back up — "+
				"every figure here is an estimate. Ask again with exact set to true for the slow, exact answer",
			sampling.EstimatedRows, sampling.Rate*100)
	}

	return fmt.Sprintf("read from %g%% of visitors and scaled back up — totals are estimates", sampling.Rate*100)
}
