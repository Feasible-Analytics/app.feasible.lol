//
// seed_test.go
// Tests for the `seed` subcommand.
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
)

// TestSeedGeneratesADataset drives the command the way the Makefile does and
// checks it produced databases. It is deliberately tiny: the shape of the data
// is the seed package's business, and what belongs here is that the command
// wires the flags to it and reports what it did.
func TestSeedGeneratesADataset(t *testing.T) {
	dir := t.TempDir()

	code, stdout, stderr := run(t, "seed",
		"--data-dir", dir,
		"--pageviews", "400",
		"--days", "3",
		"--sites", "2",
		"--seed", "5",
	)

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "events") || !strings.Contains(stdout, "visits") {
		t.Fatalf("the run said nothing about what it generated: %q", stdout)
	}

	if _, err := os.Stat(filepath.Join(dir, "control.db")); err != nil {
		t.Fatalf("no control database: %v", err)
	}

	if _, err := os.Stat(accounts.Path(dir, 1)); err != nil {
		t.Fatalf("no account database: %v", err)
	}
}

// TestSeedRefusesProduction pins the one refusal that matters. Seeding writes
// fake traffic into whatever data directory it is pointed at, and --fresh
// deletes what is already there.
func TestSeedRefusesProduction(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", "production")

	code, _, stderr := run(t, "seed", "--data-dir", t.TempDir(), "--pageviews", "10")

	if code != ExitError {
		t.Fatalf("exit code %d, want %d", code, ExitError)
	}

	if !strings.Contains(stderr, "refusing to seed") {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}

// TestSeedFreshRemovesTheOldDataset checks that --fresh really starts over,
// because a seed that quietly appended a second copy of six weeks of history
// would double every number on the dashboard with no way to tell.
func TestSeedFreshRemovesTheOldDataset(t *testing.T) {
	dir := t.TempDir()

	first, _, stderr := run(t, "seed", "--data-dir", dir, "--pageviews", "400", "--days", "3", "--sites", "1")
	if first != ExitOK {
		t.Fatalf("first run: exit code %d, stderr: %s", first, stderr)
	}

	before := countEvents(t, dir)

	second, _, stderr := run(t, "seed", "--data-dir", dir, "--fresh", "--pageviews", "400", "--days", "3", "--sites", "1")
	if second != ExitOK {
		t.Fatalf("second run: exit code %d, stderr: %s", second, stderr)
	}

	// Not an equality check. The generated window ends at the current moment, so
	// two runs seconds apart legitimately disagree by a handful of events in the
	// partial final day — the same seed, a slightly different clock. What this
	// test exists to catch is a --fresh that appends instead of replacing, and
	// that shows up as roughly double, never as a couple of events.
	after := countEvents(t, dir)

	if after > before+before/2 {
		t.Fatalf("a fresh run holds %d events where the first held %d, which looks appended rather than replaced", after, before)
	}

	if after < before/2 {
		t.Fatalf("a fresh run holds %d events where the first held %d, so --fresh deleted more than it rebuilt", after, before)
	}
}

// countEvents reads how many event rows the first account holds.
func countEvents(t *testing.T, dataDir string) int64 {
	t.Helper()

	ctx := context.Background()

	manager := accounts.NewManager(dataDir)
	defer manager.CloseAll() //nolint:errcheck // the count is what the test wants

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}

	var count int64
	if err := account.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}

	return count
}
