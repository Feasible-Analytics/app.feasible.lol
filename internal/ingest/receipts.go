//
// receipts.go
// The job that ages out the event receipts every accepted event writes.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// The queue and kind this job runs under, and how often it ticks.
const (
	QueueMaintenance   = "maintenance"
	KindPruneReceipts  = "ingest.prune_receipts"
	PruneReceiptsEvery = time.Hour
)

// PruneNotBefore is when the first receipt may be removed.
//
// A tracker cached before the stamp existed stashed a bare body, and there is
// nothing to date it by — so it is dated from the day the stamp shipped and
// stays replayable for a week after that, however old it really is. Its receipt
// may already be older than the retention window, and pruning it before the
// entry expires is the one path to a doubled pageview. Nothing is removed until
// no undatable entry can arrive.
var PruneNotBefore = time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)

// ReceiptRetention is how long a receipt has to be recognisable for.
//
// The tracker gives up on a stashed event after seven days, so that is how long
// a browser can still replay one. The rest is margin — a wrong client clock, a
// tab open across the boundary, a prune that has not run for a while, a browser
// on a cached older tracker — so that the server is never the reason a
// legitimate replay is counted twice.
const ReceiptRetention = 30 * 24 * time.Hour

// PruneBatch bounds one delete so the write lock is never held for long. The
// job runs until a pass deletes fewer rows than this, so a backlog clears over
// several passes rather than in one transaction that stops ingestion.
const PruneBatch = 5000

// ReceiptPruner ages out recent_event_ids across every account on the shard.
//
// The table is WITHOUT ROWID over a random uuid, so it is its own index and
// every insert lands at a random position in a tree that only grows. While that
// tree fits in memory the random position is free; once it does not, every
// accepted event pays a random disk read on the write path and never stops.
type ReceiptPruner struct {
	Accounts *accounts.Manager
	Log      *logger.Logger

	// Owners lists the accounts to sweep, one database each. It is a function
	// so a test can name two accounts without a control database.
	Owners func(ctx context.Context) ([]int64, error)

	// Now is the clock the cutoff is measured from.
	Now func() time.Time
}

// now reads the pruner's clock.
func (p *ReceiptPruner) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}

	return p.Now().UTC()
}

// SystemOwners lists every account with a site, read from system.db.
//
// The routing snapshot would be the obvious source and is the wrong one: a
// refresh that failed, or a boot before the first one, leaves it empty — and an
// empty list is a prune that removes nothing and reports success, for ever.
// A failed query is an error the job reports; an empty table is an install with
// no accounts.
func SystemOwners(control *sql.DB) func(context.Context) ([]int64, error) {
	return func(ctx context.Context) ([]int64, error) {
		rows, err := control.QueryContext(ctx,
			"SELECT DISTINCT account_id FROM sites ORDER BY account_id")
		if err != nil {
			return nil, fmt.Errorf("ingest: read accounts to prune: %w", err)
		}
		defer func() { _ = rows.Close() }()

		owners := []int64{}

		for rows.Next() {
			var owner int64

			if err := rows.Scan(&owner); err != nil {
				return nil, fmt.Errorf("ingest: read accounts to prune: %w", err)
			}

			owners = append(owners, owner)
		}

		return owners, rows.Err()
	}
}

// Register attaches the job to the runner and its tick to the cron.
func (p *ReceiptPruner) Register(runner *jobs.Runner, cron *jobs.Cron) {
	runner.Register(QueueMaintenance, KindPruneReceipts, jobs.Reporting(p.Log, p.Run))
	cron.Add(QueueMaintenance, KindPruneReceipts, PruneReceiptsEvery)
}

// Run prunes every account and says how many receipts it removed.
//
// One account's failure does not stop the rest: a single unreadable database
// would otherwise leave every other account's table growing, which is the thing
// this job exists to prevent.
func (p *ReceiptPruner) Run(ctx context.Context, _ jobs.Job) (jobs.Outcome, error) {
	if p.Accounts == nil || p.Owners == nil {
		return jobs.Outcome{}, fmt.Errorf("ingest: the receipt pruner needs an account manager and an account list")
	}

	now := p.now()

	if now.Before(PruneNotBefore) {
		return jobs.Nothing("no receipt is removed until " + PruneNotBefore.Format("2 January 2006") +
			", by when an entry stashed before the tracker stamped one has expired"), nil
	}

	owners, err := p.Owners(ctx)
	if err != nil {
		return jobs.Outcome{}, err
	}

	cutoff := now.Add(-ReceiptRetention).Unix()

	removed, failed := 0, 0

	for _, owner := range owners {
		count, err := p.pruneAccount(ctx, owner, cutoff)
		if err != nil {
			failed++

			if p.Log != nil {
				p.Log.Error("event receipts could not be pruned", "account", owner, "error", err)
			}

			continue
		}

		removed += count
	}

	if failed > 0 {
		return jobs.Outcome{Handled: removed, Skipped: failed},
			fmt.Errorf("ingest: %d of %d accounts could not be pruned", failed, len(owners))
	}

	if removed == 0 {
		return jobs.Nothing(fmt.Sprintf("no event receipt across %d accounts is older than %d days",
			len(owners), int(ReceiptRetention.Hours()/24))), nil
	}

	return jobs.Outcome{Handled: removed}, nil
}

// pruneAccount deletes one account's aged receipts, a batch at a time.
func (p *ReceiptPruner) pruneAccount(ctx context.Context, accountID, cutoff int64) (int, error) {
	// A scan: this walks every account on the shard every hour, and promoting
	// each one would leave the handle cache holding whichever it visited last.
	lease, err := p.Accounts.AcquireForScan(ctx, accountID)
	if err != nil {
		return 0, err
	}
	defer lease.Release() //nolint:errcheck // the prune result is more useful than an unlock error

	removed := 0

	for {
		result, err := lease.Account.Writer().ExecContext(ctx, `
			DELETE FROM recent_event_ids
			WHERE event_uuid IN (
				SELECT event_uuid FROM recent_event_ids WHERE received_at < ? LIMIT ?
			)`, cutoff, PruneBatch)
		if err != nil {
			return removed, fmt.Errorf("ingest: prune receipts: %w", err)
		}

		count, err := result.RowsAffected()
		if err != nil {
			return removed, fmt.Errorf("ingest: prune receipts: %w", err)
		}

		removed += int(count)

		if count < PruneBatch {
			return removed, nil
		}

		// A pause between batches, for the reason the roll-up rests between
		// chunks: storing an event matters and tidying a table does not, so
		// when the two want the same lock the tidying gives way.
		select {
		case <-ctx.Done():
			return removed, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
