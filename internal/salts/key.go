//
// key.go
// The key that encrypts the fingerprint salts at rest.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package salts

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is 32 bytes, for AES-256-GCM. The salts table is as sensitive as raw
// IP logs — anyone holding a salt and the stored hashes can brute-force the
// inputs back out — so it is encrypted with the strongest thing that costs
// nothing on a path that touches it twice a day.
const KeySize = 32

// KeyFileName is where a generated key is kept when the operator supplies
// none. It sits beside the databases because it is worthless without them and
// useless separated from them: back up the data directory and the install is
// whole.
const KeyFileName = "salt.key"

// LoadKey resolves the encryption key, generating and storing one on first run.
// A self-hoster who configures nothing still gets encryption at rest, which is
// the only way "encrypted at rest" is true in practice rather than only when
// somebody remembered to set a variable.
func LoadKey(dataDir, configured string) ([]byte, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		key, err := hex.DecodeString(configured)
		if err != nil {
			return nil, fmt.Errorf("salt key: expected %d hex bytes: %w", KeySize, err)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("salt key: got %d bytes, want %d", len(key), KeySize)
		}

		return key, nil
	}

	return keyFromFile(filepath.Join(dataDir, KeyFileName))
}

// keyFromFile reads the generated key, creating it if it is not there. The file
// is written with O_EXCL so two processes starting at once cannot both generate
// a key and leave one of them unable to read salts the other wrote.
func keyFromFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != KeySize {
			return nil, fmt.Errorf("salt key %s is corrupt — every salt encrypted with it is unreadable", path)
		}

		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("salt key %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("salt key: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Losing the race means another process wrote a key a moment ago, and
		// that key is the right one to use.
		if os.IsExist(err) {
			return keyFromFile(path)
		}
		return nil, fmt.Errorf("salt key %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.WriteString(hex.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("salt key %s: %w", path, err)
	}

	return key, nil
}
