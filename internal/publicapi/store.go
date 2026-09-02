//
// store.go
// The system-database reads and writes the provisioning endpoints need.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// ErrNotFound means the row does not exist, or exists but belongs to somebody
// else. The two are one error deliberately: an API that distinguishes them is an
// oracle for which domains are registered with us.
var ErrNotFound = errors.New("not found")

// ErrConflict means the write would violate something the caller can see and
// fix — a domain that is already registered, a guest who is already a guest.
var ErrConflict = errors.New("already exists")

// SystemStore is the provisioning endpoints' access to system.db.
//
// It is a type of its own rather than raw SQL in the handlers because every one
// of these queries carries a team-id predicate, and a handler that forgets one
// is a handler that returns somebody else's site. Keeping them here means the
// predicate is written next to the query it belongs to and can be read in one
// place.
type SystemStore struct {
	db *sql.DB

	// Now is the clock, injectable so a test can assert on created_at without
	// reading the wall clock twice and hoping.
	Now func() time.Time
}

// NewSystemStore builds a store over the system database.
func NewSystemStore(db *sql.DB) *SystemStore {
	return &SystemStore{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the store's clock.
func (c *SystemStore) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}

	return c.Now()
}

// Site is one site as the provisioning API sees it: everything on the row,
// unlike the routing snapshot, which carries only what the ingest path needs.
type Site struct {
	ID             int64  `json:"-"`
	Domain         string `json:"domain"`
	DisplayName    string `json:"display_name"`
	Timezone       string `json:"timezone"`
	IsPublic       bool   `json:"is_public"`
	StatsStartDate *int64 `json:"stats_start_date,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// siteColumns is the shared select list, so a column added to one read cannot
// be missing from another.
const siteColumns = `id, domain, display_name, timezone, is_public, stats_start_date, created_at, updated_at`

// scanSite reads one site row.
func scanSite(scan func(...any) error) (*Site, error) {
	var (
		site     Site
		isPublic int
		start    sql.NullInt64
	)

	if err := scan(&site.ID, &site.Domain, &site.DisplayName, &site.Timezone,
		&isPublic, &start, &site.CreatedAt, &site.UpdatedAt); err != nil {
		return nil, err
	}

	site.IsPublic = isPublic != 0
	if start.Valid {
		value := start.Int64
		site.StatsStartDate = &value
	}

	return &site, nil
}

// CreateSite registers a domain to a team.
//
// The domain is normalised to the same form the routing map is keyed by, so that
// a site created as "WWW.Example.com" and a tracker snippet that says
// "example.com" are the same site. Registering them as two would be a silent,
// total data loss for whichever one is not in the map.
func (c *SystemStore) CreateSite(ctx context.Context, teamID int64, domain, displayName, timezone string) (*Site, error) {
	normalised := sites.Normalise(domain)
	now := c.now().Unix()

	if displayName == "" {
		displayName = normalised
	}

	result, err := c.db.ExecContext(ctx, `
		INSERT INTO sites (account_id, owner_team_id, domain, display_name, timezone, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, teamID, teamID, normalised, displayName, timezone, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s is already registered", ErrConflict, normalised)
		}

		return nil, fmt.Errorf("publicapi: create site: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("publicapi: create site: %w", err)
	}

	return &Site{
		ID: id, Domain: normalised, DisplayName: displayName, Timezone: timezone,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// isUniqueViolation reports whether an error is a uniqueness conflict.
//
// The driver's error is matched on its text because modernc.org/sqlite does not
// expose an extended result code through database/sql. That is fragile enough to
// be worth saying out loud: the fallback is a 500 rather than a 409, which is
// wrong but not dangerous.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}

// GetSite reads one site belonging to a team.
func (c *SystemStore) GetSite(ctx context.Context, teamID int64, domain string) (*Site, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT `+siteColumns+` FROM sites WHERE COALESCE(owner_team_id, account_id) = ? AND domain = ?`, teamID, sites.Normalise(domain))

	site, err := scanSite(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("publicapi: get site: %w", err)
	}

	return site, nil
}

// ListSites returns one page of a team's sites, in domain order. The order is
// stable because pagination over an unordered list returns a second page that is
// not the rest of the first.
func (c *SystemStore) ListSites(ctx context.Context, teamID int64, limit, offset int) ([]*Site, int, error) {
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites WHERE COALESCE(owner_team_id, account_id) = ?`, teamID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("publicapi: list sites: %w", err)
	}

	rows, err := c.db.QueryContext(ctx,
		`SELECT `+siteColumns+` FROM sites WHERE COALESCE(owner_team_id, account_id) = ? ORDER BY domain LIMIT ? OFFSET ?`,
		teamID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("publicapi: list sites: %w", err)
	}
	defer func() { _ = rows.Close() }()

	list := []*Site{}

	for rows.Next() {
		site, err := scanSite(rows.Scan)
		if err != nil {
			return nil, 0, fmt.Errorf("publicapi: list sites: %w", err)
		}

		list = append(list, site)
	}

	return list, total, rows.Err()
}

// UpdateSite changes the fields a customer may change. The domain is one of
// them: renaming a site keeps its history, where deleting and recreating would
// throw it away, and people do rename domains.
func (c *SystemStore) UpdateSite(ctx context.Context, teamID, siteID int64, domain, displayName, timezone *string, isPublic *bool) (*Site, error) {
	sets := []string{"updated_at = ?"}
	args := []any{c.now().Unix()}

	if domain != nil {
		sets = append(sets, "domain = ?")
		args = append(args, sites.Normalise(*domain))
	}

	if displayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *displayName)
	}

	if timezone != nil {
		sets = append(sets, "timezone = ?")
		args = append(args, *timezone)
	}

	if isPublic != nil {
		flag := 0
		if *isPublic {
			flag = 1
		}
		sets = append(sets, "is_public = ?")
		args = append(args, flag)
	}

	args = append(args, siteID, teamID)

	result, err := c.db.ExecContext(ctx,
		`UPDATE sites SET `+strings.Join(sets, ", ")+` WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: that domain is already registered", ErrConflict)
		}

		return nil, fmt.Errorf("publicapi: update site: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("publicapi: update site: %w", err)
	}

	if affected == 0 {
		return nil, ErrNotFound
	}

	row := c.db.QueryRowContext(ctx, `SELECT `+siteColumns+` FROM sites WHERE id = ?`, siteID)

	site, err := scanSite(row.Scan)
	if err != nil {
		return nil, fmt.Errorf("publicapi: update site: %w", err)
	}

	return site, nil
}

// TrackerConfig is the per-site script configuration.
type TrackerConfig struct {
	APIEndpoint    string `json:"api_endpoint"`
	HashRouting    bool   `json:"hash_routing"`
	ManualTagging  bool   `json:"manual_tagging"`
	OutboundLinks  bool   `json:"outbound_links"`
	FileDownloads  bool   `json:"file_downloads"`
	Track404       bool   `json:"track_404"`
	TrackLocalhost bool   `json:"track_localhost"`
	ExcludedPages  string `json:"excluded_pages"`
	FileTypes      string `json:"file_types"`
}

// TrackerConfig reads a site's script configuration, answering the defaults for
// a site that has never been configured. A missing row is not an error: every
// site has a tracker configuration, and the row only exists once somebody has
// changed something.
func (c *SystemStore) TrackerConfig(ctx context.Context, siteID int64) (*TrackerConfig, error) {
	var (
		config                                             TrackerConfig
		hash, manual, outbound, downloads, notFound, local int
	)

	err := c.db.QueryRowContext(ctx, `
		SELECT api_endpoint, hash_routing, manual_tagging, outbound_links, file_downloads,
		       track_404, track_localhost, excluded_pages, file_types
		FROM site_tracker_config WHERE site_id = ?`, siteID).
		Scan(&config.APIEndpoint, &hash, &manual, &outbound, &downloads, &notFound, &local,
			&config.ExcludedPages, &config.FileTypes)

	if errors.Is(err, sql.ErrNoRows) {
		return &TrackerConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("publicapi: tracker config: %w", err)
	}

	config.HashRouting = hash != 0
	config.ManualTagging = manual != 0
	config.OutboundLinks = outbound != 0
	config.FileDownloads = downloads != 0
	config.Track404 = notFound != 0
	config.TrackLocalhost = local != 0

	return &config, nil
}

// SaveTrackerConfig writes a site's script configuration.
func (c *SystemStore) SaveTrackerConfig(ctx context.Context, siteID int64, config *TrackerConfig) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO site_tracker_config
			(site_id, api_endpoint, hash_routing, manual_tagging, outbound_links, file_downloads,
			 track_404, track_localhost, excluded_pages, file_types, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET
			api_endpoint = excluded.api_endpoint,
			hash_routing = excluded.hash_routing,
			manual_tagging = excluded.manual_tagging,
			outbound_links = excluded.outbound_links,
			file_downloads = excluded.file_downloads,
			track_404 = excluded.track_404,
			track_localhost = excluded.track_localhost,
			excluded_pages = excluded.excluded_pages,
			file_types = excluded.file_types,
			updated_at = excluded.updated_at`,
		siteID, config.APIEndpoint, boolToInt(config.HashRouting), boolToInt(config.ManualTagging),
		boolToInt(config.OutboundLinks), boolToInt(config.FileDownloads), boolToInt(config.Track404),
		boolToInt(config.TrackLocalhost), config.ExcludedPages, config.FileTypes, c.now().Unix())
	if err != nil {
		return fmt.Errorf("publicapi: save tracker config: %w", err)
	}

	return nil
}

// boolToInt renders a flag for SQLite, which has no boolean type.
func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

// CustomProperty is one allowed property name on a site.
type CustomProperty struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Scope     string `json:"scope,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// CustomProperties lists a site's allowed properties.
func (c *SystemStore) CustomProperties(ctx context.Context, siteID int64) ([]CustomProperty, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, key, created_at FROM site_custom_properties WHERE site_id = ? ORDER BY key`, siteID)
	if err != nil {
		return nil, fmt.Errorf("publicapi: custom properties: %w", err)
	}
	defer func() { _ = rows.Close() }()

	properties := []CustomProperty{}

	for rows.Next() {
		var property CustomProperty
		if err := rows.Scan(&property.ID, &property.Key, &property.CreatedAt); err != nil {
			return nil, fmt.Errorf("publicapi: custom properties: %w", err)
		}

		properties = append(properties, property)
	}

	return properties, rows.Err()
}

// AddCustomProperty allows a property on a site. Adding one that is already
// allowed is a success rather than a conflict, because an integration that
// declares its properties on every deploy should not have to remember which of
// them it declared last time.
func (c *SystemStore) AddCustomProperty(ctx context.Context, siteID int64, key string) (*CustomProperty, error) {
	now := c.now().Unix()

	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO site_custom_properties (site_id, key, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (site_id, key) DO NOTHING`, siteID, key, now); err != nil {
		return nil, fmt.Errorf("publicapi: add custom property: %w", err)
	}

	var property CustomProperty
	if err := c.db.QueryRowContext(ctx,
		`SELECT id, key, created_at FROM site_custom_properties WHERE site_id = ? AND key = ?`,
		siteID, key).Scan(&property.ID, &property.Key, &property.CreatedAt); err != nil {
		return nil, fmt.Errorf("publicapi: add custom property: %w", err)
	}

	return &property, nil
}

// DeleteCustomProperty stops allowing a property.
func (c *SystemStore) DeleteCustomProperty(ctx context.Context, siteID, id int64) error {
	return c.deleteScoped(ctx, "site_custom_properties", "site_id", siteID, id)
}

// SharedLink is one public dashboard URL.
type SharedLink struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	URL         string `json:"url"`
	HasPassword bool   `json:"has_password"`
	CreatedAt   int64  `json:"created_at"`
}

// SharedLinks lists a site's shared links.
func (c *SystemStore) SharedLinks(ctx context.Context, siteID int64) ([]SharedLink, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, name, slug, password_hash, created_at FROM shared_links WHERE site_id = ? ORDER BY id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("publicapi: shared links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	links := []SharedLink{}

	for rows.Next() {
		var (
			link         SharedLink
			passwordHash string
		)

		if err := rows.Scan(&link.ID, &link.Name, &link.Slug, &passwordHash, &link.CreatedAt); err != nil {
			return nil, fmt.Errorf("publicapi: shared links: %w", err)
		}

		link.HasPassword = passwordHash != ""
		links = append(links, link)
	}

	return links, rows.Err()
}

// DeleteSharedLink revokes a link.
func (c *SystemStore) DeleteSharedLink(ctx context.Context, siteID, id int64) error {
	return c.deleteScoped(ctx, "shared_links", "site_id", siteID, id)
}

// Guest is one person with access to a single site but not to the team.
type Guest struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// Guests lists a site's guests.
func (c *SystemStore) Guests(ctx context.Context, siteID int64) ([]Guest, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT g.id, u.email, g.role, g.created_at
		FROM guest_memberships g JOIN users u ON u.id = g.user_id
		WHERE g.site_id = ? ORDER BY g.id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("publicapi: guests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	guests := []Guest{}

	for rows.Next() {
		var guest Guest
		if err := rows.Scan(&guest.ID, &guest.Email, &guest.Role, &guest.CreatedAt); err != nil {
			return nil, fmt.Errorf("publicapi: guests: %w", err)
		}

		guests = append(guests, guest)
	}

	return guests, rows.Err()
}

// DeleteGuest removes a guest's access.
func (c *SystemStore) DeleteGuest(ctx context.Context, siteID, id int64) error {
	return c.deleteScoped(ctx, "guest_memberships", "site_id", siteID, id)
}

// Member is one person in a team.
type Member struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

// Members lists a team.
func (c *SystemStore) Members(ctx context.Context, teamID int64) ([]Member, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT m.id, u.email, u.name, m.role, m.created_at
		FROM team_memberships m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = ? ORDER BY m.id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("publicapi: members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	members := []Member{}

	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.ID, &member.Email, &member.Name, &member.Role, &member.CreatedAt); err != nil {
			return nil, fmt.Errorf("publicapi: members: %w", err)
		}

		members = append(members, member)
	}

	return members, rows.Err()
}

// MembershipTarget reads the target user and role for a team-scoped membership
// id. The HTTP layer uses the role for a clear refusal, while the teams store
// repeats hierarchy and last-owner enforcement for the user inside its API.
func (c *SystemStore) MembershipTarget(ctx context.Context, teamID, id int64) (int64, string, error) {
	var userID int64
	var role string

	err := c.db.QueryRowContext(ctx,
		`SELECT user_id, role FROM team_memberships WHERE id = ? AND team_id = ?`, id, teamID).Scan(&userID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("publicapi: read membership target: %w", err)
	}

	return userID, role, nil
}

// deleteScoped removes one row by id, but only when it belongs to the scope the
// caller is authorised for. Every delete in this file goes through it so that an
// endpoint cannot delete another team's row by guessing an id.
func (c *SystemStore) deleteScoped(ctx context.Context, table, scopeColumn string, scope, id int64) error {
	// The table and column names are constants from this file's call sites, not
	// caller input; the values that came from a caller travel as parameters.
	result, err := c.db.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE id = ? AND `+scopeColumn+` = ?`, id, scope)
	if err != nil {
		return fmt.Errorf("publicapi: delete from %s: %w", table, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("publicapi: delete from %s: %w", table, err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}
