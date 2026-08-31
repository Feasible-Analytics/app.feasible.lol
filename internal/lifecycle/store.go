//
// store.go
// Persisting the clock, and mirroring it onto the two columns the hot paths read.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store is every read and write the lifecycle makes against control.db.
//
// One thing here is worth being deliberate about: the clock lives in
// account_lifecycle, but two derived values are also written onto the teams row
// — trial_ends_at and accept_traffic_until. That duplication is not an
// oversight. The ingest path resolves a domain from an in-memory snapshot of
// teams and sites and may never touch control.db, so the date collection stops
// has to be on the row that snapshot is built from. Both writes happen in one
// transaction, so the mirror cannot lag the source.
type Store struct {
	db *sql.DB
}

// NewStore builds a store over the control database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the handle for the few callers — the deletion path, mostly — that
// need to run their own statements in the same database.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Account is one account as the sweeper sees it: its clock, who to write to,
// and what to call it.
type Account struct {
	TeamID   int64
	TeamName string
	State    State

	// Email is the billing contact, falling back to the team owner. A deletion
	// warning with nowhere to go is the one failure in this package that must
	// be loud.
	Email string

	// CustomerID is the Stripe customer to delete at day 90, empty for an
	// account that never paid.
	CustomerID string
}

// Load reads one account's clock. An account with no row is Active with a
// stopped clock, which is the correct answer for a team that has never been
// enrolled — a missing row must never read as "overdue".
func (s *Store) Load(ctx context.Context, teamID int64) (State, error) {
	var (
		trigger          string
		started, deleted sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT trigger, started_at, deleted_at FROM account_lifecycle WHERE team_id = ?
	`, teamID).Scan(&trigger, &started, &deleted)

	if errors.Is(err, sql.ErrNoRows) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("lifecycle: load %d: %w", teamID, err)
	}

	// A trigger this build does not know about must not be read as "no clock",
	// because "no clock" is the value that would quietly keep a lapsed account
	// running forever.
	if !Trigger(trigger).Valid() {
		return State{}, fmt.Errorf("lifecycle: account %d has unknown trigger %q — this binary may be older than the database", teamID, trigger)
	}

	return State{
		Trigger:   Trigger(trigger),
		StartedAt: fromUnix(started),
		DeletedAt: fromUnix(deleted),
	}, nil
}

// Save writes a clock and its two mirrors in one transaction. Doing both in one
// transaction is the whole point: a state that said "dormant" while the routing
// map still accepted traffic, or the reverse, would be invisible until somebody
// noticed months of data that should not exist.
func (s *Store) Save(ctx context.Context, teamID int64, state State) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: save %d: %w", teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	now := time.Now().UTC().Unix()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_lifecycle (team_id, trigger, started_at, deleted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO UPDATE SET
			trigger    = excluded.trigger,
			started_at = excluded.started_at,
			deleted_at = excluded.deleted_at,
			updated_at = excluded.updated_at
	`, teamID, string(state.Trigger), toUnix(state.StartedAt), toUnix(state.DeletedAt), now, now); err != nil {
		return fmt.Errorf("lifecycle: save %d: %w", teamID, err)
	}

	// The two derived columns. Both are NULL for a paying account, which is
	// what "no limit" means to the code that reads them.
	var trialEnds, acceptUntil any

	if state.Running() {
		trialEnds = state.Boundary(PhaseLocked).Unix()
		acceptUntil = state.Boundary(PhaseDormant).Unix()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE teams SET trial_ends_at = ?, accept_traffic_until = ?, updated_at = ? WHERE id = ?
	`, trialEnds, acceptUntil, now, teamID); err != nil {
		return fmt.Errorf("lifecycle: mirror %d: %w", teamID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: save %d: %w", teamID, err)
	}

	return nil
}

// Running lists every account whose clock is ticking. It is the only set the
// sweeper walks: an account that is paying has nothing to advance and reading
// it every hour would be pure cost.
func (s *Store) Running(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.team_id, COALESCE(t.name, ''), l.trigger, l.started_at, l.deleted_at,
		       COALESCE(NULLIF(s.billing_email, ''), (
		           SELECT u.email FROM team_memberships m
		           JOIN users u ON u.id = m.user_id
		           WHERE m.team_id = l.team_id AND m.role = 'owner'
		           ORDER BY m.id LIMIT 1
		       ), ''),
		       COALESCE(s.stripe_customer_id, '')
		FROM account_lifecycle l
		JOIN teams t ON t.id = l.team_id
		LEFT JOIN subscriptions s ON s.team_id = l.team_id
		WHERE l.started_at IS NOT NULL AND l.deleted_at IS NULL
		ORDER BY l.team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: running: %w", err)
	}
	defer rows.Close()

	var out []Account

	for rows.Next() {
		var (
			account          Account
			trigger          string
			started, deleted sql.NullInt64
			email            sql.NullString
		)

		if err := rows.Scan(&account.TeamID, &account.TeamName, &trigger, &started, &deleted, &email, &account.CustomerID); err != nil {
			return nil, fmt.Errorf("lifecycle: running: %w", err)
		}

		if !Trigger(trigger).Valid() {
			return nil, fmt.Errorf("lifecycle: account %d has unknown trigger %q", account.TeamID, trigger)
		}

		account.State = State{Trigger: Trigger(trigger), StartedAt: fromUnix(started), DeletedAt: fromUnix(deleted)}
		account.Email = email.String

		out = append(out, account)
	}

	return out, rows.Err()
}

// Contact resolves one account's billing contact and display name, which is
// what the usage ladder needs and what the sweeper falls back to.
func (s *Store) Contact(ctx context.Context, teamID int64) (string, string, error) {
	var (
		name  string
		email sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(t.name, ''),
		       COALESCE(NULLIF(s.billing_email, ''), (
		           SELECT u.email FROM team_memberships m
		           JOIN users u ON u.id = m.user_id
		           WHERE m.team_id = t.id AND m.role = 'owner'
		           ORDER BY m.id LIMIT 1
		       ), '')
		FROM teams t
		LEFT JOIN subscriptions s ON s.team_id = t.id
		WHERE t.id = ?
	`, teamID).Scan(&name, &email)

	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("lifecycle: team %d does not exist", teamID)
	}
	if err != nil {
		return "", "", fmt.Errorf("lifecycle: contact %d: %w", teamID, err)
	}

	return name, email.String, nil
}

// SentEmails lists the templates already sent for one clock. The set is keyed
// by the clock's start instant, so an account that lapses, pays, and lapses
// again a year later gets the whole sequence again rather than being silently
// skipped.
func (s *Store) SentEmails(ctx context.Context, teamID int64, startedAt time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT template FROM lifecycle_emails WHERE team_id = ? AND started_at = ?
	`, teamID, startedAt.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("lifecycle: sent emails %d: %w", teamID, err)
	}
	defer rows.Close()

	sent := map[string]bool{}

	for rows.Next() {
		var template string
		if err := rows.Scan(&template); err != nil {
			return nil, fmt.Errorf("lifecycle: sent emails %d: %w", teamID, err)
		}

		sent[template] = true
	}

	return sent, rows.Err()
}

// ClaimEmail reserves the right to send one message, returning false when
// somebody else already has it.
//
// The claim is taken before the message is rendered and sent, which is the only
// ordering that makes a double send impossible. The alternative — send, then
// record — has a window in which a process that dies between the two sends the
// same deletion warning twice, and a customer who receives "we delete your
// account tomorrow" twice has every reason to distrust the first one.
//
// The cost is that a send which fails after the claim is not retried. That is
// the right trade for these ten messages, and the failure is recorded on the
// row and logged at error level rather than being swallowed.
func (s *Store) ClaimEmail(ctx context.Context, teamID int64, startedAt time.Time, template, recipient string, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO lifecycle_emails (team_id, started_at, template, recipient, sent_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (team_id, started_at, template) DO NOTHING
	`, teamID, startedAt.UTC().Unix(), template, recipient, now.UTC().Unix())
	if err != nil {
		return false, fmt.Errorf("lifecycle: claim %s for %d: %w", template, teamID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lifecycle: claim %s for %d: %w", template, teamID, err)
	}

	return affected > 0, nil
}

// RecordOutcome writes what the transport actually said about a claimed
// message. It is separate from the claim so the row exists even when the send
// fails, which is what turns "we think they were warned" into an auditable
// record of whether they were.
func (s *Store) RecordOutcome(ctx context.Context, teamID int64, startedAt time.Time, template, outcome string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE lifecycle_emails SET outcome = ? WHERE team_id = ? AND started_at = ? AND template = ?
	`, outcome, teamID, startedAt.UTC().Unix(), template)
	if err != nil {
		return fmt.Errorf("lifecycle: record outcome %s for %d: %w", template, teamID, err)
	}

	return nil
}

// CancelPending removes every unsent email for an account. It runs the instant
// an account returns to Active, and it is why a customer who has just paid
// never receives "we delete your account tomorrow".
//
// Rows for messages already sent are kept: they are a record of what the
// customer was told, and deleting them would erase the history of a warning
// they genuinely received.
func (s *Store) CancelPending(ctx context.Context, teamID int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM lifecycle_emails WHERE team_id = ? AND outcome = ''
	`, teamID)
	if err != nil {
		return 0, fmt.Errorf("lifecycle: cancel pending %d: %w", teamID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("lifecycle: cancel pending %d: %w", teamID, err)
	}

	return affected, nil
}

// OpenGap records the start of a period where we were not collecting. The
// dashboard draws it as a labelled gap rather than as zeroes, because zeroes
// say "nobody visited" and that is a different and much worse thing to tell
// somebody who has just paid to come back.
func (s *Store) OpenGap(ctx context.Context, teamID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO collection_gaps (team_id, started_at, reason)
		VALUES (?, ?, 'dormant')
		ON CONFLICT (team_id, started_at) DO NOTHING
	`, teamID, at.UTC().Unix())
	if err != nil {
		return fmt.Errorf("lifecycle: open gap %d: %w", teamID, err)
	}

	return nil
}

// CloseGap ends every open gap for an account, which is what paying does.
func (s *Store) CloseGap(ctx context.Context, teamID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE collection_gaps SET ended_at = ? WHERE team_id = ? AND ended_at IS NULL
	`, at.UTC().Unix(), teamID)
	if err != nil {
		return fmt.Errorf("lifecycle: close gap %d: %w", teamID, err)
	}

	return nil
}

// Gap is one window where collection was stopped.
type Gap struct {
	StartedAt time.Time
	EndedAt   time.Time
	Reason    string
}

// Gaps lists an account's collection gaps, oldest first.
func (s *Store) Gaps(ctx context.Context, teamID int64) ([]Gap, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT started_at, ended_at, reason FROM collection_gaps WHERE team_id = ? ORDER BY started_at
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: gaps %d: %w", teamID, err)
	}
	defer rows.Close()

	var out []Gap

	for rows.Next() {
		var (
			gap   Gap
			start int64
			end   sql.NullInt64
		)

		if err := rows.Scan(&start, &end, &gap.Reason); err != nil {
			return nil, fmt.Errorf("lifecycle: gaps %d: %w", teamID, err)
		}

		gap.StartedAt = time.Unix(start, 0).UTC()
		gap.EndedAt = fromUnix(end)

		out = append(out, gap)
	}

	return out, rows.Err()
}

// LockedTeams lists the accounts whose dashboard is blocked by the lifecycle.
// The access gate holds this in memory rather than querying per request, for
// the same reason the routing map is a snapshot.
func (s *Store) LockedTeams(ctx context.Context, now time.Time) ([]int64, error) {
	accounts, err := s.Running(ctx)
	if err != nil {
		return nil, err
	}

	var ids []int64

	for _, account := range accounts {
		if !Capabilities(account.State.At(now)).Dashboard {
			ids = append(ids, account.TeamID)
		}
	}

	return ids, nil
}

// toUnix converts a time to the nullable column the schema stores. A zero time
// is NULL rather than the epoch, so "no clock" and "a clock that started in
// 1970" can never be confused — and one of those two would delete an account on
// its next sweep.
func toUnix(at time.Time) any {
	if at.IsZero() {
		return nil
	}

	return at.UTC().Unix()
}

// fromUnix converts a nullable column back, mapping NULL to the zero time.
func fromUnix(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}

	return time.Unix(value.Int64, 0).UTC()
}
