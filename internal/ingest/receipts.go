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
	"fmt"
	"sort"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// The queue and kind this job runs under.
const (
	QueueMaintenance   = "maintenance"
	KindPruneReceipts  = "ingest.prune_receipts"
	PruneReceiptsEvery = time.Hour
)

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

	// Owners lists the accounts to sweep. It is a function rather than a table
	// read so this package does not have to know where the list came from.
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

// OwnersOf lists every distinct account that has a site on this shard. The
// receipts live in the account database, so that is the unit swept.
func OwnersOf(cache interface{ All() []sites.Site }) func(context.Context) ([]int64, error) {
	return func(context.Context) ([]int64, error) {
		seen := map[int64]bool{}
		owners := []int64{}

		for _, site := range cache.All() {
			if !seen[site.AccountID] {
				seen[site.AccountID] = true
				owners = append(owners, site.AccountID)
			}
		}

		sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

		return owners, nil
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

	owners, err := p.Owners(ctx)
	if err != nil {
		return jobs.Outcome{}, err
	}

	cutoff := p.now().Add(-ReceiptRetention).Unix()

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
	lease, err := p.Accounts.Acquire(ctx, accountID)
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
