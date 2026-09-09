//
// worker.go
// The hourly job that seals finished buckets and keeps today's row fresh.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// Interval is how often the worker runs. Hourly, because that is the finest
// grain it seals: running more often would rewrite the same buckets, and
// running less often would leave the last hour of an hourly graph missing for
// longer than anybody would tolerate.
const Interval = time.Hour

// reworkDays is how far back a run rewrites. Two days rather than one because
// an event can arrive late, a visit can be merged into an earlier one by the
// session fold, and either changes a bucket that was already sealed.
const reworkDays = 2

// SiteRef is one site the worker has to keep up to date, and the account whose
// database its rows live in.
type SiteRef struct {
	AccountID int64
	Site      Site
}

// Lister supplies the sites to build. It is a function rather than a table read
// so that the worker does not have to know whether the list came from
// system.db, from a command's flags, or from a test.
type Lister func(ctx context.Context) ([]SiteRef, error)

// Worker rebuilds every site's summary on a timer.
//
// It is deliberately small: it decides *which* buckets are due and hands them
// to the builder. There is no job queue in the product yet, so this runs as one
// background loop inside `feasible serve` and as the `feasible rollup` command
// — the same code either way, so a manual rebuild cannot behave differently
// from the automatic one.
type Worker struct {
	Accounts *accounts.Manager
	Sites    Lister
	Log      *logger.Logger

	// Now is the clock the worker decides what is finished against.
	Now func() time.Time

	// Every is how often Run rebuilds. Zero means Interval.
	Every time.Duration

	// Rest is how each build hands the write lock back between chunks. Nil is
	// the builder's own pacing, which is what a running server wants; a rebuild
	// on a machine with nothing else writing can pass rollup.NoRest.
	Rest func(ctx context.Context, d time.Duration) error
}

// now reads the worker's clock.
func (w *Worker) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}

	return w.Now().UTC()
}

// every is the configured period.
func (w *Worker) every() time.Duration {
	if w.Every <= 0 {
		return Interval
	}

	return w.Every
}

// Run rebuilds on a ticker until the context is cancelled.
//
// The first pass happens immediately rather than an hour in, because a process
// that has just started may have been down over a day boundary, and a dashboard
// that is slow for an hour after every deploy is a dashboard people learn to
// distrust.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.every())
	defer ticker.Stop()

	w.once(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.once(ctx)
		}
	}
}

// once runs a pass and logs whatever went wrong. A failed
// roll-up is not a failed request — the reports simply stay slow — so it never
// stops the loop.
func (w *Worker) once(ctx context.Context) {
	err := w.Once(ctx)

	if err != nil {
		if w.Log != nil {
			w.Log.Error("roll-up build failed", "error", err)
		}

		return
	}

}

// Once rebuilds every site that is due. It is exported so the command can run a
// single pass and exit, and so a test can drive one without a ticker.
func (w *Worker) Once(ctx context.Context) error {
	if w.Sites == nil || w.Accounts == nil {
		return fmt.Errorf("rollup: the worker needs an account manager and a site list")
	}

	sites, err := w.Sites(ctx)
	if err != nil {
		return err
	}

	var firstErr error

	for _, ref := range sites {
		// One site's failure must not stop the rest: a single unreadable
		// account database would otherwise leave every other customer's
		// dashboard on the raw path.
		if err := w.buildSite(ctx, ref); err != nil {
			if w.Log != nil {
				w.Log.Error("roll-up build failed for a site", "site", ref.Site.Domain, "error", err)
			}

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// buildSite brings one site up to date at both grains and prunes what has aged
// out.
//
// The two grains cover the same window and stop at the same place: the start of
// today in the site's own timezone. Today is the one thing a summary must never
// serve, because the day is still filling up and a report drawn from a partial
// bucket is simply wrong.
func (w *Worker) buildSite(ctx context.Context, ref SiteRef) error {
	// A scan, not real use. This walks every site on the box once an hour, so
	// promoting each one would leave the handle cache holding whichever
	// accounts the walk happened to visit last.
	lease, err := w.Accounts.AcquireForScan(ctx, ref.AccountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the roll-up result is more useful than an unlock error
	account := lease.Account

	builder := New(account.Writer())
	builder.Now = w.now
	builder.Sleep = w.Rest

	location := ref.Site.Location()
	now := w.now().In(location)
	today := query.RollupBucketStart(now, query.GrainDay, location)

	// Before the native grains, and before the early return below: a site that
	// is nothing but imported history has no events to summarise and would
	// otherwise never have its archive summarised at all.
	if err := w.summariseImports(ctx, account.Writer(), ref.Site, location); err != nil {
		return err
	}

	earliest, err := FirstEvent(ctx, account.Reader(), ref.Site.ID)
	if err != nil {
		return err
	}

	if earliest.IsZero() {
		return nil
	}

	// Day first, because the derived grains are summed out of its rows: daily
	// describes how far back they may reach, and a week built from days this
	// pass has not written yet would be short.
	var daily dailyWindow

	for _, grain := range query.RollupGrains() {
		// A derived grain may not be built before the daily pass has banked a
		// window for it to stand on. With no rows underneath it a week sums to
		// zero and banks coverage saying so.
		if grain.Derived() && !daily.found {
			continue
		}

		from, ok, err := w.windowStart(ctx, builder, ref.Site, grain, earliest, today, location)
		if err != nil {
			return err
		}

		if !ok {
			continue
		}

		// A derived grain is summed out of daily rows, so it may only start on a
		// bucket those rows can fill completely. Its own bucket start is earlier
		// — a month begins up to thirty days before the day a backfill bound
		// lands on — and building from there would sum a whole month out of the
		// fortnight of daily rows that exist, then bank coverage claiming all
		// of it.
		if grain.Derived() {
			from = daily.firstWholeBucket(laterOf(from, daily.oldest), grain, location)
		}

		// Daily buckets run one day past today so that today's row exists and
		// is fresh; sealing it at midnight is then one small rebuild rather
		// than a whole day's work. Hourly buckets stop at today, because the
		// only hours a report reads from the summary are the ones on days that
		// are already over.
		to := today
		if grain == query.GrainDay {
			to = today.AddDate(0, 0, 1)
		}

		// A derived bucket is only whole once every day under it is written, and
		// the daily pass runs one day past today. Building the bucket today
		// falls in would produce a partial row that a later pass has to correct;
		// leaving it out means a report reads today from the daily rows, which
		// is what it does for every range that does not end on a bucket edge
		// anyway.
		if grain.Derived() {
			to = query.RollupBucketStart(today, grain, location)
		}

		if err := builder.Rebuild(ctx, Request{
			Site: ref.Site, Grain: grain, From: from, To: to, CoverThrough: today,
			FromBeginning: !from.After(earliest),
		}); err != nil {
			return err
		}

		if grain == query.GrainDay {
			daily, err = w.dailyWindow(ctx, builder, ref.Site, earliest, location)
			if err != nil {
				return err
			}
		}
	}

	return builder.Prune(ctx, ref.Site)
}

// summariseImports builds the week and month rows for any of a site's imports
// that lacks them.
//
// An import summarises itself as it completes, so this is normally one read
// that finds nothing. It runs anyway because the two cases it catches are
// invisible from a dashboard: an archive that landed before the summaries
// existed, and one cut in a timezone the site has since changed. Either leaves
// every wide report adding up a day at a time for ever, with nothing to say so.
func (w *Worker) summariseImports(ctx context.Context, db *sql.DB, site Site, location *time.Location) error {
	built, err := dataio.SummariseSite(ctx, db, site.ID, location)
	if err != nil {
		return err
	}

	if built > 0 && w.Log != nil {
		w.Log.Info("summarised imported history", "site", site.Domain, "imports", built)
	}

	return nil
}

// dailyWindow is how far the daily rows reach, as a grain summed out of them
// needs to see it.
type dailyWindow struct {
	// found is false when the daily grain has banked no window at all, which
	// leaves nothing for a derived grain to be summed from.
	found bool

	// oldest is the first instant a daily row may exist for.
	oldest time.Time

	// whole says there is nothing before oldest: the daily build reached the
	// site's first event, so a wide bucket that starts before it is still
	// complete — the days it is missing are days that had no traffic.
	whole bool
}

// firstWholeBucket is the earliest bucket of a grain that the daily rows can
// fill completely, at or after an instant.
func (d dailyWindow) firstWholeBucket(at time.Time, grain query.Grain, location *time.Location) time.Time {
	if d.whole {
		return query.RollupBucketStart(at, grain, location)
	}

	return firstWholeBucket(at, grain, location)
}

// dailyWindow reads what the daily pass has banked.
//
// It is the banked window rather than where this pass started rewriting: once
// the daily grain has caught up with the site's history a pass rewrites only
// the last couple of days, and a derived grain held to that would never build
// anything at all.
func (w *Worker) dailyWindow(ctx context.Context, builder *Builder, site Site, earliest time.Time, location *time.Location) (dailyWindow, error) {
	coverage, found, err := builder.Coverage(ctx, site.ID, query.GrainDay)
	if err != nil {
		return dailyWindow{}, err
	}

	if !found || coverage.Timezone != site.Zone() {
		return dailyWindow{}, nil
	}

	// A covered_from of zero is how the daily build records that it reached the
	// site's first event and that everything before it is empty.
	if coverage.From == 0 {
		return dailyWindow{found: true, oldest: earliest.In(location), whole: true}, nil
	}

	return dailyWindow{found: true, oldest: localToInstant(coverage.From, location, query.GrainDay)}, nil
}

// windowStart works out how far back this run has to rebuild.
//
// A site with nothing built is backfilled from its first event, or from the
// start of the hourly retention window, whichever is later. A site that is
// already covered only rewrites the last couple of days, which is what makes
// the hourly run cheap.
func (w *Worker) windowStart(ctx context.Context, builder *Builder, site Site, grain query.Grain, earliest, today time.Time, location *time.Location) (time.Time, bool, error) {
	oldest := earliest.In(location)

	if grain == query.GrainHour {
		limit := today.Add(-HourlyRetention)
		if oldest.Before(limit) {
			oldest = limit
		}
	}

	coverage, found, err := builder.Coverage(ctx, site.ID, grain)
	if err != nil {
		return time.Time{}, false, err
	}

	if !found || coverage.Timezone != site.Zone() {
		// A grain with nothing built covers a bounded distance rather than the
		// whole history at once, and later passes reach further back until
		// coverage meets the first event.
		//
		// When a new grain ships, every site on the box has a missing coverage
		// row at the same moment. Building every history at once pins a core for
		// as long as the longest of them takes and starves the ingest handoff
		// while it does, which costs every site's events and not only the one
		// being built.
		return laterOf(oldest, backfillReach(today, grain, location)), true, nil
	}

	// The covered window has to stay contiguous, so a run starts no later than
	// where the last one stopped. Backing up a couple of days on top of that is
	// what picks up a late event or a visit the session fold merged backwards.
	start := query.RollupBucketStart(today.AddDate(0, 0, -reworkDays), grain, location)

	covered := coverage.Through
	if local := query.RollupLocalUnix(start, location); local > covered {
		start = localToInstant(covered, location, grain)
	}

	if start.Before(oldest) {
		start = oldest
	}

	// Coverage that has not reached the site's first event yet is extended one
	// bound further back on every pass. Without this the first pass's limit
	// would be permanent, and the history before it would never be built.
	//
	// The hourly grain is not backfilled this way. Its window is bounded by
	// retention rather than by the first event, and Prune moves covered_from
	// forward to the hour the fortnight begins — which is always later in the
	// day than the day this compares against, so it would rebuild the whole
	// retention window on every pass for ever.
	if grain != query.GrainHour {
		if covered := localToInstant(coverage.From, location, grain); covered.After(oldest) {
			reach := laterOf(oldest, backfillReach(covered, grain, location))
			if reach.Before(start) {
				start = reach
			}
		}
	}

	if !start.Before(today) && grain == query.GrainHour {
		return time.Time{}, false, nil
	}

	return start, true, nil
}

// backfillPerPass bounds how much history one pass adds to a grain that reads
// raw events and does not yet reach the site's first event. Coverage walks back
// by this much per tick, so the cost of a deploy is flat rather than
// proportional to the longest history on the box.
const backfillPerPass = 90 * 24 * time.Hour

// backfillReach is how far back a pass may extend a grain's coverage from an
// instant it already covers. The zero time means as far as there is history.
//
// A derived grain is not bounded. It sums the daily rows the same pass has
// already guaranteed rather than reading events again, and each of its chunks
// commits and hands the write lock back, so reaching the whole way at once
// costs a background job its time rather than costing ingest its throughput.
func backfillReach(from time.Time, grain query.Grain, location *time.Location) time.Time {
	if grain.Derived() {
		return time.Time{}
	}

	return query.RollupBucketStart(from.Add(-backfillPerPass), grain, location)
}

// firstWholeBucket is the first bucket of a grain that begins at or after an
// instant, so a bucket is never built from a range that starts inside it.
func firstWholeBucket(at time.Time, grain query.Grain, location *time.Location) time.Time {
	start := query.RollupBucketStart(at, grain, location)
	if start.Equal(at) {
		return start
	}

	return query.RollupNextBucket(start, grain, location)
}

// laterOf is the more recent of two instants.
func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// localToInstant turns a stored local-seconds bucket back into the instant it
// began. The bucket is a wall-clock reading, so it is rendered in UTC and then
// re-read in the site's timezone, which is the inverse of how it was stored.
func localToInstant(local int64, location *time.Location, grain query.Grain) time.Time {
	wall := time.Unix(local, 0).UTC()

	at := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), 0, 0, location)

	return query.RollupBucketStart(at, grain, location)
}

// FirstEvent finds when a site's history starts, which is as far back as a
// backfill can usefully go and the point a rebuild's progress is measured from.
func FirstEvent(ctx context.Context, db *sql.DB, siteID int64) (time.Time, error) {
	var earliest sql.NullInt64

	if err := db.QueryRowContext(ctx, "SELECT MIN(timestamp) FROM events WHERE site_id = ?", siteID).Scan(&earliest); err != nil {
		return time.Time{}, fmt.Errorf("rollup: read first event: %w", err)
	}

	if !earliest.Valid {
		return time.Time{}, nil
	}

	return time.Unix(earliest.Int64, 0).UTC(), nil
}

// SystemLister reads every site out of system.db. It is what `feasible serve`
// and the rebuild command both hand the worker, so the set of sites that get
// summarised is the same set that receives traffic.
func SystemLister(control *sql.DB) Lister {
	return func(ctx context.Context) ([]SiteRef, error) {
		rows, err := control.QueryContext(ctx,
			"SELECT id, account_id, domain, timezone FROM sites ORDER BY account_id, id")
		if err != nil {
			return nil, fmt.Errorf("rollup: read sites: %w", err)
		}
		defer func() { _ = rows.Close() }()

		var refs []SiteRef

		for rows.Next() {
			var ref SiteRef

			if err := rows.Scan(&ref.Site.ID, &ref.AccountID, &ref.Site.Domain, &ref.Site.Timezone); err != nil {
				return nil, fmt.Errorf("rollup: read sites: %w", err)
			}

			refs = append(refs, ref)
		}

		return refs, rows.Err()
	}
}
