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
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
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

	// A corrupt file here means every two-factor secret encrypted with it is
	// unreadable, which is why the loader refuses rather than regenerating.
	return store.LoadOrCreateKey(filepath.Join(dataDir, KeyFileName), KeySize, "app key")
}

// Sealer encrypts and authenticates small secrets with the application key. It
// wraps the cipher rather than exposing it so that every caller gets AEAD with
// a fresh nonce, and nobody can reach for the raw block cipher and reuse one.
type Sealer struct {
	aead   cipher.AEAD
	macKey []byte
}

// NewSealer builds the AES-GCM box and derives the signing key.
//
// The two primitives never share material: the MAC key is derived from the
// application key rather than being it. The cipher keeps the raw key because
// it protects stored two-factor secrets, which cannot be re-encrypted without
// every user's cooperation; the signed values are cookies that expire in
// minutes, so their key can be anything derived from the same secret.
func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: app key: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: app key: %w", err)
	}

	macKey, err := hkdf.Key(sha256.New, key, nil, "feasible sign", KeySize)
	if err != nil {
		return nil, fmt.Errorf("auth: app key: %w", err)
	}

	return &Sealer{aead: aead, macKey: macKey}, nil
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
	mac := hmac.New(sha256.New, s.macKey)
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

	mac := hmac.New(sha256.New, s.macKey)
	mac.Write([]byte(value))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", false
	}

	return value, true
}
