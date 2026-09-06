//
// cache.go
// The compiled rule snapshot used to map raw paths as events are stored.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package pathclean

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
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

// FullRefreshInterval is how often every account is re-read whatever its marker
// says, as the backstop against a rule change nothing stamped.
const FullRefreshInterval = time.Hour

// opener is how a refresh reaches an account database. It is an interface so a
// test can count the opens one pass performs, which is the number this refresh
// schedule exists to keep at zero when nothing changed.
type opener interface {
	AcquireForScan(ctx context.Context, id int64) (*accounts.Lease, error)
}

// Cache holds every site's compiled rules. Compiling a regular expression per
// event would be the single most expensive thing on the write path, so the
// snapshot is built on a timer and swapped whole; a lookup is one atomic load
// and one map read.
type Cache struct {
	sites    *sites.Cache
	accounts opener

	// publish serialises the two writers of the snapshot: a refresh that reads
	// it, spends seconds on I/O and writes a new one, and a save from the
	// settings page that writes one in between.
	publish sync.Mutex

	// touched names the sites a save changed while a refresh was in flight.
	touched map[int64]bool

	snap atomic.Pointer[snapshot]
}

// snapshot is one immutable build.
type snapshot struct {
	bySite map[int64]*Ruleset

	// versions is the marker each account was last read at. An account absent
	// from it has never been read, so it cannot be carried forward.
	versions map[int64]int64

	builtAt time.Time
}

// New builds an empty cache.
func New(siteCache *sites.Cache, manager opener) *Cache {
	cache := &Cache{sites: siteCache, accounts: manager, touched: map[int64]bool{}}
	cache.snap.Store(&snapshot{bySite: map[int64]*Ruleset{}, versions: map[int64]int64{}})

	return cache
}

// Refresh rebuilds every account's rules, whatever their marker says. It is the
// first pass at start-up and the hourly backstop against a marker nothing
// stamped.
func (c *Cache) Refresh(ctx context.Context) error {
	// The markers are read even though nothing is carried forward, so the pass
	// records what it read at. Recording zeroes would make the next
	// incremental pass believe every stamped account had moved.
	versions, err := c.sites.RuleVersions(ctx)
	if err != nil {
		// The rebuild still runs: it is the backstop. Nothing is recorded, so
		// the next incremental pass re-reads everything.
		if refreshErr := c.refresh(ctx, nil, true); refreshErr != nil {
			return errors.Join(err, refreshErr)
		}

		return err
	}

	return c.refresh(ctx, versions, true)
}

// RefreshChanged re-reads only the accounts whose rules have moved, which in
// the common case is none. One query against system.db replaces one database
// open and one read per account.
func (c *Cache) RefreshChanged(ctx context.Context) error {
	versions, err := c.sites.RuleVersions(ctx)
	if err != nil {
		return err
	}

	return c.refresh(ctx, versions, false)
}

// refresh rebuilds the snapshot. Nil versions rebuilds everything; otherwise an
// account whose marker matches the one it was last read at keeps the compiled
// rules it already has.
//
// One account that cannot be opened is skipped with its rules intact and every
// other account still published, because abandoning the pass would hold every
// customer on the box on a stale snapshot over one busy database.
func (c *Cache) refresh(ctx context.Context, versions map[int64]int64, everything bool) error {
	current := c.begin()

	siteIDs := map[int64][]int64{}
	for _, site := range c.sites.All() {
		siteIDs[site.AccountID] = append(siteIDs[site.AccountID], site.ID)
	}

	bySite := map[int64]*Ruleset{}
	read := map[int64]int64{}

	var failures []error

	for accountID, sites := range siteIDs {
		if !everything && carriedForward(current, accountID, versions[accountID], sites, bySite) {
			read[accountID] = current.versions[accountID]

			continue
		}

		compiled, err := c.readAccount(ctx, accountID)
		if err != nil {
			failures = append(failures, err)

			for _, siteID := range sites {
				if set, ok := current.bySite[siteID]; ok {
					bySite[siteID] = set
				}
			}

			if was, seen := current.versions[accountID]; seen {
				read[accountID] = was
			}

			continue
		}

		for siteID, set := range compiled {
			bySite[siteID] = set
		}

		read[accountID] = versions[accountID]
	}

	c.commit(&snapshot{bySite: bySite, versions: read, builtAt: time.Now()})

	return joined(failures)
}

// carriedForward copies one account's compiled rules from the previous snapshot
// when its marker has not moved, and reports whether it could. An account that
// has never been read has no marker to compare and is always re-read.
func carriedForward(current *snapshot, accountID, version int64, siteIDs []int64, into map[int64]*Ruleset) bool {
	was, seen := current.versions[accountID]
	if !seen || was != version {
		return false
	}

	for _, siteID := range siteIDs {
		if set, ok := current.bySite[siteID]; ok {
			into[siteID] = set
		}
	}

	return true
}

// begin takes the snapshot a pass builds from and clears the record of what a
// save touched, so anything saved from here on is put back at commit.
func (c *Cache) begin() *snapshot {
	c.publish.Lock()
	defer c.publish.Unlock()

	clear(c.touched)

	return c.snap.Load()
}

// commit publishes a pass, keeping whatever a save changed while it ran.
func (c *Cache) commit(built *snapshot) {
	c.publish.Lock()
	defer c.publish.Unlock()

	if len(c.touched) > 0 {
		live := c.snap.Load()

		for siteID := range c.touched {
			if set, ok := live.bySite[siteID]; ok {
				built.bySite[siteID] = set
			}
		}

		clear(c.touched)
	}

	c.snap.Store(built)
}

// joined reports the accounts a pass could not read, capped so one bad minute
// on a full shard is a log line somebody can read.
func joined(failures []error) error {
	const named = 5

	if len(failures) <= named {
		return errors.Join(failures...)
	}

	return errors.Join(append(failures[:named:named],
		fmt.Errorf("pathclean: and %d more accounts", len(failures)-named))...)
}

// readAccount opens one account and compiles every site's rules in it.
func (c *Cache) readAccount(ctx context.Context, accountID int64) (map[int64]*Ruleset, error) {
	// A scan. The hourly full pass walks every account, and no read here is
	// ever a customer's request.
	lease, err := c.accounts.AcquireForScan(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("pathclean: refresh account %d: %w", accountID, err)
	}

	byID, err := allRules(ctx, lease.Account.Reader())
	if err != nil {
		_ = lease.Release()

		return nil, err
	}

	if err := lease.Release(); err != nil {
		return nil, fmt.Errorf("pathclean: release account %d: %w", accountID, err)
	}

	compiled := map[int64]*Ruleset{}

	for siteID, rules := range byID {
		set, err := Compile(rules)
		if err != nil {
			// A stored pattern that will not compile was valid when it was
			// saved, so this means the file was edited by hand. The site is
			// skipped rather than the account abandoned: one broken rule list
			// must not stop every other site's rules updating.
			continue
		}

		compiled[siteID] = set
	}

	return compiled, nil
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
	c.publish.Lock()
	defer c.publish.Unlock()

	current := c.snap.Load()

	bySite := make(map[int64]*Ruleset, len(current.bySite)+1)
	for id, existing := range current.bySite {
		bySite[id] = existing
	}
	bySite[siteID] = set

	// A refresh in flight read the snapshot before this and would put the old
	// rules back when it publishes. This is how it knows not to.
	c.touched[siteID] = true

	c.snap.Store(&snapshot{bySite: bySite, versions: current.versions, builtAt: current.builtAt})
}

// Clean applies one site's rules to a path. It implements the ingest tier's
// PathCleaner, which lets the writer store a query mapping beside the raw path
// without learning to read an account database.
func (c *Cache) Clean(siteID int64, path string) string {
	return c.snap.Load().bySite[siteID].Clean(path)
}

// BuiltAt reports when the snapshot was made.
func (c *Cache) BuiltAt() time.Time {
	return c.snap.Load().builtAt
}

// Run refreshes on a ticker until the context is cancelled.
//
// Two intervals, for the reason the shield cache has two: the short one costs
// one query when nothing changed, and the long one covers a rule saved by
// something that did not stamp a marker.
func (c *Cache) Run(ctx context.Context, onError func(error)) {
	changed := time.NewTicker(RefreshInterval)
	defer changed.Stop()

	full := time.NewTicker(FullRefreshInterval)
	defer full.Stop()

	report := func(err error) {
		if err != nil && onError != nil {
			onError(err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-changed.C:
			report(c.RefreshChanged(ctx))
		case <-full.C:
			report(c.Refresh(ctx))
		}
	}
}

// The contract with the write path. A signature change on either side is a
// compile error here rather than a cleaner that silently stops being called.
var _ ingest.PathCleaner = (*Cache)(nil)
