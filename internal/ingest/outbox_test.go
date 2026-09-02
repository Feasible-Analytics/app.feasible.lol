//
// outbox_test.go
// Durability and exact-ack coverage for the ingester-owned SQLite outbox.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// testContext returns the non-cancelled context used by synchronous outbox
// operations; Go 1.23 does not yet expose testing.T.Context.
func testContext() context.Context { return context.Background() }

// closeTestOutbox registers a checked close for one temporary outbox.
func closeTestOutbox(t *testing.T, outbox *Outbox) {
	t.Helper()
	t.Cleanup(func() {
		if err := outbox.Close(); err != nil {
			t.Error(err)
		}
	})
}

// outboxDestination is a controllable app shard that records deliveries and
// acknowledges either all events or only the first one.
type outboxDestination struct {
	down       atomic.Bool
	partial    atomic.Bool
	mu         sync.Mutex
	deliveries []Event
}

// TestOutboxTransportReleasesWaiterBeforeAppCommit verifies the hosted 202
// boundary: the in-memory batch waits for buffer.db, not for a reachable app.
func TestOutboxTransportReleasesWaiterBeforeAppCommit(t *testing.T) {
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	buffer := NewBuffer(outbox, 1, time.Hour)
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"}
	if err := buffer.AddAndWait(testContext(), event); err != nil {
		t.Fatal(err)
	}
	if outbox.Len() != 1 {
		t.Fatalf("durable boundary retained %d rows, want 1", outbox.Len())
	}
}

// ServeHTTP publishes one route and accepts private ingest batches.
func (d *outboxDestination) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case InternalDomainsPath:
		_ = json.NewEncoder(w).Encode(DomainsResponse{Shard: 1, Sites: []RoutedSite{{
			Site: sites.Site{ID: 1, AccountID: 10, Domain: "known.example"},
		}}})
	case InternalIngestPath:
		if d.down.Load() {
			http.Error(w, "app down", http.StatusServiceUnavailable)
			return
		}
		var batch IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.deliveries = append(d.deliveries, batch.Events...)
		d.mu.Unlock()
		committed := make([]uuid.UUID, 0, len(batch.Events))
		for _, event := range batch.Events {
			committed = append(committed, event.UUID)
			if d.partial.Load() {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(IngestResponse{Committed: committed})
	default:
		http.NotFound(w, r)
	}
}

// TestOutboxSurvivesAppFailureAndIngesterRestart verifies the central
// availability promise: a local commit earns 202 ownership, app downtime does
// not delete it, and a restarted ingester drains it after the app returns.
func TestOutboxSurvivesAppFailureAndIngesterRestart(t *testing.T) {
	destination := &outboxDestination{}
	destination.down.Store(true)
	server := httptest.NewServer(destination)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "buffer.db")
	signer := &InternalSigner{Key: "secret"}

	outbox, err := OpenOutbox(testContext(), path, []string{server.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example", Name: EventPageview}
	committed, err := outbox.Send(testContext(), 0, []Event{event})
	if err != nil || len(committed) != 1 || outbox.Len() != 1 {
		t.Fatalf("append committed=%v len=%d err=%v", committed, outbox.Len(), err)
	}
	if err := outbox.deliver(testContext(), 0); err == nil || outbox.Len() != 1 {
		t.Fatalf("failed delivery err=%v len=%d", err, outbox.Len())
	}
	if err := outbox.Close(); err != nil {
		t.Fatal(err)
	}

	destination.down.Store(false)
	restarted, err := OpenOutbox(testContext(), path, []string{server.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, restarted)
	if restarted.Len() != 1 {
		t.Fatalf("restart retained %d events, want 1", restarted.Len())
	}
	if err := restarted.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := restarted.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	if restarted.Len() != 0 {
		t.Fatalf("acknowledged event remained in outbox: %d", restarted.Len())
	}
}

// TestOutboxDeletesOnlyExactAcknowledgments verifies that a partial app commit
// cannot lose an unacknowledged neighbor from the same delivery batch.
func TestOutboxDeletesOnlyExactAcknowledgments(t *testing.T) {
	destination := &outboxDestination{}
	destination.partial.Store(true)
	server := httptest.NewServer(destination)
	defer server.Close()
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{server.URL},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	if err := outbox.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"},
		{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"},
	}
	if _, err := outbox.Send(testContext(), 0, events); err != nil {
		t.Fatal(err)
	}
	if err := outbox.deliver(testContext(), 0); err == nil {
		t.Fatal("partial acknowledgment was not reported for retry")
	}
	if outbox.Len() != 1 {
		t.Fatalf("partial acknowledgment left %d rows, want 1", outbox.Len())
	}
}

// TestOutboxPersistsNoRawAddress verifies the privacy boundary against the
// serialized object that actually lands in buffer.db.
func TestOutboxPersistsNoRawAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.db")
	outbox, err := OpenOutbox(testContext(), path, []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example", Country: "US"}
	if _, err := outbox.Send(testContext(), 0, []Event{event, event}); err != nil {
		t.Fatal(err)
	}
	if outbox.Len() != 1 {
		t.Fatalf("duplicate UUID created %d rows, want 1", outbox.Len())
	}
	var payload []byte
	if err := outbox.DB.QueryRow("SELECT payload FROM outbox WHERE event_uuid = ?", event.UUID.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"IP", "ClientIP", "UserAgent", "RawUserAgent"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("raw identity field %q reached buffer.db", forbidden)
		}
	}
	if err := outbox.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOutboxReplaysParkedRows verifies operator review has a supported path
// back to delivery rather than requiring edits to the SQLite file.
func TestOutboxReplaysParkedRows(t *testing.T) {
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"}
	if _, err := outbox.Send(testContext(), 0, []Event{event}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := outbox.DB.QueryRow("SELECT id FROM outbox WHERE event_uuid = ?", event.UUID.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := outbox.parkIDs(testContext(), []int64{id}, "review me"); err != nil {
		t.Fatal(err)
	}
	if outbox.Len() != 0 || outbox.Parked() != 1 {
		t.Fatalf("before replay active=%d parked=%d", outbox.Len(), outbox.Parked())
	}
	count, err := outbox.ReplayParked(testContext())
	if err != nil || count != 1 || outbox.Len() != 1 || outbox.Parked() != 0 {
		t.Fatalf("replay count=%d active=%d parked=%d err=%v", count, outbox.Len(), outbox.Parked(), err)
	}
}
