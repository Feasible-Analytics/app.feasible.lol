//
// avatar.go
// The `avatar backfill` subcommand.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/avatar"
)

const avatarHelp = `feasible avatar — the picture on the account button.

A picture is looked up on sign-in and never again, so an account that predates
the feature keeps its letter until its owner next signs in — which, on a
fourteen-day session, may be months away.

Commands:
  backfill  Ask a provider about everybody who has never been asked.
`

const avatarBackfillHelp = `feasible avatar backfill — ask about everybody once.

Asks Gravatar about every account that has no stored picture and no recent miss
on file. A person who already has one, or who was asked about within the last
week, is left alone, so running this twice costs nothing.

It needs outbound access and FEASIBLE_APP_GRAVATAR switched on.

Flags:
`

// runAvatar dispatches the avatar subcommands.
func runAvatar(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, avatarHelp)
		return ExitUsage
	}

	switch args[0] {
	case "backfill":
		return avatarBackfill(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown avatar command %q\n\n", args[0])
		fmt.Fprint(e.stderr, avatarHelp)
		return ExitUsage
	}
}

// avatarBackfill asks about everybody who has never been asked.
func avatarBackfill(e *env, args []string) int {
	fs := newFlagSet("avatar backfill", e, avatarBackfillHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(e.stderr, "unexpected avatar backfill argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	ctx := context.Background()

	control, err := openSystem(ctx, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	refresher := newAvatarRefresher(e, control)

	// Synchronous, so the count printed is work that actually finished.
	refresher.Run = func(work func()) { work() }

	asked, err := refresher.Backfill(ctx, verifiedPeople(control))
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if asked == 0 {
		fmt.Fprintln(e.stdout, "every account already has a picture or a recent answer on file")
		return ExitOK
	}

	fmt.Fprintf(e.stdout, "asked a provider about %d accounts\n", asked)

	return ExitOK
}

// verifiedPeople lists everybody who could have a picture.
//
// Unverified addresses are left out: the address has not been proved to belong
// to the person, and asking a third party about the hash of one somebody typed
// is a lookup nobody consented to.
func verifiedPeople(control *sql.DB) func(context.Context) ([]avatar.Person, error) {
	return func(ctx context.Context) ([]avatar.Person, error) {
		rows, err := control.QueryContext(ctx,
			"SELECT id, email FROM users WHERE email_verified_at IS NOT NULL ORDER BY id")
		if err != nil {
			return nil, fmt.Errorf("avatar backfill: read accounts: %w", err)
		}
		defer func() { _ = rows.Close() }()

		people := []avatar.Person{}

		for rows.Next() {
			var person avatar.Person

			if err := rows.Scan(&person.ID, &person.Email); err != nil {
				return nil, fmt.Errorf("avatar backfill: read accounts: %w", err)
			}

			people = append(people, person)
		}

		return people, rows.Err()
	}
}
