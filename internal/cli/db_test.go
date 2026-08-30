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

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// seedDataDir builds a data directory holding a control database and one
// account database, in the layout every db command walks: control.db at the
// top, and one directory per account under accounts/.
func seedDataDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, path := range []string{
		filepath.Join(dir, "control.db"),
		accounts.Path(dir, 1),
	} {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}

		db.Close()
	}

	return dir
}

// schemaVersion reads a database's migration level straight off the file, which
// is how an operator checks a migration actually landed.
func schemaVersion(t *testing.T, path string) int {
	t.Helper()

	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	version, err := store.SchemaVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}

	return version
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

// TestMigrateWalksEveryDatabase is the whole command: control.db and every
// account database brought up to date in one run. An account that is missed is
// an account whose events stop being accepted after a deploy.
func TestMigrateWalksEveryDatabase(t *testing.T) {
	dir := seedDataDir(t)
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)

	code, stdout, stderr := run(t, "db", "migrate")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "control.db") || !strings.Contains(stdout, "000001") {
		t.Fatalf("not every database was visited: %q", stdout)
	}

	for _, path := range []string{filepath.Join(dir, "control.db"), accounts.Path(dir, 1)} {
		if version := schemaVersion(t, path); version < 1 {
			t.Fatalf("%s is still at version %d", path, version)
		}
	}

	// The schemas are real, not just a version stamp.
	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var count int
	if err := control.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatalf("the control schema was not applied: %v", err)
	}
}

// TestMigrateIsIdempotent is what makes an interrupted run recoverable: the fix
// is always to run it again, so a second pass has to change nothing and say so.
func TestMigrateIsIdempotent(t *testing.T) {
	dir := seedDataDir(t)
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("first run: exit code %d, stderr: %s", code, stderr)
	}

	code, stdout, stderr := run(t, "db", "migrate")

	if code != ExitOK {
		t.Fatalf("second run: exit code %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "database migrated") {
		t.Fatalf("the second run applied migrations: %q", stdout)
	}
	if strings.Count(stdout, "database already current") != 2 {
		t.Fatalf("the second run was not clear about doing nothing: %q", stdout)
	}
}

// TestMigrateCreatesTheControlDatabase covers a fresh install, where nothing
// exists yet. This is the first command a new install runs, so it has to create
// control.db rather than report that it is missing.
func TestMigrateCreatesTheControlDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)

	code, stdout, stderr := run(t, "db", "migrate")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "database migrated") {
		t.Fatalf("nothing was migrated: %q", stdout)
	}

	if version := schemaVersion(t, filepath.Join(dir, "control.db")); version < 1 {
		t.Fatalf("control.db was left at version %d", version)
	}
}

// TestMigrateFreshRebuilds covers the destructive development flag. It has to
// leave a database that is migrated and empty, not one that needs deleting by
// hand.
func TestMigrateFreshRebuilds(t *testing.T) {
	dir := seedDataDir(t)
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.ExecContext(context.Background(),
		"INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	control.Close()

	code, stdout, stderr := run(t, "db", "migrate", "--fresh")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "database emptied") {
		t.Fatalf("a destructive run said nothing about it: %q", stdout)
	}

	rebuilt, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()

	var count int
	if err := rebuilt.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM teams").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("--fresh left %d rows behind", count)
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

	// Every account database is called analytics.db, so the account id has to
	// be in the snapshot name or a directory of backups is a directory of
	// collisions.
	var named bool
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "account-000001-") {
			named = true
		}
	}
	if !named {
		t.Fatalf("an account snapshot is not named after its account: %v", entries)
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
