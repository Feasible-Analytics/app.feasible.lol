//
// jobs.go
// The background workers that run an import and build an export.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pathclean"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// ImportArgs is what an import job carries. The account is named alongside the
// site because the worker has to open a database before it can look anything
// up, and resolving the account from the site would need the very database it
// is trying to reach.
type ImportArgs struct {
	AccountID int64 `json:"account_id"`
	SiteID    int64 `json:"site_id"`
	ImportID  int64 `json:"import_id"`
}

// ExportArgs is what an export job carries.
type ExportArgs struct {
	AccountID int64 `json:"account_id"`
	SiteID    int64 `json:"site_id"`
	ExportID  int64 `json:"export_id"`
}

// Workers runs the import and export jobs. It holds the account manager and the
// site snapshot rather than system.db, because everything it writes is
// site-scoped and lives in one account database.
type Workers struct {
	Accounts *accounts.Manager
	Sites    *sites.Cache
	DataDir  string

	// Now is injectable so a test can assert on the timestamps a finished
	// import carries rather than on whatever the clock said.
	Now func() time.Time
}

// now reads the workers' clock.
func (w *Workers) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}

	return w.Now().UTC()
}

// Register attaches both workers to a runner. Registration is by kind, which is
// what makes "which worker ran this job" a fact rather than something inferred
// from the queue it was on — the inference that let an incumbent's cleanup
// purge fifteen completed imports after an unrelated worker crashed.
func (w *Workers) Register(runner *jobs.Runner) {
	runner.Register(jobs.QueueImports, jobs.KindCSVImport, jobs.WorkerFunc(w.RunCSVImport))
	runner.Register(jobs.QueueExports, jobs.KindSiteExport, jobs.WorkerFunc(w.RunExport))
}

// location resolves a site's timezone, falling back to UTC. A site whose zone
// cannot be loaded is a site whose days would be silently wrong, so the
// fallback is named in the log rather than assumed everywhere.
func (w *Workers) location(siteID int64) *time.Location {
	for _, site := range w.Sites.All() {
		if site.ID != siteID {
			continue
		}

		if loaded, err := time.LoadLocation(site.Timezone); err == nil {
			return loaded
		}

		break
	}

	return time.UTC
}

// RunCSVImport reads an uploaded file into imported roll-up rows.
//
// Every failure is written onto the import row before it is returned, so the
// customer sees the reason on the imports page rather than a job that simply
// stopped. That is the same rule the whole product runs on: a failure nobody
// can see is worse than the failure itself.
func (w *Workers) RunCSVImport(ctx context.Context, job jobs.Job) error {
	var args ImportArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.PermanentError(fmt.Errorf("import job %d has arguments that cannot be read: %w", job.ID, err))
	}

	lease, err := w.Accounts.Acquire(ctx, args.AccountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the job result is more useful than an unlock error
	account := lease.Account

	record, err := GetImportByID(ctx, account.Writer(), args.ImportID)
	if err != nil {
		return jobs.PermanentError(err)
	}

	if record.UploadPath == "" {
		return w.failImport(ctx, account.Writer(), record.ID,
			"the uploaded file is missing — start the import again")
	}

	sources, closeArchive, err := SourcesFromUpload(record.UploadPath)
	if err != nil {
		return w.failImport(ctx, account.Writer(), record.ID, err.Error())
	}
	defer closeArchive() //nolint:errcheck // closing a read-only archive cannot lose anything

	err = ImportCSV(ctx, account.Writer(), account.Intern, record, sources,
		w.location(record.SiteID), w.now)
	if err != nil {
		return w.failImport(ctx, account.Writer(), record.ID, err.Error())
	}

	// An import brings in paths nobody has interned before, and a site with
	// cleaning rules needs those paths in the map or its imported history is
	// the only part of the site the rules do not apply to.
	if _, err := pathclean.Materialise(ctx, account.Writer(), account.Intern, record.SiteID); err != nil {
		return err
	}

	// The uploaded file has done its job. Keeping it would mean a copy of the
	// customer's history sitting on disk indefinitely for no reader.
	if err := os.Remove(record.UploadPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return SetUploadPath(ctx, account.Writer(), record.ID, "")
}

// failImport writes the reason onto the row and stops the job being retried.
// Retrying a malformed CSV three more times only delays the moment the customer
// is told what is wrong with their file.
func (w *Workers) failImport(ctx context.Context, db *sql.DB, id int64, reason string) error {
	if err := FailImport(ctx, db, id, reason, w.now()); err != nil {
		return err
	}

	return jobs.PermanentError(fmt.Errorf("%s", reason))
}

// RunExport builds a site's archive and opens its download window.
func (w *Workers) RunExport(ctx context.Context, job jobs.Job) error {
	var args ExportArgs
	if err := json.Unmarshal(job.Args, &args); err != nil {
		return jobs.PermanentError(fmt.Errorf("export job %d has arguments that cannot be read: %w", job.ID, err))
	}

	lease, err := w.Accounts.Acquire(ctx, args.AccountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the job result is more useful than an unlock error
	account := lease.Account

	destination := ExportPath(w.DataDir, args.AccountID, args.ExportID)

	size, err := BuildExport(ctx, account.Reader(), args.SiteID, w.location(args.SiteID), destination)
	if err != nil {
		if failErr := FailExport(ctx, account.Writer(), args.ExportID, err.Error(), w.now()); failErr != nil {
			return failErr
		}

		return jobs.PermanentError(err)
	}

	return CompleteExport(ctx, account.Writer(), args.ExportID, destination, size, w.now())
}
