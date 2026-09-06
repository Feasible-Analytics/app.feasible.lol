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
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// opener is how a refresh reaches an account database. It is an interface so a
// test can count the opens a pass performs, which is the whole property this
// cache's refresh schedule exists to keep at zero.
type opener interface {
	Acquire(ctx context.Context, id int64) (*accounts.Lease, error)
}

// Cache holds the compiled rules for every site this process serves. It is a
// snapshot swapped whole for the same reason the routing map is: an event
// lookup is one atomic load and one map read, with no lock and no I/O on a path
// that runs for every request on the box.
type Cache struct {
	sites    *sites.Cache
	accounts opener

	// publish serialises the two writers of the snapshot. A refresh reads the
	// current one, spends seconds on I/O and writes a new one; a save from the
	// settings page writes one in between. Without this the save is lost, and
	// with carry-forward it stays lost until the marker brings it back.
	publish sync.Mutex

	// touched names the sites a save changed while a refresh was in flight, so
	// the refresh can put them back before it publishes.
	touched map[int64]bool

	// Rejections reads hostname facts already committed by the writer for the
	// settings page's one-click allow flow.
	Rejections *Rejections

	snap atomic.Pointer[snapshot]
}

// snapshot is one immutable build of the rules.
type snapshot struct {
	bySite map[int64]*Ruleset

	// rules is what each site compiled from, kept so an account whose marker
	// has not moved can be carried forward without opening its database.
	rules map[int64][]Rule

	// domains is the domain each site compiled against, since a compiled
	// ruleset carries the site's own domain and cannot be reused across a
	// rename.
	domains map[int64]string

	// versions is the marker each account was last read at. An account absent
	// from it has never been read.
	versions map[int64]int64

	builtAt time.Time
}

// New builds an empty cache. Nothing is read until Refresh runs, so a process
// can construct it before it has decided whether it will serve traffic.
func New(siteCache *sites.Cache, manager opener) *Cache {
	cache := &Cache{sites: siteCache, accounts: manager, touched: map[int64]bool{}}
	cache.snap.Store(&snapshot{
		bySite:   map[int64]*Ruleset{},
		rules:    map[int64][]Rule{},
		domains:  map[int64]string{},
		versions: map[int64]int64{},
	})

	return cache
}

// Refresh rebuilds every account's rules, whatever their marker says.
//
// It is the first pass at start-up and the hourly backstop. A marker that was
// never stamped — an install upgraded mid-flight, a rule written by something
// that does not stamp — is invisible to the incremental pass, and this is what
// covers it.
func (c *Cache) Refresh(ctx context.Context) error {
	// The markers are read even though nothing is carried forward, so the pass
	// records what it read at. Recording zeroes would make the next
	// incremental pass believe every stamped account had moved and re-open all
	// of them fifteen seconds later.
	versions, err := c.sites.RuleVersions(ctx)
	if err != nil {
		// The rebuild still runs: it is the backstop, and a control database
		// that cannot be read is not a reason to stop applying rules. Nothing
		// is recorded, so the next incremental pass re-reads everything.
		if refreshErr := c.refresh(ctx, nil, true); refreshErr != nil {
			return errors.Join(err, refreshErr)
		}

		return err
	}

	return c.refresh(ctx, versions, true)
}

// RefreshChanged re-reads only the accounts whose rules have moved since the
// last pass, which in the common case is none of them.
//
// One query against system.db replaces one database open and one read per
// account, and a carried-forward site keeps its compiled ruleset rather than
// being recompiled. What a quiet pass costs is that query plus one walk of the
// site list. At two thousand accounts that is the difference between about a
// second of work every fifteen seconds and about a millisecond.
func (c *Cache) RefreshChanged(ctx context.Context) error {
	versions, err := c.sites.RuleVersions(ctx)
	if err != nil {
		return err
	}

	return c.refresh(ctx, versions, false)
}

// refresh rebuilds the snapshot. Nil versions rebuilds everything; otherwise an
// account whose marker matches the one it was last read at is carried forward.
//
// One account that cannot be opened is skipped with its previous rules intact,
// and every other account is still published. Abandoning the pass would leave
// every customer on the box holding a stale snapshot because one database was
// busy, and the odds of at least one being busy rise with the account count.
func (c *Cache) refresh(ctx context.Context, versions map[int64]int64, everything bool) error {
	current := c.begin()

	all := c.sites.All()

	byAccount := map[int64][]int64{}
	for _, site := range all {
		byAccount[site.AccountID] = append(byAccount[site.AccountID], site.ID)
	}

	rulesBySite := map[int64][]Rule{}
	read := map[int64]int64{}
	carried := map[int64]bool{}

	var failures []error

	for accountID := range byAccount {
		if !everything && carriedForward(current, accountID, versions[accountID], byAccount[accountID], rulesBySite) {
			read[accountID] = current.versions[accountID]
			carried[accountID] = true

			continue
		}

		rules, err := c.readAccount(ctx, accountID)
		if err != nil {
			failures = append(failures, err)

			// The account keeps the rules it had. A rule the customer wrote
			// goes on being applied rather than lapsing because of a lock.
			for _, siteID := range byAccount[accountID] {
				if had, ok := current.rules[siteID]; ok {
					rulesBySite[siteID] = had
				}
			}

			if was, ok := current.versions[accountID]; ok {
				read[accountID] = was
			}

			continue
		}

		for siteID, list := range rules {
			rulesBySite[siteID] = list
		}

		read[accountID] = versions[accountID]
	}

	bySite := map[int64]*Ruleset{}
	domains := map[int64]string{}

	for _, site := range all {
		domains[site.ID] = site.Domain

		// A carried-forward account's rules did not change, so neither did what
		// they compile to — unless the site's own domain moved, which is part
		// of the compiled result.
		if carried[site.AccountID] && current.domains[site.ID] == site.Domain {
			if set, ok := current.bySite[site.ID]; ok {
				bySite[site.ID] = set

				continue
			}
		}

		bySite[site.ID] = CompileFor(site.Domain, rulesBySite[site.ID])
	}

	c.commit(&snapshot{
		bySite:   bySite,
		rules:    rulesBySite,
		domains:  domains,
		versions: read,
		builtAt:  time.Now(),
	})

	return joined(failures)
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

			if rules, ok := live.rules[siteID]; ok {
				built.rules[siteID] = rules
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
		fmt.Errorf("shields: and %d more accounts", len(failures)-named))...)
}

// carriedForward copies one account's compiled input from the previous snapshot
// when its marker has not moved, and reports whether it could.
func carriedForward(current *snapshot, accountID, version int64, siteIDs []int64, into map[int64][]Rule) bool {
	was, seen := current.versions[accountID]
	if !seen || was != version {
		return false
	}

	for _, siteID := range siteIDs {
		if had, ok := current.rules[siteID]; ok {
			into[siteID] = had
		}
	}

	return true
}

// readAccount opens one account and reads its rules.
func (c *Cache) readAccount(ctx context.Context, accountID int64) (map[int64][]Rule, error) {
	lease, err := c.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("shields: refresh account %d: %w", accountID, err)
	}

	rules, err := allRules(ctx, lease.Account.Reader())
	if err != nil {
		_ = lease.Release()

		return nil, err
	}

	if err := lease.Release(); err != nil {
		return nil, fmt.Errorf("shields: release account %d: %w", accountID, err)
	}

	return rules, nil
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
	c.publish.Lock()
	defer c.publish.Unlock()

	current := c.snap.Load()

	bySite := make(map[int64]*Ruleset, len(current.bySite)+1)
	for id, set := range current.bySite {
		bySite[id] = set
	}

	domain := current.domains[siteID]
	for _, site := range c.sites.All() {
		if site.ID == siteID {
			domain = site.Domain

			break
		}
	}

	bySite[siteID] = CompileFor(domain, rules)

	// The compiled input travels with it. Dropping it would make the next
	// incremental pass believe no account had ever been read, and re-open all
	// of them.
	byInput := make(map[int64][]Rule, len(current.rules)+1)
	for id, list := range current.rules {
		byInput[id] = list
	}

	byInput[siteID] = rules

	domains := make(map[int64]string, len(current.domains)+1)
	for id, was := range current.domains {
		domains[id] = was
	}

	domains[siteID] = domain

	// A refresh in flight read the snapshot before this, and would put the old
	// rules back when it publishes. This is how it knows not to.
	c.touched[siteID] = true

	c.snap.Store(&snapshot{
		bySite:   bySite,
		rules:    byInput,
		domains:  domains,
		versions: current.versions,
		builtAt:  current.builtAt,
	})
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

// Run refreshes on a ticker until the context is cancelled.
//
// Two intervals. The short one matches the site-cache refresh, so a rule saved
// in the dashboard is live on every process within fifteen seconds, and it
// costs one query against system.db when nothing changed. The long one rebuilds
// everything regardless, because a marker that was never stamped is invisible
// to the short pass and an hour is a bounded time to be wrong for.
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
	// catalogue. The string it names says what the address means and what the
	// consequence is; on a hosted account the proxy is ours, so the fix is not
	// the reader's to make.
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
