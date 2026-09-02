//
// internal_auth_test.go
// Authentication coverage for private ingester-to-app requests.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestInternalAuthenticationAcceptsRotatingKeys verifies that an ingester uses
// the first key while an app can accept an overlapping previous generation.
func TestInternalAuthenticationAcceptsRotatingKeys(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	keys := []InternalKey{{ID: "new", Secret: "new-secret"}, {ID: "old", Secret: "old-secret"}}
	called := false
	handler := VerifyInternal(keys, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	body := []byte(`{"events":[]}`)
	request := httptest.NewRequest(http.MethodPost, InternalIngestPath, bytes.NewReader(body))
	signer := &InternalSigner{Keys: keys[1:], Now: func() time.Time { return now }}
	if err := signer.Sign(request, body); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent || !called {
		t.Fatalf("signed request answered %d, called=%v", recorder.Code, called)
	}
}

// TestInternalAuthenticationRejectsTampering verifies that the signature binds
// the method, path, timestamp, and exact body bytes.
func TestInternalAuthenticationRejectsTampering(t *testing.T) {
	keys := []InternalKey{{ID: "active", Secret: "secret"}}
	body := []byte(`{"events":[]}`)
	request := httptest.NewRequest(http.MethodPost, InternalIngestPath, bytes.NewReader(body))
	if err := (&InternalSigner{Keys: keys}).Sign(request, body); err != nil {
		t.Fatal(err)
	}
	request.Body = http.NoBody

	recorder := httptest.NewRecorder()
	VerifyInternal(keys, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("tampered request reached the private handler")
	})).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tampered request answered %d, want 401", recorder.Code)
	}
}

// TestInternalAuthenticationRejectsOldRequests verifies that a captured request
// cannot be replayed outside the five-minute clock window.
func TestInternalAuthenticationRejectsOldRequests(t *testing.T) {
	keys := []InternalKey{{ID: "active", Secret: "secret"}}
	body := []byte(`{}`)
	request := httptest.NewRequest(http.MethodPost, InternalIngestPath, bytes.NewReader(body))
	signer := &InternalSigner{Keys: keys, Now: func() time.Time { return time.Now().Add(-internalClockSkew - time.Minute) }}
	if err := signer.Sign(request, body); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	VerifyInternal(keys, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("expired request reached the private handler")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired request answered %d, want 401", recorder.Code)
	}
}
