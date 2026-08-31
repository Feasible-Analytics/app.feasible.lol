//
// webhooks.go
// Endpoints, their secrets, and the delivery log the customer reads.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package webhooks pushes events to customer endpoints.
//
// Two rules shape everything here. The first is that a delivery never happens
// on the path that produced the event: publishing writes a row and enqueues a
// job and returns, so a customer endpoint that takes thirty seconds to answer
// costs a worker thirty seconds and costs event collection nothing. The second
// is that nothing fails silently — every attempt, including the ones that
// failed, is written to a log the customer can read, because a webhook that
// never arrived and left no trace is the complaint this feature exists to
// answer.
package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The event types. They are constants rather than free strings because a
// subscription to a misspelled type is a webhook that silently never fires, and
// the validator below is what turns that into a 400 at subscription time.
const (
	// EventGoalConverted fires when a visitor completes a goal.
	EventGoalConverted = "goal.converted"

	// EventTrafficSpike and EventTrafficDrop fire when a site's traffic leaves
	// the band its recent history predicts.
	EventTrafficSpike = "traffic.spike"
	EventTrafficDrop  = "traffic.drop"

	// EventUsageOverLimit fires when a team passes its plan's event allowance.
	EventUsageOverLimit = "usage.over_limit"

	// EventImportCompleted and EventImportFailed close the loop on a data
	// import, which can run for hours.
	EventImportCompleted = "import.completed"
	EventImportFailed    = "import.failed"

	// EventSiteCreated fires when a site is added, which is what an agency
	// provisioning sites programmatically hangs its own automation off.
	EventSiteCreated = "site.created"
)

// EventTypes is every type a customer may subscribe to, sorted so the API and
// the documentation list them in the same order every time.
func EventTypes() []string {
	types := []string{
		EventGoalConverted,
		EventImportCompleted,
		EventImportFailed,
		EventSiteCreated,
		EventTrafficDrop,
		EventTrafficSpike,
		EventUsageOverLimit,
	}

	sort.Strings(types)

	return types
}

// ValidEventType reports whether a name is one we send.
func ValidEventType(name string) bool {
	for _, known := range EventTypes() {
		if known == name {
			return true
		}
	}

	return false
}

// Failure thresholds. The warning goes out well before the endpoint is turned
// off, because "your webhook has been disabled" is a notice that arrives too
// late to act on — by then the events are already missed.
const (
	// WarnAfterFailures is when we email. Five consecutive failed attempts is
	// about twenty minutes on the backoff schedule, which is long enough to
	// rule out a deploy or a blip and early enough to be worth acting on.
	WarnAfterFailures = 5

	// DisableAfterFailures is when we stop trying. Fifteen consecutive failed
	// attempts is well over a day of backoff, so an endpoint is only turned off
	// after a day of never answering — and the warning went out on day one.
	DisableAfterFailures = 15
)

// ErrNotFound means no endpoint or delivery with that id belongs to the team.
var ErrNotFound = errors.New("webhook endpoint not found")

// Endpoint is one customer destination.
type Endpoint struct {
	ID     int64  `json:"id"`
	TeamID int64  `json:"-"`
	SiteID *int64 `json:"site_id,omitempty"`

	URL         string   `json:"url"`
	Description string   `json:"description"`
	EventTypes  []string `json:"event_types"`

	Enabled             bool   `json:"enabled"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	DisabledReason      string `json:"disabled_reason,omitempty"`

	// Secret is only ever populated on creation and on rotation. Listing
	// endpoints leaves it empty: showing a signing key on every list response
	// puts it in a log or a screenshot eventually.
	Secret string `json:"secret,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// Wants reports whether this endpoint has subscribed to a type. An empty list
// means every type, so a customer who wants everything is not signed up to a
// list they have to keep current as we add types.
func (e *Endpoint) Wants(eventType string) bool {
	if len(e.EventTypes) == 0 {
		return true
	}

	for _, subscribed := range e.EventTypes {
		if subscribed == eventType {
			return true
		}
	}

	return false
}

// Delivery is one row of the log.
type Delivery struct {
	ID         int64  `json:"id"`
	EndpointID int64  `json:"endpoint_id"`
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	Payload    string `json:"payload"`

	State       string `json:"state"`
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`

	ResponseStatus int    `json:"response_status,omitempty"`
	ResponseBody   string `json:"response_body,omitempty"`
	Error          string `json:"error,omitempty"`
	DurationMS     int    `json:"duration_ms"`

	CreatedAt     int64 `json:"created_at"`
	AttemptedAt   int64 `json:"attempted_at,omitempty"`
	NextAttemptAt int64 `json:"next_attempt_at,omitempty"`
	DeliveredAt   int64 `json:"delivered_at,omitempty"`
}

// Delivery states.
const (
	StatePending   = "pending"
	StateDelivered = "delivered"
	StateFailed    = "failed"
)

// Store reads and writes endpoints and deliveries in control.db.
type Store struct {
	db *sql.DB

	// Now is the clock, injectable so a test can walk a backoff schedule
	// without sleeping through it.
	Now func() time.Time
}

// NewStore builds a store over the control database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the store's clock.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now()
}

// DB exposes the handle the dispatcher and the worker share. They are separate
// types from the store but write to the same rows in the same transactions, and
// handing them a second connection would mean two writers to one SQLite file.
func (s *Store) DB() *sql.DB {
	return s.db
}

// ValidateURL refuses a destination we cannot usefully post to.
//
// The scheme check is the one that matters: a webhook that goes out over plain
// HTTP puts the signed payload — which can name a converting visitor's page and
// properties — on the wire in the clear. Plain HTTP is allowed only for
// loopback, because that is how somebody tests locally.
func ValidateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("url is required")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url is not a valid URL: %v", err)
	}

	if parsed.Host == "" {
		return errors.New("url must be absolute, for example https://example.com/hooks/feasible")
	}

	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return errors.New("url must use https — a signed payload on plain http is readable by anyone on the path")
	default:
		return fmt.Errorf("url scheme %q is not supported — use https", parsed.Scheme)
	}
}

// ValidateEventTypes refuses a subscription to a type we never send, naming the
// ones we do. A silent no-op subscription is a customer waiting forever for an
// event that was never going to arrive.
func ValidateEventTypes(types []string) error {
	for _, name := range types {
		if !ValidEventType(name) {
			return fmt.Errorf("unknown event type %q — the known types are %s", name, strings.Join(EventTypes(), ", "))
		}
	}

	return nil
}

// Create registers an endpoint and mints its signing secret. The plaintext
// secret is returned on the endpoint exactly once, here.
func (s *Store) Create(ctx context.Context, teamID int64, siteID *int64, rawURL, description string, eventTypes []string) (*Endpoint, error) {
	if teamID < 1 {
		return nil, errors.New("a webhook endpoint needs a team")
	}

	if err := ValidateURL(rawURL); err != nil {
		return nil, err
	}

	if err := ValidateEventTypes(eventTypes); err != nil {
		return nil, err
	}

	if eventTypes == nil {
		eventTypes = []string{}
	}

	encoded, err := json.Marshal(eventTypes)
	if err != nil {
		return nil, fmt.Errorf("webhooks: create: %w", err)
	}

	secret, err := NewSecret()
	if err != nil {
		return nil, err
	}

	now := s.now().Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_endpoints (team_id, site_id, url, description, event_types, secret, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		teamID, siteID, strings.TrimSpace(rawURL), description, string(encoded), secret, now, now)
	if err != nil {
		return nil, fmt.Errorf("webhooks: create: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("webhooks: create: %w", err)
	}

	return &Endpoint{
		ID: id, TeamID: teamID, SiteID: siteID, URL: strings.TrimSpace(rawURL),
		Description: description, EventTypes: eventTypes, Enabled: true,
		Secret: secret, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// endpointColumns is the select list every endpoint read shares, so a column
// added to one query cannot be missing from another.
const endpointColumns = `id, team_id, site_id, url, description, event_types, secret,
	previous_secret, previous_secret_until, enabled, consecutive_failures,
	warned_at, disabled_at, disabled_reason, created_at, updated_at`

// scanEndpoint reads one row, including the two secrets. The secrets are
// stripped by the callers that hand an endpoint to an API response; they are
// read here because the delivery path needs them.
func scanEndpoint(scan func(...any) error) (*Endpoint, string, string, int64, error) {
	var (
		endpoint      Endpoint
		siteID        sql.NullInt64
		types         string
		secret        string
		prevSecret    string
		prevUntil     sql.NullInt64
		enabled       int
		warnedAt      sql.NullInt64
		disabledAt    sql.NullInt64
		disabledCause string
	)

	if err := scan(&endpoint.ID, &endpoint.TeamID, &siteID, &endpoint.URL, &endpoint.Description,
		&types, &secret, &prevSecret, &prevUntil, &enabled, &endpoint.ConsecutiveFailures,
		&warnedAt, &disabledAt, &disabledCause, &endpoint.CreatedAt, &endpoint.UpdatedAt); err != nil {
		return nil, "", "", 0, err
	}

	if siteID.Valid {
		value := siteID.Int64
		endpoint.SiteID = &value
	}

	if err := json.Unmarshal([]byte(types), &endpoint.EventTypes); err != nil {
		// An unreadable subscription list must not become "every event": that
		// would start sending a customer types they never asked for.
		return nil, "", "", 0, fmt.Errorf("webhooks: endpoint %d has an unreadable event list: %w", endpoint.ID, err)
	}

	endpoint.Enabled = enabled != 0
	endpoint.DisabledReason = disabledCause

	var until int64
	if prevUntil.Valid {
		until = prevUntil.Int64
	}

	return &endpoint, secret, prevSecret, until, nil
}

// Get reads one endpoint belonging to a team, without its secret.
func (s *Store) Get(ctx context.Context, teamID, id int64) (*Endpoint, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+endpointColumns+` FROM webhook_endpoints WHERE id = ? AND team_id = ?`, id, teamID)

	endpoint, _, _, _, err := scanEndpoint(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: get: %w", err)
	}

	return endpoint, nil
}

// List returns a team's endpoints, newest first.
func (s *Store) List(ctx context.Context, teamID int64) ([]*Endpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+endpointColumns+` FROM webhook_endpoints WHERE team_id = ? ORDER BY id DESC`, teamID)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	defer rows.Close()

	endpoints := []*Endpoint{}

	for rows.Next() {
		endpoint, _, _, _, err := scanEndpoint(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("webhooks: list: %w", err)
		}

		endpoints = append(endpoints, endpoint)
	}

	return endpoints, rows.Err()
}

// Update changes an endpoint's destination, description, subscription or
// enabled flag. Re-enabling also clears the failure counter, because otherwise
// an endpoint that was disabled after fifteen failures would be one failure
// away from being disabled again the moment it is turned back on.
func (s *Store) Update(ctx context.Context, teamID, id int64, rawURL, description *string, eventTypes *[]string, enabled *bool) (*Endpoint, error) {
	existing, err := s.Get(ctx, teamID, id)
	if err != nil {
		return nil, err
	}

	if rawURL != nil {
		if err := ValidateURL(*rawURL); err != nil {
			return nil, err
		}
		existing.URL = strings.TrimSpace(*rawURL)
	}

	if description != nil {
		existing.Description = *description
	}

	if eventTypes != nil {
		if err := ValidateEventTypes(*eventTypes); err != nil {
			return nil, err
		}
		existing.EventTypes = *eventTypes
	}

	reEnabled := false
	if enabled != nil {
		reEnabled = *enabled && !existing.Enabled
		existing.Enabled = *enabled
	}

	encoded, err := json.Marshal(existing.EventTypes)
	if err != nil {
		return nil, fmt.Errorf("webhooks: update: %w", err)
	}

	now := s.now().Unix()
	flag := 0
	if existing.Enabled {
		flag = 1
	}

	query := `UPDATE webhook_endpoints SET url = ?, description = ?, event_types = ?, enabled = ?, updated_at = ?`
	args := []any{existing.URL, existing.Description, string(encoded), flag, now}

	if reEnabled {
		query += `, consecutive_failures = 0, warned_at = NULL, disabled_at = NULL, disabled_reason = ''`
		existing.ConsecutiveFailures = 0
		existing.DisabledReason = ""
	}

	query += ` WHERE id = ? AND team_id = ?`
	args = append(args, id, teamID)

	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("webhooks: update: %w", err)
	}

	existing.UpdatedAt = now

	return existing, nil
}

// Delete removes an endpoint and, by cascade, its delivery log.
func (s *Store) Delete(ctx context.Context, teamID, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM webhook_endpoints WHERE id = ? AND team_id = ?`, id, teamID)
	if err != nil {
		return fmt.Errorf("webhooks: delete: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("webhooks: delete: %w", err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// RotationGrace is how long the previous secret keeps verifying after a
// rotation. An hour is long enough for a customer to deploy the new secret and
// short enough that a leaked one is not useful for long.
const RotationGrace = time.Hour

// Rotate mints a new signing secret and keeps the old one valid for a grace
// period. Without the grace period, rotating means every delivery between the
// rotation and the receiver's redeploy fails verification — which teaches
// people never to rotate, which is the opposite of what a rotatable secret is
// for.
func (s *Store) Rotate(ctx context.Context, teamID, id int64) (*Endpoint, error) {
	endpoint, err := s.Get(ctx, teamID, id)
	if err != nil {
		return nil, err
	}

	current, err := s.secretOf(ctx, id)
	if err != nil {
		return nil, err
	}

	next, err := NewSecret()
	if err != nil {
		return nil, err
	}

	now := s.now()

	if _, err := s.db.ExecContext(ctx, `
		UPDATE webhook_endpoints
		SET secret = ?, previous_secret = ?, previous_secret_until = ?, updated_at = ?
		WHERE id = ? AND team_id = ?`,
		next, current, now.Add(RotationGrace).Unix(), now.Unix(), id, teamID); err != nil {
		return nil, fmt.Errorf("webhooks: rotate: %w", err)
	}

	endpoint.Secret = next
	endpoint.UpdatedAt = now.Unix()

	return endpoint, nil
}

// secretOf reads the current signing secret for an endpoint.
func (s *Store) secretOf(ctx context.Context, id int64) (string, error) {
	var secret string

	err := s.db.QueryRowContext(ctx, `SELECT secret FROM webhook_endpoints WHERE id = ?`, id).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("webhooks: read secret: %w", err)
	}

	return secret, nil
}

// Secrets returns the secrets a delivery may be signed with: the current one,
// and the previous one while its grace period is still running.
func (s *Store) Secrets(ctx context.Context, id int64) (current string, previous string, err error) {
	var (
		prev  string
		until sql.NullInt64
	)

	err = s.db.QueryRowContext(ctx,
		`SELECT secret, previous_secret, previous_secret_until FROM webhook_endpoints WHERE id = ?`, id).
		Scan(&current, &prev, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("webhooks: read secrets: %w", err)
	}

	if until.Valid && s.now().Unix() < until.Int64 {
		previous = prev
	}

	return current, previous, nil
}

// deliveryColumns is the shared select list for the log.
const deliveryColumns = `id, endpoint_id, event_id, event_type, payload, state, attempt, max_attempts,
	response_status, response_body, error, duration_ms, created_at, attempted_at, next_attempt_at, delivered_at`

// scanDelivery reads one log row.
func scanDelivery(scan func(...any) error) (*Delivery, error) {
	var (
		delivery    Delivery
		status      sql.NullInt64
		attemptedAt sql.NullInt64
		nextAt      sql.NullInt64
		deliveredAt sql.NullInt64
	)

	if err := scan(&delivery.ID, &delivery.EndpointID, &delivery.EventID, &delivery.EventType, &delivery.Payload,
		&delivery.State, &delivery.Attempt, &delivery.MaxAttempts, &status, &delivery.ResponseBody,
		&delivery.Error, &delivery.DurationMS, &delivery.CreatedAt, &attemptedAt, &nextAt, &deliveredAt); err != nil {
		return nil, err
	}

	if status.Valid {
		delivery.ResponseStatus = int(status.Int64)
	}
	if attemptedAt.Valid {
		delivery.AttemptedAt = attemptedAt.Int64
	}
	if nextAt.Valid {
		delivery.NextAttemptAt = nextAt.Int64
	}
	if deliveredAt.Valid {
		delivery.DeliveredAt = deliveredAt.Int64
	}

	return &delivery, nil
}

// Deliveries returns the log for one endpoint, newest first. It is paginated
// because a busy endpoint accumulates thousands of rows and a log page that
// tries to render all of them is a log page nobody opens twice.
func (s *Store) Deliveries(ctx context.Context, teamID, endpointID int64, limit, offset int) ([]*Delivery, error) {
	if _, err := s.Get(ctx, teamID, endpointID); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deliveryColumns+` FROM webhook_deliveries WHERE endpoint_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		endpointID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("webhooks: deliveries: %w", err)
	}
	defer rows.Close()

	deliveries := []*Delivery{}

	for rows.Next() {
		delivery, err := scanDelivery(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("webhooks: deliveries: %w", err)
		}

		deliveries = append(deliveries, delivery)
	}

	return deliveries, rows.Err()
}

// Delivery reads one log row belonging to a team.
func (s *Store) Delivery(ctx context.Context, teamID, id int64) (*Delivery, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+prefixColumns("d", deliveryColumns)+`
		FROM webhook_deliveries d
		JOIN webhook_endpoints e ON e.id = d.endpoint_id
		WHERE d.id = ? AND e.team_id = ?`, id, teamID)

	delivery, err := scanDelivery(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: delivery: %w", err)
	}

	return delivery, nil
}

// prefixColumns qualifies a shared select list with a table alias. It exists so
// that the joined read above and the plain one share exactly one definition of
// which columns a delivery has.
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}

	return strings.Join(parts, ", ")
}
