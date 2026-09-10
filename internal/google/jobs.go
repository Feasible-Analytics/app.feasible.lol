//
// jobs.go
// The backfill a customer starts, and the nightly top-up nobody sees.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pathclean"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// SyncEvery is how often the top-up runs. Google publishes once a day, so
// anything more frequent spends a customer's API quota re-reading numbers that
// have not moved.
const SyncEvery = 6 * time.Hour

// SyncLookback bounds outage recovery. A missed night is caught up by the next
// run anyway — every run re-reads the last several days — so this only has to
// survive a restart, not a week off.
const SyncLookback = 24 * time.Hour

// ImportArgs is what a backfill job carries.
//
// The window travels on the job rather than being worked out when the job runs,
// because a retry a day later must ask for the same days as the first attempt.
// A window derived from the clock would slide forward between attempts and skip
// whichever day the cursor had already passed.
type ImportArgs struct {
	AccountID int64  `json:"account_id"`
	SiteID    int64  `json:"site_id"`
	ImportID  int64  `json:"import_id"`
	Property  string `json:"property,omitempty"`

	// From and To are inclusive day bounds as unix seconds.
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Workers runs GA4 imports and both Search Console jobs.
//
// It holds the OAuth application rather than reaching for configuration,
// because an install with no Google credentials must have a worker that says so
// once, not one that fails every night with a token error.
type Workers struct {
	Accounts *accounts.Manager
	Sites    *sites.Cache
	App      *App
	Log      *logger.Logger

	// Now is injectable so a test can decide which days a refresh asks for.
	Now func() time.Time
}

// now reads the workers' clock.
func (w *Workers) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}

	return w.Now().UTC()
}

// Register attaches the Google import workers and the recurring search tick.
//
// Registration happens even with no OAuth application configured. A kind with
// no worker is discarded with that reason on the row, and a backfill enqueued
// by a build that had credentials and drained by one that does not should say
// so rather than sit in the queue looking claimable.
func (w *Workers) Register(runner *jobs.Runner, cron *jobs.Cron) {
	if runner == nil {
		return
	}

	runner.Register(jobs.QueueImports, jobs.KindGA4Import, jobs.WorkerFunc(w.RunGA4Import))
	runner.Register(jobs.QueueImports, jobs.KindSearchConsoleImport, jobs.WorkerFunc(w.RunImport))
	runner.Register(jobs.QueueImports, jobs.KindSearchConsoleSync, jobs.Reporting(w.Log, w.RunSync))

	if cron != nil {
		cron.AddCatchUp(jobs.QueueImports, jobs.KindSearchConsoleSync, SyncEvery, SyncLookback)
	}
}

// RunImport backfills one site's search history.
//
// Every failure is written onto the import row before it is returned, so the
// customer reads the reason on the imports screen rather than watching a
// progress bar stop.
func (w *Workers) RunImport(ctx context.Context, job jobs.Job) error {
	if w.App == nil {
		return jobs.PermanentError(errors.New("this install has no Google application configured, so search history cannot be imported"))
	}

	var args ImportArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.PermanentError(fmt.Errorf("search import job %d has arguments that cannot be read: %w", job.ID, err))
	}

	lease, err := w.Accounts.Acquire(ctx, args.AccountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the job result is more useful than an unlock error
	account := lease.Account

	record, err := dataio.GetImportByID(ctx, account.Writer(), args.ImportID)
	if err != nil {
		return jobs.PermanentError(err)
	}

	location, err := w.location(record.SiteID)
	if err != nil {
		return w.fail(ctx, lease, record.ID, err.Error())
	}

	connection, err := GetConnection(ctx, account.Reader(), record.SiteID, ProviderSearchConsole)
	if err != nil {
		return w.fail(ctx, lease, record.ID, err.Error())
	}

	if connection == nil {
		return w.fail(ctx, lease, record.ID,
			"this site is no longer connected to Search Console — connect it again to import its search history")
	}

	from, to := BackfillWindow(w.now())

	if args.From > 0 && args.To > 0 {
		from, to = time.Unix(args.From, 0).UTC(), time.Unix(args.To, 0).UTC()
	}

	if err := w.App.SearchConsoleImport(ctx, account.Writer(), record, connection, from, to, location, w.now); err != nil {
		return w.fail(ctx, lease, record.ID, err.Error())
	}

	return nil
}

// RunSync re-reads the last few days for every connected site on this shard.
//
// It walks accounts rather than sites because the grants live in the account
// database: one query per account answers for every site that account owns, and
// most accounts answer with nothing at all.
func (w *Workers) RunSync(ctx context.Context, _ jobs.Job) (jobs.Outcome, error) {
	if w.App == nil {
		return jobs.Nothing("this install has no Google application configured, so there is no search data to refresh"), nil
	}

	owned := map[int64][]sites.Site{}
	for _, site := range w.Sites.All() {
		owned[site.AccountID] = append(owned[site.AccountID], site)
	}

	if len(owned) == 0 {
		return jobs.Nothing("this shard serves no sites"), nil
	}

	outcome := jobs.Outcome{}
	var failures []string

	for accountID, accountSites := range owned {
		handled, skipped, problems := w.syncAccount(ctx, accountID, accountSites)

		outcome.Handled += handled
		outcome.Skipped += skipped
		failures = append(failures, problems...)
	}

	if len(failures) > 0 {
		// One site's expired grant is no reason for another customer's numbers
		// to stop moving, so every site is attempted and the failures are
		// reported together.
		return outcome, fmt.Errorf("google: %d connected sites could not be refreshed: %s",
			len(failures), strings.Join(failures, "; "))
	}

	if outcome.Handled == 0 {
		outcome.Note = fmt.Sprintf("no site on this shard has a Search Console property chosen (%d skipped)", outcome.Skipped)
	}

	return outcome, nil
}

// syncAccount refreshes every connected site in one account database.
func (w *Workers) syncAccount(ctx context.Context, accountID int64, accountSites []sites.Site) (handled, skipped int, failures []string) {
	// AcquireForScan rather than Acquire: a nightly walk over every account
	// would otherwise evict the handles belonging to whoever is actually
	// looking at a dashboard right now.
	lease, err := w.Accounts.AcquireForScan(ctx, accountID)
	if err != nil {
		return 0, 0, []string{fmt.Sprintf("account %d: %v", accountID, err)}
	}
	defer lease.Release() //nolint:errcheck // the refresh result is more useful than an unlock error

	connections, err := ListConnections(ctx, lease.Account.Reader(), ProviderSearchConsole)
	if err != nil {
		return 0, 0, []string{fmt.Sprintf("account %d: %v", accountID, err)}
	}

	byID := make(map[int64]sites.Site, len(accountSites))
	for _, site := range accountSites {
		byID[site.ID] = site
	}

	for index := range connections {
		connection := &connections[index]

		site, ok := byID[connection.SiteID]
		if !ok {
			// The grant belongs to a site another shard serves. Its own shard
			// will refresh it.
			skipped++
			continue
		}

		if strings.TrimSpace(connection.Property) == "" || connection.NeedsReconnect() {
			skipped++
			continue
		}

		if err := w.refresh(ctx, lease, connection, site); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", site.Domain, err))
			continue
		}

		handled++
	}

	return handled, skipped, failures
}

// refresh reads one site's recent window.
func (w *Workers) refresh(ctx context.Context, lease *accounts.Lease, connection *Connection, site sites.Site) error {
	location, err := time.LoadLocation(site.Timezone)
	if err != nil {
		return fmt.Errorf("its timezone %q could not be loaded, so its days cannot be worked out: %w", site.Timezone, err)
	}

	now := w.now()
	to := SearchConsoleThrough(now)
	from := to.AddDate(0, 0, -(SearchConsoleRefreshDays - 1))

	written, err := w.App.SearchConsoleRefresh(ctx, lease.Account.Writer(), connection, from, to, location, now)
	if err != nil {
		return err
	}

	if w.Log != nil {
		w.Log.Info("search performance refreshed",
			"domain", site.Domain, "property", connection.Property, "rows", written)
	}

	return nil
}

// location resolves a site's timezone.
//
// It reports an error rather than falling back to UTC, because the timezone
// decides which local day every imported row lands in. Quietly using UTC for a
// site in Auckland produces a whole history shifted by a day, which nobody
// would ever trace back to here.
func (w *Workers) location(siteID int64) (*time.Location, error) {
	for _, site := range w.Sites.All() {
		if site.ID != siteID {
			continue
		}

		loaded, err := time.LoadLocation(site.Timezone)
		if err != nil {
			return nil, fmt.Errorf("this site's timezone %q could not be loaded, so its days cannot be worked out: %w",
				site.Timezone, err)
		}

		return loaded, nil
	}

	return nil, fmt.Errorf("site %d is not in this shard's site list, so its timezone is unknown", siteID)
}

// fail writes the reason onto the import row and stops the job being retried.
func (w *Workers) fail(ctx context.Context, lease *accounts.Lease, id int64, reason string) error {
	if err := dataio.FailImport(ctx, lease.Account.Writer(), id, reason, w.now()); err != nil {
		return err
	}

	return jobs.PermanentError(errors.New(reason))
}

// RunGA4Import imports the explicitly selected property and historical window.
// The job pins its property so reconnecting while queued cannot import another
// property's history. Failures clear partial data and appear on the import row.
func (w *Workers) RunGA4Import(ctx context.Context, job jobs.Job) error {
	var args ImportArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.PermanentError(err)
	}
	lease, err := w.Accounts.Acquire(ctx, args.AccountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // retain the useful import result
	account := lease.Account
	record, err := dataio.GetImportByID(ctx, account.Writer(), args.ImportID)
	if err != nil {
		return jobs.PermanentError(err)
	}
	if record.SiteID != args.SiteID || record.Source != dataio.SourceGA4 {
		return jobs.PermanentError(errors.New("GA4 job does not match its import"))
	}
	if record.Status == dataio.StatusCompleted {
		_, err := pathclean.Materialise(ctx, account.Writer(), account.Intern, record.SiteID)
		return err
	}
	if w.App == nil {
		return w.fail(ctx, lease, record.ID, "Google Analytics is not configured on this install")
	}
	location, err := w.location(record.SiteID)
	if err != nil {
		return w.fail(ctx, lease, record.ID, err.Error())
	}
	connection, err := GetConnection(ctx, account.Reader(), record.SiteID, ProviderGA4)
	if err != nil {
		return w.fail(ctx, lease, record.ID, err.Error())
	}
	if connection == nil || connection.NeedsReconnect() {
		return w.fail(ctx, lease, record.ID, "Reconnect Google Analytics before importing history")
	}
	if args.Property == "" || args.From <= 0 || args.To < args.From {
		return w.fail(ctx, lease, record.ID, "Choose a GA4 property and a valid date range")
	}
	if connection.Property != args.Property {
		return w.fail(ctx, lease, record.ID, "The Analytics connection changed; choose the property and start the import again")
	}
	from, to := time.Unix(args.From, 0).In(location), time.Unix(args.To, 0).In(location)
	if err := w.App.GA4Import(ctx, account.Writer(), account.Intern, record, connection, from, to, location, w.now); err != nil {
		return w.fail(ctx, lease, record.ID, err.Error())
	}
	// Populate existing path-cleaning rules for paths introduced by the import.
	if _, err := pathclean.Materialise(ctx, account.Writer(), account.Intern, record.SiteID); err != nil {
		return err
	}
	return nil
}
