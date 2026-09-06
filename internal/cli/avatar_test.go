//
// avatar_test.go
// What `avatar backfill` asks about, and what it refuses.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestAvatarBackfillOnlyAsksAboutVerifiedAddresses is the consent boundary.
//
// An unverified address has not been proved to belong to the person who typed
// it, and asking a third party about the hash of one is a lookup nobody agreed
// to.
func TestAvatarBackfillOnlyAsksAboutVerifiedAddresses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("migrate: code=%d stderr=%q", code, stderr)
	}

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	ctx := context.Background()

	if _, err := control.ExecContext(ctx, `
		INSERT INTO users (email, name, email_verified_at, created_at, updated_at)
		VALUES ('verified@example.com', 'V', 1, 1, 1), ('pending@example.com', 'P', NULL, 1, 1)`); err != nil {
		t.Fatal(err)
	}

	people, err := verifiedPeople(control)(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(people) != 1 || people[0].Email != "verified@example.com" {
		t.Fatalf("the backfill would ask about %+v, want only the verified address", people)
	}
}

// TestAvatarBackfillRefusesWithGravatarOff keeps a command that could do nothing
// from reporting that it did something.
func TestAvatarBackfillRefusesWithGravatarOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_APP_GRAVATAR", "false")

	if code, _, stderr := run(t, "db", "migrate"); code != ExitOK {
		t.Fatalf("migrate: code=%d stderr=%q", code, stderr)
	}

	code, _, stderr := run(t, "avatar", "backfill", "--data-dir", dir)
	if code == ExitOK {
		t.Fatal("a backfill with Gravatar switched off reported success")
	}

	if !strings.Contains(stderr, "Gravatar") {
		t.Errorf("the refusal does not say what is switched off: %q", stderr)
	}
}

// TestAvatarWithoutASubcommandExplainsItself keeps the command discoverable.
func TestAvatarWithoutASubcommandExplainsItself(t *testing.T) {
	code, _, stderr := run(t, "avatar")
	if code != ExitUsage {
		t.Fatalf("code = %d, want a usage exit", code)
	}

	if !strings.Contains(stderr, "backfill") {
		t.Errorf("the help does not name the one subcommand: %q", stderr)
	}
}
