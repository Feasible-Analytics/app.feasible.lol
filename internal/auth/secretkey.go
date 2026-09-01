//
// secretkey.go
// The key that encrypts two-factor secrets and signs the short-lived cookies.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is 32 bytes, for AES-256-GCM.
const KeySize = 32

// KeyFileName is where a generated key is kept when the operator supplies none.
// It sits beside the databases, like every other key in this project, because
// it is worthless without them: back up the data directory and the install is
// whole.
const KeyFileName = "app.key"

// LoadKey resolves the application key, generating and storing one on first
// run. A self-hoster who configures nothing still gets encrypted two-factor
// secrets, which is the only way "encrypted at rest" is true in practice rather
// than only when somebody remembered to set a variable.
func LoadKey(dataDir, configured string) ([]byte, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		key, err := hex.DecodeString(configured)
		if err != nil {
			return nil, fmt.Errorf("app key: expected %d hex bytes: %w", KeySize, err)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("app key: got %d bytes, want %d", len(key), KeySize)
		}

		return key, nil
	}

	return keyFromFile(filepath.Join(dataDir, KeyFileName))
}

// keyFromFile reads the generated key, creating it if it is not there. O_EXCL
// means two processes starting at once cannot both generate a key and leave one
// of them unable to read what the other wrote.
func keyFromFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != KeySize {
			return nil, fmt.Errorf("app key %s is corrupt — every two-factor secret encrypted with it is unreadable", path)
		}

		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("app key %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("app key: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Losing the race means another process wrote a key a moment ago, and
		// that key is the right one to use.
		if os.IsExist(err) {
			return keyFromFile(path)
		}
		return nil, fmt.Errorf("app key %s: %w", path, err)
	}
	if _, err := file.WriteString(hex.EncodeToString(key)); err != nil {
		writeErr := fmt.Errorf("app key %s: %w", path, err)
		if closeErr := file.Close(); closeErr != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("app key %s: close after write failure: %w", path, closeErr))
		}
		return nil, writeErr
	}

	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("app key %s: close: %w", path, err)
	}

	return key, nil
}

// Sealer encrypts and authenticates small secrets with the application key. It
// wraps the cipher rather than exposing it so that every caller gets AEAD with
// a fresh nonce, and nobody can reach for the raw block cipher and reuse one.
type Sealer struct {
	aead cipher.AEAD
	key  []byte
}

// NewSealer builds the AES-GCM box.
func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: app key: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: app key: %w", err)
	}

	return &Sealer{aead: aead, key: key}, nil
}

// Seal encrypts a value and returns it as one base64 string.
//
// The nonce is random per call and stored in front of the ciphertext. Deriving
// it from anything about the record — the user id, a counter — is how a nonce
// gets reused, and a reused GCM nonce does not merely weaken the encryption, it
// exposes the plaintext of both messages.
func (s *Sealer) Seal(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: seal: %w", err)
	}

	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)

	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Open decrypts what Seal produced. A value that fails to authenticate is an
// error rather than an empty string, because a two-factor secret that silently
// decrypts to nothing would lock somebody out with no explanation.
func (s *Sealer) Open(sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("auth: open: %w", err)
	}

	if len(raw) < s.aead.NonceSize() {
		return "", fmt.Errorf("auth: open: value is too short to be sealed")
	}

	nonce, body := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]

	plaintext, err := s.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("auth: open: %w", err)
	}

	return string(plaintext), nil
}

// SignedValue produces `<value>.<mac>` for the short-lived cookies that carry
// state between two requests — the pending two-factor user, the OAuth PKCE
// verifier, the CSRF token. They are signed rather than stored because they are
// worthless a minute later and a database row per redirect is a table that only
// ever needs cleaning up.
func (s *Sealer) SignedValue(value string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(value))

	return value + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySignedValue checks a signature and returns the value. The comparison is
// constant-time, which matters here more than most places: an attacker can
// submit as many candidate signatures as they like.
func (s *Sealer) VerifySignedValue(signed string) (string, bool) {
	dot := strings.LastIndex(signed, ".")
	if dot <= 0 {
		return "", false
	}

	value, sig := signed[:dot], signed[dot+1:]

	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(value))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", false
	}

	return value, true
}
