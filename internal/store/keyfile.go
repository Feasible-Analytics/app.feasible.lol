//
// keyfile.go
// A random key kept beside the databases, generated once and read back forever.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateKey reads a hex-encoded key of exactly size bytes from path,
// generating and storing one if the file does not exist. The label names the
// key in errors ("app key", "script key") so an operator reading a corrupt-file
// message knows which one it is and what depends on it.
//
// The file is created with O_EXCL so two processes starting at once cannot
// both generate a key and leave one of them unable to read what the other
// wrote; the loser of that race reads the winner's file instead. It is 0600
// because the key is the only thing standing between the data directory and
// whatever it protects.
func LoadOrCreateKey(path string, size int, label string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != size {
			return nil, fmt.Errorf("%s %s is corrupt: expected %d hex-encoded bytes", label, path, size)
		}

		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s %s: %w", label, path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	key := make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return LoadOrCreateKey(path, size, label)
		}

		return nil, fmt.Errorf("%s %s: %w", label, path, err)
	}
	if _, err := file.WriteString(hex.EncodeToString(key)); err != nil {
		writeErr := fmt.Errorf("%s %s: %w", label, path, err)
		if closeErr := file.Close(); closeErr != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("%s %s: close after write failure: %w", label, path, closeErr))
		}

		return nil, writeErr
	}

	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("%s %s: close: %w", label, path, err)
	}

	return key, nil
}
