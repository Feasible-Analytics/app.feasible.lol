//
// mcp.go
// `feasible mcp` — the local MCP server, and `feasible api-key` to get into it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mcp"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"

	// Aliased for the same reason it is in commerce.go: this package already has
	// a package-level `usage`, which is the help text every subcommand prints.
	volume "github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

const mcpHelp = `feasible mcp — serve the Model Context Protocol over stdin and stdout.

This is the local transport: a desktop assistant launches the binary and talks
to it over a pipe. The remote transport is POST /mcp on the running app and
needs no command of its own.

The API key comes from FEASIBLE_MCP_API_KEY, or from --api-key. Prefer the
environment variable: a secret on the command line is visible in the process
list to every user on the machine.

Flags:
`

// runMCP serves one stdio session.
//
// Nothing but protocol messages may ever reach stdout on this path, which is
// why the logger is pointed at stderr before anything else happens: one stray
// log line on stdout is indistinguishable from a malformed message and takes
// the session down with no explanation the person can see.
func runMCP(e *env, args []string) int {
	fs := newFlagSet("mcp", e, mcpHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")
	apiKey := fs.String("api-key", e.cfg.API.MCPKey, "the API key this session runs as")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if strings.TrimSpace(*apiKey) == "" {
		fmt.Fprintf(e.stderr, "no API key: set FEASIBLE_MCP_API_KEY or pass --api-key\n")
		return ExitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	control, err := openControl(ctx, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	manager := accounts.NewManager(*dataDir)
	defer manager.CloseAll() //nolint:errcheck // the process is exiting either way

	cache := sites.New(control)
	if err := cache.Refresh(ctx); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	keys := apikeys.NewStore(control)

	key, err := keys.Authenticate(ctx, strings.TrimSpace(*apiKey))
	if err != nil {
		fmt.Fprintf(e.stderr, "that API key is not valid\n")
		return ExitError
	}

	// The same lock the remote transport honours. A stdio session is the same
	// key against the same account, so leaving it ungated would turn "run the
	// local server instead" into the way around a locked dashboard. On an
	// install with no billing nothing is ever in the locked set, so this costs
	// a self-hoster one query every fifteen seconds and refuses nothing.
	gate := access.New(lifecycle.NewStore(control), volume.NewStore(control), cache, e.log)

	public := buildPublic(e, control, cache, manager, gate)

	// The routing snapshot is refreshed in the background, because a session can
	// stay open for hours and a site added in the dashboard should show up in
	// list_sites without the assistant being restarted.
	go cache.Run(ctx, func(err error) { fmt.Fprintf(e.stderr, "site refresh: %v\n", err) })
	go gate.Run(ctx)

	if err := mcp.ServeStdio(ctx, public.MCP, mcp.StdioOptions{
		In:       os.Stdin,
		Out:      e.stdout,
		Key:      key,
		Validate: keys.Validate,
	}); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	return ExitOK
}

const apiKeyHelp = `feasible api-key — create, list and revoke public API keys.

One key type does everything: the Stats API, the Sites API, webhooks and the MCP
server. It is included in every plan and every build.

Usage:
  feasible api-key create --team <id> --user <id> [--name <name>] [--limit <n>]
  feasible api-key list --team <id>
  feasible api-key revoke --team <id> --id <key id>

Flags:
`

// runAPIKeys dispatches the key subcommands.
func runAPIKeys(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, apiKeyHelp)
		return ExitUsage
	}

	action := args[0]

	fs := newFlagSet("api-key "+action, e, apiKeyHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db")
	team := fs.Int64("team", 0, "the team the key belongs to")
	user := fs.Int64("user", 0, "the user the key is issued to")
	id := fs.Int64("id", 0, "the key to revoke")
	name := fs.String("name", "", "a label so somebody can tell their keys apart")
	limit := fs.Int("limit", 0, "requests per hour, or 0 to use the deployment's configured limit")

	if code, ok := parseFlags(fs, args[1:]); !ok {
		return code
	}

	if *team < 1 {
		fmt.Fprintf(e.stderr, "--team is required\n")
		return ExitUsage
	}

	ctx := context.Background()

	control, err := openControl(ctx, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	store := apikeys.NewStore(control)

	switch action {
	case "create":
		if *user < 1 {
			fmt.Fprintf(e.stderr, "--user is required\n")
			return ExitUsage
		}

		key, plaintext, err := store.Create(ctx, *team, *user, *name, nil, *limit)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		// The plaintext is printed once and never again: only its hash is
		// stored, so there is no "show me that key" to come back to.
		fmt.Fprintf(e.stdout, "key %d created. This is the only time it is shown:\n\n  %s\n\n", key.ID, plaintext)

		return ExitOK

	case "list":
		keys, err := store.List(ctx, *team)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		out := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(out, "ID\tPREFIX\tNAME\tLIMIT/HOUR\tLAST USED\tREVOKED")

		for _, key := range keys {
			fmt.Fprintf(out, "%d\t%s\t%s\t%s\t%s\t%s\n",
				key.ID, key.Prefix, key.Name, limitLabel(key.HourlyLimit),
				stamp(key.LastUsedAt), stamp(key.RevokedAt))
		}

		return flushOrFail(e, out)

	case "revoke":
		if *id < 1 {
			fmt.Fprintf(e.stderr, "--id is required\n")
			return ExitUsage
		}

		if err := store.Revoke(ctx, *team, *id); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		fmt.Fprintf(e.stdout, "key %d revoked\n", *id)

		return ExitOK

	default:
		fmt.Fprintf(e.stderr, "unknown api-key command %q\n\n", action)
		fmt.Fprint(e.stderr, apiKeyHelp)

		return ExitUsage
	}
}

// limitLabel renders a key's ceiling, saying which number a zero means rather
// than printing a zero that reads like "no requests allowed".
func limitLabel(limit int) string {
	if limit == 0 {
		return "default"
	}

	return fmt.Sprint(limit)
}

// stamp renders an optional timestamp.
func stamp(at time.Time) string {
	if at.IsZero() {
		return "-"
	}

	return at.Format("2006-01-02 15:04")
}

// flushOrFail writes out a table and maps a write failure onto an exit code.
func flushOrFail(e *env, out *tabwriter.Writer) int {
	if err := out.Flush(); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	return ExitOK
}
