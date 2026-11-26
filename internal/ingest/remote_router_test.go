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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// routingServer publishes one mutable shard contribution and can be made
// unavailable without discarding the previous contribution in the ingester.
type routingServer struct {
	failed  atomic.Bool
	shard   int
	site    sites.Site
	allowed []string
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
	_ = json.NewEncoder(w).Encode(DomainsResponse{Shard: s.shard, Sites: []RoutedSite{{Site: s.site, AllowedHostnames: s.allowed}}})
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

// TestRemoteRouterAllowsTheHostnamesTheShardPublished checks an ingester's
// early hostname check reads the site's hostname rules from the routing
// snapshot, and keeps them across a restart from the disk cache, so its debug
// answer agrees with the writer.
func TestRemoteRouterAllowsTheHostnamesTheShardPublished(t *testing.T) {
	server := &routingServer{
		shard:   1,
		site:    sites.Site{ID: 4, AccountID: 10, Domain: "known.example"},
		allowed: []string{"checkout.partner.example"},
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	db := openRoutingDB(t)
	signer := &InternalSigner{Key: "secret"}
	router, err := NewRemoteRouter(context.Background(), db, []string{httpServer.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if router.AllowsHostname(4, "checkout.partner.example") {
		t.Fatal("a hostname was allowed before any shard published it")
	}
	if err := router.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	for hostname, want := range map[string]bool{
		"checkout.partner.example":     true,
		"www.Checkout.Partner.example": true,
		"other.partner.example":        false,
	} {
		if got := router.AllowsHostname(4, hostname); got != want {
			t.Errorf("AllowsHostname(%q) = %v, want %v", hostname, got, want)
		}
	}
	if router.AllowsHostname(5, "checkout.partner.example") {
		t.Fatal("one site's allowed hostname was applied to another site")
	}

	restarted, err := NewRemoteRouter(context.Background(), db, []string{httpServer.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.AllowsHostname(4, "checkout.partner.example") {
		t.Fatal("the allowed hostname was lost across a restart from the disk cache")
	}
}

// growingServer publishes a shard's site list that can gain a site between
// polls, and counts the polls, so a test can see how many a miss caused.
type growingServer struct {
	mu    sync.Mutex
	list  []sites.Site
	polls atomic.Int64
}

// add publishes one more site from the next poll on.
func (s *growingServer) add(site sites.Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, site)
}

// ServeHTTP returns the current list as shard one's routing snapshot.
func (s *growingServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.polls.Add(1)
	s.mu.Lock()
	response := DomainsResponse{Shard: 1}
	for _, site := range s.list {
		response.Sites = append(response.Sites, RoutedSite{Site: site})
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// newGrowingRouter starts a growing shard with one site and a router that has
// already polled it once, so its map is complete.
func newGrowingRouter(t *testing.T) (*growingServer, *RemoteRouter) {
	t.Helper()
	server := &growingServer{list: []sites.Site{{ID: 1, AccountID: 10, Domain: "first.example"}}}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	router, err := NewRemoteRouter(context.Background(), openRoutingDB(t), []string{httpServer.URL}, &InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !router.Complete() {
		t.Fatal("routing map is incomplete after its only shard succeeded")
	}

	return server, router
}

// TestRemoteRouterConfirmFindsASiteAddedAfterThePoll is the first visit to a
// new site: the app shard knows the site, this ingester's last poll did not,
// and the event must be counted rather than dropped as unknown.
func TestRemoteRouterConfirmFindsASiteAddedAfterThePoll(t *testing.T) {
	server, router := newGrowingRouter(t)
	server.add(sites.Site{ID: 2, AccountID: 10, Domain: "new.example"})

	if _, ok := router.Lookup("new.example"); ok {
		t.Fatal("the new site was known before any poll published it")
	}
	site, ok := router.Confirm(context.Background(), "WWW.New.example")
	if !ok || site.ID != 2 {
		t.Fatalf("Confirm = %+v, %v; want the new site", site, ok)
	}
	if _, ok := router.Lookup("new.example"); !ok {
		t.Fatal("the poll a miss started did not publish the new site")
	}
}

// TestRemoteRouterConfirmCoolsDownADomainNobodyOwns checks that a junk domain
// costs one poll per cooldown, not one per event.
func TestRemoteRouterConfirmCoolsDownADomainNobodyOwns(t *testing.T) {
	server, router := newGrowingRouter(t)
	now := time.Now().UTC()
	router.Now = func() time.Time { return now }

	before := server.polls.Load()
	if _, ok := router.Confirm(context.Background(), "junk.example"); ok {
		t.Fatal("a domain no shard owns was confirmed")
	}
	if got := server.polls.Load() - before; got != 1 {
		t.Fatalf("the first miss caused %d polls, want 1", got)
	}

	for range 5 {
		if _, ok := router.Confirm(context.Background(), "junk.example"); ok {
			t.Fatal("a domain no shard owns was confirmed")
		}
	}
	if got := server.polls.Load() - before; got != 1 {
		t.Fatalf("repeat misses inside the cooldown caused %d polls, want 1", got)
	}

	// Once the cooldown passes, the domain is due another look. A site added
	// in the meantime is found by it.
	server.add(sites.Site{ID: 3, AccountID: 10, Domain: "junk.example"})
	now = now.Add(missCooldown)
	if _, ok := router.Confirm(context.Background(), "junk.example"); !ok {
		t.Fatal("a domain added after its cooldown was not found")
	}
}

// TestRemoteRouterConfirmSharesOnePoll checks that a burst of misses, such as
// a page's first pageview and its engagement event arriving together, waits
// on one poll instead of starting one each.
func TestRemoteRouterConfirmSharesOnePoll(t *testing.T) {
	server, router := newGrowingRouter(t)
	server.add(sites.Site{ID: 2, AccountID: 10, Domain: "new.example"})

	// The gap since the setup poll is waited out first, so every caller below
	// arrives while the one poll they share is due or running.
	time.Sleep(missRefreshGap)
	before := server.polls.Load()

	var group sync.WaitGroup
	var found atomic.Int64
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, ok := router.Confirm(context.Background(), "new.example"); ok {
				found.Add(1)
			}
		}()
	}
	group.Wait()

	if found.Load() != 20 {
		t.Fatalf("%d of 20 concurrent misses found the new site", found.Load())
	}
	if got := server.polls.Load() - before; got != 1 {
		t.Fatalf("20 concurrent misses caused %d polls, want 1", got)
	}
}
