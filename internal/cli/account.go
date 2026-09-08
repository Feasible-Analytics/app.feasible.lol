//
// account.go
// Operator-managed account creation for self-hosted installations.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

const accountHelp = `feasible account — manage accounts.

Usage:
  feasible account create --email owner@example.com [--name "Owner name"]
  feasible account delete --email spam@example.com [--email more@example.com] [--yes]

The create command is available only when FEASIBLE_APP_HOSTED=false. It creates
a verified owner and prints a generated password exactly once. Run ` + "`feasible db migrate`" + `
before creating the first account.

The delete command removes an owner, their team and everything belonging to
both, through the same durable workflow an owner's own deletion request uses.
It prints what it would remove and stops there unless --yes is given, because
one mistyped address is otherwise an account nobody can get back.

Flags:
`

// runAccount dispatches operator-only account management. Keeping creation
// under an account namespace leaves room for future list and recovery commands
// without adding unrelated root commands.
func runAccount(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, accountHelp)
		return ExitUsage
	}

	switch args[0] {
	case "create":
		return accountCreate(e, args[1:])
	case "delete":
		return accountDelete(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown account command %q\n\n", args[0])
		fmt.Fprint(e.stderr, accountHelp)
		return ExitUsage
	}
}

// emailList collects a --email flag given more than once, so one run can clear
// a whole abusive batch under a single confirmation.
type emailList []string

// String renders the addresses for the flag package's usage output.
func (e *emailList) String() string { return strings.Join(*e, ", ") }

// Set normalises and appends one address.
func (e *emailList) Set(value string) error {
	*e = append(*e, auth.NormaliseEmail(value))
	return nil
}

// doomed is one account the delete command resolved and is ready to remove.
type doomed struct {
	user   *auth.User
	teamID int64
	sites  int
}

// accountDelete removes owners and their teams by email address.
//
// It resolves every address first and refuses the whole run if any one of them
// is unclear, so a batch is all-or-nothing rather than half-applied. An address
// that owns no team, or more than one, is a decision for a person rather than a
// guess made here.
func accountDelete(e *env, args []string) int {
	var emails emailList

	fs := newFlagSet("account delete", e, accountHelp)
	fs.Var(&emails, "email", "email address of an account owner; repeat for more than one")
	confirm := fs.Bool("yes", false, "actually delete; without it the command only reports what it would remove")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if fs.NArg() != 0 {
		fmt.Fprintf(e.stderr, "unexpected account delete argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	if len(emails) == 0 {
		fmt.Fprintln(e.stderr, "account delete requires at least one --email address")
		return ExitUsage
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

	defer manager.CloseAll() //nolint:errcheck // the process is exiting either way

	targets, code := resolveAccounts(ctx, e, control, emails)
	if code != ExitOK {
		return code
	}

	for _, t := range targets {
		fmt.Fprintf(e.stdout, "%s  user %d, team %d, %d site(s), %s\n",
			t.user.Email, t.user.ID, t.teamID, t.sites, verifiedLabel(t.user))
	}

	if !*confirm {
		fmt.Fprintf(e.stdout, "\nNothing was deleted. Re-run with --yes to remove %d account(s).\n", len(targets))
		return ExitOK
	}

	// The mailer is built because the durable workflow the purger runs can
	// send, and a transport this binary does not know is refused now rather
	// than partway through a batch.
	mailer, err := buildMailer(e)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	com := buildCommerce(e, control, manager, nil, mailer)

	deleter := auth.NewDeleter(com.Purger, e.log)

	removed := 0

	for _, t := range targets {
		if err := deleter.DeleteAccount(ctx, t.user.ID, t.teamID); err != nil {
			fmt.Fprintf(e.stderr, "%s: %v\n", t.user.Email, err)
			fmt.Fprintf(e.stderr, "stopped after %d of %d\n", removed, len(targets))

			return ExitError
		}

		removed++

		fmt.Fprintf(e.stdout, "deleted %s (user %d, team %d)\n", t.user.Email, t.user.ID, t.teamID)
	}

	fmt.Fprintf(e.stdout, "%d account(s) deleted.\n", removed)

	return ExitOK
}

// resolveAccounts turns addresses into the owner and team each one names, and
// reports every problem it found rather than only the first: an operator
// fixing a batch wants the whole list, not one address per run.
func resolveAccounts(ctx context.Context, e *env, control *sql.DB, emails emailList) ([]doomed, int) {
	users := auth.NewStore(control)
	teamStore := teams.NewStore(control)

	targets := make([]doomed, 0, len(emails))
	problems := 0

	for _, email := range emails {
		user, err := users.UserByEmail(ctx, email)
		if err != nil {
			fmt.Fprintf(e.stderr, "%s: no such account (%v)\n", email, err)
			problems++

			continue
		}

		owned, err := teamStore.TeamIDs(ctx, user.ID, teams.PermDeleteTeam)
		if err != nil {
			fmt.Fprintf(e.stderr, "%s: %v\n", email, err)
			problems++

			continue
		}

		if len(owned) != 1 {
			fmt.Fprintf(e.stderr, "%s: owns %d teams, so which one to delete is not this command's guess to make\n",
				email, len(owned))
			problems++

			continue
		}

		// The durable workflow claims the account through its lifecycle row.
		// A team without one cannot be claimed, and saying so here is kinder
		// than the "claim lost" the purger would report from three layers down.
		var lifecycles int
		if err := control.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM account_lifecycle WHERE team_id = ?`, owned[0]).Scan(&lifecycles); err != nil {
			fmt.Fprintf(e.stderr, "%s: read lifecycle: %v\n", email, err)
			problems++

			continue
		}

		if lifecycles == 0 {
			fmt.Fprintf(e.stderr, "%s: team %d has no lifecycle row, which the deletion workflow claims through\n",
				email, owned[0])
			problems++

			continue
		}

		var count int
		if err := control.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sites WHERE owner_team_id = ?`, owned[0]).Scan(&count); err != nil {
			fmt.Fprintf(e.stderr, "%s: count sites: %v\n", email, err)
			problems++

			continue
		}

		targets = append(targets, doomed{user: user, teamID: owned[0], sites: count})
	}

	if problems > 0 {
		fmt.Fprintf(e.stderr, "\n%d address(es) could not be resolved; nothing was deleted\n", problems)
		return nil, ExitError
	}

	return targets, ExitOK
}

// verifiedLabel says whether somebody ever proved they own the address. It is
// on the plan because a verified account is the one worth pausing over.
func verifiedLabel(user *auth.User) string {
	if user.EmailVerifiedAt == 0 {
		return "unverified"
	}

	return "VERIFIED"
}

// accountCreate creates one verified owner and team without a trial or billing
// state. Multiple calls create multiple independent accounts; filesystem access
// to system.db, rather than an existing browser session, is the authorization.
func accountCreate(e *env, args []string) int {
	fs := newFlagSet("account create", e, accountHelp)
	email := fs.String("email", "", "email address of the account owner")
	name := fs.String("name", "", "display name of the account owner")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(e.stderr, "unexpected account create argument %q\n", fs.Arg(0))
		return ExitUsage
	}
	if e.cfg.App.Hosted {
		fmt.Fprintln(e.stderr, "account create is only available when FEASIBLE_APP_HOSTED=false; hosted accounts register through the web")
		return ExitError
	}

	normalized := auth.NormaliseEmail(*email)
	if !auth.LooksLikeEmail(normalized) {
		fmt.Fprintln(e.stderr, "account create requires a valid --email address")
		return ExitUsage
	}

	password, err := generatedAccountPassword()
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	control, err := openSystem(context.Background(), *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	user, team, err := auth.NewStore(control).CreateOperatorUser(
		context.Background(), normalized, strings.TrimSpace(*name), hash)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	fmt.Fprintf(e.stdout, "account %d created for %s (user %d)\npassword: %s\n", team.ID, user.Email, user.ID, password)
	fmt.Fprintln(e.stdout, "Save this password now; it will not be shown again. The owner can change it after signing in.")
	return ExitOK
}

// generatedAccountPassword returns a URL-safe credential with 144 bits of
// entropy. It is generated instead of accepted as a flag so passwords never
// appear in shell history or the process list.
func generatedAccountPassword() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("account create: generate password: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}
