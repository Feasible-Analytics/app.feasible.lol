//
// main_test.go
// Tests for the test-site static handler.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandlerSubstitutesBaseURL is the reason this tool exists rather than a
// plain file server: over Tailscale the snippet has to point at the MagicDNS
// hostname, and a page hard-coding localhost would silently send nothing.
func TestHandlerSubstitutesBaseURL(t *testing.T) {
	dir := t.TempDir()
	page := `<script src="` + baseURLPlaceholder + `/js/f.js"></script>`

	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler(dir, "http://rager.example.ts.net:19300").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if strings.Contains(body, baseURLPlaceholder) {
		t.Fatalf("placeholder survived: %q", body)
	}
	if !strings.Contains(body, "http://rager.example.ts.net:19300/js/f.js") {
		t.Fatalf("base URL not substituted: %q", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("pages must not be cached; the point is to reload and watch the tracker fire")
	}
}

// TestHandlerRefusesEscapingPaths checks a request cannot read outside the
// served directory. It is a throwaway tool, but it is one that gets pointed at a
// repository checkout.
func TestHandlerRefusesEscapingPaths(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler(dir, "http://localhost:19300").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/../secret.txt", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}
