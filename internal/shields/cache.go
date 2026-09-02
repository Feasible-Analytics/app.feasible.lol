//
// cache.go
// The rule snapshot every running process evaluates against.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// Cache holds the compiled rules for every site this process serves. It is a
// snapshot swapped whole for the same reason the routing map is: an event
// lookup is one atomic load and one map read, with no lock and no I/O on a path
// that runs for every request on the box.
type Cache struct {
	sites    *sites.Cache
	accounts *accounts.Manager

	// Rejections reads hostname facts already committed by the writer for the
	// settings page's one-click allow flow.
	Rejections *Rejections

	snap atomic.Pointer[snapshot]
}

// snapshot is one immutable build of the rules.
type snapshot struct {
	bySite  map[int64]*Ruleset
	builtAt time.Time
}

// New builds an empty cache. Nothing is read until Refresh runs, so a process
// can construct it before it has decided whether it will serve traffic.
func New(siteCache *sites.Cache, manager *accounts.Manager) *Cache {
	cache := &Cache{sites: siteCache, accounts: manager}
	cache.snap.Store(&snapshot{bySite: map[int64]*Ruleset{}})

	return cache
}

// Refresh rebuilds the snapshot. It reads one query per account rather than one
// per site: a team with forty sites is one read, and the rule tables are small
// enough that reading all of them is cheaper than working out which changed.
func (c *Cache) Refresh(ctx context.Context) error {
	byAccount := map[int64][]int64{}
	for _, site := range c.sites.All() {
		byAccount[site.AccountID] = append(byAccount[site.AccountID], site.ID)
	}

	bySite := map[int64]*Ruleset{}
	rulesBySite := map[int64][]Rule{}

	for accountID := range byAccount {
		lease, err := c.accounts.Acquire(ctx, accountID)
		if err != nil {
			// One unreadable account must not blank every other account's
			// rules. Skipping keeps the previous snapshot's entries for it,
			// which is the safe direction: rules stay applied.
			return fmt.Errorf("shields: refresh account %d: %w", accountID, err)
		}

		rules, err := allRules(ctx, lease.Account.Reader())
		if err != nil {
			_ = lease.Release()
			return err
		}
		if err := lease.Release(); err != nil {
			return fmt.Errorf("shields: release account %d: %w", accountID, err)
		}

		for siteID, list := range rules {
			rulesBySite[siteID] = list
		}
	}

	for _, site := range c.sites.All() {
		bySite[site.ID] = CompileFor(site.Domain, rulesBySite[site.ID])
	}

	c.snap.Store(&snapshot{bySite: bySite, builtAt: time.Now()})

	return nil
}

// allRules reads every site's rules from one account database.
func allRules(ctx context.Context, db *sql.DB) (map[int64][]Rule, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, site_id, kind, value, note, created_at FROM shield_rules")
	if err != nil {
		return nil, fmt.Errorf("shields: read rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	bySite := map[int64][]Rule{}

	for rows.Next() {
		var rule Rule
		if err := rows.Scan(&rule.ID, &rule.SiteID, &rule.Kind, &rule.Value, &rule.Note, &rule.CreatedAt); err != nil {
			return nil, fmt.Errorf("shields: read rules: %w", err)
		}

		bySite[rule.SiteID] = append(bySite[rule.SiteID], rule)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shields: read rules: %w", err)
	}

	return bySite, nil
}

// Set replaces one site's rules in the current snapshot without reading the
// database. It exists for tests and for the settings page, which has just saved
// a rule and should not make the customer wait out a refresh interval to see it
// take effect.
func (c *Cache) Set(siteID int64, rules []Rule) {
	current := c.snap.Load()

	bySite := make(map[int64]*Ruleset, len(current.bySite)+1)
	for id, set := range current.bySite {
		bySite[id] = set
	}
	domain := ""
	for _, site := range c.sites.All() {
		if site.ID == siteID {
			domain = site.Domain
			break
		}
	}
	bySite[siteID] = CompileFor(domain, rules)

	c.snap.Store(&snapshot{bySite: bySite, builtAt: current.builtAt})
}

// Ruleset returns one site's compiled rules, or nil when it has none.
func (c *Cache) Ruleset(siteID int64) *Ruleset {
	return c.snap.Load().bySite[siteID]
}

// BlockedIPPrefixes returns the canonical IP rules that an app shard publishes
// to ingesters before they discard visitor addresses.
func (c *Cache) BlockedIPPrefixes(siteID int64) []string {
	return c.Ruleset(siteID).BlockedIPPrefixes()
}

// BuiltAt reports when the snapshot was made, so a stalled refresh is visible
// rather than merely quiet.
func (c *Cache) BuiltAt() time.Time {
	return c.snap.Load().builtAt
}

// Blocked implements the ingest tier's IP shield. This runs in the one place
// the raw address still exists, which is why the IP rule kind cannot be
// evaluated anywhere else in the system.
func (c *Cache) Blocked(siteID int64, addr netip.Addr) bool {
	return c.snap.Load().bySite[siteID].BlocksIP(addr)
}

// Allowed implements the account-writer shield: country, page and hostname.
func (c *Cache) Allowed(siteID int64, hostname, pathname, country string) (bool, string) {
	set := c.snap.Load().bySite[siteID]
	if set.Empty() {
		return true, ""
	}

	return set.Allowed(hostname, pathname, country)
}

// AllowsHostname performs the same hostname check as the writer without
// recording a rejection.
func (c *Cache) AllowsHostname(siteID int64, hostname string) bool {
	return c.snap.Load().bySite[siteID].HostnameAllowed(hostname)
}

// Run refreshes on a ticker until the context is cancelled. The interval
// matches the site-cache refresh, so rules and newly added sites propagate on
// the same bounded schedule.
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

// These assertions are the contract between this package and the ingest tier.
// A signature change on either side is a compile error here rather than a
// shield that silently stops being consulted.
var (
	_ ingest.IPShield    = (*Cache)(nil)
	_ ingest.ShardShield = (*Cache)(nil)
)

// Viewer is the address the settings page shows the customer, with enough
// evidence for them to tell a working proxy from a broken one.
type Viewer struct {
	// Address is the resolved public IP, as text. Empty when nothing could be
	// resolved at all.
	Address string

	// Source names the header it came from, using the same vocabulary the
	// ingest debug endpoint uses.
	Source string

	// Private reports that the address is not routable on the internet. This is
	// the self-hosting trap: behind a reverse proxy that does not forward
	// X-Forwarded-For, every visitor resolves to the proxy, and the settings
	// page would otherwise show the customer their own router's LAN address —
	// 192.168.178.1 — and cheerfully let them build a rule on it that blocks
	// every visitor sharing that proxy address.
	Private bool

	// Warning names the catalogue string to show when Private is true. It is an
	// id rather than the sentence, because this package has no request and so
	// no language, and because the copy a customer reads belongs in the one
	// catalogue. The string it names gives the fix rather than the symptom:
	// nobody can act on "this looks like a private address".
	Warning string
}

// ResolveViewer works out the address of whoever is looking at the settings
// page, so that "block my own traffic" is one click rather than a hunt through
// a third-party site. It resolves through exactly the same precedence the
// ingest tier uses, because an address resolved a different way here would be a
// rule that does not match the traffic it was created from.
func ResolveViewer(r *http.Request, trusted *clientip.TrustedProxies) Viewer {
	client := clientip.ResolveClientIP(r, trusted)

	viewer := Viewer{Address: client.String(), Source: client.Source}

	if !client.Addr.IsValid() {
		viewer.Warning = "auth.shields.warning_unresolved"
		viewer.Private = true

		return viewer
	}

	if clientip.IsPrivateOrLocal(client.Addr) {
		viewer.Private = true
		viewer.Warning = "auth.shields.warning_private"
	}

	return viewer
}
