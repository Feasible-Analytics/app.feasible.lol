//
// litestream_test.go
// Tests for the `litestream config` and `litestream check` subcommands.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// litestreamDataDir builds a data directory with a control database and the
// account directories named, which is the layout every replication command
// walks.
func litestreamDataDir(t *testing.T, ids ...int64) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "control.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, id := range ids {
		path := accounts.Path(dir, id)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// TestLitestreamWithoutSubcommand checks the bare command lists what it offers
// rather than dumping the whole program's help.
func TestLitestreamWithoutSubcommand(t *testing.T) {
	code, _, stderr := run(t, "litestream")

	if code != ExitUsage {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "config") || !strings.Contains(stderr, "check") {
		t.Fatalf("the help is missing a subcommand: %q", stderr)
	}
}

// TestLitestreamConfigRefusesWithoutAReplicaURL stops a shard from writing a
// configuration that replicates to nowhere and reporting success.
func TestLitestreamConfigRefusesWithoutAReplicaURL(t *testing.T) {
	t.Setenv("FEASIBLE_APP_DATA_DIR", litestreamDataDir(t))

	code, _, stderr := run(t, "litestream", "config", "-replica-url", "")

	if code != ExitError {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "FEASIBLE_LITESTREAM_REPLICA_URL") {
		t.Fatalf("the message does not name the variable to set: %q", stderr)
	}
}

// TestLitestreamConfigPrintsWithoutWriting is how "what would this replicate" is
// answered on a box whose real configuration file belongs to root.
func TestLitestreamConfigPrintsWithoutWriting(t *testing.T) {
	dir := litestreamDataDir(t, 3)
	out := filepath.Join(t.TempDir(), "litestream.yml")

	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", "s3://bucket/shard-01")
	t.Setenv("FEASIBLE_LITESTREAM_CONFIG", out)

	code, stdout, stderr := run(t, "litestream", "config", "-print")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "account-000003") {
		t.Fatalf("the account database is missing from the output:\n%s", stdout)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("-print wrote the file")
	}
}

// TestLitestreamConfigWritesEveryDatabase is the command doing its job: control
// and every account, in one file the daemon can read.
func TestLitestreamConfigWritesEveryDatabase(t *testing.T) {
	dir := litestreamDataDir(t, 1, 2)
	out := filepath.Join(t.TempDir(), "litestream.yml")

	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", "s3://bucket/shard-01")
	t.Setenv("FEASIBLE_LITESTREAM_CONFIG", out)

	if code, _, stderr := run(t, "litestream", "config"); code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"control.db", "000001", "000002"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("%q is not in the written configuration:\n%s", want, body)
		}
	}
}

// TestLitestreamConfigRunsTheHookOnlyOnAChange is the decision the watcher is
// built on. Restarting the daemon interrupts replication for every database on
// the box, so it happens when an account appears and not on every pass.
func TestLitestreamConfigRunsTheHookOnlyOnAChange(t *testing.T) {
	dir := litestreamDataDir(t, 1)
	work := t.TempDir()
	out := filepath.Join(work, "litestream.yml")
	marker := filepath.Join(work, "restarts")

	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", "s3://bucket/shard-01")
	t.Setenv("FEASIBLE_LITESTREAM_CONFIG", out)
	t.Setenv("FEASIBLE_LITESTREAM_ON_CHANGE", "echo restarted >> "+marker)

	if code, _, stderr := run(t, "litestream", "config"); code != ExitOK {
		t.Fatalf("first run: exit code %d, stderr: %s", code, stderr)
	}
	if restarts(t, marker) != 1 {
		t.Fatalf("the first write ran the hook %d times, want 1", restarts(t, marker))
	}

	if code, _, stderr := run(t, "litestream", "config"); code != ExitOK {
		t.Fatalf("second run: exit code %d, stderr: %s", code, stderr)
	}
	if restarts(t, marker) != 1 {
		t.Fatalf("an unchanged configuration ran the hook again")
	}

	path := accounts.Path(dir, 8)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := run(t, "litestream", "config"); code != ExitOK {
		t.Fatalf("third run: exit code %d, stderr: %s", code, stderr)
	}
	if restarts(t, marker) != 2 {
		t.Fatalf("a new account database ran the hook %d times, want 2", restarts(t, marker))
	}
}

// restarts counts how many times the change hook has run.
func restarts(t *testing.T, marker string) int {
	t.Helper()

	body, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}

	return strings.Count(string(body), "restarted")
}

// TestLitestreamCheckFailsOnAnUnreplicatedAccount is the alarm this whole
// command exists for: an account with no replication produces no error anywhere
// else, so the check has to exit non-zero and name the file.
func TestLitestreamCheckFailsOnAnUnreplicatedAccount(t *testing.T) {
	dir := litestreamDataDir(t, 1)
	out := filepath.Join(t.TempDir(), "litestream.yml")

	t.Setenv("FEASIBLE_APP_DATA_DIR", dir)
	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", "s3://bucket/shard-01")
	t.Setenv("FEASIBLE_LITESTREAM_CONFIG", out)

	if code, _, stderr := run(t, "litestream", "config"); code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	if code, stdout, stderr := run(t, "litestream", "check"); code != ExitOK {
		t.Fatalf("a current configuration failed the check: %d %s %s", code, stdout, stderr)
	}

	path := accounts.Path(dir, 4)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := run(t, "litestream", "check")

	if code != ExitError {
		t.Fatalf("an unreplicated account passed the check, exit code %d", code)
	}
	if !strings.Contains(stderr, path) {
		t.Fatalf("the check did not name the unreplicated file: %q", stderr)
	}
}
