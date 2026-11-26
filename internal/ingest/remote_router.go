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

const (
	// missCooldown is how long a domain that a fresh poll still did not know
	// waits before it can ask for another one. A stale snippet or a junk
	// domain with steady traffic therefore costs one poll per cooldown, not
	// one per event.
	missCooldown = 5 * time.Second

	// missRefreshGap is the least time between two polls started by misses,
	// across every domain, so a flood of made-up domains cannot turn into a
	// flood of requests to the app shards.
	missRefreshGap = time.Second

	// missWaitLimit bounds how long one event waits for that poll before it
	// is dropped as unknown, the same answer it would have had without one.
	missWaitLimit = 3 * time.Second

	// missMemory caps how many confirmed-absent domains are remembered. Past
	// it, a domain that is not in the map is dropped without a second look
	// until old entries expire, which is what happened to every unknown
	// domain before second looks existed.
	missMemory = 10000
)

// RemoteRouter merges the domain lists independently published by every
// configured app shard. Failed polls retain the last successful contribution.
type RemoteRouter struct {
	Shards []string
	Cache  *sites.Cache
	DB     *sql.DB
	Client *http.Client
	Signer *InternalSigner
	Now    func() time.Time

	// refreshMu serialises polls. A miss can start one while the background
	// loop runs its own, and the older of two snapshots must never be the one
	// published last.
	refreshMu sync.Mutex

	// missMu guards the second-look state below.
	missMu      sync.Mutex
	missRunning chan struct{}
	missLast    time.Time
	missDone    time.Time
	missUntil   map[string]time.Time

	mu          sync.RWMutex
	byShard     map[int]map[string]RoutedSite
	etags       map[int]string
	lastSuccess map[int]time.Time
	account     map[int64]int
	blocked     map[int64][]netip.Prefix
	allowed     map[int64]map[string]bool
}

// NewRemoteRouter loads the last disk-cached map before any network poll. A
// restarted ingester can therefore keep accepting known domains while an app
// shard is unavailable.
func NewRemoteRouter(ctx context.Context, db *sql.DB, shards []string, signer *InternalSigner) (*RemoteRouter, error) {
	router := &RemoteRouter{
		Shards: shards, Cache: sites.NewEmpty(), DB: db, Signer: signer,
		byShard: map[int]map[string]RoutedSite{}, etags: map[int]string{},
		lastSuccess: map[int]time.Time{}, account: map[int64]int{}, blocked: map[int64][]netip.Prefix{},
		allowed: map[int64]map[string]bool{}, missUntil: map[string]time.Time{},
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

// Confirm takes a second look at a domain the merged map does not hold before
// the event is dropped as unknown. A site is in its app shard's list the moment
// it is created, but this ingester only learns of it on its next poll, and the
// first visit usually arrives in that gap: the owner pastes the snippet and
// loads their page straight away. One fresh poll closes the gap.
//
// The poll is shared and rate-limited. Concurrent misses wait on one poll,
// polls started by misses are at least missRefreshGap apart, and a domain a
// fresh poll still did not know is not looked at again for missCooldown.
func (r *RemoteRouter) Confirm(ctx context.Context, domain string) (sites.Site, bool) {
	asked := time.Now()
	if site, ok := r.Lookup(domain); ok {
		return site, true
	}

	key := sites.Normalise(domain)
	if key == "" || !r.mayConfirm(key) {
		return sites.Site{}, false
	}

	r.refreshForMiss(ctx, asked)

	site, ok := r.Lookup(domain)
	if !ok {
		r.rememberMiss(key)
	}

	return site, ok
}

// mayConfirm reports whether a domain is due a second look. It also sweeps
// expired entries once the memory is full, and refuses when it is still full
// after that, so a stream of made-up domains cannot grow it without limit.
func (r *RemoteRouter) mayConfirm(key string) bool {
	r.missMu.Lock()
	defer r.missMu.Unlock()

	now := r.clock()
	if until, seen := r.missUntil[key]; seen {
		if now.Before(until) {
			return false
		}
		delete(r.missUntil, key)
	}

	if len(r.missUntil) >= missMemory {
		for domain, until := range r.missUntil {
			if !now.Before(until) {
				delete(r.missUntil, domain)
			}
		}
		if len(r.missUntil) >= missMemory {
			return false
		}
	}

	return true
}

// rememberMiss starts a domain's cooldown after a fresh poll did not know it.
func (r *RemoteRouter) rememberMiss(key string) {
	r.missMu.Lock()
	defer r.missMu.Unlock()

	if len(r.missUntil) < missMemory {
		r.missUntil[key] = r.clock().Add(missCooldown)
	}
}

// refreshForMiss joins the poll a miss already started, or starts one once
// missRefreshGap has passed since the last, and waits at most missWaitLimit for
// it. A miss poll that finished after asked has already answered the caller,
// so none is started for it.
//
// The poll runs in its own goroutine on its own context, so one visitor
// closing the tab cannot cancel it for everyone else waiting, and a background
// poll holding the lock cannot keep an event waiting past the limit. The gap
// is measured on the wall clock rather than the injectable one: it is a real
// wait, and a frozen test clock would turn it into an endless one.
func (r *RemoteRouter) refreshForMiss(ctx context.Context, asked time.Time) {
	ctx, cancel := context.WithTimeout(ctx, missWaitLimit)
	defer cancel()

	for {
		r.missMu.Lock()
		if r.missDone.After(asked) {
			r.missMu.Unlock()
			return
		}
		running := r.missRunning
		if running == nil {
			wait := missRefreshGap - time.Since(r.missLast)
			if wait > 0 {
				r.missMu.Unlock()

				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
					continue
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}

			running = make(chan struct{})
			r.missRunning = running
			r.missLast = time.Now()
			go r.pollForMiss(running)
		}
		r.missMu.Unlock()

		select {
		case <-running:
		case <-ctx.Done():
		}

		return
	}
}

// pollForMiss runs one poll for the misses waiting on it, then releases them.
// A failed poll is not reported here: the background loop reports the same
// shard on its own schedule, and the events behind it are counted as unknown.
func (r *RemoteRouter) pollForMiss(running chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), missWaitLimit)
	defer cancel()

	_ = r.RefreshAll(ctx)

	r.missMu.Lock()
	r.missRunning = nil
	r.missDone = time.Now()
	r.missMu.Unlock()
	close(running)
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

// AllowsHostname reports only the extra hostnames a site allowed; the pipeline
// performs the registered-domain and subdomain rule before consulting this
// method.
func (r *RemoteRouter) AllowsHostname(siteID int64, hostname string) bool {
	if siteID == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.allowed[siteID][sites.Normalise(hostname)]
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
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

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
	defer func() { _ = response.Body.Close() }()
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = tx.Rollback() }()
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

// rebuildLocked publishes the merged site cache and the three hot lookup maps.
func (r *RemoteRouter) rebuildLocked() {
	all := make([]sites.Site, 0)
	r.account = map[int64]int{}
	r.blocked = map[int64][]netip.Prefix{}
	r.allowed = map[int64]map[string]bool{}
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
			for _, hostname := range routed.AllowedHostnames {
				if r.allowed[routed.Site.ID] == nil {
					r.allowed[routed.Site.ID] = map[string]bool{}
				}
				r.allowed[routed.Site.ID][sites.Normalise(hostname)] = true
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
