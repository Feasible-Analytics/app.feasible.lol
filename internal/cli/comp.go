//
// comp.go
// The `comp` subcommand: grant durable complimentary access by owner email.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
)

const compHelp = `feasible comp — grant an account complimentary access.

Usage:
  feasible comp --email owner@example.com

The email must belong to the owner of exactly one team. Complimentary access is
durable: it stops the lifecycle clock and later trial or failed-payment signals
cannot restart it. Owner-requested account deletion remains available.

Flags:
`

// runComp resolves one owner email and permanently exempts that team from the
// payment lifecycle. Repeating the command is safe and reports the existing
// comp instead of changing its audit timestamp.
func runComp(e *env, args []string) int {
	fs := newFlagSet("comp", e, compHelp)
	email := fs.String("email", "", "email address of the account owner")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(e.stderr, "unexpected comp argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	normalized := strings.ToLower(strings.TrimSpace(*email))
	if normalized == "" {
		fmt.Fprintln(e.stderr, "usage: feasible comp --email owner@example.com")
		return ExitUsage
	}

	control, err := openControl(context.Background(), *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	result, err := lifecycle.NewStore(control).CompByOwnerEmail(context.Background(), normalized)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if result.AlreadyComped {
		fmt.Fprintf(e.stdout, "account %d (%s) is already comped\n", result.TeamID, result.OwnerEmail)
		return ExitOK
	}

	fmt.Fprintf(e.stdout, "account %d (%s) is now comped; dashboard and collection are unlocked\n", result.TeamID, result.OwnerEmail)
	return ExitOK
}
