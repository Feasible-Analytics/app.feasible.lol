//
// swarm.go
// Catching one crawler that loads each page from a different address.
//
// Created: 2026-10-20
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// swarmWindowSeconds is how close together two visits must start to belong to
// the same swarm.
const swarmWindowSeconds = 120

// swarmPages is how many different entry pages the unread visits around a
// visit must reach before it is a swarm. One page is a crowd following a
// shared link; a crawler walks the site.
const swarmPages = 3

// swarmCandidate reports whether a visit can be part of a swarm: our tracker
// sent its pageviews, and nothing referred it.
func (s *Session) swarmCandidate() bool {
	return s.Pageviews > 0 && s.Source == "" && s.ScreenSize != ""
}

// swarmKey is the device and place a swarm's visits share, as the interned ids
// the sessions table stores.
type swarmKey struct {
	siteID         int64
	screen         int64
	device         int64
	browser        int64
	browserVersion int64
	os             int64
	osVersion      int64
	language       int64
	country        int64
	region         int64
	city           int64
}

// swarmKeyOf builds a visit's key from the ids this batch interned.
func swarmKeyOf(s *Session, ids *dimensionIDs) swarmKey {
	return swarmKey{
		siteID:         s.SiteID,
		screen:         ids.of(intern.ScreenSize, s.ScreenSize),
		device:         ids.of(intern.DeviceType, s.DeviceType),
		browser:        ids.of(intern.Browser, s.Browser),
		browserVersion: ids.of(intern.BrowserVersion, s.BrowserVersion),
		os:             ids.of(intern.OS, s.OS),
		osVersion:      ids.of(intern.OSVersion, s.OSVersion),
		language:       ids.of(intern.Language, s.Language),
		country:        ids.of(intern.Country, s.Country),
		region:         ids.of(intern.Region, s.Region),
		city:           ids.of(intern.City, s.City),
	}
}

// swarmMember is one stored visit that shares a swarm key.
type swarmMember struct {
	sessionID int64
	startedAt int64
	entryPage int64
	engaged   bool

	// marked and unmarked say whether any event carries the swarm reason, and
	// whether any carries no reason at all, so only a change is written.
	marked   bool
	unmarked bool
}

// selectSwarmMembers reads the direct, tracker-sent visits that share one key
// and start inside a window.
const selectSwarmMembers = `
	SELECT s.id, s.started_at, s.entry_page_id,
		EXISTS (SELECT 1 FROM events e JOIN dim_event_name n ON n.id = e.name_id
			WHERE e.session_id = s.id AND n.value = 'engagement'),
		EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.bot_reason_id = ?),
		EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.bot_reason_id = 0)
	FROM sessions s
	WHERE s.site_id = ? AND s.started_at BETWEEN ? AND ?
	  AND s.source_id = 0 AND s.pageviews > 0 AND s.is_imported = 0
	  AND s.screen_size_id = ? AND s.screen_size_id <> 0 AND s.device_type_id = ?
	  AND s.browser_id = ? AND s.browser_version_id = ?
	  AND s.os_id = ? AND s.os_version_id = ? AND s.language_id = ?
	  AND s.country_id = ? AND s.region_id = ? AND s.city_id = ?
	ORDER BY s.started_at`

// markSwarms recomputes the swarm verdict around every dirty visit that could
// be in one, and writes each changed verdict onto that visit's events.
//
// A visit is in a swarm when it was never read and the unread visits sharing
// its key within the window reached swarmPages different entry pages. That
// depends on the neighbours, so a ping on one visit can clear the others. The
// read spans two windows past the dirty visits, which holds every neighbour of
// every visit judged here.
//
// Like the other visit verdicts it classifies rather than deletes, and leaves
// an event's own reason alone.
func markSwarms(ctx context.Context, tx *sql.Tx, dirty []*Session, ids *dimensionIDs) error {
	type span struct{ first, last int64 }

	spans := map[swarmKey]*span{}
	for _, session := range dirty {
		if !session.swarmCandidate() {
			continue
		}

		key := swarmKeyOf(session, ids)
		if got, ok := spans[key]; ok {
			got.first = min(got.first, session.StartedAt)
			got.last = max(got.last, session.StartedAt)
			continue
		}
		spans[key] = &span{session.StartedAt, session.StartedAt}
	}
	if len(spans) == 0 {
		return nil
	}

	reason := ids.of(intern.BotReason, ReasonCrawlerSwarm)
	if reason == intern.EmptyID {
		return fmt.Errorf("write batch: mark swarm: %q was not interned", ReasonCrawlerSwarm)
	}

	for key, bounds := range spans {
		members, err := loadSwarmMembers(ctx, tx, key, reason, bounds.first-2*swarmWindowSeconds, bounds.last+2*swarmWindowSeconds)
		if err != nil {
			return err
		}

		for i := range members {
			member := &members[i]
			if member.startedAt < bounds.first-swarmWindowSeconds || member.startedAt > bounds.last+swarmWindowSeconds {
				continue
			}

			swarm := !member.engaged && swarmEntryPages(members, member.startedAt) >= swarmPages

			switch {
			case swarm && member.unmarked:
				if _, err := tx.ExecContext(ctx,
					"UPDATE events SET bot_reason_id = ? WHERE session_id = ? AND bot_reason_id = 0",
					reason, member.sessionID,
				); err != nil {
					return fmt.Errorf("write batch: mark swarm: %w", err)
				}
			case !swarm && member.marked:
				if _, err := tx.ExecContext(ctx,
					"UPDATE events SET bot_reason_id = 0 WHERE session_id = ? AND bot_reason_id = ?",
					member.sessionID, reason,
				); err != nil {
					return fmt.Errorf("write batch: clear swarm: %w", err)
				}
			}
		}
	}

	return nil
}

// loadSwarmMembers reads every visit sharing a key that starts between two
// times.
func loadSwarmMembers(ctx context.Context, tx *sql.Tx, key swarmKey, reason, from, to int64) ([]swarmMember, error) {
	rows, err := tx.QueryContext(ctx, selectSwarmMembers,
		reason, key.siteID, from, to,
		key.screen, key.device, key.browser, key.browserVersion,
		key.os, key.osVersion, key.language,
		key.country, key.region, key.city,
	)
	if err != nil {
		return nil, fmt.Errorf("write batch: read swarm: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []swarmMember
	for rows.Next() {
		var member swarmMember
		if err := rows.Scan(
			&member.sessionID, &member.startedAt, &member.entryPage,
			&member.engaged, &member.marked, &member.unmarked,
		); err != nil {
			return nil, fmt.Errorf("write batch: read swarm: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("write batch: read swarm: %w", err)
	}

	return members, nil
}

// swarmEntryPages counts the different entry pages of the unread visits that
// start within one window of a time.
func swarmEntryPages(members []swarmMember, at int64) int {
	pages := map[int64]struct{}{}
	for _, other := range members {
		if other.engaged || other.startedAt < at-swarmWindowSeconds || other.startedAt > at+swarmWindowSeconds {
			continue
		}
		pages[other.entryPage] = struct{}{}
	}

	return len(pages)
}
