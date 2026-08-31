//
// rollups.go
// Building the pre-aggregated tables at the end of a seed run.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
)

// buildRollups rebuilds an account's pre-aggregated tables at the end of a run.
//
// It runs here rather than being left to the worker because the first dashboard
// load against a freshly seeded database must not be the thing that builds
// them. A seed that left the summaries empty would measure the raw-scan path
// and report it as the roll-up path, which is the wrong number by two orders of
// magnitude — and the whole reason to generate a million pageviews is to
// measure something.
//
// Both grains are built over the whole generated history, and both stop at the
// start of today in the site's own timezone. Today is the one thing a summary
// must never serve.
func buildRollups(ctx context.Context, run *accountRun, from, now time.Time) error {
	builder := rollup.New(run.account.Writer())
	builder.Now = func() time.Time { return now }

	for _, site := range run.sites {
		target := rollup.Site{
			ID:       site.seeded.ID,
			Domain:   site.domain,
			Timezone: site.seeded.Fixture.Timezone,
		}

		location := target.Location()
		today := query.RollupBucketStart(now.In(location), query.GrainDay, location)

		start := query.RollupBucketStart(from.In(location), query.GrainDay, location)

		for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
			oldest := start

			// Hourly buckets age out, so a seed that generated more history
			// than the retention window only builds the tail of it — the same
			// window the worker would keep.
			if grain == query.GrainHour {
				if limit := today.Add(-rollup.HourlyRetention); oldest.Before(limit) {
					oldest = query.RollupBucketStart(limit, grain, location)
				}
			}

			// The daily build runs one day past today so that today's row
			// exists; the covered window still stops at midnight.
			to := today
			if grain == query.GrainDay {
				to = today.AddDate(0, 0, 1)
			}

			if err := builder.Rebuild(ctx, rollup.Request{
				Site: target, Grain: grain, From: oldest, To: to, CoverThrough: today,
				FromBeginning: !oldest.After(start),
			}); err != nil {
				return fmt.Errorf("seed: build roll-ups for %s: %w", site.domain, err)
			}
		}
	}

	return nil
}
