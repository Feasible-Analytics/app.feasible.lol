//
// stats.go
// Revalidating public and shared-link capabilities for each stats request.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// Capability headers are separate from Authorization because they identify a
// public/shared view, not a user or API-key principal. Reverse proxies normally
// omit request headers from access logs, which also keeps a share slug out of
// the URL used for every stats call.
const (
	HeaderShare  = "X-Feasible-Share"
	HeaderPublic = "X-Feasible-Public"
)

// ErrNoStatsCapability means the request did not present either public/shared
// mode. The serving layer may then try the authenticated-session path.
var ErrNoStatsCapability = errors.New("sharing: no stats capability")

// StatsAccess is the authorization result the stats handler needs. Filters are
// appended server-side and CacheKey separates otherwise-identical requests
// made through capabilities with different pinned segments.
type StatsAccess struct {
	Filters  []query.Filter
	CacheKey string
}

// StatsAuthorizer validates public and shared-link access against control.db.
// It deliberately holds no cache: revoking a link, making a site private, or
// adding a password must take effect on the very next stats request.
type StatsAuthorizer struct {
	Store  *Store
	Secret []byte
}

// Authorize validates a capability for the requested domain. The boolean says
// whether a capability was presented at all, allowing the caller to fall back
// to session authorization only when neither header exists.
func (a StatsAuthorizer) Authorize(r *http.Request, domain string) (StatsAccess, bool, error) {
	share := strings.TrimSpace(r.Header.Get(HeaderShare))
	public := strings.TrimSpace(r.Header.Get(HeaderPublic))

	if share == "" && public == "" {
		return StatsAccess{}, false, ErrNoStatsCapability
	}
	if share != "" && public != "" {
		return StatsAccess{}, true, errors.New("sharing: a request may present only one capability")
	}

	if public != "" {
		if public != "public" {
			return StatsAccess{}, true, ErrNotFound
		}

		link, err := a.Store.PublicSite(r.Context(), domain)
		if err != nil {
			return StatsAccess{}, true, err
		}

		return StatsAccess{CacheKey: fmt.Sprintf("public:%d", link.SiteID)}, true, nil
	}

	link, err := a.Store.Resolve(r.Context(), share)
	if err != nil {
		return StatsAccess{}, true, err
	}
	if sites.Normalise(link.Domain) != sites.Normalise(domain) {
		return StatsAccess{}, true, ErrNotFound
	}

	if link.HasPassword {
		cookie, cookieErr := r.Cookie(CookieName(link.Slug))
		if cookieErr != nil || !ValidSignature(a.Secret, link.Slug, cookie.Value) {
			return StatsAccess{}, true, ErrPasswordRequired
		}
	}

	filters, err := a.Store.SegmentFilters(r.Context(), link.SiteID, link.SegmentID)
	if err != nil {
		return StatsAccess{}, true, err
	}

	return StatsAccess{
		Filters:  filters,
		CacheKey: fmt.Sprintf("share:%d:segment:%d", link.ID, link.SegmentID),
	}, true, nil
}

// SegmentFilters loads and strictly decodes the filter set pinned to a link.
// A missing or malformed segment fails closed; returning no filters would turn
// a narrow shared view into the whole site's dashboard.
func (s *Store) SegmentFilters(ctx context.Context, siteID, segmentID int64) ([]query.Filter, error) {
	if segmentID == 0 {
		return nil, nil
	}

	var raw string

	err := s.db.QueryRowContext(ctx, `
		SELECT filters FROM saved_segments WHERE id = ? AND site_id = ?
	`, segmentID, siteID).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("%w: pinned segment is unavailable", ErrNotFound)
	}

	var filters []query.Filter
	if err := json.Unmarshal([]byte(raw), &filters); err != nil {
		return nil, fmt.Errorf("sharing: decode pinned segment %d: %w", segmentID, err)
	}
	if len(filters) == 0 {
		return nil, errors.New("sharing: a pinned segment has no filters")
	}

	return filters, nil
}

// CreateSegment stores an immutable filter set and returns its id. Segment
// editing creates a new row so links already sent keep the exact population
// their creator reviewed when they issued them.
func (s *Store) CreateSegment(ctx context.Context, siteID int64, name string, filters []query.Filter) (int64, error) {
	if siteID < 1 || len(filters) == 0 {
		return 0, errors.New("sharing: a segment needs a site and at least one filter")
	}

	raw, err := json.Marshal(filters)
	if err != nil {
		return 0, fmt.Errorf("sharing: encode segment: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO saved_segments (site_id, name, filters, created_at)
		VALUES (?, ?, ?, ?)
	`, siteID, strings.TrimSpace(name), string(raw), s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("sharing: create segment: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sharing: create segment: %w", err)
	}

	return id, nil
}
