//
// key_test.go
// Tests for the per-site script token.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package tracker

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTokenIsStableAndOpaque covers the two properties the path has to have at
// once: the dashboard must be able to render the same snippet twice, and the
// path must not be something a filter list can compute from the domain.
func TestTokenIsStableAndOpaque(t *testing.T) {
	keyer := NewKeyer(bytes.Repeat([]byte{1}, SecretSize), nil)

	token := keyer.Token("example.com")

	if token != keyer.Token("example.com") {
		t.Fatal("the same domain produced two tokens")
	}

	if len(token) != 16 {
		t.Fatalf("token %q is %d characters, want 16", token, len(token))
	}

	if strings.Contains(token, "example") {
		t.Fatalf("token %q leaks the domain", token)
	}

	other := NewKeyer(bytes.Repeat([]byte{2}, SecretSize), nil)
	if other.Token("example.com") == token {
		t.Fatal("two different secrets produced the same token — rotating would not rename anything")
	}
}

// TestTokenNormalisesLikeTheRoutingMap is the www trap. A site registered as
// example.com whose snippet says www.example.com has to reach the same script,
// or the customer sees a 404 for a snippet that looks entirely correct.
func TestTokenNormalisesLikeTheRoutingMap(t *testing.T) {
	keyer := NewKeyer(bytes.Repeat([]byte{3}, SecretSize), nil)

	want := keyer.Token("example.com")

	for _, variant := range []string{"WWW.Example.com", " example.com ", "example.com."} {
		if got := keyer.Token(variant); got != want {
			t.Errorf("%q derived %q, want %q", variant, got, want)
		}
	}
}

// TestPathRoundTrips is the contract between what the dashboard prints and what
// the server answers: the two are the same string builder, so they cannot drift.
func TestPathRoundTrips(t *testing.T) {
	keyer := NewKeyer(bytes.Repeat([]byte{4}, SecretSize), fakeSites{domains: []string{"example.com"}})

	path := keyer.Path("example.com")

	token, ok := siteToken(path)
	if !ok {
		t.Fatalf("%q is not a per-site path", path)
	}

	domain, ok := keyer.Resolve(token)
	if !ok || domain != "example.com" {
		t.Fatalf("resolved to %q, %v", domain, ok)
	}
}

// TestResolveWithoutSites is the process that serves the script with no
// database behind it. It has to say no rather than panic.
func TestResolveWithoutSites(t *testing.T) {
	keyer := NewKeyer(bytes.Repeat([]byte{5}, SecretSize), nil)

	if _, ok := keyer.Resolve(keyer.Token("example.com")); ok {
		t.Fatal("resolved a token with no routing map")
	}
}

// TestLoadSecretGeneratesOnce is the self-hoster path: configure nothing, get a
// working per-site path, and get the same one after a restart. A secret that
// changed on every boot would silently invalidate every snippet in the world.
func TestLoadSecretGeneratesOnce(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadSecret(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != SecretSize {
		t.Fatalf("secret is %d bytes, want %d", len(first), SecretSize)
	}

	second, err := LoadSecret(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("a second load generated a different secret — every snippet would 404 after a restart")
	}

	info, err := os.Stat(filepath.Join(dir, SecretFileName))
	if err != nil {
		t.Fatal(err)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("secret file mode is %o, want 600", mode)
	}
}

// TestLoadSecretRejectsCorruption. A truncated secret would derive paths that
// resolve to nothing, which reads as "the tracker stopped working" with no
// clue as to why — so it has to be an error with the file name in it.
func TestLoadSecretRejectsCorruption(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, SecretFileName), []byte("nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadSecret(dir)
	if err == nil {
		t.Fatal("a corrupt secret was accepted")
	}

	if !strings.Contains(err.Error(), SecretFileName) {
		t.Fatalf("the error does not name the file: %v", err)
	}
}
