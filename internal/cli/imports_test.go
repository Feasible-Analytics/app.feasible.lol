//
// imports_test.go
// Tests for the `imports summarise` and `imports rename` subcommands.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestImportsWithoutSubcommandListsRename checks `feasible imports` names both
// subcommands.
func TestImportsWithoutSubcommandListsRename(t *testing.T) {
	code, _, stderr := run(t, "imports")

	if code != ExitUsage {
		t.Errorf("exit code %d, want %d", code, ExitUsage)
	}

	if !strings.Contains(stderr, "summarise") || !strings.Contains(stderr, "rename") {
		t.Errorf("the help does not list the subcommands:\n%s", stderr)
	}
}

// seedStoredImport gives account 1 a site and a completed import whose one row
// still holds an operating system spelled the way an export spells it.
func seedStoredImport(t *testing.T, dir string) {
	t.Helper()

	ctx := context.Background()

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("migrate: exit code %d, stderr: %s", code, stderr)
	}

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	for _, statement := range []string{
		"INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Acme', 0, 0)",
		"INSERT INTO sites (id, account_id, domain, timezone, created_at, updated_at) VALUES (5, 1, 'acme.example', 'UTC', 0, 0)",
	} {
		if _, err := control.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	manager := accounts.NewManager(dir)
	defer func() { _ = manager.CloseAll() }()

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

	record, err := dataio.CreateImport(ctx, account.Writer(), 5, dataio.SourcePlausible, "export.zip", now)
	if err != nil {
		t.Fatal(err)
	}

	os, err := account.Intern.ID(ctx, intern.OS, "Mac")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO imported_rollups (import_id, site_id, timestamp, os_id, visitors) VALUES (?, 5, ?, ?, 3)",
		record.ID, now.AddDate(0, -1, 0).Unix(), os); err != nil {
		t.Fatal(err)
	}

	if err := dataio.CompleteImport(ctx, account.Writer(), record.ID, []string{"visit:os"}, 0, 0, 1, now); err != nil {
		t.Fatal(err)
	}
}

// storedSystem reads the operating system on account 1's one imported row.
func storedSystem(t *testing.T, dir string) string {
	t.Helper()

	db, err := store.Open(accounts.Path(dir, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var value string
	if err := db.QueryRow("SELECT d.value FROM imported_rollups r JOIN dim_os d ON d.id = r.os_id").Scan(&value); err != nil {
		t.Fatal(err)
	}

	return value
}

// TestImportsRename checks a dry run reports the rename and writes nothing,
// and a real run writes it and says how much it changed.
func TestImportsRename(t *testing.T) {
	dir := seedDataDir(t)
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	seedStoredImport(t, dir)

	code, stdout, stderr := run(t, "imports", "rename", "--dry-run")
	if code != ExitOK {
		t.Fatalf("dry run: exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"Mac" -> "macOS"`) || !strings.Contains(stdout, "nothing was written") {
		t.Errorf("the dry run did not report the rename:\n%s", stdout)
	}
	if got := storedSystem(t, dir); got != "Mac" {
		t.Fatalf("a dry run wrote %q", got)
	}

	code, stdout, stderr = run(t, "imports", "rename")
	if code != ExitOK {
		t.Fatalf("rename: exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "1 of 1 completed import(s) renamed, 1 row(s)") {
		t.Errorf("the run did not report what it changed:\n%s", stdout)
	}
	if got := storedSystem(t, dir); got != "macOS" {
		t.Fatalf("after renaming the system is %q, want macOS", got)
	}

	code, stdout, _ = run(t, "imports", "rename")
	if code != ExitOK || !strings.Contains(stdout, "0 of 1 completed import(s) renamed") {
		t.Errorf("a second run changed something: exit %d\n%s", code, stdout)
	}
}
