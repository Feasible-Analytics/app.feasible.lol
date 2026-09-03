//
// panel.go
// Reading the record back, and turning it into warnings somebody can act on.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// Window is how far back the panel looks. Twenty-four hours is the span in
// which somebody can still remember what they changed, which is what makes the
// numbers actionable rather than merely true.
const Window = 24 * time.Hour

// CurrentTrackerVersion is the version the shipped script reports.
//
// It must be bumped together with VERSION in tracker/src/state.js. The pair is
// what makes the "old tracker version detected" warning possible at all: a
// script sits in browser caches and in copy-pasted snippets for months, and a
// customer running a build from before a fix has no other way to find out.
const CurrentTrackerVersion = 1

// ProxyWarningShare is the fraction of traffic resolved straight from the
// socket that means a proxy is not forwarding the visitor's address.
//
// It is a majority rather than any occurrence, because a small share is normal:
// a server-side API caller, a health check, a request that genuinely arrived
// direct. A majority is not normal. It means every visitor is being resolved to
// the proxy's own address, which collapses them into one visitor and geolocates
// them all to the datacentre — and nothing anywhere reports it.
const ProxyWarningShare = 0.5

// ProxyWarningMinimum is how much traffic there has to be before the share
// above is worth believing. Two requests in a day, both from the socket, is a
// quiet site rather than a broken proxy.
const ProxyWarningMinimum = 20

// ErrUnknownSite means no site is registered for that domain.
var ErrUnknownSite = errors.New("health: no such site")

// Count is one line of the accepted-and-dropped table.
type Count struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`

	// Explanation is the reason in a sentence. The closed reason set is
	// machine-readable by design, and a panel that showed only the machine form
	// would be asking every customer to look up "no_session_for_engagement".
	Explanation string `json:"explanation"`
}

// Seen is one observed value and how often.
type Seen struct {
	Value     string `json:"value"`
	Count     int64  `json:"count"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// LastRequest is the derived view of the most recent event, which is the debug
// output a customer would otherwise have to produce with curl.
type LastRequest struct {
	ReceivedAt int64 `json:"received_at"`

	// ClientIP is the address we resolved, and ClientIPSource is the header we
	// believed it from. These two fields are the reason this panel exists:
	// getting them wrong is invisible, costs every visitor's identity and
	// location, and is the root cause of four separate documented incumbent
	// bugs.
	ClientIP       string `json:"client_ip"`
	ClientIPSource string `json:"client_ip_source"`

	// TrustedProxy reports whether anything is on the trusted-proxy list at
	// all, because "I set X-Feasible-IP and it was ignored" is otherwise
	// indistinguishable from the header not arriving.
	TrustedProxy bool `json:"trusted_proxy_configured"`

	Hostname       string `json:"hostname"`
	Pathname       string `json:"pathname"`
	UserAgent      string `json:"user_agent"`
	TrackerVersion int    `json:"tracker_version"`
	DropReason     string `json:"drop_reason"`

	// Debug is the whole derived event, so the panel can show every field
	// without this struct growing one per derivation step.
	Debug map[string]any `json:"debug"`
}

// Warning codes. They are stable strings so the front end can attach an action
// to one without matching on English.
const (
	WarnProxyNotForwarding = "proxy_not_forwarding"
	WarnUnknownHostname    = "unknown_hostname"
	WarnPropsTruncated     = "props_truncated"
	WarnOldTracker         = "old_tracker"
	WarnHostnameBlocked    = "hostname_blocked"
	WarnNoTraffic          = "no_traffic"
)

// Action names what the front end may offer to do about a warning.
const (
	ActionAllowHostname = "allow_hostname"
)

// Warning is a named problem with the number behind it.
type Warning struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`

	// Count is the traffic the warning is about, so nobody has to guess
	// whether it is one event or a hundred thousand.
	Count int64 `json:"count"`

	// Hostname is the subject of a hostname warning, and what the Allow button
	// sends back.
	Hostname string `json:"hostname,omitempty"`

	// Action is the one-click remedy, when there is one.
	Action string `json:"action,omitempty"`
}

// Panel is everything the ingestion health screen shows for one site.
type Panel struct {
	Domain    string `json:"domain"`
	SiteID    int64  `json:"site_id"`
	AccountID int64  `json:"-"`

	From int64 `json:"from"`
	To   int64 `json:"to"`

	Accepted   int64 `json:"accepted"`
	Dropped    int64 `json:"dropped"`
	Classified int64 `json:"classified"`

	Drops           []Count `json:"drops"`
	Classifications []Count `json:"classifications"`
	Truncations     []Count `json:"truncations"`

	LastRequest      *LastRequest `json:"last_request"`
	Warnings         []Warning    `json:"warnings"`
	TrackerVersions  []Seen       `json:"tracker_versions"`
	IPSources        []Seen       `json:"ip_sources"`
	UnknownHostnames []Seen       `json:"unknown_hostnames"`
	AllowedHostnames []string     `json:"allowed_hostnames"`
}

// Store reads the panel and applies its one-click remedy.
type Store struct {
	Accounts *accounts.Manager
	Sites    *sites.Cache

	// System is the installation-wide database where the hostname allow-list
	// originates. The app publishes that list to ingesters through its signed
	// private endpoint; an ingester never opens this database itself.
	System *sql.DB

	// Now bounds the window, injectable for tests.
	Now func() time.Time
}

// NewStore builds a panel reader.
func NewStore(manager *accounts.Manager, cache *sites.Cache, control *sql.DB) *Store {
	return &Store{
		Accounts: manager,
		Sites:    cache,
		System:   control,
		Now:      func() time.Time { return time.Now().UTC() },
	}
}

// now reads the injected clock, falling back to the real one.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Panel assembles one site's ingestion health.
func (s *Store) Panel(ctx context.Context, domain string) (Panel, error) {
	site, ok := s.Sites.Lookup(domain)
	if !ok {
		return Panel{}, ErrUnknownSite
	}

	account, err := s.Accounts.Open(ctx, site.AccountID)
	if err != nil {
		return Panel{}, fmt.Errorf("health: open account %d: %w", site.AccountID, err)
	}

	now := s.now()
	from := now.Add(-Window).Unix()

	panel := Panel{
		Domain:           site.Domain,
		SiteID:           site.ID,
		AccountID:        site.AccountID,
		From:             from,
		To:               now.Unix(),
		Drops:            []Count{},
		Classifications:  []Count{},
		Truncations:      []Count{},
		Warnings:         []Warning{},
		TrackerVersions:  []Seen{},
		IPSources:        []Seen{},
		UnknownHostnames: []Seen{},
		AllowedHostnames: site.AllowedHostnames,
	}

	if err := s.readCounts(ctx, account.Reader(), &panel, from, panel.To); err != nil {
		return Panel{}, err
	}

	if err := s.readObservations(ctx, account.Reader(), &panel, from, panel.To); err != nil {
		return Panel{}, err
	}

	if err := s.readLastRequest(ctx, account.Reader(), &panel, from, panel.To); err != nil {
		return Panel{}, err
	}

	panel.Warnings = buildWarnings(panel)

	return panel, nil
}

// readCounts fills in the accepted, dropped, classified and truncated tallies.
func (s *Store) readCounts(ctx context.Context, db *sql.DB, panel *Panel, from, to int64) error {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, reason, SUM(count)
		FROM ingest_health
		WHERE site_id = ? AND observed_at > ? AND observed_at <= ?
		GROUP BY kind, reason
	`, panel.SiteID, from, to)
	if err != nil {
		return fmt.Errorf("health: read counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			kind, reason string
			count        int64
		)

		if err := rows.Scan(&kind, &reason, &count); err != nil {
			return fmt.Errorf("health: read counts: %w", err)
		}

		line := Count{Reason: reason, Count: count, Explanation: Explain(reason)}

		switch kind {
		case KindAccepted:
			panel.Accepted += count
		case KindDropped:
			panel.Dropped += count
			panel.Drops = append(panel.Drops, line)
		case KindClassified:
			panel.Classified += count
			panel.Classifications = append(panel.Classifications, line)
		case KindTruncated:
			panel.Truncations = append(panel.Truncations, line)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("health: read counts: %w", err)
	}

	// Biggest first: the reason with the most events behind it is the one worth
	// fixing, and a table sorted by an id makes somebody read all of it to find
	// that out.
	sortCounts(panel.Drops)
	sortCounts(panel.Classifications)
	sortCounts(panel.Truncations)

	return nil
}

// sortCounts orders a tally by size, then by name so ties are stable.
func sortCounts(counts []Count) {
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count != counts[j].Count {
			return counts[i].Count > counts[j].Count
		}

		return counts[i].Reason < counts[j].Reason
	})
}

// readObservations fills in the hostnames, tracker versions and address
// sources seen.
func (s *Store) readObservations(ctx context.Context, db *sql.DB, panel *Panel, from, to int64) error {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, value, SUM(count), MIN(first_seen_at), MAX(last_seen_at)
		FROM ingest_observations
		WHERE site_id = ? AND observed_at > ? AND observed_at <= ?
		GROUP BY kind, value
		ORDER BY SUM(count) DESC, value
	`, panel.SiteID, from, to)
	if err != nil {
		return fmt.Errorf("health: read observations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			kind string
			seen Seen
		)

		if err := rows.Scan(&kind, &seen.Value, &seen.Count, &seen.FirstSeen, &seen.LastSeen); err != nil {
			return fmt.Errorf("health: read observations: %w", err)
		}

		switch kind {
		case KindTrackerVersion:
			panel.TrackerVersions = append(panel.TrackerVersions, seen)
		case KindIPSource:
			panel.IPSources = append(panel.IPSources, seen)
		case KindUnknownHostname:
			panel.UnknownHostnames = append(panel.UnknownHostnames, seen)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("health: read observations: %w", err)
	}

	return nil
}

// readLastRequest fills in the derived view of the most recent event.
func (s *Store) readLastRequest(ctx context.Context, db *sql.DB, panel *Panel, from, to int64) error {
	var (
		request LastRequest
		trusted int
		debug   string
	)

	err := db.QueryRowContext(ctx, `
		SELECT received_at, client_ip, client_ip_source, trusted_proxy, hostname, pathname,
		       user_agent, tracker_version, drop_reason, debug
		FROM ingest_last_request WHERE site_id = ? AND received_at > ? AND received_at <= ?
	`, panel.SiteID, from, to).Scan(&request.ReceivedAt, &request.ClientIP, &request.ClientIPSource, &trusted,
		&request.Hostname, &request.Pathname, &request.UserAgent, &request.TrackerVersion,
		&request.DropReason, &debug)

	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("health: read last request: %w", err)
	}

	request.TrustedProxy = trusted != 0

	// A debug blob that will not parse is shown as absent rather than failing
	// the whole panel. Every other field on this row is still the answer to the
	// question somebody opened the page with.
	_ = json.Unmarshal([]byte(debug), &request.Debug)

	panel.LastRequest = &request

	return nil
}

// AllowHostname adds a hostname to a site's allow-list. It is the one-click
// remedy behind the "events arriving from a hostname not on your allow-list"
// warning, and it also clears the observation so the warning goes away rather
// than staying on the screen looking unfixed.
func (s *Store) AllowHostname(ctx context.Context, domain, hostname string) error {
	site, ok := s.Sites.Lookup(domain)
	if !ok {
		return ErrUnknownSite
	}

	hostname = sites.Normalise(hostname)
	if hostname == "" {
		return fmt.Errorf("health: a hostname is required")
	}

	// The site's own domain has to go on the list at the same time. An
	// allow-list is all-or-nothing: adding one hostname to an empty list would
	// otherwise start dropping the site's own traffic, which is the worst
	// possible outcome of clicking a button labelled Allow.
	registered := sites.Normalise(site.Domain)

	tx, err := s.System.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("health: allow hostname: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	now := s.now().Unix()

	for _, allow := range []string{registered, hostname} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO site_allowed_hostnames (site_id, hostname, created_at)
			VALUES (?, ?, ?)
			ON CONFLICT (site_id, hostname) DO NOTHING
		`, site.ID, allow, now); err != nil {
			return fmt.Errorf("health: allow hostname: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("health: allow hostname: %w", err)
	}

	account, err := s.Accounts.Open(ctx, site.AccountID)
	if err != nil {
		return fmt.Errorf("health: open account %d: %w", site.AccountID, err)
	}

	if _, err := account.Writer().ExecContext(ctx, `
		DELETE FROM ingest_observations WHERE site_id = ? AND kind = ? AND value = ?
	`, site.ID, KindUnknownHostname, hostname); err != nil {
		return fmt.Errorf("health: clear hostname observation: %w", err)
	}

	// The routing snapshot is what the ingest path reads, so refreshing it here
	// makes the button take effect now rather than at the next fifteen-second
	// tick. A button whose result is visible on the *next* page load is a
	// button people press twice.
	if err := s.Sites.Refresh(ctx); err != nil {
		return fmt.Errorf("health: refresh sites: %w", err)
	}

	return nil
}

// buildWarnings turns the panel's numbers into the named problems.
//
// Every warning here says what is wrong, how much traffic it affects, and where
// possible what to do about it. A warning without a number is one somebody
// ignores; a warning without a remedy is one they resent.
func buildWarnings(panel Panel) []Warning {
	warnings := []Warning{}

	if socket, total := ipSourceShare(panel.IPSources); total >= ProxyWarningMinimum &&
		float64(socket)/float64(total) > ProxyWarningShare {
		warnings = append(warnings, Warning{
			Code:  WarnProxyNotForwarding,
			Title: "Your proxy is not forwarding X-Forwarded-For",
			Detail: fmt.Sprintf(
				"%d of the last %d requests had no forwarding header, so the address we resolved was the one that "+
					"connected to us rather than the visitor's. Every one of those visitors is being counted as the "+
					"same person and located wherever your proxy is. Set X-Forwarded-For on the proxy in front of us.",
				socket, total),
			Count: socket,
		})
	}

	for _, hostname := range panel.UnknownHostnames {
		warnings = append(warnings, Warning{
			Code:  WarnUnknownHostname,
			Title: "Events are arriving from " + hostname.Value,
			Detail: fmt.Sprintf(
				"%d events came from a hostname that is not this site's domain and is not on its allow-list. "+
					"That is usually a staging copy or somebody else's page running your snippet. Allow it if it "+
					"is yours, and its traffic will be counted; leave it and add the hostnames you do want.",
				hostname.Count),
			Count:    hostname.Count,
			Hostname: hostname.Value,
			Action:   ActionAllowHostname,
		})
	}

	for _, drop := range panel.Drops {
		if drop.Reason != ingest.ReasonHostnameNotAllowed {
			continue
		}

		warnings = append(warnings, Warning{
			Code:  WarnHostnameBlocked,
			Title: "Events are being dropped by your hostname allow-list",
			Detail: fmt.Sprintf(
				"%d events were refused because their hostname is not on this site's allow-list. If that is not "+
					"what you meant, add the hostname or clear the list entirely to accept every hostname.",
				drop.Count),
			Count: drop.Count,
		})
	}

	for _, truncation := range panel.Truncations {
		if truncation.Reason != ingest.TruncationProps {
			continue
		}

		warnings = append(warnings, Warning{
			Code:  WarnPropsTruncated,
			Title: fmt.Sprintf("Custom properties were discarded on %d events", truncation.Count),
			Detail: fmt.Sprintf(
				"More than %d custom properties were sent on %d events; properties past the limit were not stored. They are gone rather than "+
					"queued, so any report built on them will be missing those values entirely.",
				ingest.MaxProps, truncation.Count),
			Count: truncation.Count,
		})
	}

	if old, count := oldTrackerVersions(panel.TrackerVersions); old != "" {
		warnings = append(warnings, Warning{
			Code:  WarnOldTracker,
			Title: "An old tracker version is still in the wild",
			Detail: fmt.Sprintf(
				"%d events arrived from tracker version %s; the current version is %d. A cached script can survive "+
					"for months, so re-check the snippet on any page still sending the old one.",
				count, old, CurrentTrackerVersion),
			Count: count,
		})
	}

	if panel.Accepted == 0 && panel.Dropped == 0 {
		warnings = append(warnings, Warning{
			Code:  WarnNoTraffic,
			Title: "No events at all in the last 24 hours",
			Detail: "Nothing has reached us for this site — not even a dropped event. That is a snippet that is " +
				"not installed, not loading, or being blocked before it can send. Use the test event below to " +
				"prove the endpoint is reachable from here.",
		})
	}

	return warnings
}

// ipSourceShare counts how much traffic was resolved from the socket rather
// than from a forwarding header, and how much there was in total.
func ipSourceShare(sources []Seen) (socket, total int64) {
	for _, source := range sources {
		total += source.Count

		if source.Value == clientip.SourceSocket {
			socket += source.Count
		}
	}

	return socket, total
}

// oldTrackerVersions returns the highest out-of-date version seen and how many
// events came from an out-of-date script. The highest rather than the lowest,
// because it is the one closest to current and therefore the one whose absence
// after a fix proves the fix landed.
func oldTrackerVersions(versions []Seen) (string, int64) {
	var (
		highest string
		best    int
		count   int64
	)

	for _, version := range versions {
		parsed, err := strconv.Atoi(version.Value)
		if err != nil || parsed <= 0 || parsed >= CurrentTrackerVersion {
			continue
		}

		count += version.Count

		if parsed > best {
			best, highest = parsed, version.Value
		}
	}

	return highest, count
}

// explanations maps the closed reason set onto the message ids that explain it.
//
// It is a map in this package rather than a field on the reason constants
// because the constants are a wire contract shared with the tracker and the
// response header, and the sentence a customer reads is a product decision that
// will be reworded far more often than the reason string may change.
//
// The ids are written out as literals rather than built from the reason, which
// is the difference between a catalogue check that can see these strings and
// one that cannot: a constructed id is invisible to the scan that proves every
// id has a translation, so a missing one would reach the panel as its own name.
var explanations = map[string]string{
	ingest.ReasonHostnameNotAllowed: "settings.health.reason.hostname_not_allowed",
	ingest.ReasonUnknownSite:        "settings.health.reason.unknown_site",
	ingest.ReasonAccountDormant:     "settings.health.reason.account_dormant",
	ingest.ReasonSiteDeleted:        "settings.health.reason.site_deleted",
	ingest.ReasonShieldIP:           "settings.health.reason.shield_ip",
	ingest.ReasonShieldCountry:      "settings.health.reason.shield_country",
	ingest.ReasonShieldPage:         "settings.health.reason.shield_page",
	ingest.ReasonNoSessionForEngage: "settings.health.reason.no_session_for_engagement",
	ingest.ReasonRateLimited:        "settings.health.reason.rate_limited",
	ingest.ReasonInvalidPayload:     "settings.health.reason.invalid_payload",
	ingest.ReasonInternalError:      "settings.health.reason.internal_error",
	ingest.ReasonBot:                "settings.health.reason.bot",
	ingest.ReasonDatacenterIP:       "settings.health.reason.datacenter_ip",
	ingest.ReasonReferrerSpam:       "settings.health.reason.referrer_spam",
	ingest.ReasonOutdatedBrowser:    "settings.health.reason.outdated_browser",
	ingest.ReasonAutomation:         "settings.health.reason.automation",

	ingest.TruncationProps:           "settings.health.reason.truncation_props",
	ingest.TruncationPropName:        "settings.health.reason.truncation_prop_name",
	ingest.TruncationPropValue:       "settings.health.reason.truncation_prop_value",
	ingest.TruncationPropUnsupported: "settings.health.reason.truncation_prop_unsupported",
	ingest.TruncationURL:             "settings.health.reason.truncation_url",
	ingest.TruncationEngagement:      "settings.health.reason.truncation_engagement",
}

// Explain renders one reason in a sentence, in the source language.
//
// The panel translates these at render, so this is what fills the JSON the API
// returns: a program reads the machine reason beside it, and a payload whose
// wording moved with the reader's language would be a payload nobody could
// match on.
func Explain(reason string) string {
	return ExplainIn(i18n.DefaultLocale, reason)
}

// ExplainIn renders one reason for a reader, falling back to the raw reason so
// that a reason added without an explanation is visible rather than blank.
func ExplainIn(locale, reason string) string {
	if id, ok := explanations[reason]; ok {
		return i18n.T(locale, id, "limit", ingest.MaxProps)
	}

	if reason == "" {
		return ""
	}

	return reason
}
