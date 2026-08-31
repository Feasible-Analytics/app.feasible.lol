//
// billing_test.go
// The support commands, driven the way a person on a box would drive them.
//
// Created: 2026-08-30
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

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// billingDataDir builds a migrated control database with one team and an owner,
// which is the smallest install any of these commands can act on.
func billingDataDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	if _, err := migrate.Run(context.Background(), control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Unix()

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, name, created_at, updated_at) VALUES (1, 'owner@example.com', 'Owner', ?, ?)`, []any{now, now}},
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)`, []any{now, now}},
		{`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (1, 1, 'owner', ?)`, []any{now}},
	} {
		if _, err := control.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// TestBillingWithoutSubcommand checks the command lists what it offers rather
// than dumping the whole program's help, which is the difference between
// finding `billing status` and giving up.
func TestBillingWithoutSubcommand(t *testing.T) {
	code, _, stderr := run(t, "billing")

	if code != ExitUsage {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "status") || !strings.Contains(stderr, "sweep") {
		t.Fatalf("the help does not list the subcommands: %q", stderr)
	}
}

// TestBillingUnknownSubcommand makes sure a typo says so rather than doing
// something adjacent.
func TestBillingUnknownSubcommand(t *testing.T) {
	code, _, stderr := run(t, "billing", "delete-everything")

	if code != ExitUsage {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "unknown billing command") {
		t.Fatalf("stderr is %q", stderr)
	}
}

// TestBillingTrialAndStatus is the path an operator actually walks: put an
// account on the clock, then ask what is going to happen to it.
func TestBillingTrialAndStatus(t *testing.T) {
	dir := billingDataDir(t)

	code, stdout, stderr := run(t, "billing", "-data-dir", dir, "trial", "1")
	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "trial started") {
		t.Fatalf("stdout is %q", stdout)
	}

	// Enrolling twice must not move day 0, and must say so rather than looking
	// like it worked.
	code, stdout, _ = run(t, "billing", "-data-dir", dir, "trial", "1")
	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "already on a clock") {
		t.Fatalf("a second enrolment said %q", stdout)
	}

	code, stdout, stderr = run(t, "billing", "-data-dir", dir, "status", "1")
	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	// The deletion date is what somebody is calling about, so it has to be on
	// the screen along with what the account can still do.
	for _, want := range []string{"phase", "grace", "deletes", "collecting", "export", "contact", "owner@example.com"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status does not report %q:\n%s", want, stdout)
		}
	}
}

// TestBillingStatusListsEveryRunningClock covers the no-argument form, which is
// the "what is about to be deleted" question.
func TestBillingStatusListsEveryRunningClock(t *testing.T) {
	dir := billingDataDir(t)

	code, stdout, _ := run(t, "billing", "-data-dir", dir, "status")
	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "no account is on a lifecycle clock") {
		t.Fatalf("a fresh install reported %q", stdout)
	}

	run(t, "billing", "-data-dir", dir, "trial", "1")

	code, stdout, _ = run(t, "billing", "-data-dir", dir, "status")
	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	for _, want := range []string{"TEAM", "DELETES ON", "Example Co", "trial"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the listing is missing %q:\n%s", want, stdout)
		}
	}
}

// TestBillingSweepRuns checks the manual sweep, which is what an operator runs
// after a process has been down and what a self-hosted cron job would call.
func TestBillingSweepRuns(t *testing.T) {
	dir := billingDataDir(t)

	run(t, "billing", "-data-dir", dir, "trial", "1")

	code, stdout, stderr := run(t, "billing", "-data-dir", dir, "sweep")
	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "lifecycle:") || !strings.Contains(stdout, "volume:") {
		t.Fatalf("sweep reported %q", stdout)
	}
}

// TestBillingEventsReadsTheWebhookLog is the first thing to run when a customer
// says they paid and the account still says otherwise.
func TestBillingEventsReadsTheWebhookLog(t *testing.T) {
	dir := billingDataDir(t)

	code, stdout, _ := run(t, "billing", "-data-dir", dir, "events")
	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "no webhook deliveries recorded") {
		t.Fatalf("an empty log reported %q", stdout)
	}

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	if _, err := control.Exec(`
		INSERT INTO stripe_events (event_id, type, team_id, payload, received_at, handled_at, outcome)
		VALUES ('evt_1', 'invoice.payment_failed', 1, '{}', ?, ?, 'applied')
	`, time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ = run(t, "billing", "-data-dir", dir, "events")
	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	for _, want := range []string{"evt_1", "invoice.payment_failed", "applied"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the log listing is missing %q:\n%s", want, stdout)
		}
	}
}

// TestBillingRepliedUnlocks covers the one command that exists because an email
// thread is not machine-readable: a person records the reply, and the dashboard
// comes back.
func TestBillingRepliedUnlocks(t *testing.T) {
	dir := billingDataDir(t)

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	now := time.Now().UTC().Unix()

	if _, err := control.Exec(`
		INSERT INTO usage_overages (team_id, period, asked_at, reply_deadline, locked_at, updated_at)
		VALUES (1, '2026-03', ?, ?, ?, ?)
	`, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, "billing", "-data-dir", dir, "replied", "1")
	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dashboard unlocked") {
		t.Fatalf("stdout is %q", stdout)
	}

	var locked sql.NullInt64
	if err := control.QueryRow(`SELECT locked_at FROM usage_overages WHERE team_id = 1`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if locked.Valid {
		t.Fatal("recording a reply did not clear the lock")
	}
}

// TestBillingRejectsABadAccountID makes sure a mistyped id is refused rather
// than silently acting on account zero.
func TestBillingRejectsABadAccountID(t *testing.T) {
	dir := billingDataDir(t)

	for _, bad := range []string{"abc", "0", "-1"} {
		if code, _, _ := run(t, "billing", "-data-dir", dir, "trial", bad); code != ExitUsage {
			t.Errorf("%q gave exit code %d, want %d", bad, code, ExitUsage)
		}
	}
}
