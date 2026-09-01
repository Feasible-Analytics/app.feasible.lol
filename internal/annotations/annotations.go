//
// annotations.go
// Dated notes on the graph: what happened, on the day it happened.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package annotations stores the dated notes that render as markers on the main
// graph. They exist to answer the question every traffic spike produces — "what
// did we do that day?" — six months after everybody has forgotten.
//
// A note is stored against a date rather than an instant. An annotation is
// about a day, not a moment: "we launched", "the CDN outage", "the newsletter
// went out". Storing an instant and converting it back would move somebody's
// marker by a day depending on which continent they read the dashboard from,
// which is a bug nobody would ever be able to describe clearly.
package annotations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// MaxBodyLength caps a note. Two hundred characters is a sentence and a link,
// which is what fits in a tooltip on a graph; anything longer is a document and
// belongs somewhere a person can read it properly.
const MaxBodyLength = 200

// datePattern is the only shape a date may take. It is matched rather than
// parsed-and-reformatted so that "2026-8-1" is refused instead of silently
// becoming a different string than the one the graph groups by.
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// The failures a caller must be able to tell apart.
var (
	// ErrNotFound means no annotation with that id belongs to that site.
	ErrNotFound = errors.New("annotations: not found")

	// ErrInvalid means the note itself is unusable.
	ErrInvalid = errors.New("annotations: invalid")
)

// Annotation is one dated note.
type Annotation struct {
	ID     int64 `json:"id"`
	SiteID int64 `json:"site_id"`

	// ShownOn is the local date the marker appears on, as YYYY-MM-DD in the
	// site's own timezone.
	ShownOn string `json:"shown_on"`

	Body string `json:"body"`

	// AuthorUserID and AuthorName are denormalised because the users table is
	// in another database entirely, and a marker's tooltip has to still say who
	// wrote it after they have left the team.
	AuthorUserID int64  `json:"author_user_id"`
	AuthorName   string `json:"author_name"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Validate rejects a note that cannot be stored.
func (a Annotation) Validate() error {
	if !datePattern.MatchString(a.ShownOn) {
		return fmt.Errorf("%w: the date must be written as YYYY-MM-DD", ErrInvalid)
	}

	if _, err := time.Parse("2006-01-02", a.ShownOn); err != nil {
		return fmt.Errorf("%w: %s is not a real date", ErrInvalid, a.ShownOn)
	}

	body := strings.TrimSpace(a.Body)

	if body == "" {
		return fmt.Errorf("%w: an annotation needs something written on it", ErrInvalid)
	}

	if len(body) > MaxBodyLength {
		return fmt.Errorf("%w: an annotation is at most %d characters", ErrInvalid, MaxBodyLength)
	}

	return nil
}

// Store reads and writes annotations in the per-account databases.
type Store struct {
	Accounts *accounts.Manager

	// Now stamps creation and edits.
	Now func() time.Time
}

// NewStore builds a store over the account manager.
func NewStore(manager *accounts.Manager) *Store {
	return &Store{Accounts: manager, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the injected clock, falling back to the real one.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// List returns a site's annotations within a date range, inclusive at both
// ends. The bounds are the same strings the dates are stored as, so the
// comparison is a string comparison — which is correct for ISO dates and needs
// no timezone reasoning at all.
func (s *Store) List(ctx context.Context, accountID, siteID int64, from, to string) ([]Annotation, error) {
	account, err := s.Accounts.Open(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("annotations: open account %d: %w", accountID, err)
	}

	// An open-ended range is the common case for a dashboard that has not
	// resolved its dates yet, and answering it with everything is far better
	// than answering it with nothing.
	if from == "" {
		from = "0000-01-01"
	}
	if to == "" {
		to = "9999-12-31"
	}

	rows, err := account.Reader().QueryContext(ctx, `
		SELECT id, site_id, shown_on, body, author_user_id, author_name, created_at, updated_at
		FROM annotations
		WHERE site_id = ? AND shown_on >= ? AND shown_on <= ?
		ORDER BY shown_on, id
	`, siteID, from, to)
	if err != nil {
		return nil, fmt.Errorf("annotations: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Annotation{}

	for rows.Next() {
		var annotation Annotation

		if err := rows.Scan(&annotation.ID, &annotation.SiteID, &annotation.ShownOn, &annotation.Body,
			&annotation.AuthorUserID, &annotation.AuthorName,
			&annotation.CreatedAt, &annotation.UpdatedAt); err != nil {
			return nil, fmt.Errorf("annotations: list: %w", err)
		}

		out = append(out, annotation)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("annotations: list: %w", err)
	}

	return out, nil
}

// Create writes a new note.
func (s *Store) Create(ctx context.Context, accountID int64, annotation Annotation) (Annotation, error) {
	if err := annotation.Validate(); err != nil {
		return Annotation{}, err
	}

	account, err := s.Accounts.Open(ctx, accountID)
	if err != nil {
		return Annotation{}, fmt.Errorf("annotations: open account %d: %w", accountID, err)
	}

	annotation.Body = strings.TrimSpace(annotation.Body)
	annotation.CreatedAt = s.now().Unix()
	annotation.UpdatedAt = annotation.CreatedAt

	result, err := account.Writer().ExecContext(ctx, `
		INSERT INTO annotations (site_id, shown_on, body, author_user_id, author_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, annotation.SiteID, annotation.ShownOn, annotation.Body,
		annotation.AuthorUserID, annotation.AuthorName, annotation.CreatedAt, annotation.UpdatedAt)
	if err != nil {
		return Annotation{}, fmt.Errorf("annotations: create: %w", err)
	}

	annotation.ID, _ = result.LastInsertId()

	return annotation, nil
}

// Update edits a note's date or text. The author is not changed: the person who
// wrote it is a fact about the note, and rewriting it on every edit would make
// the tooltip say whoever last fixed a typo.
func (s *Store) Update(ctx context.Context, accountID int64, annotation Annotation) error {
	if err := annotation.Validate(); err != nil {
		return err
	}

	account, err := s.Accounts.Open(ctx, accountID)
	if err != nil {
		return fmt.Errorf("annotations: open account %d: %w", accountID, err)
	}

	result, err := account.Writer().ExecContext(ctx, `
		UPDATE annotations SET shown_on = ?, body = ?, updated_at = ?
		WHERE id = ? AND site_id = ?
	`, annotation.ShownOn, strings.TrimSpace(annotation.Body), s.now().Unix(),
		annotation.ID, annotation.SiteID)
	if err != nil {
		return fmt.Errorf("annotations: update: %w", err)
	}

	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	return nil
}

// Delete removes a note. The site id is part of the predicate so that an id
// from one site can never delete a row belonging to another site in the same
// account database.
func (s *Store) Delete(ctx context.Context, accountID, siteID, id int64) error {
	account, err := s.Accounts.Open(ctx, accountID)
	if err != nil {
		return fmt.Errorf("annotations: open account %d: %w", accountID, err)
	}

	result, err := account.Writer().ExecContext(ctx, `
		DELETE FROM annotations WHERE id = ? AND site_id = ?
	`, id, siteID)
	if err != nil {
		return fmt.Errorf("annotations: delete: %w", err)
	}

	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	return nil
}

// scanOne is the shared single-row read, used by the tests and by any caller
// that needs to read back what it just wrote.
func scanOne(row *sql.Row) (Annotation, error) {
	var annotation Annotation

	err := row.Scan(&annotation.ID, &annotation.SiteID, &annotation.ShownOn, &annotation.Body,
		&annotation.AuthorUserID, &annotation.AuthorName, &annotation.CreatedAt, &annotation.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Annotation{}, ErrNotFound
	}
	if err != nil {
		return Annotation{}, fmt.Errorf("annotations: read: %w", err)
	}

	return annotation, nil
}

// Get reads one annotation.
func (s *Store) Get(ctx context.Context, accountID, siteID, id int64) (Annotation, error) {
	account, err := s.Accounts.Open(ctx, accountID)
	if err != nil {
		return Annotation{}, fmt.Errorf("annotations: open account %d: %w", accountID, err)
	}

	return scanOne(account.Reader().QueryRowContext(ctx, `
		SELECT id, site_id, shown_on, body, author_user_id, author_name, created_at, updated_at
		FROM annotations WHERE id = ? AND site_id = ?
	`, id, siteID))
}
