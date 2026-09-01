//
// shields.go
// Storing and evaluating the four kinds of traffic a customer can exclude.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package shields keeps traffic the customer does not want out of their
// numbers: their own office, a country they do not serve, a staging path, a
// domain that is copying their tracker snippet.
//
// The rules live in the account database, which makes them site-scoped
// configuration like everything else, and they are evaluated in two different
// places for one reason. An IP rule can only be checked in the ingest tier,
// because that tier is the only place the raw address exists at all — by design
// the address is geolocated, hashed and discarded before anything is written or
// stored. Country, page and hostname rules are checked by the account writer,
// where the live snapshot can share the fact transaction's decision boundary.
//
// Shields are in every build. They are missing from the incumbent's community
// edition entirely, which means the people most likely to be measuring their
// own traffic are the ones who cannot exclude it.
package shields

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// The four rule kinds.
const (
	// KindIP blocks an address or a CIDR block. Evaluated in the ingest tier.
	KindIP = "ip"

	// KindCountry blocks an ISO 3166-1 alpha-2 country.
	KindCountry = "country"

	// KindPage blocks a path, or every path under it when it ends in a *.
	KindPage = "page"

	// KindHostname adds a host outside the registered domain and its subdomains
	// to the allow-list. It is what makes an external preview or checkout host
	// eligible without weakening the default claim-versus-page check.
	KindHostname = "hostname"
)

// Kinds is every rule kind, in the order the settings page shows them.
var Kinds = []string{KindIP, KindCountry, KindPage, KindHostname}

// MaxRulesPerKind is the cap, per kind, per site. It is thirty rather than
// unlimited because every rule is evaluated on the hot path for every event,
// and because a list longer than this is a sign the customer wants a different
// feature — a segment, or a second site — rather than more rules.
const MaxRulesPerKind = 30

// RefreshInterval is how often a running process rebuilds its rule snapshot. It
// matches the site-cache refresh, so a rule takes effect within one cycle rather
// than the "a few minutes" the incumbent's documentation promises.
const RefreshInterval = 15 * time.Second

// Rule is one stored rule.
type Rule struct {
	ID        int64
	SiteID    int64
	Kind      string
	Value     string
	Note      string
	CreatedAt int64
}

// Normalise puts a value into the one form the evaluator compares against, so
// that matching never depends on how somebody typed it. It returns an error for
// a value that cannot match anything, because a rule that silently blocks
// nothing is worse than no rule at all: the customer believes the problem is
// solved and stops looking.
func Normalise(kind, value string) (string, error) {
	value = strings.TrimSpace(value)

	if value == "" {
		return "", fmt.Errorf("a %s rule needs a value", kind)
	}

	switch kind {
	case KindIP:
		if strings.Contains(value, "/") {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return "", fmt.Errorf("%q is not an address or a CIDR block", value)
			}
			return prefix.Masked().String(), nil
		}

		addr, err := netip.ParseAddr(value)
		if err != nil {
			return "", fmt.Errorf("%q is not an IP address", value)
		}

		return addr.Unmap().String(), nil

	case KindCountry:
		code := strings.ToUpper(value)
		if len(code) != 2 {
			return "", fmt.Errorf("%q is not a two-letter country code — use US, GB, DE", value)
		}
		return code, nil

	case KindPage:
		if !strings.HasPrefix(value, "/") {
			value = "/" + value
		}
		return value, nil

	case KindHostname:
		host := strings.ToLower(value)
		host = strings.TrimPrefix(host, "http://")
		host = strings.TrimPrefix(host, "https://")
		host = strings.TrimPrefix(host, "www.")
		host = strings.TrimSuffix(strings.TrimSuffix(host, "/"), ".")

		if host == "" || strings.ContainsAny(host, " /") {
			return "", fmt.Errorf("%q is not a hostname", value)
		}

		return host, nil

	default:
		return "", fmt.Errorf("%q is not a shield rule kind", kind)
	}
}

// List reads one site's rules, ordered so the settings page renders them the
// same way twice.
func List(ctx context.Context, db *sql.DB, siteID int64) ([]Rule, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, site_id, kind, value, note, created_at
		FROM shield_rules WHERE site_id = ? ORDER BY kind, id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("shields: read rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rules []Rule

	for rows.Next() {
		var rule Rule
		if err := rows.Scan(&rule.ID, &rule.SiteID, &rule.Kind, &rule.Value, &rule.Note, &rule.CreatedAt); err != nil {
			return nil, fmt.Errorf("shields: read rules: %w", err)
		}
		rules = append(rules, rule)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shields: read rules: %w", err)
	}

	return rules, nil
}

// Add stores one rule. The cap is counted here rather than enforced by the
// schema because a CHECK cannot count sibling rows, and a trigger that could
// would fail with a constraint message nobody can act on — where this returns
// the sentence the customer needs to read.
func Add(ctx context.Context, db *sql.DB, siteID int64, kind, value, note string, now time.Time) (Rule, error) {
	normalised, err := Normalise(kind, value)
	if err != nil {
		return Rule{}, err
	}

	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM shield_rules WHERE site_id = ? AND kind = ?", siteID, kind).Scan(&count); err != nil {
		return Rule{}, fmt.Errorf("shields: count rules: %w", err)
	}

	if count >= MaxRulesPerKind {
		return Rule{}, fmt.Errorf("a site may hold at most %d %s rules — remove one before adding another", MaxRulesPerKind, kind)
	}

	result, err := db.ExecContext(ctx,
		"INSERT INTO shield_rules (site_id, kind, value, note, created_at) VALUES (?, ?, ?, ?, ?)",
		siteID, kind, normalised, strings.TrimSpace(note), now.Unix())
	if err != nil {
		// The unique index is the only constraint that can fire here, and the
		// customer's version of it is not a database error.
		if strings.Contains(err.Error(), "UNIQUE") {
			return Rule{}, fmt.Errorf("%s is already on the %s list", normalised, kind)
		}

		return Rule{}, fmt.Errorf("shields: add rule: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Rule{}, fmt.Errorf("shields: add rule: %w", err)
	}

	return Rule{ID: id, SiteID: siteID, Kind: kind, Value: normalised, Note: note, CreatedAt: now.Unix()}, nil
}

// Delete removes one rule, scoped to its site so an id from another account
// cannot be used to delete somebody else's.
func Delete(ctx context.Context, db *sql.DB, siteID, id int64) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM shield_rules WHERE site_id = ? AND id = ?", siteID, id); err != nil {
		return fmt.Errorf("shields: delete rule: %w", err)
	}

	return nil
}

// Ruleset is one site's rules compiled for evaluation. The IP prefixes are
// parsed once here rather than per event, and the country and hostname lists
// are maps because both are exact-match lookups.
type Ruleset struct {
	// domain is the site's registered domain. It makes the default hostname
	// policy active even when the customer has configured no rules.
	domain string

	prefixes  []netip.Prefix
	countries map[string]bool
	hostnames map[string]bool

	// pages holds exact paths, prefixes holds the ones written with a trailing
	// star. They are separate so an exact rule never accidentally becomes a
	// prefix rule and takes a whole section of the site with it.
	pages     map[string]bool
	pagePaths []string
}

// Compile turns stored rules into a ruleset. A rule that no longer parses is
// skipped rather than fatal: the stored value was normalised when it was saved,
// so an unparseable one means the file was edited by hand, and refusing to
// serve traffic over it would be the wrong trade.
func Compile(rules []Rule) *Ruleset {
	return CompileFor("", rules)
}

// CompileFor compiles rules with the registered domain used by the default
// hostname policy.
func CompileFor(domain string, rules []Rule) *Ruleset {
	set := &Ruleset{
		domain:    normaliseHostname(domain),
		countries: map[string]bool{},
		hostnames: map[string]bool{},
		pages:     map[string]bool{},
	}

	for _, rule := range rules {
		switch rule.Kind {
		case KindIP:
			if strings.Contains(rule.Value, "/") {
				if prefix, err := netip.ParsePrefix(rule.Value); err == nil {
					set.prefixes = append(set.prefixes, prefix.Masked())
				}
				continue
			}

			if addr, err := netip.ParseAddr(rule.Value); err == nil {
				set.prefixes = append(set.prefixes, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			}

		case KindCountry:
			set.countries[rule.Value] = true

		case KindHostname:
			set.hostnames[rule.Value] = true

		case KindPage:
			if strings.HasSuffix(rule.Value, "*") {
				set.pagePaths = append(set.pagePaths, strings.TrimSuffix(rule.Value, "*"))
				continue
			}

			set.pages[rule.Value] = true
		}
	}

	sort.Strings(set.pagePaths)

	return set
}

// AllowedHostnames lists the explicit additive hostname rules.
func (r *Ruleset) AllowedHostnames() []string {
	if r == nil || len(r.hostnames) == 0 {
		return nil
	}

	hostnames := make([]string, 0, len(r.hostnames))
	for hostname := range r.hostnames {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)

	return hostnames
}

// HostnameAllowed exposes the hostname decision without evaluating other
// shield kinds.
func (r *Ruleset) HostnameAllowed(hostname string) bool {
	return r.hostnameAllowed(hostname)
}

// BlocksIP reports whether an address is on the blocked list.
func (r *Ruleset) BlocksIP(addr netip.Addr) bool {
	if r == nil || len(r.prefixes) == 0 || !addr.IsValid() {
		return false
	}

	// A v4 address arriving as a v4-mapped v6 would not match a v4 prefix, and
	// that is exactly the shape a dual-stack listener produces.
	addr = addr.Unmap()

	for _, prefix := range r.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// Allowed evaluates the three account-writer rule kinds and names the reason
// when one blocks. The order is cheapest first; hostname is last because its
// registered-domain and additive allow-list check is the broadest decision.
func (r *Ruleset) Allowed(hostname, pathname, country string) (bool, string) {
	if r == nil {
		return true, ""
	}

	if country != "" && r.countries[strings.ToUpper(country)] {
		return false, ingest.ReasonShieldCountry
	}

	if r.blocksPage(pathname) {
		return false, ingest.ReasonShieldPage
	}

	if !r.hostnameAllowed(hostname) {
		return false, ingest.ReasonHostnameNotAllowed
	}

	return true, ""
}

// hostnameAllowed accepts the registered domain, its subdomains, or an
// explicit additive hostname. A parent domain and a lookalike remain rejected.
func (r *Ruleset) hostnameAllowed(hostname string) bool {
	if r == nil {
		return true
	}

	host := normaliseHostname(hostname)
	if host == "" || host == ingest.NoneHostname {
		return false
	}
	if r.hostnames[host] {
		return true
	}
	if r.domain == "" {
		return len(r.hostnames) == 0
	}

	return host == r.domain || strings.HasSuffix(host, "."+r.domain)
}

// blocksPage answers the page rules: an exact path, or any path under one
// written with a trailing star.
func (r *Ruleset) blocksPage(pathname string) bool {
	if pathname == "" {
		return false
	}

	if r.pages[pathname] {
		return true
	}

	for _, prefix := range r.pagePaths {
		if strings.HasPrefix(pathname, prefix) {
			return true
		}
	}

	return false
}

// Empty reports whether a ruleset would block anything at all, so a site with
// no rules can be skipped without walking four empty collections per event.
func (r *Ruleset) Empty() bool {
	return r == nil ||
		(r.domain == "" && len(r.prefixes) == 0 && len(r.countries) == 0 && len(r.hostnames) == 0 &&
			len(r.pages) == 0 && len(r.pagePaths) == 0)
}

// normaliseHostname folds an event's hostname the same way a stored rule was
// folded. The ingest pipeline already lower-cases and strips a leading www, but
// doing it again here costs nothing and means the two can never drift.
func normaliseHostname(hostname string) string {
	host := strings.ToLower(strings.TrimSpace(hostname))
	host = strings.TrimPrefix(host, "www.")

	return strings.TrimSuffix(host, ".")
}
