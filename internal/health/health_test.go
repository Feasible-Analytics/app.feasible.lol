//
// health_test.go
// Tests for the readiness report.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// find returns one component out of a report.
func find(t *testing.T, report Report, name string) Component {
	t.Helper()

	for _, component := range report.Components {
		if component.Name == name {
			return component
		}
	}

	t.Fatalf("the report has no component named %q", name)

	return Component{}
}

// TestRequiredFailureIsNotReady checks a broken dependency keeps traffic away
// and says which one it was.
func TestRequiredFailureIsNotReady(t *testing.T) {
	set := &Set{}
	set.Require("system_db", func(context.Context) error { return errors.New("the file is gone") })
	set.Require("salts", func(context.Context) error { return nil })

	report := set.Run(context.Background())

	if report.Ready() {
		t.Fatal("the process reported ready with a required dependency down")
	}
	if report.Status != StatusNotReady {
		t.Errorf("status = %q, want %q", report.Status, StatusNotReady)
	}

	failed := find(t, report, "system_db")
	if failed.Status != StatusFailed || failed.Detail != "the file is gone" {
		t.Errorf("component = %+v, want failed with the reason attached", failed)
	}

	// Every check runs even after one fails: the first failure is rarely the
	// whole story, and a report that stopped at it would send somebody to fix
	// the wrong thing.
	if ok := find(t, report, "salts"); ok.Status != StatusOK {
		t.Errorf("the checks after the failure did not run: %+v", ok)
	}
}

// TestOptionalFailureIsDegradedNotDown checks an optional dependency is
// reported and harmless. A missing geolocation database makes the dashboard
// worse; a process that refused traffic over it would make an optional data
// file an outage.
func TestOptionalFailureIsDegradedNotDown(t *testing.T) {
	set := &Set{}
	set.Optional("geolocation", func(context.Context) error { return errors.New("no database") })

	report := set.Run(context.Background())

	if !report.Ready() {
		t.Fatal("an optional dependency took the process out of rotation")
	}

	if got := find(t, report, "geolocation"); got.Status != StatusDegraded {
		t.Errorf("component = %+v, want degraded", got)
	}
}

// TestDirectoryProbeChecksWritingRatherThanExisting checks the probe answers
// the question being asked. A full disk, a read-only remount and a permissions
// change all leave a directory that stats perfectly well and cannot take an
// event.
func TestDirectoryProbeChecksWritingRatherThanExisting(t *testing.T) {
	root := t.TempDir()

	if err := Directory(filepath.Join(root, "accounts"))(context.Background()); err != nil {
		t.Fatalf("a writable directory reported %v", err)
	}

	// A path whose parent is a file cannot be created, which is the cheapest
	// honest version of "this directory is not usable".
	blocked := filepath.Join(root, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Directory(filepath.Join(blocked, "accounts"))(context.Background()); err == nil {
		t.Fatal("an unusable directory reported no error")
	}
}

// TestDirectoryProbeIsCached checks a load balancer probing every second does
// not create and delete a file every second.
func TestDirectoryProbeIsCached(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "accounts")
	probe := Directory(dir)

	if err := probe(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The directory is removed after the first probe. A cached answer still
	// says yes; an uncached one would recreate it as a side effect of being
	// asked, which is its own bug.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := probe(context.Background()); err != nil {
		t.Fatalf("the second probe was not cached: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the cached probe recreated the directory")
	}
}

// TestConditionCarriesItsMessage checks a plain boolean becomes a component
// with a reason. "Not ready" on its own is twenty minutes of guessing.
func TestConditionCarriesItsMessage(t *testing.T) {
	err := Condition(func() bool { return false }, "the routing map is empty")(context.Background())

	if err == nil || err.Error() != "the routing map is empty" {
		t.Fatalf("error = %v, want the message", err)
	}

	if err := Condition(func() bool { return true }, "unused")(context.Background()); err != nil {
		t.Fatalf("a satisfied condition reported %v", err)
	}
}
