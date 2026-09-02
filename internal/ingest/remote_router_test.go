//
// remote_router_test.go
// Failure and restart coverage for the ingester's app-shard routing map.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// routingServer publishes one mutable shard contribution and can be made
// unavailable without discarding the previous contribution in the ingester.
type routingServer struct {
	failed atomic.Bool
	shard  int
	site   sites.Site
}

// ServeHTTP returns a signed-request-aware routing snapshot for one shard.
func (s *routingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(internalTimeHeader) == "" || r.Header.Get(internalSigHeader) == "" {
		http.Error(w, "missing signature", http.StatusUnauthorized)
		return
	}
	if s.failed.Load() {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"route"`)
	_ = json.NewEncoder(w).Encode(DomainsResponse{Shard: s.shard, Sites: []RoutedSite{{Site: s.site}}})
}

// openRoutingDB opens a temporary SQLite handle for routing tests.
func openRoutingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "buffer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRemoteRouterRetainsFailedShardAndRestoresDiskCache verifies that a poll
// failure and an ingester restart do not turn known customer domains unknown.
func TestRemoteRouterRetainsFailedShardAndRestoresDiskCache(t *testing.T) {
	first := &routingServer{shard: 1, site: sites.Site{ID: 1, AccountID: 10, Domain: "one.example"}}
	second := &routingServer{shard: 2, site: sites.Site{ID: 2, AccountID: 20, Domain: "two.example"}}
	firstHTTP, secondHTTP := httptest.NewServer(first), httptest.NewServer(second)
	defer firstHTTP.Close()
	defer secondHTTP.Close()

	db := openRoutingDB(t)
	now := time.Now().UTC()
	signer := &InternalSigner{Key: "secret", Now: func() time.Time { return now }}
	router, err := NewRemoteRouter(context.Background(), db, []string{firstHTTP.URL, secondHTTP.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	router.Now = func() time.Time { return now }
	if err := router.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !router.Complete() {
		t.Fatal("routing map is incomplete after every shard succeeded")
	}
	if _, ok := router.Lookup("two.example"); !ok {
		t.Fatal("second shard's domain is missing")
	}

	second.failed.Store(true)
	if err := router.RefreshAll(context.Background()); err == nil {
		t.Fatal("failed shard poll was not reported")
	}
	if _, ok := router.Lookup("two.example"); !ok {
		t.Fatal("failed poll discarded the last successful shard contribution")
	}

	now = now.Add(routingFreshness + time.Second)
	if router.Complete() {
		t.Fatal("stale shard contribution still marks the map complete")
	}
	if unknown, ok := router.Lookup("unknown.example"); !ok || unknown.Domain != "unknown.example" {
		t.Fatalf("unknown domain was not held while map incomplete: %+v, %v", unknown, ok)
	}

	restarted, err := NewRemoteRouter(context.Background(), db, []string{firstHTTP.URL, secondHTTP.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.Lookup("two.example"); !ok {
		t.Fatal("disk-cached route was not restored after restart")
	}
	if restarted.Complete() {
		t.Fatal("disk cache incorrectly claimed live completeness after restart")
	}
}

// TestRemoteRouterDropsUnknownOnlyWhenComplete verifies that silence from one
// configured shard is the difference between holding and rejecting a domain.
func TestRemoteRouterDropsUnknownOnlyWhenComplete(t *testing.T) {
	server := &routingServer{shard: 1, site: sites.Site{ID: 1, AccountID: 10, Domain: "known.example"}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	db := openRoutingDB(t)
	signer := &InternalSigner{Key: "secret"}
	router, err := NewRemoteRouter(context.Background(), db, []string{httpServer.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := router.Lookup("unknown.example"); !ok {
		t.Fatal("cold incomplete map rejected unknown domain")
	}
	if err := router.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := router.Lookup("unknown.example"); ok {
		t.Fatal("complete map accepted a domain no shard owns")
	}
}

// TestRemoteRouterRejectsMisorderedShardIdentity verifies that changing URL
// order cannot silently redirect an existing outbox partition to another app.
func TestRemoteRouterRejectsMisorderedShardIdentity(t *testing.T) {
	server := &routingServer{shard: 2, site: sites.Site{ID: 2, AccountID: 20, Domain: "wrong.example"}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	router, err := NewRemoteRouter(context.Background(), openRoutingDB(t), []string{httpServer.URL},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.RefreshAll(context.Background()); err == nil {
		t.Fatal("position one accepted app shard id two")
	}
	if router.DestinationReady(0) {
		t.Fatal("misordered destination was enabled for delivery")
	}
}
