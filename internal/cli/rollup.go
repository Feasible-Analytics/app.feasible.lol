//
// rollup.go
// The `rollup build`, `rollup rebuild` and `rollup status` subcommands.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

const rollupHelp = `feasible rollup — the pre-aggregated report tables.

Reports over a month of a busy account read millions of rows to produce a few
thousand numbers. These tables hold those numbers. The worker inside
` + "`feasible serve`" + ` keeps them up to date on its own; these commands exist for
the first build, for a repair, and for finding out what is covered.

Commands:
  build     Bring every site up to date — one pass of what the worker does hourly.
  rebuild   Throw a site's summary away and build it again from raw.
  status    Print what is built, per site and grain.
`

const rollupBuildHelp = `feasible rollup build — one pass of the hourly worker.

Seals the buckets that have finished, recomputes today's daily row from raw, and
drops hourly buckets that have aged out. Safe to run at any time and safe to run
twice: every bucket is deleted and rebuilt rather than incremented.

Flags:
`

const rollupRebuildHelp = `feasible rollup rebuild — start a site's summary again.

Deletes every summary row for the site and builds it back from the raw events.
This is the repair: a roll-up is a cache, never the truth, so a bug in the build
is a re-run rather than lost data.

Flags:
`

const rollupStatusHelp = `feasible rollup status — what is built.

Prints the covered window per site and grain, in the timezone the buckets were
cut in. A report outside the covered window is answered from raw events, which
is correct and slow.

Flags:
`

// runRollup dispatches the two-word roll-up commands.
func runRollup(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, rollupHelp)
		return ExitUsage
	}

	switch args[0] {
	case "build":
		return runRollupBuild(e, args[1:])
	case "rebuild":
		return runRollupRebuild(e, args[1:])
	case "status":
		return runRollupStatus(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown rollup command %q\n\n", args[0])
		fmt.Fprint(e.stderr, rollupHelp)
		return ExitUsage
	}
}

// runRollupBuild runs a single pass of the worker and exits. It is the same
// code the background loop runs, so a manual build can never behave differently
// from the automatic one.
func runRollupBuild(e *env, args []string) int {
	fs := newFlagSet("rollup build", e, rollupBuildHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	ctx := context.Background()

	worker, closers, err := buildRollupWorker(ctx, e, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer closers()

	started := time.Now()

	if err := worker.Once(ctx); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	fmt.Fprintf(e.stdout, "roll-ups built in %s\n", time.Since(started).Round(time.Millisecond))

	return ExitOK
}

// runRollupRebuild throws one site's summary away and builds it again.
func runRollupRebuild(e *env, args []string) int {
	fs := newFlagSet("rollup rebuild", e, rollupRebuildHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")
	domain := fs.String("site", "", "the site to rebuild; empty means every site")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	ctx := context.Background()

	worker, closers, err := buildRollupWorker(ctx, e, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer closers()

	refs, err := worker.Sites(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	started := time.Now()

	for _, ref := range refs {
		if *domain != "" && sites.Normalise(ref.Site.Domain) != sites.Normalise(*domain) {
			continue
		}

		lease, err := worker.Accounts.Acquire(ctx, ref.AccountID)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
		account := lease.Account

		builder := rollup.New(account.Writer())

		for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
			if err := builder.Reset(ctx, ref.Site.ID, grain); err != nil {
				_ = lease.Release()
				fmt.Fprintf(e.stderr, "%v\n", err)
				return ExitError
			}
		}
		if err := lease.Release(); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		fmt.Fprintf(e.stdout, "  %s cleared\n", ref.Site.Domain)
	}

	if err := worker.Once(ctx); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	fmt.Fprintf(e.stdout, "rebuilt in %s\n", time.Since(started).Round(time.Millisecond))

	return ExitOK
}

// runRollupStatus prints what is covered. It also lists the dimensions the
// summary is keyed by, because "why is this report still slow" is nearly always
// "that dimension is not summarised".
func runRollupStatus(e *env, args []string) int {
	fs := newFlagSet("rollup status", e, rollupStatusHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	ctx := context.Background()

	worker, closers, err := buildRollupWorker(ctx, e, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer closers()

	refs, err := worker.Sites(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	for _, ref := range refs {
		lease, err := worker.Accounts.Acquire(ctx, ref.AccountID)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
		account := lease.Account

		builder := rollup.New(account.Writer())

		for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
			coverage, found, err := builder.Coverage(ctx, ref.Site.ID, grain)
			if err != nil {
				_ = lease.Release()
				fmt.Fprintf(e.stderr, "%v\n", err)
				return ExitError
			}

			if !found {
				fmt.Fprintf(e.stdout, "  %-32s %-5s nothing built\n", ref.Site.Domain, grain)
				continue
			}

			fmt.Fprintf(e.stdout, "  %-32s %-5s %s\n", ref.Site.Domain, grain, coverage)
		}
		if err := lease.Release(); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
	}

	fmt.Fprintf(e.stdout, "\nkeyed by: %s\n", strings.Join(query.RollupDimensionNames(), ", "))

	return ExitOK
}

// buildRollupWorker opens system.db and an account manager and wires a worker
// over them. The returned function closes both, and it is one function so a
// caller cannot close half of what it opened.
func buildRollupWorker(ctx context.Context, e *env, dataDir string) (*rollup.Worker, func(), error) {
	control, err := openSystem(ctx, dataDir)
	if err != nil {
		return nil, nil, err
	}

	manager := accounts.NewManager(dataDir)

	worker := &rollup.Worker{
		Accounts: manager,
		Sites:    rollup.SystemLister(control),
		Log:      e.log,
	}

	return worker, func() {
		_ = manager.CloseAll()
		_ = control.Close()
	}, nil
}
