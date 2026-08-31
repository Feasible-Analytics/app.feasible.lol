//
// event.go
// The derived event: everything the shard needs, and nothing that identifies anyone.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"github.com/google/uuid"
)

// Event is one fully derived event, on its way from an ingestor to the shard
// that owns it. It is the boundary type of the whole pipeline: everything above
// it deals in HTTP and raw headers, everything below it deals in rows.
//
// What is *not* in this struct is the point of it. There is no IP address and
// no raw user agent — the address is used for geolocation and the fingerprint
// and then discarded, before anything is written or forwarded. The IP address
// never reaches disk, and the only way to keep that promise is for the type
// that crosses the wire to have nowhere to put one.
type Event struct {
	// UUID is stamped when the event is derived and never changes. It is what
	// makes a redelivery harmless: the shard has seen this id or it has not.
	UUID uuid.UUID

	// Shard is where this event belongs. Every event carries one even when
	// there is exactly one shard, because the seam is what is expensive to
	// retrofit — the shard count is just configuration.
	Shard int

	// AccountID names the database this is written to; SiteID is the site
	// within it. Both are resolved from the site cache, never from control.db
	// on the hot path.
	AccountID int64
	SiteID    int64

	// Timestamp is unix seconds in UTC. Every accumulation rule keys off this
	// rather than arrival order, which is what makes a retry harmless.
	Timestamp int64

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

	Country       string
	Region        string
	CityGeonameID int64

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
