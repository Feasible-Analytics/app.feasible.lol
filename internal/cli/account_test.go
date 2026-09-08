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
	"time"

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

// seedAccounts creates n self-hosted accounts and returns the data directory
// they live in, so the delete tests have something real to remove.
func seedAccounts(t *testing.T, emails ...string) string {
	t.Helper()

	dir := t.TempDir()

	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_APP_MAIL_TRANSPORT", "log")

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("migrate: code=%d stderr=%q", code, stderr)
	}

	for _, email := range emails {
		if code, _, stderr := run(t, "account", "create", "--data-dir", dir, "--email", email); code != ExitOK {
			t.Fatalf("create %s: code=%d stderr=%q", email, code, stderr)
		}
	}

	startTrialClocks(t, dir)

	return dir
}

// startTrialClocks gives every team the lifecycle row a hosted sign-up writes.
// The operator create command makes no trial, and the deletion workflow claims
// an account through that row.
func startTrialClocks(t *testing.T, dir string) {
	t.Helper()

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer control.Close() //nolint:errcheck // the test fails on the assertion, not the close

	now := time.Now().UTC().Unix()
	if _, err := control.Exec(`
		INSERT INTO account_lifecycle (team_id, trigger, started_at, created_at, updated_at)
		SELECT id, 'trial', ?, ?, ? FROM teams
		WHERE id NOT IN (SELECT team_id FROM account_lifecycle)
	`, now, now, now); err != nil {
		t.Fatal(err)
	}
}

// accountExists reports whether an address still has a row.
func accountExists(t *testing.T, dir, email string) bool {
	t.Helper()

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer control.Close() //nolint:errcheck // the test fails on the assertion, not the close

	_, err = auth.NewStore(control).UserByEmail(context.Background(), email)

	return err == nil
}

// One mistyped address is otherwise an account nobody can get back, so the
// command reports and stops until it is told twice.
func TestAccountDeleteWithoutYesRemovesNothing(t *testing.T) {
	dir := seedAccounts(t, "keep@example.com")

	code, stdout, stderr := run(t, "account", "delete", "--data-dir", dir, "--email", "keep@example.com")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	if !strings.Contains(stdout, "keep@example.com") || !strings.Contains(stdout, "Nothing was deleted") {
		t.Fatalf("the plan should name the account and say nothing happened, got %q", stdout)
	}

	if !accountExists(t, dir, "keep@example.com") {
		t.Fatal("the account was deleted without --yes")
	}
}

func TestAccountDeleteRemovesTheOwnerAndTheTeam(t *testing.T) {
	dir := seedAccounts(t, "spam@example.com", "keep@example.com")

	code, stdout, stderr := run(t, "account", "delete", "--data-dir", dir,
		"--email", "spam@example.com", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	if !strings.Contains(stdout, "deleted spam@example.com") || !strings.Contains(stdout, "1 account(s) deleted") {
		t.Fatalf("delete output = %q", stdout)
	}

	if accountExists(t, dir, "spam@example.com") {
		t.Error("the account is still there")
	}

	// Everything hanging off the user and the team goes with it, and nothing
	// belonging to anybody else does.
	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer control.Close() //nolint:errcheck // the test fails on the assertion, not the close

	for _, query := range []string{
		`SELECT COUNT(*) FROM teams WHERE id = 1`,
		`SELECT COUNT(*) FROM team_memberships WHERE team_id = 1`,
	} {
		var count int
		if err := control.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}

		if count != 0 {
			t.Errorf("%s left %d row(s)", query, count)
		}
	}

	if !accountExists(t, dir, "keep@example.com") {
		t.Error("deleting one account took another with it")
	}
}

// A batch is all-or-nothing: an operator clearing a spam run wants the whole
// list checked before anything is destroyed.
func TestAccountDeleteRefusesTheWholeBatchWhenOneAddressIsUnknown(t *testing.T) {
	dir := seedAccounts(t, "real@example.com")

	code, _, stderr := run(t, "account", "delete", "--data-dir", dir,
		"--email", "real@example.com", "--email", "ghost@example.com", "--yes")
	if code != ExitError {
		t.Fatalf("code=%d, wanted a refusal; stderr=%q", code, stderr)
	}

	if !strings.Contains(stderr, "ghost@example.com") || !strings.Contains(stderr, "nothing was deleted") {
		t.Fatalf("stderr should name the bad address and say nothing happened, got %q", stderr)
	}

	if !accountExists(t, dir, "real@example.com") {
		t.Fatal("a refused batch still deleted an account")
	}
}

func TestAccountDeleteClearsAWholeBatch(t *testing.T) {
	dir := seedAccounts(t, "one@example.com", "two@example.com", "keep@example.com")

	code, stdout, stderr := run(t, "account", "delete", "--data-dir", dir,
		"--email", "one@example.com", "--email", "two@example.com", "--yes")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	if !strings.Contains(stdout, "2 account(s) deleted") {
		t.Fatalf("delete output = %q", stdout)
	}

	for _, email := range []string{"one@example.com", "two@example.com"} {
		if accountExists(t, dir, email) {
			t.Errorf("%s is still there", email)
		}
	}

	if !accountExists(t, dir, "keep@example.com") {
		t.Error("the batch took an account it was not given")
	}
}

// The plan says which accounts somebody actually proved they owned, because a
// verified account is the one worth pausing over.
func TestAccountDeletePlanMarksAVerifiedAccount(t *testing.T) {
	dir := seedAccounts(t, "owner@example.com")

	_, stdout, _ := run(t, "account", "delete", "--data-dir", dir, "--email", "owner@example.com")
	if !strings.Contains(stdout, "VERIFIED") {
		t.Fatalf("an operator-created account is verified and the plan should say so, got %q", stdout)
	}
}

func TestAccountDeleteNeedsAnAddress(t *testing.T) {
	dir := seedAccounts(t)

	code, _, stderr := run(t, "account", "delete", "--data-dir", dir, "--yes")
	if code != ExitUsage {
		t.Fatalf("code=%d, wanted a usage error; stderr=%q", code, stderr)
	}

	if !strings.Contains(stderr, "requires at least one --email") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// An operator-created account has no trial clock, so the deletion workflow has
// nothing to claim. The command says that rather than reporting a lost claim.
func TestAccountDeleteExplainsAMissingLifecycleRow(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_APP_MAIL_TRANSPORT", "log")

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("migrate: code=%d stderr=%q", code, stderr)
	}

	if code, _, stderr := run(t, "account", "create", "--data-dir", dir, "--email", "operator@example.com"); code != ExitOK {
		t.Fatalf("create: code=%d stderr=%q", code, stderr)
	}

	code, _, stderr := run(t, "account", "delete", "--data-dir", dir, "--email", "operator@example.com", "--yes")
	if code != ExitError {
		t.Fatalf("code=%d, wanted a refusal; stderr=%q", code, stderr)
	}

	if !strings.Contains(stderr, "no lifecycle row") {
		t.Fatalf("stderr should explain why, got %q", stderr)
	}

	if !accountExists(t, dir, "operator@example.com") {
		t.Fatal("a refused deletion still removed the account")
	}
}
