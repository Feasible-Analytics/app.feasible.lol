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
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
)

const accountHelp = `feasible account — manage self-hosted accounts.

Usage:
  feasible account create --email owner@example.com [--name "Owner name"]

The create command is available only when FEASIBLE_APP_HOSTED=false. It creates
a verified owner and prints a generated password exactly once. Run ` + "`feasible db migrate`" + `
before creating the first account.

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
	default:
		fmt.Fprintf(e.stderr, "unknown account command %q\n\n", args[0])
		fmt.Fprint(e.stderr, accountHelp)
		return ExitUsage
	}
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
