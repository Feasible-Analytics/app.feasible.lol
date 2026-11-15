//
// store.go
// Every read and write the onboarding emails make against system.db.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store reads who is owed an email and records who asked for them to stop.
type Store struct {
	db *sql.DB
}

// NewStore builds a store over the system database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Team is one account inside the onboarding window, with the owner the emails
// are addressed to.
type Team struct {
	ID        int64
	CreatedAt time.Time

	OwnerEmail string
	OwnerName  string

	// Verified is whether the owner has proved their address.
	Verified bool
}

// Site is the site an email names and links the install screen of.
type Site struct {
	ID     int64
	Domain string
}

// Start reports the instant the onboarding emails began. A zero time means the
// row is missing, and the caller sends nothing rather than guessing.
func (s *Store) Start(ctx context.Context) (time.Time, error) {
	var started int64

	err := s.db.QueryRowContext(ctx, `SELECT started_at FROM onboarding_start WHERE singleton = 1`).Scan(&started)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("onboarding: read start: %w", err)
	}

	return time.Unix(started, 0).UTC(), nil
}

// Candidates lists every account created at or after an instant that may still
// be written to.
//
// Left out: an account with complimentary access, one that asked for these
// emails to stop, one being deleted, one with no billing lifecycle (created by
// an operator rather than by signing up), and one whose owner also belongs to
// somebody else's team or site. That last owner signed up to accept an
// invitation, and the empty team they were given is not one they mean to use.
func (s *Store) Candidates(ctx context.Context, since time.Time) ([]Team, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.created_at, u.email, u.name, u.email_verified_at
		FROM teams t
		JOIN account_lifecycle l ON l.team_id = t.id AND l.deleted_at IS NULL
		JOIN team_memberships m ON m.id = (
			SELECT MIN(first.id) FROM team_memberships first
			WHERE first.team_id = t.id AND first.role = 'owner'
		)
		JOIN users u ON u.id = m.user_id
		WHERE t.created_at >= ?
		  AND NOT EXISTS (SELECT 1 FROM account_comps c WHERE c.team_id = t.id)
		  AND NOT EXISTS (SELECT 1 FROM onboarding_optouts o WHERE o.team_id = t.id)
		  AND NOT EXISTS (
		      SELECT 1 FROM team_memberships other
		      WHERE other.user_id = u.id AND other.team_id <> t.id
		  )
		  AND NOT EXISTS (SELECT 1 FROM guest_memberships g WHERE g.user_id = u.id)
		ORDER BY t.id
	`, since.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("onboarding: candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Team

	for rows.Next() {
		var (
			team     Team
			created  int64
			verified sql.NullInt64
		)

		if err := rows.Scan(&team.ID, &created, &team.OwnerEmail, &team.OwnerName, &verified); err != nil {
			return nil, fmt.Errorf("onboarding: candidates: %w", err)
		}

		team.CreatedAt = time.Unix(created, 0).UTC()
		team.Verified = verified.Valid

		out = append(out, team)
	}

	return out, rows.Err()
}

// Sent maps each onboarding template already settled for a team onto when it
// was settled. The markers are the completed rows of the same outbox the
// billing lifecycle emails use, keyed by the team's creation instant.
func (s *Store) Sent(ctx context.Context, team Team) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT template, completed_at FROM lifecycle_outbox
		WHERE team_id = ? AND started_at = ? AND completed_at IS NOT NULL
	`, team.ID, team.CreatedAt.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("onboarding: sent %d: %w", team.ID, err)
	}
	defer func() { _ = rows.Close() }()

	sent := map[string]time.Time{}

	for rows.Next() {
		var (
			template  string
			completed int64
		)

		if err := rows.Scan(&template, &completed); err != nil {
			return nil, fmt.Errorf("onboarding: sent %d: %w", team.ID, err)
		}

		sent[template] = time.Unix(completed, 0).UTC()
	}

	return sent, rows.Err()
}

// FirstSite returns the oldest site on an account, and false when it has none.
func (s *Store) FirstSite(ctx context.Context, teamID int64) (Site, bool, error) {
	var site Site

	err := s.db.QueryRowContext(ctx, `
		SELECT id, domain FROM sites WHERE account_id = ? ORDER BY id LIMIT 1
	`, teamID).Scan(&site.ID, &site.Domain)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, false, nil
	}
	if err != nil {
		return Site{}, false, fmt.Errorf("onboarding: first site %d: %w", teamID, err)
	}

	return site, true, nil
}

// OptOut stops every onboarding email for one team. A second call, or a call
// for a team that no longer exists, changes nothing and is not an error.
func (s *Store) OptOut(ctx context.Context, teamID int64, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO onboarding_optouts (team_id, created_at)
		SELECT id, ? FROM teams WHERE id = ?
		ON CONFLICT (team_id) DO NOTHING
	`, now.UTC().Unix(), teamID)
	if err != nil {
		return false, fmt.Errorf("onboarding: opt out %d: %w", teamID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("onboarding: opt out %d: %w", teamID, err)
	}

	return affected > 0, nil
}
