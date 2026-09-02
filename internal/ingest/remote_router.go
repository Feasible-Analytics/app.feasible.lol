//
// remote_router.go
// Shard-pull routing for standalone store-and-forward ingesters.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

const routingFreshness = 60 * time.Second

// RemoteRouter merges the domain lists independently published by every
// configured app shard. Failed polls retain the last successful contribution.
type RemoteRouter struct {
	Shards []string
	Cache  *sites.Cache
	DB     *sql.DB
	Client *http.Client
	Signer *InternalSigner
	Now    func() time.Time

	mu          sync.RWMutex
	byShard     map[int]map[string]RoutedSite
	etags       map[int]string
	lastSuccess map[int]time.Time
	account     map[int64]int
	blocked     map[int64][]netip.Prefix
}

// NewRemoteRouter loads the last disk-cached map before any network poll. A
// restarted ingester can therefore keep accepting known domains while an app
// shard is unavailable.
func NewRemoteRouter(ctx context.Context, db *sql.DB, shards []string, signer *InternalSigner) (*RemoteRouter, error) {
	router := &RemoteRouter{
		Shards: shards, Cache: sites.NewEmpty(), DB: db, Signer: signer,
		byShard: map[int]map[string]RoutedSite{}, etags: map[int]string{},
		lastSuccess: map[int]time.Time{}, account: map[int64]int{}, blocked: map[int64][]netip.Prefix{},
	}
	if err := router.createSchema(ctx); err != nil {
		return nil, err
	}
	if err := router.load(ctx); err != nil {
		return nil, err
	}

	return router, nil
}

// Lookup resolves a known domain. While any configured shard is silent, an
// unknown claim becomes an unrouted placeholder rather than a destructive drop.
func (r *RemoteRouter) Lookup(domain string) (sites.Site, bool) {
	if site, ok := r.Cache.Lookup(domain); ok {
		return site, true
	}
	if !r.Complete() {
		return sites.Site{Domain: sites.Normalise(domain)}, true
	}

	return sites.Site{}, false
}

// Shard resolves an account to its current owner. Account zero is the
// placeholder used for an unknown domain accepted under an incomplete map.
func (r *RemoteRouter) Shard(accountID int64) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if accountID == 0 {
		return -1, !r.completeLocked(r.clock())
	}
	shard, ok := r.account[accountID]

	return shard, ok
}

// Complete reports whether every configured shard has checked in recently.
func (r *RemoteRouter) Complete() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.completeLocked(r.clock())
}

// DestinationReady reports whether this configured list position has recently
// proven it serves the matching one-based app shard identity.
func (r *RemoteRouter) DestinationReady(shard int) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	last := r.lastSuccess[shard]

	return !last.IsZero() && r.clock().Sub(last) <= routingFreshness
}

// Blocked applies the IP prefixes delivered with one site's routing record.
func (r *RemoteRouter) Blocked(siteID int64, addr netip.Addr) bool {
	if siteID == 0 || !addr.IsValid() {
		return false
	}
	r.mu.RLock()
	prefixes := r.blocked[siteID]
	r.mu.RUnlock()
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// AllowsHostname reports only additive hostname entries; the pipeline performs
// the registered-domain and subdomain rule before consulting this method.
func (r *RemoteRouter) AllowsHostname(siteID int64, hostname string) bool {
	if siteID == 0 {
		return false
	}
	for _, site := range r.Cache.All() {
		if site.ID != siteID {
			continue
		}
		hostname = sites.Normalise(hostname)
		for _, allowed := range site.AllowedHostnames {
			if sites.Normalise(allowed) == hostname {
				return true
			}
		}
	}

	return false
}

// ResolveEvent updates an unrouted or stale event from the latest merged map.
func (r *RemoteRouter) ResolveEvent(event *Event) (resolved bool, absent bool) {
	site, ok := r.Cache.Lookup(event.Domain)
	if !ok {
		return false, r.Complete()
	}
	shard, ok := r.Shard(site.AccountID)
	if !ok {
		return false, false
	}
	event.SiteID = site.ID
	event.AccountID = site.AccountID
	event.Shard = shard
	event.Domain = sites.Normalise(site.Domain)

	return true, false
}

// RefreshAll polls every shard independently. One failure is returned for
// visibility without discarding any previously successful shard contribution.
func (r *RemoteRouter) RefreshAll(ctx context.Context) error {
	var first error
	for shard := range r.Shards {
		if err := r.refreshShard(ctx, shard); err != nil && first == nil {
			first = err
		}
	}

	return first
}

// Refresh implements the pipeline resolver lifecycle with the same all-shard
// poll used by the background loop.
func (r *RemoteRouter) Refresh(ctx context.Context) error {
	return r.RefreshAll(ctx)
}

// Run polls immediately and then at the documented fifteen-second interval.
func (r *RemoteRouter) Run(ctx context.Context, onError func(error)) {
	if err := r.RefreshAll(ctx); err != nil && onError != nil {
		onError(err)
	}
	ticker := time.NewTicker(sites.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.RefreshAll(ctx); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// refreshShard replaces only one shard's successful snapshot and persists it
// atomically before publishing the merged routing map.
func (r *RemoteRouter) refreshShard(ctx context.Context, shard int) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(r.Shards[shard], "/")+InternalDomainsPath, nil)
	if err != nil {
		return fmt.Errorf("routing shard %d: build request: %w", shard, err)
	}
	r.mu.RLock()
	etag := r.etags[shard]
	validated := !r.lastSuccess[shard].IsZero()
	r.mu.RUnlock()
	if etag != "" && validated {
		request.Header.Set("If-None-Match", etag)
	}
	if err := r.Signer.Sign(request, nil); err != nil {
		return fmt.Errorf("routing shard %d: %w", shard, err)
	}

	response, err := r.client().Do(request)
	if err != nil {
		return fmt.Errorf("routing shard %d: %w", shard, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		r.mu.Lock()
		r.lastSuccess[shard] = r.clock()
		r.mu.Unlock()
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("routing shard %d: app returned %s", shard, response.Status)
	}

	var payload DomainsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return fmt.Errorf("routing shard %d: decode: %w", shard, err)
	}
	if payload.Shard != shard+1 {
		return fmt.Errorf("routing position %d expected app shard id %d, got %d", shard, shard+1, payload.Shard)
	}
	byDomain := make(map[string]RoutedSite, len(payload.Sites))
	for _, routed := range payload.Sites {
		byDomain[sites.Normalise(routed.Site.Domain)] = routed
	}
	if err := r.persistShard(ctx, shard, response.Header.Get("ETag"), byDomain); err != nil {
		return err
	}

	r.mu.Lock()
	r.byShard[shard] = byDomain
	r.etags[shard] = response.Header.Get("ETag")
	r.lastSuccess[shard] = r.clock()
	r.rebuildLocked()
	r.mu.Unlock()

	return nil
}

// createSchema creates the routing cache inside the ingester-owned database.
func (r *RemoteRouter) createSchema(ctx context.Context) error {
	_, err := r.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS routing_cache (
			shard_id INTEGER NOT NULL,
			domain TEXT NOT NULL,
			payload BLOB NOT NULL,
			etag TEXT NOT NULL DEFAULT '',
			refreshed_at INTEGER NOT NULL,
			PRIMARY KEY (shard_id, domain)
		)`)
	if err != nil {
		return fmt.Errorf("routing cache schema: %w", err)
	}

	return nil
}

// load restores disk-cached routes but deliberately leaves every shard stale
// until a live poll succeeds.
func (r *RemoteRouter) load(ctx context.Context) error {
	rows, err := r.DB.QueryContext(ctx, "SELECT shard_id, domain, payload, etag FROM routing_cache ORDER BY shard_id, domain")
	if err != nil {
		return fmt.Errorf("routing cache load: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var shard int
		var domain, etag string
		var payload []byte
		if err := rows.Scan(&shard, &domain, &payload, &etag); err != nil {
			return fmt.Errorf("routing cache load: %w", err)
		}
		var routed RoutedSite
		if err := json.Unmarshal(payload, &routed); err != nil {
			return fmt.Errorf("routing cache decode %s: %w", domain, err)
		}
		if r.byShard[shard] == nil {
			r.byShard[shard] = map[string]RoutedSite{}
		}
		r.byShard[shard][domain] = routed
		r.etags[shard] = etag
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("routing cache load: %w", err)
	}
	r.mu.Lock()
	r.rebuildLocked()
	r.mu.Unlock()

	return nil
}

// persistShard replaces one cached contribution in a single transaction.
func (r *RemoteRouter) persistShard(ctx context.Context, shard int, etag string, routes map[string]RoutedSite) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("routing shard %d: begin cache update: %w", shard, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM routing_cache WHERE shard_id = ?", shard); err != nil {
		return fmt.Errorf("routing shard %d: clear cache: %w", shard, err)
	}
	for domain, routed := range routes {
		payload, err := json.Marshal(routed)
		if err != nil {
			return fmt.Errorf("routing shard %d: encode %s: %w", shard, domain, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO routing_cache (shard_id, domain, payload, etag, refreshed_at) VALUES (?, ?, ?, ?, ?)",
			shard, domain, payload, etag, r.clock().Unix()); err != nil {
			return fmt.Errorf("routing shard %d: cache %s: %w", shard, domain, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("routing shard %d: commit cache: %w", shard, err)
	}

	return nil
}

// rebuildLocked publishes the merged site cache and the two hot lookup maps.
func (r *RemoteRouter) rebuildLocked() {
	all := make([]sites.Site, 0)
	r.account = map[int64]int{}
	r.blocked = map[int64][]netip.Prefix{}
	shards := make([]int, 0, len(r.byShard))
	for shard := range r.byShard {
		shards = append(shards, shard)
	}
	sort.Ints(shards)
	seen := map[string]struct{}{}
	for _, shard := range shards {
		for domain, routed := range r.byShard[shard] {
			if _, duplicate := seen[domain]; duplicate {
				continue
			}
			seen[domain] = struct{}{}
			all = append(all, routed.Site)
			r.account[routed.Site.AccountID] = shard
			for _, value := range routed.BlockedIPs {
				if prefix, err := netip.ParsePrefix(value); err == nil {
					r.blocked[routed.Site.ID] = append(r.blocked[routed.Site.ID], prefix.Masked())
				}
			}
		}
	}
	r.Cache.Replace(all, r.clock())
}

// completeLocked evaluates completeness against the static configured shard
// list, which is the denominator that makes a never-seen shard detectable.
func (r *RemoteRouter) completeLocked(now time.Time) bool {
	if len(r.Shards) == 0 {
		return false
	}
	for shard := range r.Shards {
		if last := r.lastSuccess[shard]; last.IsZero() || now.Sub(last) > routingFreshness {
			return false
		}
	}

	return true
}

// client returns the configured transport or a bounded default.
func (r *RemoteRouter) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}

	return &http.Client{Timeout: 10 * time.Second}
}

// clock returns the injected UTC clock or wall time.
func (r *RemoteRouter) clock() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}

	return time.Now().UTC()
}
