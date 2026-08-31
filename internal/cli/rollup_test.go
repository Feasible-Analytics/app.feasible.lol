//
// rollup_test.go
// Tests for the `rollup build`, `rollup rebuild` and `rollup status` subcommands.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// migratedDataDir builds a data directory whose control database is at the
// current schema, which is what every roll-up command needs before it can list
// a single site.
func migratedDataDir(t *testing.T) string {
	t.Helper()

	dir := seedDataDir(t)

	db, err := store.Open(dir + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := migrate.Run(context.Background(), db, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	return dir
}

// TestRollupWithoutSubcommand checks `feasible rollup` lists what it offers.
// Printing the whole program's help here is the difference between finding
// `rollup status` and giving up.
func TestRollupWithoutSubcommand(t *testing.T) {
	code, _, stderr := run(t, "rollup")

	if code != ExitUsage {
		t.Errorf("exit code %d, want %d", code, ExitUsage)
	}

	if !strings.Contains(stderr, "rebuild") || !strings.Contains(stderr, "status") {
		t.Errorf("the help does not list the subcommands:\n%s", stderr)
	}
}

// TestRollupUnknownSubcommand checks a typo is named rather than swallowed.
func TestRollupUnknownSubcommand(t *testing.T) {
	code, _, stderr := run(t, "rollup", "buidl")

	if code != ExitUsage {
		t.Errorf("exit code %d, want %d", code, ExitUsage)
	}

	if !strings.Contains(stderr, "buidl") {
		t.Errorf("the error does not name what was typed:\n%s", stderr)
	}
}

// TestRollupBuildOnAnEmptyInstall checks the command is a no-op rather than a
// failure when there is nothing to summarise. A fresh install runs it from cron
// before it has a single site.
func TestRollupBuildOnAnEmptyInstall(t *testing.T) {
	dir := migratedDataDir(t)

	code, stdout, stderr := run(t, "rollup", "build", "--data-dir", dir)

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "built in") {
		t.Errorf("the command said nothing about what it did:\n%s", stdout)
	}
}

// TestRollupStatusListsWhatItIsKeyedBy checks the status output carries the
// dimension list. "Why is this report still slow" is nearly always "that
// dimension is not summarised", and the answer belongs in the same output as
// the coverage window.
func TestRollupStatusListsWhatItIsKeyedBy(t *testing.T) {
	dir := migratedDataDir(t)

	code, stdout, stderr := run(t, "rollup", "status", "--data-dir", dir)

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "visit:source") || !strings.Contains(stdout, "event:page") {
		t.Errorf("the status output does not say what the summary is keyed by:\n%s", stdout)
	}
}

// TestRollupRefusesADataDirectoryItCannotRead checks the failure path. A
// control database that has not been migrated is the most common mistake on a
// fresh box, and the message has to say so rather than crash.
func TestRollupRefusesADataDirectoryItCannotRead(t *testing.T) {
	dir := seedDataDir(t)

	code, _, stderr := run(t, "rollup", "build", "--data-dir", dir)

	if code != ExitError {
		t.Errorf("exit code %d, want %d", code, ExitError)
	}

	if !strings.Contains(stderr, "db migrate") {
		t.Errorf("the error does not say what to run:\n%s", stderr)
	}
}
