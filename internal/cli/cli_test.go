//
// cli_test.go
// Tests for the root command: global flags, exit codes and dispatch.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
)

// applyControlMigrations builds the complete merged control schema for command
// tests that seed records directly before exercising a process boundary.
func applyControlMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := migrate.Run(context.Background(), db, migrate.Control()); err != nil {
		t.Fatal(err)
	}
}

// run drives the command the way a shell would and hands back both streams. It
// exists so no test has to build a binary or care which stream a message went
// to until it wants to.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := Run(Options{
		Args:              args,
		Stdout:            &stdout,
		Stderr:            &stderr,
		ControlMigrations: migrate.Control(),
	})

	return code, stdout.String(), stderr.String()
}

// setProductionOperator supplies the legal identity required for production
// command tests whose subject is a different fail-closed guard.
func setProductionOperator(t *testing.T) {
	t.Helper()
	t.Setenv("FEASIBLE_OPERATOR_NAME", "Example Operator, Inc.")
	t.Setenv("FEASIBLE_OPERATOR_ADDRESS", "123 Example Street")
	t.Setenv("FEASIBLE_OPERATOR_EMAIL", "privacy@example.test")
}

// TestVersionFlag checks --version prints the stamped build line on stdout.
// Release tooling and support both read this, so the stream and the exit code
// matter as much as the text.
func TestVersionFlag(t *testing.T) {
	code, stdout, stderr := run(t, "--version")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, "feasible ") || !strings.Contains(stdout, "commit") {
		t.Fatalf("unexpected version line: %q", stdout)
	}
}

// TestVersionSubcommand covers `feasible version`, which is what everyone types
// first regardless of what the help says.
func TestVersionSubcommand(t *testing.T) {
	code, stdout, _ := run(t, "version")

	if code != ExitOK || !strings.HasPrefix(stdout, "feasible ") {
		t.Fatalf("code %d, stdout %q", code, stdout)
	}
}

// TestHelp checks that asking for help succeeds and lists every subcommand.
// A command missing from the help is a command nobody finds.
func TestHelp(t *testing.T) {
	code, stdout, _ := run(t, "help")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}

	for _, want := range []string{"serve", "ingest", "db migrate", "db backup", "seed", "comp", "--trace-events"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help does not mention %q", want)
		}
	}
}

// TestNoArgumentsIsUsageError pins the exit code for a bare invocation. Two
// means "you typed it wrong" and has to stay distinct from one, so a supervisor
// can tell a bad command line from a crash.
func TestNoArgumentsIsUsageError(t *testing.T) {
	code, _, stderr := run(t)

	if code != ExitUsage {
		t.Fatalf("exit code %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("no usage on stderr: %q", stderr)
	}
}

// TestUnknownCommand checks a typo is rejected rather than ignored, and that the
// message names what was not understood.
func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "migrate")

	if code != ExitUsage {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, `unknown command "migrate"`) {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}

// TestUnknownFlag makes sure an unrecognised global flag fails rather than being
// silently dropped, which is how a mistyped --trace-events would look like the
// feature not working.
func TestUnknownFlag(t *testing.T) {
	code, _, _ := run(t, "--trace-event", "serve")

	if code != ExitUsage {
		t.Fatalf("exit code %d, want %d", code, ExitUsage)
	}
}
