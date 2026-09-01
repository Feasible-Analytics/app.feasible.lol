//
// cli.go
// Root command: global flags, configuration, logging and subcommand dispatch.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package cli implements the `feasible` command. It lives outside package main
// so the whole command surface — flag parsing, config loading, exit codes — can
// be driven from tests with ordinary function calls and captured output.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/build"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// Exit codes. Two is the conventional "you typed it wrong" code and is kept
// distinct from one so a supervisor can tell a bad invocation from a crash.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// usage is the help text. It is a constant rather than generated from the flag
// package because the subcommands carry the meaning here, and flag's own output
// cannot express "db migrate".
const usage = `feasible — privacy-first web analytics in one binary.

Usage:
  feasible [flags] <command> [command flags]

Commands:
  serve        Run the whole product in one process. The default, and the only
               thing a self-hoster ever runs.
  ingest       Run the event endpoint separately over the shared databases.
  db migrate   Migrate control.db and every account database. Never automatic.
  db backup    Write a consistent snapshot of every database.
  litestream   Generate and check the continuous replication configuration,
               which has to be regenerated whenever an account is created.
  rollup       Build, rebuild or inspect the pre-aggregated report tables.
  seed         Generate realistic fake traffic to build and measure against.
  api-key      Create, list and revoke public API keys. One key type, and it
               works for the Stats API, the Sites API, webhooks and MCP.
  mcp          Serve the Model Context Protocol over stdin and stdout, for a
               desktop assistant. The remote transport is POST /mcp on serve.
  billing      Inspect and drive the account lifecycle: status, trial, sweep.

Flags:
  --version         Print version, commit and build date, then exit.
  --trace-events    Print the fully derived event for every request.
  -h, --help        Show this help.

Configuration is read from $CONFIG_DIR/<NAME> first, then the environment, then
.env outside production. See .env.sample for every variable.
`

// env is everything a subcommand needs: the resolved configuration, a logger
// and the streams to write to. Passing one struct keeps subcommand signatures
// stable as the foundation grows.
type env struct {
	cfg    *config.Config
	log    *logger.Logger
	stdout io.Writer
	stderr io.Writer
}

// Options are the inputs to Run. Tests supply their own streams and arguments;
// main supplies the real ones. Making these explicit is what keeps the command
// testable without a subprocess.
type Options struct {
	Args   []string
	Stdout io.Writer
	Stderr io.Writer
}

// Main is the entry point package main calls. It exists so main.go stays three
// lines and every decision — including the exit code — is testable.
func Main() int {
	return Run(Options{Args: os.Args[1:], Stdout: os.Stdout, Stderr: os.Stderr})
}

// Run parses the root command and dispatches. It returns an exit code rather
// than calling os.Exit so tests can assert on it and deferred cleanups still
// run.
func Run(opts Options) int {
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	root := flag.NewFlagSet("feasible", flag.ContinueOnError)
	root.SetOutput(stderr)
	root.Usage = func() { fmt.Fprint(stderr, usage) }

	showVersion := root.Bool("version", false, "print version, commit and build date")
	traceEvents := root.Bool("trace-events", false, "print the fully derived event for every request")

	if err := root.Parse(opts.Args); err != nil {
		// -h and --help arrive here as ErrHelp after the usage text has already
		// been printed, and asking for help is not an error.
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}

	// --version is answered before the configuration is touched. "What version
	// is this?" has to work on a box whose configuration is broken, since that
	// is exactly when someone asks.
	if *showVersion {
		fmt.Fprintln(stdout, build.String())
		return ExitOK
	}

	args := root.Args()
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}

	if args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return ExitOK
	}

	if args[0] == "version" {
		fmt.Fprintln(stdout, build.String())
		return ExitOK
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "configuration error: %v\n", err)
		return ExitError
	}

	// The flag is an override, never a downgrade: a deployment that turned
	// tracing on through the environment keeps it on.
	if *traceEvents {
		cfg.Shared.TraceEvents = true
	}

	e := &env{
		cfg: cfg,
		log: logger.New(logger.Options{
			Level:       cfg.Shared.LogLevel,
			Format:      cfg.Shared.LogFormat,
			TraceEvents: cfg.Shared.TraceEvents,
			Output:      stdout,
		}),
		stdout: stdout,
		stderr: stderr,
	}

	switch args[0] {
	case "serve":
		return runServe(e, args[1:])
	case "ingest":
		return runIngest(e, args[1:])
	case "db":
		return runDB(e, args[1:])
	case "litestream":
		return runLitestream(e, args[1:])
	case "seed":
		return runSeed(e, args[1:])
	case "mcp":
		return runMCP(e, args[1:])
	case "api-key":
		return runAPIKeys(e, args[1:])
	case "rollup":
		return runRollup(e, args[1:])
	case "billing":
		return runBilling(e, args[1:])
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
}

// newFlagSet builds a subcommand flag set that reports errors the same way the
// root does. Every subcommand needs the identical four lines, and a subcommand
// that printed its errors somewhere else would be a small, permanent annoyance.
func newFlagSet(name string, e *env, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprint(e.stderr, help)
		fs.PrintDefaults()
	}

	return fs
}

// parseFlags runs a subcommand flag set and maps its outcome onto an exit code,
// so each subcommand does not repeat the ErrHelp special case.
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		return ExitUsage, false
	}

	return ExitOK, true
}
