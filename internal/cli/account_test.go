//
// account_test.go
// Tests for operator-managed self-hosted account creation.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestAccountCreateMakesVerifiedUnlimitedOwners proves the CLI can create more
// than one self-hosted account, emits usable credentials, and leaves every
// commercial lifecycle date absent.
func TestAccountCreateMakesVerifiedUnlimitedOwners(t *testing.T) {
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	dir := t.TempDir()
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("migrate: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr := run(t, "account", "create", "--data-dir", dir,
		"--email", "OWNER@example.com", "--name", "Owner")
	if code != ExitOK {
		t.Fatalf("create: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "account 1 created for owner@example.com") {
		t.Fatalf("create output = %q", stdout)
	}
	password := outputPassword(t, stdout)

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	user, err := auth.NewStore(control).UserByEmail(context.Background(), "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !user.Verified() || !auth.CheckPassword(user.PasswordHash, password) {
		t.Fatal("operator-created credential is not verified and usable")
	}

	var trial, traffic sql.NullInt64
	if err := control.QueryRow(`SELECT trial_ends_at, accept_traffic_until FROM teams WHERE id = 1`).Scan(&trial, &traffic); err != nil {
		t.Fatal(err)
	}
	if trial.Valid || traffic.Valid {
		t.Fatalf("self-hosted account has lifecycle dates: trial=%v traffic=%v", trial, traffic)
	}

	if code, stdout, stderr = run(t, "account", "create", "--data-dir", dir,
		"--email", "second@example.com"); code != ExitOK || !strings.Contains(stdout, "account 2 created") {
		t.Fatalf("second account: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// TestAccountCreateRejectsHostedMode keeps the operator command from becoming
// an unverified back door around the hosted service's registration flow.
func TestAccountCreateRejectsHostedMode(t *testing.T) {
	t.Setenv("FEASIBLE_APP_HOSTED", "true")

	code, stdout, stderr := run(t, "account", "create", "--email", "owner@example.com")
	if code != ExitError || stdout != "" || !strings.Contains(stderr, "FEASIBLE_APP_HOSTED=false") {
		t.Fatalf("hosted create: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// outputPassword extracts the one-time password line from command output.
func outputPassword(t *testing.T, output string) string {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		if password, ok := strings.CutPrefix(line, "password: "); ok && password != "" {
			return password
		}
	}

	t.Fatalf("no password in output %q", output)
	return ""
}
