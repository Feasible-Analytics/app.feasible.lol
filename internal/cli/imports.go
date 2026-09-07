//
// imports.go
// The `imports summarise` subcommand.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
)

const importsHelp = `feasible imports — the history a customer brought with them.

Commands:
  summarise  Build the week and month summaries an import needs to read quickly.
`

const importsSummariseHelp = `feasible imports summarise — build the week and month summaries.

An import summarises itself as it completes, and a site whose timezone changes
re-cuts them on the next run of this. Anything with no summaries is read a day
at a time, which on a large archive is millions of rows for every wide report.

It sums rows already held, so it needs no source data and no re-import. Running
it twice costs nothing: an import whose summaries are current is skipped.

Flags:
`

// runImports dispatches the imports subcommands.
func runImports(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, importsHelp)
		return ExitUsage
	}

	switch args[0] {
	case "summarise", "summarize":
		return runImportsSummarise(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown imports command %q\n\n", args[0])
		fmt.Fprint(e.stderr, importsHelp)
		return ExitUsage
	}
}

// runImportsSummarise builds the wide rows for every import that lacks them or
// holds them in a timezone the site no longer uses.
func runImportsSummarise(e *env, args []string) int {
	fs := newFlagSet("imports summarise", e, importsSummariseHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	ctx := context.Background()

	control, err := openSystem(ctx, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	manager := accounts.NewManager(*dataDir)
	manager.MaxOpen = e.cfg.App.MaxOpenAccounts

	defer func() { _ = manager.CloseAll() }()

	sites, err := rollup.SystemLister(control)(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	summarised := 0

	for _, ref := range sites {
		// A walk over every account, so the handles it opens must not evict the
		// ones taking traffic.
		lease, err := manager.AcquireForScan(ctx, ref.AccountID)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		done, err := dataio.SummariseSite(ctx, lease.Account.Writer(), ref.Site.ID, ref.Site.Location())
		_ = lease.Release()

		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		summarised += done
	}

	if summarised == 0 {
		fmt.Fprintln(e.stdout, "every import already has its week and month summaries")
		return ExitOK
	}

	fmt.Fprintf(e.stdout, "summarised %d import(s)\n", summarised)

	return ExitOK
}
