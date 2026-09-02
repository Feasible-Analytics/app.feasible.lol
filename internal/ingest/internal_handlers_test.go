//
// internal_handlers_test.go
// Protocol coverage for the app shard's private endpoints.
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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// testBatchWriter records accepted events and acknowledges each UUID.
type testBatchWriter struct {
	events []Event
}

// Write records the batch and reports an exact durable acknowledgment.
func (w *testBatchWriter) Write(_ context.Context, events []Event) ([]uuid.UUID, error) {
	w.events = append(w.events, events...)
	ids := make([]uuid.UUID, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.UUID)
	}
	return ids, nil
}

// testRoutingShields publishes the one rule that must cross the privacy
// boundary before an ingester discards the raw address.
type testRoutingShields struct{}

// BlockedIPPrefixes returns one deterministic network for protocol assertions.
func (testRoutingShields) BlockedIPPrefixes(int64) []string { return []string{"192.0.2.0/24"} }

// TestInternalShardPublishesRouting verifies an ingester can fetch ownership
// without opening the app's system database.
func TestInternalShardPublishesRouting(t *testing.T) {
	cache := sites.NewEmpty()
	cache.Replace([]sites.Site{
		{ID: 1, AccountID: 10, Domain: "active.example"},
		{ID: 2, AccountID: 20, Domain: "expired.example", AcceptTrafficUntil: time.Now().Add(-time.Hour).Unix()},
	}, time.Now())
	shard := &InternalShard{ID: 7, Sites: cache, Shields: testRoutingShields{}, Writer: &testBatchWriter{}}

	routing := httptest.NewRecorder()
	shard.Handler().ServeHTTP(routing, httptest.NewRequest(http.MethodGet, InternalDomainsPath, nil))
	if routing.Code != http.StatusOK || routing.Header().Get("ETag") == "" {
		t.Fatalf("routing answered %d with ETag %q", routing.Code, routing.Header().Get("ETag"))
	}
	var domains DomainsResponse
	if err := json.Unmarshal(routing.Body.Bytes(), &domains); err != nil {
		t.Fatal(err)
	}
	if domains.Shard != 7 || len(domains.Sites) != 1 || domains.Sites[0].Site.Domain != "active.example" || len(domains.Sites[0].BlockedIPs) != 1 {
		t.Fatalf("routing snapshot = %+v", domains)
	}

	notModified := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, InternalDomainsPath, nil)
	request.Header.Set("If-None-Match", routing.Header().Get("ETag"))
	shard.Handler().ServeHTTP(notModified, request)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("matching ETag answered %d, want 304", notModified.Code)
	}
}

// TestInternalShardAcknowledgesOnlyOwnedEvents verifies that stale routing
// cannot make one app shard write another shard's account.
func TestInternalShardAcknowledgesOnlyOwnedEvents(t *testing.T) {
	cache := sites.NewEmpty()
	cache.Replace([]sites.Site{{ID: 1, AccountID: 10, Domain: "owned.example"}}, time.Now())
	writer := &testBatchWriter{}
	shard := &InternalShard{ID: 1, Sites: cache, Writer: writer}
	ownedID, foreignID := uuid.New(), uuid.New()
	body, err := json.Marshal(IngestBatch{Events: []Event{
		{UUID: ownedID, SiteID: 1, AccountID: 10, Domain: "owned.example"},
		{UUID: foreignID, SiteID: 2, AccountID: 20, Domain: "moved.example"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	shard.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, InternalIngestPath, bytes.NewReader(body)))
	var response IngestResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(writer.events) != 1 || writer.events[0].UUID != ownedID || len(response.Committed) != 1 || response.Committed[0] != ownedID {
		t.Fatalf("writer=%+v response=%+v", writer.events, response)
	}
	if len(response.NotMine) != 1 || response.NotMine[0] != "moved.example" {
		t.Fatalf("not_mine = %v", response.NotMine)
	}
}
