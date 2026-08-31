//
// litestream_test.go
// Tests for the generated replication configuration.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package litestream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// dataDir builds a data directory holding a control database and the account
// directories named, in the layout every command walks.
func dataDir(t *testing.T, ids ...int64) string {
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

// options returns a valid configuration pointed at a directory.
func options(dir string) Options {
	return Options{DataDir: dir, ReplicaURL: "s3://bucket/shard-01"}
}

// TestPlanCoversControlAndEveryAccount is the whole promise of generating this
// file: an account that is missed is an account with no replication and no
// error message anywhere.
func TestPlanCoversControlAndEveryAccount(t *testing.T) {
	dir := dataDir(t, 1, 42)

	plan, err := Plan(options(dir))
	if err != nil {
		t.Fatal(err)
	}

	if len(plan) != 3 {
		t.Fatalf("planned %d databases, want 3", len(plan))
	}

	want := []string{"control", "account-000001", "account-000042"}
	for i, name := range want {
		if plan[i].Name != name {
			t.Fatalf("database %d is named %q, want %q", i, plan[i].Name, name)
		}
	}

	if plan[2].ReplicaURL != "s3://bucket/shard-01/account-000042" {
		t.Fatalf("replica URL is %q", plan[2].ReplicaURL)
	}
}

// TestPlanIncludesControlBeforeItExists keeps a brand-new shard covered from
// the moment it is built rather than from its first customer.
func TestPlanIncludesControlBeforeItExists(t *testing.T) {
	plan, err := Plan(options(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}

	if len(plan) != 1 || plan[0].Name != ControlName {
		t.Fatalf("an empty install planned %d databases", len(plan))
	}
}

// TestPlanWritesAbsolutePaths matters because Litestream runs as its own
// service with its own working directory: a relative path would point at
// nothing and replicate nothing, silently.
func TestPlanWritesAbsolutePaths(t *testing.T) {
	dir := dataDir(t, 7)

	relative, err := filepath.Rel(mustGetwd(t), dir)
	if err != nil {
		t.Skipf("the temporary directory is not relative to the working directory: %v", err)
	}

	plan, err := Plan(options(relative))
	if err != nil {
		t.Fatal(err)
	}

	for _, db := range plan {
		if !filepath.IsAbs(db.Path) {
			t.Fatalf("%s is not absolute", db.Path)
		}
	}
}

// mustGetwd reports the working directory or fails the test.
func mustGetwd(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	return wd
}

// TestValidateRejectsRetentionShorterThanSnapshots guards the one misconfigured
// value that produces a replica nobody can restore from, months after it was
// set and in the middle of an incident.
func TestValidateRejectsRetentionShorterThanSnapshots(t *testing.T) {
	opts := Options{
		DataDir:          t.TempDir(),
		ReplicaURL:       "s3://bucket/shard-01",
		SnapshotInterval: 24 * time.Hour,
		Retention:        time.Hour,
	}

	err := opts.Validate()
	if err == nil {
		t.Fatal("a retention shorter than the snapshot interval was accepted")
	}
	if !strings.Contains(err.Error(), "retention") {
		t.Fatalf("unhelpful message: %v", err)
	}
}

// TestValidateRequiresAReplicaURL keeps a shard from believing it is replicated
// because a variable was never set.
func TestValidateRequiresAReplicaURL(t *testing.T) {
	err := Options{DataDir: t.TempDir()}.Validate()

	if err == nil {
		t.Fatal("an empty replica URL was accepted")
	}
	if !strings.Contains(err.Error(), "FEASIBLE_LITESTREAM_REPLICA_URL") {
		t.Fatalf("the message does not name the variable to set: %v", err)
	}
}

// TestRenderIsStable is what makes "did this change" a byte comparison, and
// therefore what keeps the restart hook from firing on every tick.
func TestRenderIsStable(t *testing.T) {
	dir := dataDir(t, 1, 2, 3)

	plan, err := Plan(options(dir))
	if err != nil {
		t.Fatal(err)
	}

	first := string(Render(plan, options(dir)))
	second := string(Render(plan, options(dir)))

	if first != second {
		t.Fatal("two renders of the same plan differ")
	}

	for _, want := range []string{"dbs:", "sync-interval: 1s", "snapshot-interval: 6h", "retention: 72h"} {
		if !strings.Contains(first, want) {
			t.Fatalf("the rendered config is missing %q:\n%s", want, first)
		}
	}
}

// TestRenderCarriesNoCredentials is a rule rather than an accident: this file is
// rewritten every time somebody signs up, and a secret in it would be copied
// into whatever backup or config-management system watches it.
func TestRenderCarriesNoCredentials(t *testing.T) {
	dir := dataDir(t, 1)

	plan, err := Plan(options(dir))
	if err != nil {
		t.Fatal(err)
	}

	body := string(Render(plan, options(dir)))

	for _, forbidden := range []string{"access-key-id", "secret-access-key"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the rendered config carries %q", forbidden)
		}
	}
}

// TestSyncReportsOnlyRealChanges is the decision the watcher is built on:
// restarting the daemon interrupts replication for every database on the box,
// so it must happen when an account appears and not once a minute.
func TestSyncReportsOnlyRealChanges(t *testing.T) {
	dir := dataDir(t, 1)
	configPath := filepath.Join(t.TempDir(), "litestream.yml")

	first, err := Sync(configPath, options(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Fatal("writing the file for the first time did not count as a change")
	}

	second, err := Sync(configPath, options(dir))
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatal("an unchanged install rewrote the file")
	}

	// A new account database is exactly the event the watcher exists for.
	path := accounts.Path(dir, 9)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	third, err := Sync(configPath, options(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !third.Changed {
		t.Fatal("a new account database did not change the configuration")
	}
	if third.Databases != 3 {
		t.Fatalf("the configuration names %d databases, want 3", third.Databases)
	}
}

// TestMissingNamesUnreplicatedDatabases is the alarm. An account whose database
// is not in the running configuration is unprotected and completely silent, so
// the check has to name the files rather than count them.
func TestMissingNamesUnreplicatedDatabases(t *testing.T) {
	dir := dataDir(t, 1)
	configPath := filepath.Join(t.TempDir(), "litestream.yml")

	if _, err := Sync(configPath, options(dir)); err != nil {
		t.Fatal(err)
	}

	path := accounts.Path(dir, 5)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := Plan(options(dir))
	if err != nil {
		t.Fatal(err)
	}

	missing, err := Missing(configPath, plan)
	if err != nil {
		t.Fatal(err)
	}

	if len(missing) != 1 || missing[0] != path {
		t.Fatalf("missing is %v, want [%s]", missing, path)
	}
}

// TestMissingTreatsNoConfigAsNothingReplicated stops a box with no
// configuration file at all from reporting itself healthy.
func TestMissingTreatsNoConfigAsNothingReplicated(t *testing.T) {
	dir := dataDir(t, 1)

	plan, err := Plan(options(dir))
	if err != nil {
		t.Fatal(err)
	}

	missing, err := Missing(filepath.Join(t.TempDir(), "absent.yml"), plan)
	if err != nil {
		t.Fatal(err)
	}

	if len(missing) != len(plan) {
		t.Fatalf("%d of %d databases reported missing", len(missing), len(plan))
	}
}
