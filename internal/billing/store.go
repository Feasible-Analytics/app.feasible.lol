//
// store.go
// The mirror of the payment provider's records, and the log support reads.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package billing connects the payment provider to the account lifecycle. It
// owns the webhook endpoint, the checkout and portal redirects, and the local
// mirror of what the provider believes about each account.
//
// One rule governs the whole package: nothing here decides anything from the
// event that woke it. Every handler re-reads the provider's current state and
// acts on that. Webhooks arrive out of order, arrive twice, and arrive minutes
// late; a handler that trusted the payload would eventually mark a paying
// customer as lapsed because a stale `past_due` snapshot overtook a fresh
// `active` one.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Subscription is the local mirror of one account's billing state. The payment
// provider is the source of truth; this copy exists so a page load does not
// make a network call.
type Subscription struct {
	TeamID            int64
	CustomerID        string
	SubscriptionID    string
	Status            string
	Plan              string
	PriceID           string
	CurrentPeriodEnd  time.Time
	CancelAtPeriodEnd bool
	BillingEmail      string
	PaymentState      string
}

// Store is every read and write this package makes against control.db.
type Store struct {
	db *sql.DB

	// Now is injectable so the webhook tests can assert on stored timestamps
	// without depending on when the suite runs.
	Now func() time.Time
}

// NewStore builds a store over the control database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the handle for callers that need to run their own statements
// against the same database.
func (s *Store) DB() *sql.DB {
	return s.db
}

// now returns the store's clock.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Load reads one account's mirrored billing state. A team with no row has never
// been to checkout, which is the normal state of a trial and not an error.
func (s *Store) Load(ctx context.Context, teamID int64) (Subscription, error) {
	var (
		out         Subscription
		customer    sql.NullString
		subID       sql.NullString
		periodEnd   sql.NullInt64
		cancelAtEnd int64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, stripe_subscription_id, status, plan,
		       stripe_price_id, current_period_end, cancel_at_period_end, billing_email,
		       payment_state
		FROM subscriptions WHERE team_id = ?
	`, teamID).Scan(&customer, &subID, &out.Status, &out.Plan, &out.PriceID, &periodEnd, &cancelAtEnd, &out.BillingEmail, &out.PaymentState)

	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{TeamID: teamID, Status: "none"}, nil
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("billing: load %d: %w", teamID, err)
	}

	out.TeamID = teamID
	out.CustomerID = customer.String
	out.SubscriptionID = subID.String
	out.CancelAtPeriodEnd = cancelAtEnd != 0

	if periodEnd.Valid {
		out.CurrentPeriodEnd = time.Unix(periodEnd.Int64, 0).UTC()
	}

	return out, nil
}

// Save writes the mirror back. It is a full overwrite rather than a set of
// partial updates because the whole row is read from the provider in one go —
// updating some columns from a fresh read and leaving others from an older one
// is how a row ends up describing a state that never existed.
func (s *Store) Save(ctx context.Context, sub Subscription) error {
	now := s.now().Unix()

	var periodEnd any
	if !sub.CurrentPeriodEnd.IsZero() {
		periodEnd = sub.CurrentPeriodEnd.UTC().Unix()
	}

	cancelAtEnd := 0
	if sub.CancelAtPeriodEnd {
		cancelAtEnd = 1
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, current_period_end, cancel_at_period_end, billing_email,
			 payment_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO UPDATE SET
			stripe_customer_id     = excluded.stripe_customer_id,
			stripe_subscription_id = excluded.stripe_subscription_id,
			status                 = excluded.status,
			plan                   = excluded.plan,
			stripe_price_id        = excluded.stripe_price_id,
			current_period_end     = excluded.current_period_end,
			cancel_at_period_end   = excluded.cancel_at_period_end,
			billing_email          = CASE WHEN excluded.billing_email <> ''
			                              THEN excluded.billing_email
			                              ELSE subscriptions.billing_email END,
			payment_state          = excluded.payment_state,
			updated_at             = excluded.updated_at
	`, sub.TeamID, nullIfEmpty(sub.CustomerID), nullIfEmpty(sub.SubscriptionID), sub.Status, sub.Plan,
		sub.PriceID, periodEnd, cancelAtEnd, sub.BillingEmail, sub.PaymentState, now, now)
	if err != nil {
		return fmt.Errorf("billing: save %d: %w", sub.TeamID, err)
	}

	return nil
}

// TeamForCustomer maps a payment-provider customer back to an account. It is
// the fallback when an event carries no metadata — which is every event created
// by somebody clicking around the provider's own dashboard.
func (s *Store) TeamForCustomer(ctx context.Context, customerID string) (int64, error) {
	if customerID == "" {
		return 0, nil
	}

	var teamID int64

	err := s.db.QueryRowContext(ctx, `
		SELECT team_id FROM subscriptions WHERE stripe_customer_id = ?
	`, customerID).Scan(&teamID)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("billing: team for customer %s: %w", customerID, err)
	}

	return teamID, nil
}

// EventStatus is what happened to one webhook delivery.
const (
	// OutcomeApplied means the delivery changed something.
	OutcomeApplied = "applied"

	// OutcomeIgnored means we recorded it and did nothing, because it is a type
	// this product does not act on.
	OutcomeIgnored = "ignored"

	// OutcomeDuplicate means we had already applied this event id.
	OutcomeDuplicate = "duplicate"

	// OutcomeError means the handler failed. The delivery is left unhandled so
	// the provider's own retry can run it again, which is safe because the
	// handler reconciles from current state rather than replaying the event.
	OutcomeError = "error"
)

// ClaimEvent records a delivery and reports whether it still needs handling.
//
// The three-way answer is the whole of the idempotency design. A brand-new
// event is claimed and handled. An event we have already applied is a
// duplicate and is skipped. An event whose previous attempt failed is handled
// again — which is safe precisely because the handler is a function of the
// provider's current state, so a second run against a changed world produces
// the right answer rather than replaying a stale one.
func (s *Store) ClaimEvent(ctx context.Context, eventID, eventType string, teamID int64, payload []byte) (bool, error) {
	now := s.now().Unix()

	var (
		handled sql.NullInt64
		outcome string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT handled_at, outcome FROM stripe_events WHERE event_id = ?
	`, eventID).Scan(&handled, &outcome)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not seen before. Record it now, so that even a handler that crashes
		// leaves evidence the delivery arrived.
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO stripe_events (event_id, type, team_id, payload, received_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (event_id) DO NOTHING
		`, eventID, eventType, nullIfZero(teamID), string(payload), now); err != nil {
			return false, fmt.Errorf("billing: claim event %s: %w", eventID, err)
		}

		return true, nil

	case err != nil:
		return false, fmt.Errorf("billing: claim event %s: %w", eventID, err)

	case handled.Valid && outcome != OutcomeError:
		return false, nil

	default:
		return true, nil
	}
}

// FinishEvent records what the handler decided.
func (s *Store) FinishEvent(ctx context.Context, eventID, outcome string, teamID int64, handlerErr error) error {
	message := ""
	if handlerErr != nil {
		message = handlerErr.Error()
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE stripe_events
		SET handled_at = ?, outcome = ?, error = ?, team_id = COALESCE(?, team_id)
		WHERE event_id = ?
	`, s.now().Unix(), outcome, message, nullIfZero(teamID), eventID)
	if err != nil {
		return fmt.Errorf("billing: finish event %s: %w", eventID, err)
	}

	return nil
}

// LoggedEvent is one delivery as the support screen shows it.
type LoggedEvent struct {
	EventID    string
	Type       string
	TeamID     int64
	ReceivedAt time.Time
	HandledAt  time.Time
	Outcome    string
	Error      string
}

// RecentEvents lists the last deliveries, newest first, optionally for one
// account. This is what "logged where support can read it" means in practice:
// a person answering "they say they paid" needs the events, their order, and
// what each one did.
func (s *Store) RecentEvents(ctx context.Context, teamID int64, limit int) ([]LoggedEvent, error) {
	query := `
		SELECT event_id, type, COALESCE(team_id, 0), received_at, handled_at, outcome, error
		FROM stripe_events
	`
	args := []any{}

	if teamID > 0 {
		query += " WHERE team_id = ?"
		args = append(args, teamID)
	}

	query += " ORDER BY received_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("billing: recent events: %w", err)
	}
	defer rows.Close()

	var out []LoggedEvent

	for rows.Next() {
		var (
			entry    LoggedEvent
			received int64
			handled  sql.NullInt64
		)

		if err := rows.Scan(&entry.EventID, &entry.Type, &entry.TeamID, &received, &handled, &entry.Outcome, &entry.Error); err != nil {
			return nil, fmt.Errorf("billing: recent events: %w", err)
		}

		entry.ReceivedAt = time.Unix(received, 0).UTC()
		if handled.Valid {
			entry.HandledAt = time.Unix(handled.Int64, 0).UTC()
		}

		out = append(out, entry)
	}

	return out, rows.Err()
}

// nullIfEmpty writes NULL instead of an empty string, so that a unique index on
// a provider id does not collide across every account that has none.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}

// nullIfZero writes NULL instead of a zero id, so an event we could not route
// is visibly unrouted rather than attributed to account zero.
func nullIfZero(id int64) any {
	if id < 1 {
		return nil
	}

	return id
}
