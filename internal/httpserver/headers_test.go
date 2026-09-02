//
// headers_test.go
// The default headers, and that a handler can still take them back.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityHeadersAreTheDefault checks a handler that does nothing gets the
// safe set, with HSTS following the HTTPS switch.
func TestSecurityHeadersAreTheDefault(t *testing.T) {
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	plain := httptest.NewRecorder()
	SecurityHeaders(false, noop).ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/", nil))

	for name, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if got := plain.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	if plain.Header().Get("Strict-Transport-Security") != "" {
		t.Error("an http listener set HSTS, which would make itself unreachable")
	}

	secure := httptest.NewRecorder()
	SecurityHeaders(true, noop).ServeHTTP(secure, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := secure.Header().Get("Strict-Transport-Security"); got != HSTSMaxAge {
		t.Errorf("Strict-Transport-Security = %q, want %q", got, HSTSMaxAge)
	}
}

// TestAHandlerCanRelaxTheDefaults is what the embeddable share page relies on:
// the middleware must not have the last word.
func TestAHandlerCanRelaxTheDefaults(t *testing.T) {
	embed := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Del("X-Frame-Options")
		w.Header().Set("Content-Security-Policy", "frame-ancestors *")
		w.WriteHeader(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	SecurityHeaders(true, embed).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/share/abc?embed=true", nil))

	if got := recorder.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q after the handler deleted it, so the iframe would be blank", got)
	}

	if got := recorder.Header().Get("Content-Security-Policy"); got != "frame-ancestors *" {
		t.Errorf("Content-Security-Policy = %q, want the handler's value", got)
	}

	if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("relaxing framing dropped the other defaults")
	}
}
