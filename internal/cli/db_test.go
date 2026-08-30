//
// db_test.go
// Tests for the `db migrate` and `db backup` subcommands.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// seedDataDir builds a data directory holding a control database and one
// account database, which is the shape every db command walks.
func seedDataDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, path := range []string{
		filepath.Join(dir, "control.db"),
		filepath.Join(dir, "accounts", "acct-000001.db"),
	} {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := db.ExecContext(context.Background(), "CREATE TABLE t (id INTEGER)"); err != nil {
			t.Fatal(err)
		}

		db.Close()
	}

	return dir
}

// TestDBWithoutSubcommand checks `feasible db` lists what it offers rather than
// dumping the whole program's help, which is the difference between finding
// `db backup` and giving up.
func TestDBWithoutSubcommand(t *testing.T) {
	code, _, stderr := run(t, "db")

	if code != ExitUsage {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "migrate") || !strings.Contains(stderr, "backup") {
		t.Fatalf("db help is missing a subcommand: %q", stderr)
	}
}

// TestDBUnknownSubcommand makes sure a typo is refused rather than treated as a
// no-op success, which on a migration command would be dangerous.
func TestDBUnknownSubcommand(t *testing.T) {
	code, _, stderr := run(t, "db", "vacuum")

	if code != ExitUsage {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, `unknown db command "vacuum"`) {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}

// TestMigrateWalksEveryDatabase checks the walk covers control.db and the
// account databases, and reports a schema version for each. With one database
// per account, a migration is only resumable if you can see where it stopped.
func TestMigrateWalksEveryDatabase(t *testing.T) {
	t.Setenv("FEASIBLE_APP_DATA_DIR", seedDataDir(t))

	code, stdout, stderr := run(t, "db", "migrate")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "control.db") || !strings.Contains(stdout, "acct-000001.db") {
		t.Fatalf("not every database was visited: %q", stdout)
	}
	if strings.Count(stdout, "schema_version=0") != 2 {
		t.Fatalf("expected a schema version per database: %q", stdout)
	}
}

// TestMigrateEmptyDataDir covers a first run, where nothing exists yet. That has
// to succeed quietly rather than look like a failure.
func TestMigrateEmptyDataDir(t *testing.T) {
	t.Setenv("FEASIBLE_APP_DATA_DIR", t.TempDir())

	code, stdout, _ := run(t, "db", "migrate")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "no databases found yet") {
		t.Fatalf("silent on an empty data directory: %q", stdout)
	}
}

// TestMigrateFreshRefusedInProduction is the guard on the destructive flag.
// `--fresh` drops customer data and there is no reason it should ever be
// reachable on a production box.
func TestMigrateFreshRefusedInProduction(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", "production")
	t.Setenv("FEASIBLE_APP_DATA_DIR", seedDataDir(t))

	code, _, stderr := run(t, "db", "migrate", "--fresh")

	if code != ExitError {
		t.Fatalf("exit code %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "refusing") {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}

// TestBackupWritesASnapshotPerDatabase exercises the real VACUUM INTO path
// through the command, including the dated filenames a retention job relies on.
func TestBackupWritesASnapshotPerDatabase(t *testing.T) {
	dataDir := seedDataDir(t)
	out := filepath.Join(t.TempDir(), "snapshots")

	t.Setenv("FEASIBLE_APP_DATA_DIR", dataDir)

	code, stdout, stderr := run(t, "db", "backup", "-out", out)

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected one snapshot per database, got %d", len(entries))
	}
	if !strings.Contains(stdout, "database backed up") {
		t.Fatalf("backups were not reported: %q", stdout)
	}
}

// TestBackupWithNothingToDoFails is the "never fail silently" rule applied to
// backups: a command that exits zero having backed nothing up is how people
// find out they have no backups at the worst possible moment.
func TestBackupWithNothingToDoFails(t *testing.T) {
	t.Setenv("FEASIBLE_APP_DATA_DIR", t.TempDir())

	code, _, stderr := run(t, "db", "backup")

	if code != ExitError {
		t.Fatalf("exit code %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "nothing was backed up") {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}
