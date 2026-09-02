//
// keyfile_test.go
// Tests for the generate-once key file.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateKeyGeneratesOnce is the self-hoster path: configure nothing,
// get a key, and get the same key after a restart. A key that changed on every
// boot would silently invalidate everything derived from it.
func TestLoadOrCreateKeyGeneratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "test.key")

	first, err := LoadOrCreateKey(path, 32, "test key")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(first))
	}

	second, err := LoadOrCreateKey(path, 32, "test key")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("a second load generated a different key")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key file mode is %o, want 600", mode)
	}
}

// TestLoadOrCreateKeyRejectsCorruption checks that a truncated or non-hex file
// is an error naming the file and the label, rather than a silently wrong key.
func TestLoadOrCreateKeyRejectsCorruption(t *testing.T) {
	for name, contents := range map[string]string{
		"not hex":      "nonsense",
		"wrong length": "abcd",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.key")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadOrCreateKey(path, 32, "test key")
			if err == nil {
				t.Fatal("a corrupt key was accepted")
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "test key") {
				t.Fatalf("error does not name the file and label: %v", err)
			}
		})
	}
}
