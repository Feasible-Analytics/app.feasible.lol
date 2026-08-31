//
// persist.go
// Writing the live session cache to disk on shutdown and reading it back at boot.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SessionFileName is where the cache is kept between runs. It sits under the
// data directory with everything else, so backing up one directory still backs
// up the whole install.
const SessionFileName = "sessions.json"

// SessionDirName is the subdirectory the ingest tier's own state lives in.
const SessionDirName = "ingest"

// SessionSnapshot is the on-disk form. The version is there so a later change
// to the Session struct is a skipped restore rather than a garbled one — a
// half-understood session is worse than no session, because it would produce a
// row that looks plausible and is wrong.
type SessionSnapshot struct {
	Version   int       `json:"version"`
	WrittenAt int64     `json:"written_at"`
	Sessions  []Session `json:"sessions"`
}

// sessionSnapshotVersion is bumped whenever Session gains or loses a field that
// the fold depends on.
const sessionSnapshotVersion = 3

// SessionFilePath is where the snapshot lives under a data directory.
func SessionFilePath(dataDir string) string {
	return filepath.Join(dataDir, SessionDirName, SessionFileName)
}

// PersistSessions writes the live cache to disk. Without it a restart splits
// every in-flight session in two, one visitor becomes two, and nothing
// afterwards can tell which visits were really one — the same reason the
// incumbent does this.
func PersistSessions(cache *SessionCache, path string) error {
	snapshot := SessionSnapshot{
		Version:   sessionSnapshotVersion,
		WrittenAt: time.Now().Unix(),
		Sessions:  cache.Snapshot(),
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("persist sessions: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("persist sessions: %w", err)
		}
	}

	// Written to a temporary file and renamed, so a process killed mid-write
	// leaves the previous snapshot intact rather than a truncated one that the
	// next boot would refuse.
	temp := path + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return fmt.Errorf("persist sessions: %w", err)
	}

	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("persist sessions: %w", err)
	}

	return nil
}

// RestoreSessions reads a snapshot back into the cache and removes the file.
// The file is removed on purpose: it is only ever correct immediately after the
// shutdown that wrote it, and a stale one reloaded a week later would resurrect
// visits that ended long ago.
func RestoreSessions(cache *SessionCache, path string, now int64) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("restore sessions: %w", err)
	}

	// The file is removed whatever happens next. A snapshot we could not read
	// is not going to become readable, and leaving it would make every boot
	// report the same failure.
	defer os.Remove(path) //nolint:errcheck // a failed cleanup is not worth failing the boot

	var snapshot SessionSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return 0, fmt.Errorf("restore sessions: %w", err)
	}

	if snapshot.Version != sessionSnapshotVersion {
		return 0, fmt.Errorf("restore sessions: snapshot is version %d, this build reads %d", snapshot.Version, sessionSnapshotVersion)
	}

	return cache.Restore(snapshot.Sessions, now), nil
}
