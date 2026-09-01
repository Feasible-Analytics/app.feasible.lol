//
// cache.go
// The compiled rule snapshot the write path applies per event.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package pathclean

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// RefreshInterval matches the routing poll, so a rule saved in the dashboard
// starts applying to new events within one cycle rather than at the next
// restart.
const RefreshInterval = 15 * time.Second

// Cache holds every site's compiled rules. Compiling a regular expression per
// event would be the single most expensive thing on the write path, so the
// snapshot is built on a timer and swapped whole; a lookup is one atomic load
// and one map read.
type Cache struct {
	sites    *sites.Cache
	accounts *accounts.Manager

	snap atomic.Pointer[snapshot]
}

// snapshot is one immutable build.
type snapshot struct {
	bySite  map[int64]*Ruleset
	builtAt time.Time
}

// New builds an empty cache.
func New(siteCache *sites.Cache, manager *accounts.Manager) *Cache {
	cache := &Cache{sites: siteCache, accounts: manager}
	cache.snap.Store(&snapshot{bySite: map[int64]*Ruleset{}})

	return cache
}

// Refresh rebuilds the snapshot, one read per account.
func (c *Cache) Refresh(ctx context.Context) error {
	accountIDs := map[int64]bool{}
	for _, site := range c.sites.All() {
		accountIDs[site.AccountID] = true
	}

	bySite := map[int64]*Ruleset{}

	for accountID := range accountIDs {
		account, err := c.accounts.Open(ctx, accountID)
		if err != nil {
			return fmt.Errorf("pathclean: refresh account %d: %w", accountID, err)
		}

		byID, err := allRules(ctx, account.Reader())
		if err != nil {
			return err
		}

		for siteID, rules := range byID {
			set, err := Compile(rules)
			if err != nil {
				// A stored pattern that will not compile was valid when it was
				// saved, so this means the file was edited by hand. The site is
				// skipped rather than the whole refresh abandoned: one broken
				// rule list must not stop every other site's rules updating.
				continue
			}

			bySite[siteID] = set
		}
	}

	c.snap.Store(&snapshot{bySite: bySite, builtAt: time.Now()})

	return nil
}

// allRules reads every site's rules out of one account database.
func allRules(ctx context.Context, db *sql.DB) (bySite map[int64][]Rule, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, site_id, position, pattern, replacement, label, is_enabled
		FROM path_clean_rules ORDER BY site_id, position`)
	if err != nil {
		return nil, fmt.Errorf("pathclean: read rules: %w", err)
	}
	defer closePathRows(rows, &err, "read rules")

	bySite = map[int64][]Rule{}

	for rows.Next() {
		var rule Rule
		var enabled int

		if err := rows.Scan(&rule.ID, &rule.SiteID, &rule.Position, &rule.Pattern,
			&rule.Replacement, &rule.Label, &enabled); err != nil {
			return nil, fmt.Errorf("pathclean: read rules: %w", err)
		}

		rule.Enabled = enabled == 1
		bySite[rule.SiteID] = append(bySite[rule.SiteID], rule)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pathclean: read rules: %w", err)
	}

	return bySite, nil
}

// Set replaces one site's ruleset in the current snapshot. The settings page
// calls it after a save so the customer sees the rule apply immediately rather
// than after a refresh interval they have no way to know about.
func (c *Cache) Set(siteID int64, set *Ruleset) {
	current := c.snap.Load()

	bySite := make(map[int64]*Ruleset, len(current.bySite)+1)
	for id, existing := range current.bySite {
		bySite[id] = existing
	}
	bySite[siteID] = set

	c.snap.Store(&snapshot{bySite: bySite, builtAt: current.builtAt})
}

// Clean applies one site's rules to a path. It implements the ingest tier's
// PathCleaner, which is how the rules reach the write path without that package
// learning to read an account database.
func (c *Cache) Clean(siteID int64, path string) string {
	return c.snap.Load().bySite[siteID].Clean(path)
}

// BuiltAt reports when the snapshot was made.
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

// The contract with the write path. A signature change on either side is a
// compile error here rather than a cleaner that silently stops being called.
var _ ingest.PathCleaner = (*Cache)(nil)
