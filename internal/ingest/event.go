//
// event.go
// The derived event: everything the account writer needs, without raw identity.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"github.com/google/uuid"
)

// Event is one fully derived event on its way to the owning account database.
// It is the boundary type of the pipeline: everything above it deals in HTTP
// and raw headers, while everything below it deals in rows.
//
// What is *not* in this struct is the point of it. There is no IP address and
// no raw user agent — the address is used for geolocation and the fingerprint
// and then discarded before anything is written. The IP address never reaches
// disk, and the only way to keep that promise is for the durable boundary type
// to have nowhere to put one.
type Event struct {
	// UUID is stamped when the event is derived and never changes. It is what
	// makes a redelivery harmless: the account receipt exists or it does not.
	UUID uuid.UUID

	// Shard is the destination app shard's position in the ingester's static
	// shard list. Minus one means routing was incomplete when the event arrived;
	// the durable resolver will attach ownership before delivery.
	Shard int

	// AccountID names the database this is written to; SiteID is the site
	// within it. Both are resolved from the site cache, never from system.db
	// on the hot path.
	AccountID int64
	SiteID    int64

	// Domain is the claimed tracking domain. It survives derivation so a row
	// accepted while one app shard is unreachable can be routed after the
	// shard map becomes complete, without retaining the visitor's address.
	Domain string

	// Timestamp is unix seconds in UTC. Every accumulation rule keys off this
	// rather than arrival order, which is what makes a retry harmless.
	Timestamp int64

	// DerivedAt is the nanosecond this event was derived, forced strictly
	// increasing within the process. Timestamp is only accurate to the second,
	// so two pageviews of one visit routinely share one, and the accumulation
	// rules would otherwise have to settle "which came first" on the event
	// uuid — a coin toss that hands a visit's attribution to whichever page
	// happened to draw the lower id. This is stamped once, travels with the
	// event, and is never stored, so a retried or reordered delivery settles
	// the tie exactly the way the first delivery did.
	DerivedAt int64

	Name string

	// UserID is the fingerprint under today's salt. PreviousUserID is the same
	// visitor under yesterday's and exists only as a session-lookup fallback,
	// so a visitor mid-session at midnight keeps one identity instead of
	// splitting in two. It is never stored.
	UserID         int64
	PreviousUserID int64

	Hostname  string
	Pathname  string
	PageTitle string

	Referrer    string
	Source      string
	Channel     string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMContent  string
	UTMTerm     string

	// ClickIDParam is the *name* of the click-id parameter that was present,
	// never its value. A click id is a unique per-click identifier and is not
	// GDPR-compliant to store without consent, but knowing one was there is
	// what separates a paid click from an organic one.
	ClickIDParam string

	Country string
	Region  string
	City    string

	DeviceType     string
	ScreenSize     string
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Language       string

	ScrollDepth    int
	EngagementTime int64

	// BotReason is empty for a human. A classified event is still written —
	// filtering happens at query time behind a customer toggle, because
	// deleting it means a wrongly-classified visitor is gone forever.
	BotReason string

	// RejectReason carries the public tier's advisory claim-versus-page result.
	// It never authorizes a drop: the writer's live hostname rules are
	// authoritative and may have changed between derivation and commit.
	RejectReason string `json:",omitempty"`

	// Interactive drives the bounce rule for non-pageview events.
	Interactive bool

	// Props, Revenue and FullURL land in the cold event_details table, and only
	// when there is something to write.
	Props   map[string]string
	Revenue *Revenue
	FullURL string
}

// IsPageview reports whether this event counts towards the pageview metrics.
func (e *Event) IsPageview() bool {
	return e.Name == EventPageview
}

// IsEngagement reports whether this is an engagement ping. They are the one
// event kind that does not increment a session's event count — they exist to
// refresh last_seen_at and to carry time-on-page and scroll depth.
func (e *Event) IsEngagement() bool {
	return e.Name == EventEngagement
}

// HasDetails reports whether the cold table needs a row. Checking here keeps
// the decision in one place, so the flag stored on the hot row and the
// existence of the detail row can never disagree.
func (e *Event) HasDetails() bool {
	return len(e.Props) > 0 || e.Revenue != nil || e.UTMContent != "" || e.UTMTerm != "" || e.FullURL != ""
}
