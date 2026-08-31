//
// secretkey_test.go
// The application key, the AES-GCM box and the signed cookie values.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadKeyGeneratesOnceAndReuses checks that a self-hoster who configures
// nothing still gets encryption at rest, and that a restart does not invent a
// new key and strand every enrolled authenticator app.
func TestLoadKeyGeneratesOnceAndReuses(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadKey(dir, "")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	if len(first) != KeySize {
		t.Fatalf("want a %d-byte key, got %d", KeySize, len(first))
	}

	second, err := LoadKey(dir, "")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	if hex.EncodeToString(first) != hex.EncodeToString(second) {
		t.Error("a second read must return the key that was generated first")
	}

	info, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key file should be owner-only, got %v", info.Mode().Perm())
	}
}

// TestLoadKeyPrefersTheConfiguredValue checks the operator-supplied key wins,
// and that a wrong length is refused rather than silently padded.
func TestLoadKeyPrefersTheConfiguredValue(t *testing.T) {
	configured := strings.Repeat("ab", KeySize)

	key, err := LoadKey(t.TempDir(), configured)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}

	if hex.EncodeToString(key) != configured {
		t.Error("the configured key should be used as-is")
	}

	if _, err := LoadKey(t.TempDir(), "abcd"); err == nil {
		t.Error("a short key should be refused")
	}

	if _, err := LoadKey(t.TempDir(), strings.Repeat("z", 64)); err == nil {
		t.Error("a key that is not hex should be refused")
	}
}

// TestSealerRoundTripAndTamper checks AES-GCM is doing both of its jobs: the
// value comes back, and a modified one is rejected rather than decrypting to
// something else.
func TestSealerRoundTripAndTamper(t *testing.T) {
	sealer := newTestSealer(t)

	sealed, err := sealer.Seal("a secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	opened, err := sealer.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if opened != "a secret" {
		t.Errorf("want %q, got %q", "a secret", opened)
	}

	// Two seals of the same value must differ, or the nonce is not random — and
	// a reused GCM nonce exposes the plaintext of both messages.
	other, err := sealer.Seal("a secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if other == sealed {
		t.Error("sealing the same value twice must produce different ciphertext")
	}

	if _, err := sealer.Open(sealed[:len(sealed)-2] + "AA"); err == nil {
		t.Error("a tampered value must not open")
	}

	if _, err := sealer.Open("short"); err == nil {
		t.Error("a value too short to be sealed must not open")
	}
}

// TestSignedValueVerifies checks the HMAC that stops a browser handing back a
// short-lived cookie value we never issued.
func TestSignedValueVerifies(t *testing.T) {
	sealer := newTestSealer(t)

	signed := sealer.SignedValue("value")

	value, ok := sealer.VerifySignedValue(signed)
	if !ok || value != "value" {
		t.Errorf("a signed value should verify, got %q ok=%v", value, ok)
	}

	if _, ok := sealer.VerifySignedValue("value.wrong-signature"); ok {
		t.Error("a bad signature must not verify")
	}

	if _, ok := sealer.VerifySignedValue("no-signature-at-all"); ok {
		t.Error("a value with no signature must not verify")
	}

	// A value containing a dot still works, because the split is on the last
	// one — the OAuth state payload is base64 and can carry anything.
	dotted := sealer.SignedValue("a.b.c")

	if value, ok := sealer.VerifySignedValue(dotted); !ok || value != "a.b.c" {
		t.Errorf("a value containing dots should round-trip, got %q ok=%v", value, ok)
	}
}
