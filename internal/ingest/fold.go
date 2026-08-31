//
// fold.go
// The accumulation rules: how one event changes the session row it belongs to.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

// newSession builds the initial state for a visit.
//
//	is_bounce   true unless the first event is a non-pageview interactive one
//	pageviews   1 for a pageview, 0 otherwise
//	events      1
//	duration    0
//	entry/exit  only set for a pageview
//	entry_props the first event's properties
//
// Everything above is then established by fold, which runs immediately after,
// so this function only has to set identity and the sentinels fold compares
// against. Splitting it that way is what keeps one copy of each rule: an
// initial state written out separately would be a second implementation of the
// same table, free to drift.
func (c *SessionCache) newSession(event *Event) *Session {
	return &Session{
		ID:        c.allocateID(event.AccountID),
		AccountID: event.AccountID,
		SiteID:    event.SiteID,
		UserID:    event.UserID,

		StartedAt:  event.Timestamp,
		LastSeenAt: event.Timestamp,

		// The sentinels put the first event unambiguously before the entry
		// marker and after the exit marker, so fold needs no "is this the first
		// one" branch.
		FirstAt: maxInt64,
		EntryAt: maxInt64,
		ExitAt:  minInt64,
	}
}

// fold applies one event to a session. Every rule below is a function of the
// event's own timestamp and the facts already recorded, never of the order
// events arrived in — which is what makes a retried or reordered stream produce
// exactly the same row.
func (s *Session) fold(event *Event) {
	s.Dirty = true

	// The two ends of the visit. Taking the minimum and the maximum rather than
	// assuming forward motion is what lets a late event extend a session
	// backwards, which is exactly what a retry does.
	if event.Timestamp < s.StartedAt {
		s.StartedAt = event.Timestamp
	}
	if event.Timestamp > s.LastSeenAt {
		s.LastSeenAt = event.Timestamp
	}

	// Engagement pings refresh the end of the visit and nothing else. They are
	// not events a customer counted, and counting them would make every session
	// look several times busier than it was.
	if !event.IsEngagement() {
		s.Events++
	}

	if event.IsPageview() {
		s.Pageviews++
	} else if event.Interactive && !event.IsEngagement() {
		// A non-pageview interactive event ends a bounce on its own, which is
		// how a single-page site with a working sign-up form stops reporting a
		// hundred per cent bounce rate. An engagement ping is excluded because
		// it is emitted by the tracker rather than by a person: counting it
		// would make the bounce rate of every site zero.
		s.InteractiveNonPageview = true
	}

	tie := event.UUID.String()

	// Attribution and the device block are frozen at session start: they come
	// from the earliest event, whenever it arrives. This is the rule that
	// generates the most support questions — a UTM tag on the second pageview of
	// a visit is discarded — and it is correct: it produces last-click-per-
	// session attribution.
	if earlier(event.Timestamp, tie, s.FirstAt, s.FirstTie) {
		s.FirstAt, s.FirstTie = event.Timestamp, tie

		s.Referrer = event.Referrer
		s.Source = event.Source
		s.Channel = event.Channel
		s.UTMSource = event.UTMSource
		s.UTMMedium = event.UTMMedium
		s.UTMCampaign = event.UTMCampaign

		s.Country = event.Country
		s.Region = event.Region
		s.City = event.City

		s.DeviceType = event.DeviceType
		s.ScreenSize = event.ScreenSize
		s.Browser = event.Browser
		s.BrowserVersion = event.BrowserVersion
		s.OS = event.OS
		s.OSVersion = event.OSVersion
		s.Language = event.Language

		s.EntryProps = event.Props
	}

	if !event.IsPageview() {
		return
	}

	// The entry page is the earliest pageview and the exit page is the latest,
	// both by the event's own timestamp. The uuid breaks an exact tie so that
	// two pageviews in the same second always resolve the same way.
	if earlier(event.Timestamp, tie, s.EntryAt, s.EntryTie) {
		s.EntryAt, s.EntryTie = event.Timestamp, tie
		s.EntryPage = event.Pathname
		s.EntryHostname = event.Hostname
	}

	if later(event.Timestamp, tie, s.ExitAt, s.ExitTie) {
		s.ExitAt, s.ExitTie = event.Timestamp, tie
		s.ExitPage = event.Pathname
		s.ExitHostname = event.Hostname
	}
}

// absorb folds one session into another. It is the repair for an out-of-order
// event that bridged two sessions which were always one visit, and it combines
// every field the same way fold does — a merge that lost the entry page would
// be a silently wrong row that no later job could detect.
func (s *Session) absorb(other *Session) {
	s.Dirty = true

	if other.StartedAt < s.StartedAt {
		s.StartedAt = other.StartedAt
	}
	if other.LastSeenAt > s.LastSeenAt {
		s.LastSeenAt = other.LastSeenAt
	}

	s.Pageviews += other.Pageviews
	s.Events += other.Events
	s.InteractiveNonPageview = s.InteractiveNonPageview || other.InteractiveNonPageview

	if earlier(other.FirstAt, other.FirstTie, s.FirstAt, s.FirstTie) {
		s.FirstAt, s.FirstTie = other.FirstAt, other.FirstTie
		s.Referrer, s.Source, s.Channel = other.Referrer, other.Source, other.Channel
		s.UTMSource, s.UTMMedium, s.UTMCampaign = other.UTMSource, other.UTMMedium, other.UTMCampaign
		s.Country, s.Region, s.City = other.Country, other.Region, other.City
		s.DeviceType, s.ScreenSize = other.DeviceType, other.ScreenSize
		s.Browser, s.BrowserVersion = other.Browser, other.BrowserVersion
		s.OS, s.OSVersion, s.Language = other.OS, other.OSVersion, other.Language
		s.EntryProps = other.EntryProps
	}

	if earlier(other.EntryAt, other.EntryTie, s.EntryAt, s.EntryTie) {
		s.EntryAt, s.EntryTie = other.EntryAt, other.EntryTie
		s.EntryPage, s.EntryHostname = other.EntryPage, other.EntryHostname
	}

	if later(other.ExitAt, other.ExitTie, s.ExitAt, s.ExitTie) {
		s.ExitAt, s.ExitTie = other.ExitAt, other.ExitTie
		s.ExitPage, s.ExitHostname = other.ExitPage, other.ExitHostname
	}
}
