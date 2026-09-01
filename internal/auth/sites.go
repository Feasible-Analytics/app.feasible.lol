//
// sites.go
// Creating, listing, organising and reconfiguring sites.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// DualWriteWindow is how long a changed domain keeps accepting traffic on its
// old name.
//
// Seventy-two hours is not arbitrary: a domain change is almost always part of
// a migration somebody is doing over a weekend, and the snippet on the old site
// is updated last if at all. Cutting the old domain off the instant the setting
// is saved means every pageview between the change and the redeploy is lost,
// with nothing anywhere to say it happened.
const DualWriteWindow = 72 * time.Hour

// SparklineDays is how much history the little chart on the sites list covers.
// Thirty days is enough to see a trend and small enough that the query is a
// single index range per site.
const SparklineDays = 30

// Site is one tracked website. It mirrors control.db's sites row, which is the
// routing index — everything site-scoped that is not routing lives in the
// account database.
type Site struct {
	ID int64
	// AccountID is the immutable analytics database that holds the history.
	AccountID int64
	// TeamID is the current owner used for every access decision.
	TeamID              int64
	Domain              string
	DisplayName         string
	Timezone            string
	IsPublic            bool
	FolderID            int64
	StatsStartDate      int64
	Position            int64
	PinnedAt            int64
	PreviousDomain      string
	PreviousDomainUntil int64
	OnboardedAt         int64
	CreatedAt           int64
	UpdatedAt           int64

	// Sparkline is the last SparklineDays of visits, oldest first. It is filled
	// in by the list query rather than the row read, because it needs a
	// different database.
	Sparkline []int64

	// Visitors is the sum of Sparkline, which is what "sort by traffic" sorts
	// on. It is stored rather than recomputed so the sort does not walk the
	// slice once per comparison.
	Visitors int64
}

// Label is what the interface calls a site: the friendly name when there is
// one, the domain otherwise. Agencies running dozens of sites asked the
// incumbent for this for years — "acme-staging-2.vercel.app" tells nobody which
// client it belongs to.
func (s *Site) Label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}

	return s.Domain
}

// Pinned reports whether this site is held at the top of the list.
func (s *Site) Pinned() bool {
	return s.PinnedAt > 0
}

// DualWriteActive reports whether the old domain is still collecting, and is
// what the settings screen uses to show the countdown rather than leaving
// somebody to wonder whether the change took effect.
func (s *Site) DualWriteActive(now time.Time) bool {
	return s.PreviousDomain != "" && s.PreviousDomainUntil > now.Unix()
}

// Folder groups sites for someone managing a lot of them.
type Folder struct {
	ID        int64
	AccountID int64
	Name      string
	Position  int64
	CreatedAt int64

	// Sites is filled in when a folder is rendered as part of the list.
	Sites []*Site
}

// siteColumns is the shared select list, so a new column is added once.
const siteColumns = `id, account_id, COALESCE(owner_team_id, account_id), domain, display_name, timezone, is_public, folder_id,
	stats_start_date, position, pinned_at, previous_domain, previous_domain_until,
	onboarded_at, created_at, updated_at`

// scanSite reads one row in the shape siteColumns produces.
func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var (
		s          Site
		folderID   sql.NullInt64
		statsStart sql.NullInt64
		pinnedAt   sql.NullInt64
		prevUntil  sql.NullInt64
		onboarded  sql.NullInt64
	)

	err := row.Scan(&s.ID, &s.AccountID, &s.TeamID, &s.Domain, &s.DisplayName, &s.Timezone, &s.IsPublic,
		&folderID, &statsStart, &s.Position, &pinnedAt, &s.PreviousDomain, &prevUntil,
		&onboarded, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read site: %w", err)
	}

	s.FolderID = nullInt64(folderID)
	s.StatsStartDate = nullInt64(statsStart)
	s.PinnedAt = nullInt64(pinnedAt)
	s.PreviousDomainUntil = nullInt64(prevUntil)
	s.OnboardedAt = nullInt64(onboarded)

	return &s, nil
}

// CreateSite adds a site to a team.
//
// The domain is normalised the same way the ingest path normalises the one it
// reads off an event — lowercased, trailing dot and leading www stripped —
// because a site registered as "WWW.Example.com" and a snippet that reports
// "example.com" are the same site, and treating them as two is total, silent
// data loss for whichever spelling is not in the routing map.
func (s *Store) CreateSite(ctx context.Context, accountID int64, domain, displayName, timezone string) (*Site, error) {
	domain = sites.Normalise(CleanDomain(domain))
	if domain == "" {
		return nil, fmt.Errorf("auth: a domain is required")
	}

	if err := ValidateDomain(domain); err != nil {
		return nil, err
	}

	if timezone == "" {
		timezone = "Etc/UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return nil, fmt.Errorf("auth: %q is not a timezone name", timezone)
	}

	now := s.now()

	// The new site sorts to the end of the list. Positions are a sparse rank
	// with a wide gap, so a later drag between two neighbours rewrites one row
	// instead of renumbering the whole list.
	var maxPosition sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MAX(position) FROM sites WHERE COALESCE(owner_team_id, account_id) = ?", accountID).Scan(&maxPosition); err != nil {
		return nil, fmt.Errorf("auth: create site: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO sites (account_id, owner_team_id, domain, display_name, timezone, position, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, accountID, accountID, domain, strings.TrimSpace(displayName), timezone,
		nullInt64(maxPosition)+1000, now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDomainTaken
		}
		return nil, fmt.Errorf("auth: create site: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("auth: create site: %w", err)
	}

	return s.SiteByID(ctx, accountID, id)
}

// SiteByID reads one site, scoped to the account that owns it. The account id
// is in the WHERE clause on every read: a site id in a URL is guessable, and
// scoping in the query is the only version of this check that cannot be
// forgotten by a handler.
func (s *Store) SiteByID(ctx context.Context, accountID, siteID int64) (*Site, error) {
	return scanSite(s.db.QueryRowContext(ctx,
		"SELECT "+siteColumns+" FROM sites WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?", siteID, accountID))
}

// SiteByIDAny reads a site before the caller checks its live team or guest
// relationship. It belongs only in authorization paths that immediately call
// teams.AuthoriseSite; ordinary store callers should use SiteByID.
func (s *Store) SiteByIDAny(ctx context.Context, siteID int64) (*Site, error) {
	return scanSite(s.db.QueryRowContext(ctx,
		"SELECT "+siteColumns+" FROM sites WHERE id = ?", siteID))
}

// SiteByDomain finds a site by its current domain across all accounts, which is
// what the create form uses to tell somebody the domain is taken before they
// hit a constraint error.
func (s *Store) SiteByDomain(ctx context.Context, domain string) (*Site, error) {
	return scanSite(s.db.QueryRowContext(ctx,
		"SELECT "+siteColumns+" FROM sites WHERE domain = ?", sites.Normalise(CleanDomain(domain))))
}

// ListSites returns a team's sites in the requested order.
//
// Pinned sites always come first, whatever the sort. A pin is the user saying
// "this one, every time", and a sort order that could bury it would make the
// pin useless.
func (s *Store) ListSites(ctx context.Context, accountID int64, order string) (out []*Site, err error) {
	// The sort is a fixed switch rather than an interpolated column name.
	// Building an ORDER BY from a query parameter is how a sort control becomes
	// a SQL injection.
	var clause string

	switch order {
	case "name":
		clause = "CASE WHEN display_name = '' THEN domain ELSE display_name END COLLATE NOCASE ASC"
	case "created":
		clause = "created_at DESC"
	case "traffic":
		// Traffic lives in the account database, so it cannot be ordered here.
		// The rows come back in a stable order and the caller re-sorts once the
		// sparklines are attached.
		clause = "position ASC, id ASC"
	default:
		clause = "position ASC, id ASC"
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT "+siteColumns+" FROM sites WHERE COALESCE(owner_team_id, account_id) = ? ORDER BY "+
			"CASE WHEN pinned_at IS NULL THEN 1 ELSE 0 END, "+clause, accountID)
	if err != nil {
		return nil, fmt.Errorf("auth: list sites: %w", err)
	}
	defer closeSQLRows(rows, &err, "list sites")

	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, site)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list sites: %w", err)
	}

	return out, nil
}

// UpdateSiteGeneral changes the display name, the timezone and the public flag.
// The domain is deliberately not here: changing it has a routing consequence
// and a dual-write window, and bundling it with "rename this site" is how
// somebody changes a domain by accident.
func (s *Store) UpdateSiteGeneral(ctx context.Context, accountID, siteID int64, displayName, timezone string, isPublic bool) error {
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("auth: %q is not a timezone name", timezone)
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE sites SET display_name = ?, timezone = ?, is_public = ?, updated_at = ?
		WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?
	`, strings.TrimSpace(displayName), timezone, isPublic, s.now().Unix(), siteID, accountID); err != nil {
		return fmt.Errorf("auth: update site: %w", err)
	}

	return nil
}

// ChangeDomain moves a site to a new domain and opens the dual-write window.
//
// Nothing but the routing entry moves. Goals, funnels, segments and every event
// already recorded are keyed on sites.id, so the site keeps its history and its
// configuration and simply answers to a second name for three days. The
// incumbent keyed goals on the domain string instead, so their change-domain
// feature silently deleted every goal a customer had configured — the fix came
// later, and anybody who had already used the feature rebuilt by hand.
func (s *Store) ChangeDomain(ctx context.Context, accountID, siteID int64, newDomain string) error {
	newDomain = sites.Normalise(CleanDomain(newDomain))

	if err := ValidateDomain(newDomain); err != nil {
		return err
	}

	site, err := s.SiteByID(ctx, accountID, siteID)
	if err != nil {
		return err
	}

	if site.Domain == newDomain {
		return nil
	}

	now := s.now()

	_, err = s.db.ExecContext(ctx, `
		UPDATE sites
		SET domain = ?, previous_domain = ?, previous_domain_until = ?, updated_at = ?
		WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?
	`, newDomain, site.Domain, now.Add(DualWriteWindow).Unix(), now.Unix(), siteID, accountID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDomainTaken
		}
		return fmt.Errorf("auth: change domain: %w", err)
	}

	return nil
}

// SetPinned pins or unpins a site.
func (s *Store) SetPinned(ctx context.Context, accountID, siteID int64, pinned bool) error {
	var at any
	if pinned {
		at = s.now().Unix()
	}

	if _, err := s.db.ExecContext(ctx,
		"UPDATE sites SET pinned_at = ?, updated_at = ? WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?",
		at, s.now().Unix(), siteID, accountID); err != nil {
		return fmt.Errorf("auth: pin site: %w", err)
	}

	return nil
}

// MoveSite puts a site in a folder, or at the top level when folderID is zero,
// and gives it a position within it.
func (s *Store) MoveSite(ctx context.Context, accountID, siteID, folderID, position int64) error {
	var folder any
	if folderID > 0 {
		// The folder has to belong to the same team, or a crafted form would
		// move somebody else's site into your sidebar.
		ok, err := exists(ctx, s.db,
			"SELECT 1 FROM site_folders WHERE id = ? AND team_id = ?", folderID, accountID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}

		folder = folderID
	}

	if _, err := s.db.ExecContext(ctx,
		"UPDATE sites SET folder_id = ?, position = ?, updated_at = ? WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?",
		folder, position, s.now().Unix(), siteID, accountID); err != nil {
		return fmt.Errorf("auth: move site: %w", err)
	}

	return nil
}

// MarkOnboarded records that a site has finished, or skipped, installation.
// Both outcomes set the same column: the flag's only job is to stop the wizard
// reappearing, and somebody who chose to skip has answered that question just
// as clearly as somebody who finished.
func (s *Store) MarkOnboarded(ctx context.Context, accountID, siteID int64) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE sites SET onboarded_at = ?, updated_at = ? WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?",
		s.now().Unix(), s.now().Unix(), siteID, accountID); err != nil {
		return fmt.Errorf("auth: mark onboarded: %w", err)
	}

	return nil
}

// DeleteSite removes the routing entry. The stored events are deleted by the
// caller, which owns the account database handle; this is only the half that
// stops new traffic being routed.
func (s *Store) DeleteSite(ctx context.Context, accountID, siteID int64) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM sites WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?", siteID, accountID); err != nil {
		return fmt.Errorf("auth: delete site: %w", err)
	}

	return nil
}

// ListFolders returns a team's folders in their manual order.
func (s *Store) ListFolders(ctx context.Context, accountID int64) (out []*Folder, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, team_id, name, position, created_at
		FROM site_folders WHERE team_id = ?
		ORDER BY position ASC, id ASC
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("auth: list folders: %w", err)
	}
	defer closeSQLRows(rows, &err, "list folders")

	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.AccountID, &f.Name, &f.Position, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("auth: list folders: %w", err)
		}

		out = append(out, &f)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list folders: %w", err)
	}

	return out, nil
}

// CreateFolder adds a folder at the end of the list.
func (s *Store) CreateFolder(ctx context.Context, accountID int64, name string) (*Folder, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("auth: a folder needs a name")
	}

	var maxPosition sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MAX(position) FROM site_folders WHERE team_id = ?", accountID).Scan(&maxPosition); err != nil {
		return nil, fmt.Errorf("auth: create folder: %w", err)
	}

	now := s.now().Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO site_folders (team_id, name, position, created_at) VALUES (?, ?, ?, ?)
	`, accountID, name, nullInt64(maxPosition)+1000, now)
	if err != nil {
		return nil, fmt.Errorf("auth: create folder: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("auth: create folder: %w", err)
	}

	return &Folder{ID: id, AccountID: accountID, Name: name, CreatedAt: now}, nil
}

// RenameFolder changes a folder's name.
func (s *Store) RenameFolder(ctx context.Context, accountID, folderID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("auth: a folder needs a name")
	}

	if _, err := s.db.ExecContext(ctx,
		"UPDATE site_folders SET name = ? WHERE id = ? AND team_id = ?", name, folderID, accountID); err != nil {
		return fmt.Errorf("auth: rename folder: %w", err)
	}

	return nil
}

// DeleteFolder removes a folder. The sites inside it are not deleted — the
// schema sets their folder_id to NULL, which puts them back at the top level.
// Deleting somebody's sites because they tidied up a folder would be a
// catastrophic reading of "delete".
func (s *Store) DeleteFolder(ctx context.Context, accountID, folderID int64) error {
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM site_folders WHERE id = ? AND team_id = ?", folderID, accountID); err != nil {
		return fmt.Errorf("auth: delete folder: %w", err)
	}

	return nil
}

// ReorderFolders writes a new order from the ids the drag handle produced.
//
// The whole list is renumbered in one transaction rather than patched, because
// the browser sends the order it now shows and the only thing that matters is
// that the next page load agrees with it.
func (s *Store) ReorderFolders(ctx context.Context, accountID int64, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: reorder folders: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE site_folders SET position = ? WHERE id = ? AND team_id = ?",
			int64(i+1)*1000, id, accountID); err != nil {
			return fmt.Errorf("auth: reorder folders: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: reorder folders: %w", err)
	}

	return nil
}

// ReorderSites writes a new order for the sites in one folder, or at the top
// level when folderID is zero.
func (s *Store) ReorderSites(ctx context.Context, accountID, folderID int64, ids []int64) error {
	var folder any
	if folderID > 0 {
		folder = folderID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: reorder sites: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	for i, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sites SET folder_id = ?, position = ?, updated_at = ? WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?",
			folder, int64(i+1)*1000, s.now().Unix(), id, accountID); err != nil {
			return fmt.Errorf("auth: reorder sites: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: reorder sites: %w", err)
	}

	return nil
}

// CleanDomain turns whatever somebody pasted into a bare hostname.
//
// People paste a full URL far more often than they type a hostname, and
// rejecting "https://example.com/blog?utm=x" with "that is not a domain" is a
// pointless argument with a user who gave us exactly the information we needed.
func CleanDomain(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	if strings.Contains(input, "://") {
		if parsed, err := url.Parse(input); err == nil && parsed.Host != "" {
			input = parsed.Host
		}
	}

	// A pasted URL without a scheme still has a path and a query on it.
	input = strings.SplitN(input, "/", 2)[0]
	input = strings.SplitN(input, "?", 2)[0]

	// A port is meaningful to a browser and meaningless to the routing map,
	// which is keyed on the hostname the tracker reports. A bare hostname has
	// no port and returns an error here, which is why the failure is ignored.
	if host, _, err := net.SplitHostPort(input); err == nil {
		input = host
	}

	return strings.ToLower(strings.Trim(input, "."))
}

// ValidateDomain rejects what cannot be a hostname. It is deliberately
// permissive about the shape — internal hostnames, punycode and new top-level
// domains all have to work — and only refuses what could not resolve at all.
func ValidateDomain(domain string) error {
	if domain == "" {
		return fmt.Errorf("enter the domain you want to track, such as example.com")
	}

	if len(domain) > 253 {
		return fmt.Errorf("that domain is too long")
	}

	if !strings.Contains(domain, ".") {
		return fmt.Errorf("that does not look like a domain — it needs a dot, such as example.com")
	}

	for _, r := range domain {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return fmt.Errorf("a domain can only contain letters, numbers, dots and hyphens")
		}
	}

	return nil
}
