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
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/metrics"
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
// control.db, from a command's flags, or from a test.
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

// once runs a pass, records how it went and logs whatever went wrong. A failed
// roll-up is not a failed request — the reports simply stay slow — so it never
// stops the loop.
//
// The timestamp of the last complete pass is the number that answers "is the
// worker behind": every dashboard reads summaries, and a worker that quietly
// stopped is a dashboard that is slow today and wrong tomorrow. It is recorded
// here rather than in Once, because the command runs Once for one deliberate
// rebuild and that is not the same claim as "the loop is keeping up".
func (w *Worker) once(ctx context.Context) {
	started := w.now()

	err := w.Once(ctx)

	metrics.RollupDuration.Observe(w.now().Sub(started).Seconds())

	if err != nil {
		metrics.RollupRuns.WithLabelValues(metrics.OutcomeError).Inc()

		if w.Log != nil {
			w.Log.Error("roll-up build failed", "error", err)
		}

		return
	}

	metrics.RollupRuns.WithLabelValues(metrics.OutcomeOK).Inc()
	metrics.RollupLastSuccess.Set(float64(w.now().Unix()))
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
	account, err := w.Accounts.Open(ctx, ref.AccountID)
	if err != nil {
		return err
	}

	builder := New(account.Writer())
	builder.Now = w.now

	location := ref.Site.Location()
	now := w.now().In(location)
	today := query.RollupBucketStart(now, query.GrainDay, location)

	earliest, err := firstEvent(ctx, account.Reader(), ref.Site.ID)
	if err != nil {
		return err
	}

	if earliest.IsZero() {
		return nil
	}

	for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
		from, ok, err := w.windowStart(ctx, builder, ref.Site, grain, earliest, today, location)
		if err != nil {
			return err
		}

		if !ok {
			continue
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

		if err := builder.Rebuild(ctx, Request{
			Site: ref.Site, Grain: grain, From: from, To: to, CoverThrough: today,
			FromBeginning: !from.After(earliest),
		}); err != nil {
			return err
		}
	}

	return builder.Prune(ctx, ref.Site)
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
		return oldest, true, nil
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

	if !start.Before(today) && grain == query.GrainHour {
		return time.Time{}, false, nil
	}

	return start, true, nil
}

// localToInstant turns a stored local-seconds bucket back into the instant it
// began. The bucket is a wall-clock reading, so it is rendered in UTC and then
// re-read in the site's timezone, which is the inverse of how it was stored.
func localToInstant(local int64, location *time.Location, grain query.Grain) time.Time {
	wall := time.Unix(local, 0).UTC()

	at := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), 0, 0, location)

	return query.RollupBucketStart(at, grain, location)
}

// firstEvent finds when a site's history starts, which is as far back as a
// backfill can usefully go.
func firstEvent(ctx context.Context, db *sql.DB, siteID int64) (time.Time, error) {
	var earliest sql.NullInt64

	if err := db.QueryRowContext(ctx, "SELECT MIN(timestamp) FROM events WHERE site_id = ?", siteID).Scan(&earliest); err != nil {
		return time.Time{}, fmt.Errorf("rollup: read first event: %w", err)
	}

	if !earliest.Valid {
		return time.Time{}, nil
	}

	return time.Unix(earliest.Int64, 0).UTC(), nil
}

// ControlLister reads every site out of control.db. It is what `feasible serve`
// and the rebuild command both hand the worker, so the set of sites that get
// summarised is the same set that receives traffic.
func ControlLister(control *sql.DB) Lister {
	return func(ctx context.Context) ([]SiteRef, error) {
		rows, err := control.QueryContext(ctx,
			"SELECT id, account_id, domain, timezone FROM sites ORDER BY account_id, id")
		if err != nil {
			return nil, fmt.Errorf("rollup: read sites: %w", err)
		}
		defer rows.Close()

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
