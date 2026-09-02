//
// remote_salts_test.go
// Memory-only remote fingerprint salt coverage.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
)

// TestRemoteSaltsFetchesOnceAndClones verifies that salts are fetched through
// the private protocol, cached only in memory, and isolated per caller.
func TestRemoteSaltsFetchesOnceAndClones(t *testing.T) {
	now := time.Now().UTC()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(internalSigHeader) == "" {
			http.Error(w, "missing signature", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(SaltResponse{
			Current: bytes.Repeat([]byte{7}, salts.Size), Day: salts.Day(now),
		})
	}))
	defer server.Close()

	remote := &RemoteSalts{
		URL: server.URL, Signer: &InternalSigner{Keys: []InternalKey{{ID: "active", Secret: "secret"}}},
		Now: func() time.Time { return now },
	}
	first, err := remote.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first.Current[0] = 0
	first.Erase()
	second, err := remote.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Erase()
	if requests.Load() != 1 || second.Current[0] != 7 {
		t.Fatalf("requests=%d current[0]=%d", requests.Load(), second.Current[0])
	}
}

// TestRemoteSaltsRejectsStaleMaterial verifies that an app cannot accidentally
// distribute yesterday's salt as today's identity authority.
func TestRemoteSaltsRejectsStaleMaterial(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(SaltResponse{
			Current: bytes.Repeat([]byte{7}, salts.Size), Day: salts.Day(now) - 1,
		})
	}))
	defer server.Close()
	remote := &RemoteSalts{
		URL: server.URL, Signer: &InternalSigner{Keys: []InternalKey{{ID: "active", Secret: "secret"}}},
		Now: func() time.Time { return now },
	}
	if _, err := remote.Pair(context.Background()); err == nil {
		t.Fatal("stale salt response was accepted")
	}
}
