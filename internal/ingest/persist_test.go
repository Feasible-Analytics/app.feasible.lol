//
// persist_test.go
// Tests for writing the live session cache to disk and reading it back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSessionSnapshotFileRoundTrip covers the shutdown path end to end, since
// losing it splits every in-flight session in two on every deploy.
func TestSessionSnapshotFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SessionDirName, SessionFileName)

	cache := NewSessionCache()

	first := event(EventPageview, fixtureStart.Unix(), "/")
	cache.Apply(&first)

	if err := PersistSessions(cache, path); err != nil {
		t.Fatal(err)
	}

	restored := NewSessionCache()
	count, err := RestoreSessions(restored, path, fixtureStart.Unix()+60)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("restored %d sessions, want 1", count)
	}

	// The file is removed after a successful read, because it is only ever
	// correct immediately after the shutdown that wrote it.
	if _, err := RestoreSessions(NewSessionCache(), path, fixtureStart.Unix()+60); err != nil {
		t.Fatal(err)
	}
	if again, _ := RestoreSessions(NewSessionCache(), path, fixtureStart.Unix()+60); again != 0 {
		t.Fatal("the snapshot file was read a second time")
	}
}

// TestRestoreOfAMissingFileIsNotAnError checks a first boot works. There is no
// snapshot before the first shutdown, and that must not be reported as a
// problem.
func TestRestoreOfAMissingFileIsNotAnError(t *testing.T) {
	count, err := RestoreSessions(NewSessionCache(), filepath.Join(t.TempDir(), "nothing.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("restored %d sessions from nothing", count)
	}
}

// TestCorruptSnapshotIsReportedAndRemoved checks a damaged file says so and does
// not come back on every boot. A half-understood session is worse than no
// session, because it would produce a row that looks plausible and is wrong.
func TestCorruptSnapshotIsReportedAndRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), SessionFileName)

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RestoreSessions(NewSessionCache(), path, 0); err == nil {
		t.Fatal("a corrupt snapshot was accepted")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the corrupt snapshot was left behind for the next boot to fail on")
	}
}

// TestSnapshotFromAnotherVersionIsSkipped checks the version guard. A Session
// that gained or lost a field the fold depends on cannot be restored safely, and
// skipping is the only honest answer.
func TestSnapshotFromAnotherVersionIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), SessionFileName)

	if err := os.WriteFile(path, []byte(`{"version":999,"sessions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RestoreSessions(NewSessionCache(), path, 0); err == nil {
		t.Fatal("a snapshot from another version was restored")
	}
}

// TestSnapshotFileIsPrivate checks the file cannot be read by other users. It
// holds live visitor fingerprints, which are pseudonymous rather than anonymous.
func TestSnapshotFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), SessionDirName, SessionFileName)

	cache := NewSessionCache()
	first := event(EventPageview, fixtureStart.Unix(), "/")
	cache.Apply(&first)

	if err := PersistSessions(cache, path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("snapshot mode is %o, want 600", mode)
	}
}
