//
// counters.go
// Every dropped event, truncated field and classification, counted and visible.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"sort"
	"sync"
)

// The complete set of reasons an event can be dropped or classified. It is a
// closed set on purpose: the reason travels back to the sender in the
// `x-feasible-dropped` header and lands on the customer's ingestion health
// panel, and a free-text reason would make both unqueryable.
//
// Three of these — bot, datacenter_ip and referrer_spam — are classifications
// rather than deletions. The row is still written with its bot_reason set and
// the customer gets a toggle, because the incumbent deletes bot traffic before
// storing it and a wrongly-classified visitor is then gone forever.
const (
	ReasonHostnameNotAllowed = "hostname_not_allowed"
	ReasonUnknownSite        = "unknown_site"
	ReasonAccountDormant     = "account_dormant"
	ReasonSiteDeleted        = "site_deleted"
	ReasonShieldIP           = "shield_ip"
	ReasonShieldCountry      = "shield_country"
	ReasonShieldPage         = "shield_page"
	ReasonNoSessionForEngage = "no_session_for_engagement"
	ReasonRateLimited        = "rate_limited"

	// ReasonInvalidPayload is a body we read but could not use — props or
	// revenue that are not the shape the field is for. It is a drop rather
	// than a 4xx because the sender is a beacon: a status code it cannot act
	// on only produces a retry that fails identically.
	ReasonInvalidPayload = "invalid_payload"

	// ReasonInternalError is an unrecoverable derive failure rather than a
	// temporary dependency outage. Retryable storage and salt failures answer
	// 503 without being falsely counted as accepted drops.
	ReasonInternalError = "internal_error"
)

// Reasons is every value the dropped header can carry. Tests assert that
// nothing outside this list ever reaches a response, because a typo in a reason
// is a metric that silently stops being counted.
var Reasons = []string{
	ReasonBot,
	ReasonDatacenterIP,
	ReasonReferrerSpam,
	ReasonHostnameNotAllowed,
	ReasonUnknownSite,
	ReasonAccountDormant,
	ReasonSiteDeleted,
	ReasonShieldIP,
	ReasonShieldCountry,
	ReasonShieldPage,
	ReasonNoSessionForEngage,
	ReasonRateLimited,
	ReasonInvalidPayload,
	ReasonInternalError,
}

// IsClassification reports whether a reason still results in a stored row. The
// distinction matters to the health panel: "we filed this as a bot" and "we
// threw this away" are different things to tell a customer, and conflating them
// is how somebody concludes their traffic is being lost.
func IsClassification(reason string) bool {
	switch reason {
	case ReasonBot, ReasonDatacenterIP, ReasonReferrerSpam:
		return true
	}

	return false
}

// Counters records what happened to the traffic we were sent. Never fail
// silently is the single biggest thing this product is fixing about the
// competition, and this type is where that promise is kept: every drop, every
// truncated field and every classification is counted per site, and the counts
// are what the ingestion health panel shows.
type Counters struct {
	mu sync.Mutex

	accepted map[int64]int64
	dropped  map[counterKey]int64
	truncs   map[counterKey]int64
}

// counterKey is a site and the thing being counted. Counting per site rather
// than per process is what makes the number actionable — a customer needs to
// know that *their* events are being dropped, not that some events somewhere
// are.
type counterKey struct {
	siteID int64
	reason string
}

// The truncation counter names. They are separate from the drop reasons because
// a truncated event was still accepted, and mixing them would make the drop
// count wrong.
const (
	TruncationProps      = "props_over_limit"
	TruncationPropName   = "prop_name_too_long"
	TruncationPropValue  = "prop_value_too_long"
	TruncationURL        = "url_too_long"
	TruncationEngagement = "engagement_time_clamped"

	// TruncationPropUnsupported counts properties whose value was an object,
	// an array or null. They are as lost as a thirty-first property is, and
	// the whole point of this type is that nothing is lost quietly.
	TruncationPropUnsupported = "prop_value_unsupported"
)

// NewCounters builds an empty set.
func NewCounters() *Counters {
	return &Counters{
		accepted: map[int64]int64{},
		dropped:  map[counterKey]int64{},
		truncs:   map[counterKey]int64{},
	}
}

// Accepted records one event that made it through.
//
// The per-site count here is the customer's own health panel. It stays in the
// product even though process-wide metrics are deliberately not exposed.
func (c *Counters) Accepted(siteID int64) {
	c.mu.Lock()
	c.accepted[siteID]++
	c.mu.Unlock()
}

// Dropped records one event that did not, under a reason from the closed set.
// A site id of zero means we never got as far as identifying the site, which is
// itself worth counting: it is what an unknown domain looks like.
func (c *Counters) Dropped(siteID int64, reason string) {
	c.mu.Lock()
	c.dropped[counterKey{siteID: siteID, reason: reason}]++
	c.mu.Unlock()
}

// Truncated records what an event carried that we could not keep. It takes the
// whole Truncation so the caller cannot record three of the four and leave the
// fourth invisible, which is precisely the failure mode being designed out.
func (c *Counters) Truncated(siteID int64, truncation Truncation) {
	if !truncation.Any() {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	record := func(field string, count int) {
		c.truncs[counterKey{siteID, field}] += int64(count)
	}

	if truncation.PropsDropped > 0 {
		record(TruncationProps, truncation.PropsDropped)
	}
	if truncation.PropNamesTruncated > 0 {
		record(TruncationPropName, truncation.PropNamesTruncated)
	}
	if truncation.PropValuesTruncated > 0 {
		record(TruncationPropValue, truncation.PropValuesTruncated)
	}
	if truncation.PropsUnsupported > 0 {
		record(TruncationPropUnsupported, truncation.PropsUnsupported)
	}
	if truncation.URLTruncated {
		record(TruncationURL, 1)
	}
	if truncation.EngagementClamped {
		record(TruncationEngagement, 1)
	}
}

// Count is one line on the health panel.
type Count struct {
	SiteID int64
	Reason string
	Count  int64
}

// Snapshot is the whole picture at one instant.
type Snapshot struct {
	Accepted    map[int64]int64
	Dropped     []Count
	Truncations []Count
}

// Snapshot copies the counters out in a stable order. Sorting means two reads a
// second apart produce a diff that is about the traffic rather than about Go's
// map iteration.
func (c *Counters) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := Snapshot{Accepted: make(map[int64]int64, len(c.accepted))}
	for siteID, count := range c.accepted {
		out.Accepted[siteID] = count
	}

	out.Dropped = flatten(c.dropped)
	out.Truncations = flatten(c.truncs)

	return out
}

// flatten turns a counter map into a sorted slice.
func flatten(counts map[counterKey]int64) []Count {
	out := make([]Count, 0, len(counts))
	for key, count := range counts {
		out = append(out, Count{SiteID: key.siteID, Reason: key.reason, Count: count})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].SiteID != out[j].SiteID {
			return out[i].SiteID < out[j].SiteID
		}
		return out[i].Reason < out[j].Reason
	})

	return out
}
