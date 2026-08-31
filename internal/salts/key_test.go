//
// key_test.go
// Tests for resolving and generating the salt encryption key.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package salts

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfiguredKeyIsUsedAsGiven checks an operator-supplied key reaches the
// cipher unchanged. A key that was silently transformed would make salts
// written by one build unreadable by another.
func TestConfiguredKeyIsUsedAsGiven(t *testing.T) {
	want := bytes.Repeat([]byte{0x7f}, KeySize)

	got, err := LoadKey(t.TempDir(), hex.EncodeToString(want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the configured key was not used verbatim")
	}
}

// TestBadConfiguredKeyIsRejected checks a wrong-length or non-hex key fails at
// start-up rather than at the first event.
func TestBadConfiguredKeyIsRejected(t *testing.T) {
	for _, value := range []string{"not-hex", hex.EncodeToString([]byte("too short"))} {
		if _, err := LoadKey(t.TempDir(), value); err == nil {
			t.Fatalf("LoadKey accepted %q", value)
		}
	}
}

// TestGeneratedKeyIsStableAcrossRuns is what makes encryption at rest true by
// default. A key regenerated on every boot would make yesterday's salt
// unreadable, which would split every session that spans a restart.
func TestGeneratedKeyIsStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadKey(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	second, err := LoadKey(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("a second boot generated a different key")
	}
	if len(first) != KeySize {
		t.Fatalf("generated key is %d bytes, want %d", len(first), KeySize)
	}
}

// TestGeneratedKeyFileIsPrivate checks the file cannot be read by other users on
// the box. It is as sensitive as raw IP logs, and a world-readable copy would
// undo the point of encrypting the table.
func TestGeneratedKeyFileIsPrivate(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadKey(dir, ""); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatal(err)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key file mode is %o, want 600", mode)
	}
}

// TestCorruptKeyFileIsReported checks a damaged key file says so rather than
// being replaced, because replacing it would make every stored salt unreadable
// with no warning at all.
func TestCorruptKeyFileIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFileName)

	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadKey(dir, "")
	if err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("error does not explain the problem: %v", err)
	}
}
