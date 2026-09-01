//
// comp_test.go
// Operator-command coverage for durable complimentary accounts.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestCompByOwnerEmail proves the public command stops the clock, clears its
// ingest mirrors, persists the audit marker, and stays idempotent.
func TestCompByOwnerEmail(t *testing.T) {
	dir := billingDataDir(t)

	code, stdout, stderr := run(t, "comp", "-data-dir", dir, "--email", "OWNER@example.com")
	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "account 1") || !strings.Contains(stdout, "now comped") {
		t.Fatalf("stdout is %q", stdout)
	}

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var trigger string
	var started, trialEnds, acceptUntil sql.NullInt64
	if err := control.QueryRow(`
		SELECT l.trigger, l.started_at, t.trial_ends_at, t.accept_traffic_until
		FROM account_lifecycle l JOIN teams t ON t.id = l.team_id WHERE l.team_id = 1
	`).Scan(&trigger, &started, &trialEnds, &acceptUntil); err != nil {
		t.Fatal(err)
	}
	if trigger != "" || started.Valid || trialEnds.Valid || acceptUntil.Valid {
		t.Fatalf("clock was not cleared: trigger=%q started=%v trial=%v accept=%v", trigger, started, trialEnds, acceptUntil)
	}

	var owner string
	if err := control.QueryRow(`SELECT owner_email FROM account_comps WHERE team_id = 1`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "owner@example.com" {
		t.Fatalf("comp owner = %q", owner)
	}
	if _, err := control.Exec(`
		UPDATE account_lifecycle SET trigger = 'lapse', started_at = 1 WHERE team_id = 1
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`UPDATE teams SET trial_ends_at = 1, accept_traffic_until = 2 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()

	code, stdout, stderr = run(t, "comp", "-data-dir", dir, "--email", "owner@example.com")
	if code != ExitOK || !strings.Contains(stdout, "already comped") {
		t.Fatalf("repeat: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	control, err = store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := control.QueryRow(`
		SELECT l.trigger, l.started_at, t.trial_ends_at, t.accept_traffic_until
		FROM account_lifecycle l JOIN teams t ON t.id = l.team_id WHERE l.team_id = 1
	`).Scan(&trigger, &started, &trialEnds, &acceptUntil); err != nil {
		t.Fatal(err)
	}
	if trigger != "" || started.Valid || trialEnds.Valid || acceptUntil.Valid {
		t.Fatalf("repeat did not repair the clock: trigger=%q started=%v trial=%v accept=%v", trigger, started, trialEnds, acceptUntil)
	}
}

// TestCompRejectsUnknownAndAmbiguousOwners proves lookup failures make no
// changes and explain whether the email matched zero or several teams.
func TestCompRejectsUnknownAndAmbiguousOwners(t *testing.T) {
	dir := billingDataDir(t)

	if code, _, stderr := run(t, "comp", "-data-dir", dir, "--email", "missing@example.com"); code != ExitError || !strings.Contains(stderr, "no team") {
		t.Fatalf("unknown owner: code=%d stderr=%q", code, stderr)
	}

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1)
	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (2, 'Second', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (2, 1, 'owner', ?)`, now); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()

	if code, _, stderr := run(t, "comp", "-data-dir", dir, "--email", "owner@example.com"); code != ExitError || !strings.Contains(stderr, "owns 2 teams") {
		t.Fatalf("ambiguous owner: code=%d stderr=%q", code, stderr)
	}
}

// TestCompRejectsAnActiveSubscription prevents an operator from hiding the
// billing portal while the payment provider can still charge the account.
func TestCompRejectsAnActiveSubscription(t *testing.T) {
	dir := billingDataDir(t)
	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`
		INSERT INTO subscriptions
			(team_id, stripe_subscription_id, status, created_at, updated_at)
		VALUES (1, 'sub_paying', 'active', 1, 1)
	`); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()

	code, _, stderr := run(t, "comp", "-data-dir", dir, "--email", "owner@example.com")
	if code != ExitError || !strings.Contains(stderr, "cancel it before comping") {
		t.Fatalf("active subscription: code=%d stderr=%q", code, stderr)
	}

	control, err = store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var comps int
	if err := control.QueryRow(`SELECT COUNT(*) FROM account_comps WHERE team_id = 1`).Scan(&comps); err != nil {
		t.Fatal(err)
	}
	if comps != 0 {
		t.Fatalf("active account has %d comp markers", comps)
	}
}
