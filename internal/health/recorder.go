//
// recorder.go
// Keeping enough of what arrived to explain it afterwards.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The recorder is the ingestion health panel's write side: what we accepted,
// what we threw away and why, which address we resolved for the last request
// and which header we believed it from, and the named warnings that turn all
// of that into something a customer can act on.
//
// Events are dropped and answered with a 202 and a header nobody reads;
// properties past a cap vanish; a proxy that is not forwarding the visitor's
// address turns every visitor into one. None of it raises an error anywhere, so
// without this the first sign is somebody noticing a number looks wrong weeks
// later — by which time the data is gone. Nothing is dropped, truncated or
// reclassified without a count, a reason and a name the customer can read on
// their own dashboard.

package health

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// FlushInterval is how often the in-memory aggregate reaches disk. A minute is
// short enough that somebody who has just fixed their snippet sees the drops
// stop while they are still looking at the page, and long enough that a busy
// site costs one small write per minute rather than one per event.
const FlushInterval = time.Minute

// MaxTrackedValues caps how many distinct hostnames or tracker versions are
// held per site between flushes. A misconfigured wildcard DNS record can
// produce an unbounded set of hostnames, and an unbounded map on the ingest
// path is a memory leak with a customer's name on it.
const MaxTrackedValues = 50

// MaxTrackedValuesGlobal is a hard process ceiling while the per-site cap
// prevents one noisy hostname set from consuming every observation slot.
const MaxTrackedValuesGlobal = MaxTrackedValues * 8

// The observation kinds, matching the schema's CHECK constraint.
const (
	KindUnknownHostname = "unknown_hostname"
	KindTrackerVersion  = "tracker_version"
	KindIPSource        = "ip_source"
)

// The health row kinds, matching the schema's CHECK constraint.
const (
	KindAccepted   = "accepted"
	KindDropped    = "dropped"
	KindClassified = "classified"
	KindTruncated  = "truncated"
)

// Recorder turns the ingest path's observations into rows a panel can read.
//
// It aggregates in memory and flushes on a ticker rather than writing per
// event, for the reason every part of this system writes in batches: there is
// one SQLite writer per account, and a health feature that took that writer for
// every event would slow down the thing it exists to report on.
type Recorder struct {
	Accounts *accounts.Manager
	Sites    *sites.Cache
	Log      *logger.Logger

	// Now is the clock buckets are stamped with, injectable so a test can drive
	// two hours of traffic through in a millisecond.
	Now func() time.Time

	// mu guards everything below. One mutex rather than several because the
	// critical section is a handful of map writes and the contention that
	// matters is against the flush, not between two Observe calls.
	mu       sync.Mutex
	counts   map[countKey]int64
	observed map[observationKey]*observation
	last     map[int64]*lastRequest
}

// countKey is one counted fact: a site, an exact second, what happened, and why.
type countKey struct {
	accountID  int64
	siteID     int64
	observedAt int64
	kind       string
	reason     string
}

// observationKey is one named thing seen for a site.
type observationKey struct {
	accountID int64
	siteID    int64
	kind      string
	value     string
}

// observation keeps an exact-second count for one admitted value. Admission is
// capped by value rather than by seconds, so a normal hostname observed for a
// whole minute consumes one slot rather than sixty.
type observation struct {
	buckets map[int64]int64
}

// lastRequest is the most recent derived request for a site, held whole.
type lastRequest struct {
	accountID int64
	debug     ingest.Debug
	userAgent string
	version   int
	at        int64
}

// NewRecorder builds a recorder over the account manager and the routing map.
func NewRecorder(manager *accounts.Manager, cache *sites.Cache, log *logger.Logger) *Recorder {
	return &Recorder{
		Accounts: manager,
		Sites:    cache,
		Log:      log,
		Now:      func() time.Time { return time.Now().UTC() },
		counts:   map[countKey]int64{},
		observed: map[observationKey]*observation{},
		last:     map[int64]*lastRequest{},
	}
}

// now reads the injected clock, falling back to the real one.
func (r *Recorder) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}

	return r.Now().UTC()
}

// Observe records one request. It is the ingest.Observer implementation and
// runs inline on the hot path, so it does nothing but arithmetic under a mutex.
//
// An event with no site is skipped. There is no account database to write it to
// and no customer health panel it belongs to.
func (r *Recorder) Observe(o ingest.Observation) {
	if o.SiteID == 0 || o.AccountID == 0 {
		return
	}

	at := o.ReceivedAt
	if at == 0 {
		at = r.now().Unix()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !o.Pending {
		switch {
		case o.Accepted && o.DropReason != "":
			// A classified event was stored with its reason attached. It is counted
			// as both accepted and classified, because it is genuinely both, and a
			// panel that showed only one of the two would either hide a bot filter
			// or claim data loss that did not happen.
			r.counts[countKey{o.AccountID, o.SiteID, at, KindAccepted, ""}]++
			r.counts[countKey{o.AccountID, o.SiteID, at, KindClassified, o.DropReason}]++

		case o.Accepted:
			r.counts[countKey{o.AccountID, o.SiteID, at, KindAccepted, ""}]++

		default:
			r.counts[countKey{o.AccountID, o.SiteID, at, KindDropped, o.DropReason}]++
		}
	}

	if o.OutcomeOnly {
		return
	}

	for _, reason := range truncationReasons(o.Truncation) {
		r.counts[countKey{o.AccountID, o.SiteID, at, KindTruncated, reason.name}] += reason.count
	}

	if o.Debug.ClientIPSource != "" {
		r.note(o.AccountID, o.SiteID, KindIPSource, o.Debug.ClientIPSource, at)
	}

	if o.TrackerVersion > 0 {
		r.note(o.AccountID, o.SiteID, KindTrackerVersion, strconv.Itoa(o.TrackerVersion), at)
	}

	if hostname := o.Debug.Hostname; hostname != "" && r.unexpectedHostname(o.Debug.Domain, hostname) {
		r.note(o.AccountID, o.SiteID, KindUnknownHostname, sites.Normalise(hostname), at)
	}

	r.last[o.SiteID] = &lastRequest{
		accountID: o.AccountID,
		debug:     o.Debug,
		userAgent: o.UserAgent,
		version:   o.TrackerVersion,
		at:        at,
	}
}

// note records one named observation, refusing to grow past the cap. Dropping
// the fifty-first distinct hostname is the right trade: the warning it feeds
// says "events are arriving from hostnames you did not expect", and fifty
// examples make that point as well as fifty thousand would.
func (r *Recorder) note(accountID, siteID int64, kind, value string, at int64) {
	key := observationKey{accountID, siteID, kind, value}

	if existing, ok := r.observed[key]; ok {
		existing.buckets[at]++

		return
	}

	trackedForSite := r.trackedValues(accountID, siteID)
	if trackedForSite >= MaxTrackedValues {
		return
	}
	if len(r.observed) >= MaxTrackedValuesGlobal && !r.makeFairObservationRoom(accountID, siteID, trackedForSite) {
		return
	}

	r.observed[key] = &observation{buckets: map[int64]int64{at: 1}}
}

// trackedValues counts admitted distinct values for one site in the current
// flush buffer. The map is globally bounded at 400, so this linear scan stays
// bounded on the ingest path.
func (r *Recorder) trackedValues(accountID, siteID int64) int {
	tracked := 0
	for key := range r.observed {
		if key.accountID == accountID && key.siteID == siteID {
			tracked++
		}
	}

	return tracked
}

// makeFairObservationRoom evicts one value from the most represented site when
// the global ceiling is full and a quieter site arrives. This preserves the
// hard ceiling while ensuring nine or more active sites all retain evidence
// instead of letting the first eight consume every slot.
func (r *Recorder) makeFairObservationRoom(accountID, siteID int64, incoming int) bool {
	counts := map[[2]int64]int{}
	for key := range r.observed {
		counts[[2]int64{key.accountID, key.siteID}]++
	}

	donor := [2]int64{}
	donorCount := incoming
	for candidate, count := range counts {
		if count > donorCount {
			donor = candidate
			donorCount = count
		}
	}
	if donorCount <= incoming || donorCount <= 1 {
		return false
	}

	for key := range r.observed {
		if key.accountID == donor[0] && key.siteID == donor[1] {
			delete(r.observed, key)
			return true
		}
	}

	return false
}

// unexpectedHostname reports whether an event's hostname is one this site did
// not ask for.
//
// With an explicit allow-list the answer is simply "not on it". Without one —
// which is almost every site — the useful question is whether the hostname is
// the site's own domain or a subdomain of it. A snippet copied onto a staging
// domain, a scraper mirror or somebody else's site is exactly what this catches,
// and it is one of the few analytics problems that is completely invisible from
// the numbers alone.
func (r *Recorder) unexpectedHostname(domain, hostname string) bool {
	if r.Sites == nil {
		return false
	}

	site, ok := r.Sites.Lookup(domain)
	if !ok {
		return false
	}

	if len(site.AllowedHostnames) > 0 {
		return !site.HostnameAllowed(hostname)
	}

	registered := sites.Normalise(site.Domain)
	seen := sites.Normalise(hostname)

	return seen != registered && !strings.HasSuffix(seen, "."+registered)
}

// truncationCount is one truncation reason and how many of it an event carried.
type truncationCount struct {
	name  string
	count int64
}

// truncationReasons turns a truncation into the counters it feeds. It is a
// function rather than a method on Truncation so this package owns the mapping
// onto its own row kinds without the ingest package having to know they exist.
func truncationReasons(t ingest.Truncation) []truncationCount {
	if !t.Any() {
		return nil
	}

	var out []truncationCount

	if t.PropsDropped > 0 {
		// One event contributes one affected-event count no matter how many
		// properties beyond the cap it carried.
		out = append(out, truncationCount{ingest.TruncationProps, 1})
	}
	if t.PropNamesTruncated > 0 {
		out = append(out, truncationCount{ingest.TruncationPropName, int64(t.PropNamesTruncated)})
	}
	if t.PropValuesTruncated > 0 {
		out = append(out, truncationCount{ingest.TruncationPropValue, int64(t.PropValuesTruncated)})
	}
	if t.PropsUnsupported > 0 {
		out = append(out, truncationCount{ingest.TruncationPropUnsupported, int64(t.PropsUnsupported)})
	}
	if t.URLTruncated {
		out = append(out, truncationCount{ingest.TruncationURL, 1})
	}
	if t.EngagementClamped {
		out = append(out, truncationCount{ingest.TruncationEngagement, 1})
	}

	return out
}

// Flush writes everything aggregated so far and reports how many rows it wrote.
//
// The count is returned rather than discarded because a recorder that has
// silently stopped flushing produces a health panel that reads "no drops" —
// which is the single most dangerous thing this panel could ever say wrongly.
func (r *Recorder) Flush(ctx context.Context) (int, error) {
	r.mu.Lock()
	counts := r.counts
	observed := r.observed
	last := r.last

	r.counts = map[countKey]int64{}
	r.observed = map[observationKey]*observation{}
	r.last = map[int64]*lastRequest{}
	r.mu.Unlock()

	written := 0
	var firstErr error

	for key, count := range counts {
		account, err := r.Accounts.Open(ctx, key.accountID)
		if err != nil {
			firstErr = keepFirst(firstErr, err)
			r.requeueCount(key, count)
			continue
		}

		_, err = account.Writer().ExecContext(ctx, `
			INSERT INTO ingest_health (site_id, observed_at, kind, reason, count)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (site_id, observed_at, kind, reason) DO UPDATE SET count = count + excluded.count
		`, key.siteID, key.observedAt, key.kind, key.reason, count)
		if err != nil {
			firstErr = keepFirst(firstErr, fmt.Errorf("health: write counts: %w", err))
			r.requeueCount(key, count)
			continue
		}

		written++
	}

	for key, seen := range observed {
		account, err := r.Accounts.Open(ctx, key.accountID)
		if err != nil {
			firstErr = keepFirst(firstErr, err)
			r.requeueObservation(key, seen)
			continue
		}

		for observedAt, count := range seen.buckets {
			_, err = account.Writer().ExecContext(ctx, `
				INSERT INTO ingest_observations
					(site_id, observed_at, kind, value, count, first_seen_at, last_seen_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (site_id, observed_at, kind, value) DO UPDATE SET
					count = count + excluded.count,
					last_seen_at = MAX(last_seen_at, excluded.last_seen_at)
			`, key.siteID, observedAt, key.kind, key.value, count, observedAt, observedAt)
			if err != nil {
				firstErr = keepFirst(firstErr, fmt.Errorf("health: write observations: %w", err))
				r.requeueObservation(key, &observation{buckets: map[int64]int64{observedAt: count}})
				continue
			}

			written++
		}
	}

	for siteID, request := range last {
		account, err := r.Accounts.Open(ctx, request.accountID)
		if err != nil {
			firstErr = keepFirst(firstErr, err)
			r.requeueLast(siteID, request)
			continue
		}

		encoded, err := json.Marshal(request.debug)
		if err != nil {
			firstErr = keepFirst(firstErr, fmt.Errorf("health: encode debug: %w", err))
			r.requeueLast(siteID, request)
			continue
		}

		_, err = account.Writer().ExecContext(ctx, `
			INSERT INTO ingest_last_request
				(site_id, received_at, client_ip, client_ip_source, trusted_proxy,
				 hostname, pathname, user_agent, tracker_version, drop_reason, debug)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (site_id) DO UPDATE SET
				received_at = excluded.received_at,
				client_ip = excluded.client_ip,
				client_ip_source = excluded.client_ip_source,
				trusted_proxy = excluded.trusted_proxy,
				hostname = excluded.hostname,
				pathname = excluded.pathname,
				user_agent = excluded.user_agent,
				tracker_version = excluded.tracker_version,
				drop_reason = excluded.drop_reason,
				debug = excluded.debug
		`, siteID, request.at, request.debug.ClientIP, request.debug.ClientIPSource,
			boolToInt(request.debug.TrustedProxy), request.debug.Hostname, request.debug.Pathname,
			request.userAgent, request.version, request.debug.DropReason, string(encoded))
		if err != nil {
			firstErr = keepFirst(firstErr, fmt.Errorf("health: write last request: %w", err))
			r.requeueLast(siteID, request)
			continue
		}

		written++
	}

	return written, firstErr
}

// requeueCount puts one failed delta behind observations that arrived while the
// flush was writing. Addition makes the merge lossless regardless of which
// side of the failed write an event arrived on.
func (r *Recorder) requeueCount(key countKey, count int64) {
	r.mu.Lock()
	r.counts[key] += count
	r.mu.Unlock()
}

// requeueObservation merges a failed named observation with any newer copy.
// Exact-second bucket counts are additive, preserving the complete interval
// represented by both aggregates.
func (r *Recorder) requeueObservation(key observationKey, failed *observation) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.observed[key]
	if !ok {
		buckets := make(map[int64]int64, len(failed.buckets))
		for at, count := range failed.buckets {
			buckets[at] = count
		}
		r.observed[key] = &observation{buckets: buckets}
		return
	}

	for at, count := range failed.buckets {
		current.buckets[at] += count
	}
}

// requeueLast restores a failed last-request write unless a newer request was
// already observed during the flush. In that case the newer request is the one
// the panel should eventually show.
func (r *Recorder) requeueLast(siteID int64, failed *lastRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.last[siteID]
	if !ok || failed.at > current.at {
		r.last[siteID] = failed
	}
}

// Run flushes on a ticker until the context is cancelled, and flushes once more
// on the way out so a shutdown does not throw away the last minute of evidence
// about whatever made somebody restart the process.
func (r *Recorder) Run(ctx context.Context) {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The cancelled context cannot be used for the final write, so the
			// flush gets a short one of its own.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()

			if _, err := r.Flush(final); err != nil && r.Log != nil {
				r.Log.Error("the ingestion health record could not be flushed on shutdown", "error", err)
			}

			return

		case <-ticker.C:
			if _, err := r.Flush(ctx); err != nil && r.Log != nil {
				r.Log.Error("the ingestion health record could not be flushed", "error", err)
			}
		}
	}
}

// keepFirst returns the first non-nil error, so a flush reports one failure
// rather than the last of many while still attempting every write.
func keepFirst(first, next error) error {
	if first != nil {
		return first
	}

	return next
}

// boolToInt converts for SQLite, which has no boolean type.
func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
