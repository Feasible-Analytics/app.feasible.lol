//
// dimensions.go
// Resolving every dimension string in a batch to its integer id.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// dimensionIDs holds the id for every dimension value in one batch.
type dimensionIDs struct {
	values map[dimensionKey]int64
}

// dimensionKey is one dimension and one value within it.
type dimensionKey struct {
	dimension intern.Dimension
	value     string
}

// dimensionResolver is implemented by both the warmed cache and its
// transaction-scoped view.
type dimensionResolver interface {
	ID(context.Context, intern.Dimension, string) (int64, error)
}

// of returns the id for a value. A value that was not interned cannot happen —
// internBatch walks exactly the fields the insert statements read — so a miss
// falls back to the empty-string id rather than failing an event over a
// programming error.
func (d *dimensionIDs) of(dimension intern.Dimension, value string) int64 {
	if value == "" {
		return intern.EmptyID
	}

	if id, ok := d.values[dimensionKey{dimension, value}]; ok {
		return id
	}

	return intern.EmptyID
}

// internBatch resolves every dimension string an account's batch will write.
// Both the events and the dirty sessions are walked, because a session's
// attribution is frozen at its first event and that event may have been written
// by an earlier batch — its strings are in memory but its ids are not.
func internBatch(ctx context.Context, cache dimensionResolver, rows []eventRow, sessions []*Session) (*dimensionIDs, error) {
	ids := &dimensionIDs{values: map[dimensionKey]int64{}}

	add := func(dimension intern.Dimension, value string) error {
		if value == "" {
			return nil
		}

		key := dimensionKey{dimension, value}
		if _, ok := ids.values[key]; ok {
			return nil
		}

		id, err := cache.ID(ctx, dimension, value)
		if err != nil {
			return err
		}
		ids.values[key] = id

		return nil
	}

	for _, row := range rows {
		event := row.event

		for _, pair := range []struct {
			dimension intern.Dimension
			value     string
		}{
			{intern.EventName, event.Name},
			{intern.Hostname, event.Hostname},
			{intern.Pathname, event.Pathname},
			{intern.PageTitle, event.PageTitle},
			{intern.Referrer, event.Referrer},
			{intern.Source, event.Source},
			{intern.Channel, event.Channel},
			{intern.UTMSource, event.UTMSource},
			{intern.UTMMedium, event.UTMMedium},
			{intern.UTMCampaign, event.UTMCampaign},
			{intern.Country, event.Country},
			{intern.Region, event.Region},
			{intern.City, event.City},
			{intern.DeviceType, event.DeviceType},
			{intern.ScreenSize, event.ScreenSize},
			{intern.Browser, event.Browser},
			{intern.BrowserVersion, event.BrowserVersion},
			{intern.OS, event.OS},
			{intern.OSVersion, event.OSVersion},
			{intern.Language, event.Language},
			{intern.BotReason, event.BotReason},
		} {
			if err := add(pair.dimension, pair.value); err != nil {
				return nil, err
			}
		}
	}

	for _, session := range sessions {
		for _, pair := range []struct {
			dimension intern.Dimension
			value     string
		}{
			{intern.Pathname, session.EntryPage},
			{intern.Pathname, session.ExitPage},
			{intern.Hostname, session.EntryHostname},
			{intern.Hostname, session.ExitHostname},
			{intern.Referrer, session.Referrer},
			{intern.Source, session.Source},
			{intern.Channel, session.Channel},
			{intern.UTMSource, session.UTMSource},
			{intern.UTMMedium, session.UTMMedium},
			{intern.UTMCampaign, session.UTMCampaign},
			{intern.Country, session.Country},
			{intern.Region, session.Region},
			{intern.City, session.City},
			{intern.DeviceType, session.DeviceType},
			{intern.ScreenSize, session.ScreenSize},
			{intern.Browser, session.Browser},
			{intern.BrowserVersion, session.BrowserVersion},
			{intern.OS, session.OS},
			{intern.OSVersion, session.OSVersion},
			{intern.Language, session.Language},
		} {
			if err := add(pair.dimension, pair.value); err != nil {
				return nil, err
			}
		}
	}

	return ids, nil
}
