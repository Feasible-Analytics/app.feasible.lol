//
// sites.go
// The domain-to-site routing map, held in memory and swapped whole.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package sites is the routing table the ingest path reads on every event: a
// domain in, an account and a site out. It is a package rather than a query
// because of one hard rule — nothing on the ingest hot path may touch
// control.db. A per-event lookup against a shared SQLite file would put the
// busiest path in the system behind the same write lock the dashboard, billing
// and the job queue contend for.
//
// The map is therefore a snapshot: built by a reader that runs on a timer, and
// swapped in whole. A lookup is one atomic load and one map read.
package sites

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// RefreshInterval is how often the snapshot is rebuilt. Fifteen seconds is the
// gap between adding a site in the dashboard and its first event being
// accepted, and it is short enough that nobody files a bug about it.
const RefreshInterval = 15 * time.Second

// Site is everything the ingest path needs to know about one site. It carries
// only the decisions that can be made without opening the account database,
// because that is the boundary the ingest tier lives on.
type Site struct {
	ID        int64
	AccountID int64
	Domain    string
	Timezone  string

	// AcceptTrafficUntil is when we stop accepting events for a lapsed
	// account, as unix seconds; zero means no limit. Dropping a paying
	// customer's traffic the instant a card fails loses data they can never get
	// back, so a lapse costs dashboard access first and ingestion much later.
	AcceptTrafficUntil int64
}

// Cache holds the current snapshot. The zero value is not usable — the map has
// to be built before it can be read — so callers go through New.
type Cache struct {
	db   *sql.DB
	snap atomic.Pointer[snapshot]
}

// snapshot is one immutable build of the routing map. Replacing the whole thing
// rather than mutating a shared map is what makes a lookup lock-free: a reader
// either sees the old map or the new one, never a half-updated one.
type snapshot struct {
	byDomain map[string]Site
	builtAt  time.Time
}

// New builds an empty cache over the control database. Nothing is loaded yet,
// so a process can construct the cache before it has decided whether it will
// serve traffic.
func New(db *sql.DB) *Cache {
	cache := &Cache{db: db}
	cache.snap.Store(&snapshot{byDomain: map[string]Site{}})

	return cache
}

// Refresh rebuilds the snapshot from control.db. It reads every site in one
// query because the alternative — an incremental update — needs change
// tracking that would have to be correct across restores and manual edits, for
// a table that holds a few thousand rows.
func (c *Cache) Refresh(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
		SELECT sites.id, sites.account_id, sites.domain, sites.timezone,
		       COALESCE(teams.accept_traffic_until, 0)
		FROM sites
		JOIN teams ON teams.id = sites.account_id
	`)
	if err != nil {
		return fmt.Errorf("sites: refresh: %w", err)
	}
	defer rows.Close()

	byDomain := map[string]Site{}

	for rows.Next() {
		var site Site

		if err := rows.Scan(&site.ID, &site.AccountID, &site.Domain, &site.Timezone, &site.AcceptTrafficUntil); err != nil {
			return fmt.Errorf("sites: refresh: %w", err)
		}

		byDomain[Normalise(site.Domain)] = site
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sites: refresh: %w", err)
	}

	c.snap.Store(&snapshot{byDomain: byDomain, builtAt: time.Now()})

	return nil
}

// Lookup resolves a domain. It is the single hottest read in the system and is
// deliberately trivial: an atomic load and a map read, with no locks, no
// allocation and no I/O.
func (c *Cache) Lookup(domain string) (Site, bool) {
	site, ok := c.snap.Load().byDomain[Normalise(domain)]

	return site, ok
}

// Set puts a site into the current snapshot without reading the database. It
// exists for tests and for the single-process path that has just created a
// site and should not have to wait out a refresh interval for its first event.
func (c *Cache) Set(site Site) {
	current := c.snap.Load()

	byDomain := make(map[string]Site, len(current.byDomain)+1)
	for domain, existing := range current.byDomain {
		byDomain[domain] = existing
	}
	byDomain[Normalise(site.Domain)] = site

	c.snap.Store(&snapshot{byDomain: byDomain, builtAt: current.builtAt})
}

// Domains lists every domain in the snapshot. The tracker's per-site script
// paths are an opaque token derived from the domain, and reading one back means
// deriving the token for each known domain — which needs the list, not a
// lookup. It returns a fresh slice so a caller cannot mutate the snapshot every
// event reads.
func (c *Cache) Domains() []string {
	current := c.snap.Load()

	domains := make([]string, 0, len(current.byDomain))
	for domain := range current.byDomain {
		domains = append(domains, domain)
	}

	return domains
}

// Len reports how many sites the snapshot holds. A routing map that suddenly
// went empty looks exactly like every customer stopping at once, so the count
// belongs on the health panel.
func (c *Cache) Len() int {
	return len(c.snap.Load().byDomain)
}

// BuiltAt reports when the snapshot was made, so a stalled refresh is visible
// rather than merely quiet.
func (c *Cache) BuiltAt() time.Time {
	return c.snap.Load().builtAt
}

// Run refreshes on a ticker until the context is cancelled.
func (c *Cache) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Refresh(ctx); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// Normalise puts a domain into the one form the map is keyed by. A tracker
// snippet that says "WWW.Example.com" and a site registered as "example.com"
// are the same site, and treating them as two is a silent, total data loss for
// whichever one is not in the map.
//
// It is exported because the visitor fingerprint has to hash this exact string
// rather than the raw payload field. Hashing the raw one gives a site whose
// pages disagree about the spelling of their own domain a different visitor id
// per spelling, and no later job can put those visitors back together.
func Normalise(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimSuffix(domain, ".")
	domain = strings.TrimPrefix(domain, "www.")

	return domain
}
