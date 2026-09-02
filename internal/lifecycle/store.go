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
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// emailLeaseDuration bounds how long a worker owns one outbox message. A crash
// before transport acceptance becomes retryable after this interval.
const emailLeaseDuration = 5 * time.Minute

// lifecycleLeasePoll keeps payment and warning workers responsive without
// spinning while another process holds the same account transition lease.
const lifecycleLeasePoll = 20 * time.Millisecond

// Store is every read and write the lifecycle makes against system.db.
//
// One thing here is worth being deliberate about: the clock lives in
// account_lifecycle, but two derived values are also written onto the teams row
// — trial_ends_at and accept_traffic_until. That duplication is not an
// oversight. The ingest path resolves a domain from an in-memory snapshot of
// teams and sites and may never touch system.db, so the date collection stops
// has to be on the row that snapshot is built from. Both writes happen in one
// transaction, so the mirror cannot lag the source.
type Store struct {
	db *sql.DB
}

// CompResult identifies the one account resolved by a complimentary-access
// operation and whether it already carried the durable exemption.
type CompResult struct {
	TeamID        int64
	OwnerEmail    string
	AlreadyComped bool
}

// NewStore builds a store over the system database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// IsComped reports whether billing lifecycle signals must leave this account
// alone. The marker is checked inside the lifecycle transition lease so a comp
// cannot race a failed-payment event into restarting the clock.
func (s *Store) IsComped(ctx context.Context, teamID int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM account_comps WHERE team_id = ?`, teamID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lifecycle: read account %d comp: %w", teamID, err)
	}

	return true, nil
}

// CompByOwnerEmail durably exempts the single team owned by an email address.
// Resolving and writing happen in one transaction so an ambiguous owner never
// receives a partial comp, and the lifecycle mirrors clear with the marker.
func (s *Store) CompByOwnerEmail(ctx context.Context, email string) (CompResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: begin comp: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, u.email
		FROM users u
		JOIN team_memberships m ON m.user_id = u.id AND m.role = 'owner'
		JOIN teams t ON t.id = m.team_id
		WHERE u.email = ? COLLATE NOCASE
		ORDER BY t.id
	`, strings.TrimSpace(email))
	if err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: resolve comp owner: %w", err)
	}

	var matches []CompResult
	for rows.Next() {
		var result CompResult
		if err := rows.Scan(&result.TeamID, &result.OwnerEmail); err != nil {
			_ = rows.Close()
			return CompResult{}, fmt.Errorf("lifecycle: resolve comp owner: %w", err)
		}
		matches = append(matches, result)
	}
	if err := rows.Close(); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: resolve comp owner: %w", err)
	}
	if err := rows.Err(); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: resolve comp owner: %w", err)
	}
	if len(matches) == 0 {
		return CompResult{}, fmt.Errorf("lifecycle: no team is owned by %s", email)
	}
	if len(matches) > 1 {
		return CompResult{}, fmt.Errorf("lifecycle: %s owns %d teams; use a unique owner email", email, len(matches))
	}

	result := matches[0]
	var activeSubscription string
	err = tx.QueryRowContext(ctx, `
		SELECT stripe_subscription_id
		FROM subscriptions
		WHERE team_id = ? AND stripe_subscription_id <> '' AND status NOT IN ('canceled', 'none')
	`, result.TeamID).Scan(&activeSubscription)
	if err == nil {
		return CompResult{}, fmt.Errorf("lifecycle: account %d has active subscription %s; cancel it before comping", result.TeamID, activeSubscription)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CompResult{}, fmt.Errorf("lifecycle: read account %d subscription: %w", result.TeamID, err)
	}

	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT comped_at FROM account_comps WHERE team_id = ?`, result.TeamID).Scan(&existing)
	if err == nil {
		result.AlreadyComped = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return CompResult{}, fmt.Errorf("lifecycle: read account %d comp: %w", result.TeamID, err)
	}

	now := time.Now().UTC().Unix()
	if !result.AlreadyComped {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_comps (team_id, owner_email, comped_at) VALUES (?, ?, ?)
		`, result.TeamID, result.OwnerEmail, now); err != nil {
			return CompResult{}, fmt.Errorf("lifecycle: comp account %d: %w", result.TeamID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE account_lifecycle
		SET trigger = '', started_at = NULL, deleted_at = NULL, updated_at = ?
		WHERE team_id = ?
	`, now, result.TeamID); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: stop account %d clock: %w", result.TeamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE teams
		SET trial_ends_at = NULL, accept_traffic_until = NULL, updated_at = ?
		WHERE id = ?
	`, now, result.TeamID); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: clear account %d limits: %w", result.TeamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE lifecycle_outbox
		SET completed_at = COALESCE(completed_at, ?), outcome = CASE WHEN completed_at IS NULL THEN 'cancelled: account comped' ELSE outcome END,
		    lease_token = '', lease_expires_at = 0, updated_at = ?
		WHERE team_id = ? AND completed_at IS NULL
	`, now, now, result.TeamID); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: cancel account %d notices: %w", result.TeamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE collection_gaps SET ended_at = COALESCE(ended_at, ?) WHERE team_id = ?
	`, now, result.TeamID); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: close account %d collection gap: %w", result.TeamID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_overages WHERE team_id = ?`, result.TeamID); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: clear account %d volume lock: %w", result.TeamID, err)
	}

	if err := tx.Commit(); err != nil {
		return CompResult{}, fmt.Errorf("lifecycle: commit account %d comp: %w", result.TeamID, err)
	}

	return result, nil
}

// DB exposes the handle for the few callers — the deletion path, mostly — that
// need to run their own statements in the same database.
func (s *Store) DB() *sql.DB {
	return s.db
}

// TransitionLease serializes payment transitions, warning sends, and the local
// deletion claim for one account across processes.
type TransitionLease struct {
	store  *Store
	teamID int64
	token  string
}

// Renew fences the next side effect and extends a lease that is still live.
func (l *TransitionLease) Renew(ctx context.Context) error {
	if l == nil || l.store == nil || l.token == "" {
		return nil
	}
	now := time.Now().UTC()
	result, err := l.store.db.ExecContext(ctx, `
		UPDATE lifecycle_account_leases
		SET expires_at = ?, updated_at = ?
		WHERE team_id = ? AND token = ? AND expires_at > ?
	`, now.Add(emailLeaseDuration).Unix(), now.Unix(), l.teamID, l.token, now.Unix())
	if err != nil {
		return fmt.Errorf("lifecycle: renew account %d lease: %w", l.teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: renew account %d lease: %w", l.teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("lifecycle: account %d lease expired or was replaced", l.teamID)
	}

	return nil
}

// Release removes only this worker's transition lease.
func (l *TransitionLease) Release() {
	if l == nil || l.store == nil || l.token == "" {
		return
	}
	_, _ = l.store.db.Exec(`DELETE FROM lifecycle_account_leases WHERE team_id = ? AND token = ?`, l.teamID, l.token)
}

// AcquireTransitionLease waits for exclusive lifecycle work on one live team.
func (s *Store) AcquireTransitionLease(ctx context.Context, teamID int64) (*TransitionLease, error) {
	token, err := emailToken()
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(lifecycleLeasePoll)
	defer ticker.Stop()

	for {
		now := time.Now().UTC()
		result, err := s.db.ExecContext(ctx, `
			INSERT INTO lifecycle_account_leases (team_id, token, expires_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (team_id) DO UPDATE SET
				token = excluded.token,
				expires_at = excluded.expires_at,
				updated_at = excluded.updated_at
			WHERE lifecycle_account_leases.expires_at <= ?
		`, teamID, token, now.Add(emailLeaseDuration).Unix(), now.Unix(), now.Unix())
		if err != nil {
			return nil, fmt.Errorf("lifecycle: acquire account %d lease: %w", teamID, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("lifecycle: acquire account %d lease: %w", teamID, err)
		}
		if rows == 1 {
			return &TransitionLease{store: s, teamID: teamID, token: token}, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("lifecycle: acquire account %d lease: %w", teamID, ctx.Err())
		case <-ticker.C:
		}
	}
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

// SaveIfState applies a lifecycle transition only when the row still matches
// the state the caller read. Payment and day-90 deletion therefore have one
// database winner instead of both acting on stale snapshots.
func (s *Store) SaveIfState(ctx context.Context, teamID int64, previous, next State) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("lifecycle: compare-and-swap %d: %w", teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	now := time.Now().UTC().Unix()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO account_lifecycle (team_id, trigger, started_at, deleted_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO UPDATE SET
			trigger = excluded.trigger,
			started_at = excluded.started_at,
			deleted_at = excluded.deleted_at,
			updated_at = excluded.updated_at
		WHERE account_lifecycle.trigger = ?
		  AND account_lifecycle.started_at IS ?
		  AND account_lifecycle.deleted_at IS ?
	`, teamID, string(next.Trigger), toUnix(next.StartedAt), toUnix(next.DeletedAt), now, now,
		string(previous.Trigger), toUnix(previous.StartedAt), toUnix(previous.DeletedAt))
	if err != nil {
		return false, fmt.Errorf("lifecycle: compare-and-swap %d: %w", teamID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lifecycle: compare-and-swap %d: affected rows: %w", teamID, err)
	}
	if rows != 1 {
		return false, nil
	}

	var trialEnds, acceptUntil any
	if next.Running() {
		trialEnds = next.Boundary(PhaseLocked).Unix()
		acceptUntil = next.Boundary(PhaseDormant).Unix()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE teams SET trial_ends_at = ?, accept_traffic_until = ?, updated_at = ? WHERE id = ?
	`, trialEnds, acceptUntil, now, teamID); err != nil {
		return false, fmt.Errorf("lifecycle: compare-and-swap mirror %d: %w", teamID, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("lifecycle: compare-and-swap %d: %w", teamID, err)
	}

	return true, nil
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
		WHERE l.started_at IS NOT NULL
		  AND (l.deleted_at IS NULL OR EXISTS (
		      SELECT 1 FROM account_deletions d
		      WHERE d.team_id = l.team_id AND d.completed_at IS NULL
		  ))
		ORDER BY l.team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: running: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// AccountForDeletion resolves the immutable facts an owner-requested deletion
// must capture before the team cascade removes them. The lifecycle state is
// loaded separately so the claim transaction can compare-and-swap the exact
// clock the requester observed.
func (s *Store) AccountForDeletion(ctx context.Context, teamID int64) (Account, error) {
	var account Account
	var email sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, COALESCE(t.name, ''),
		       COALESCE(NULLIF(subscriptions.billing_email, ''), (
		           SELECT u.email FROM team_memberships m
		           JOIN users u ON u.id = m.user_id
		           WHERE m.team_id = t.id AND m.role = 'owner'
		           ORDER BY m.id LIMIT 1
		       ), ''),
		       COALESCE(subscriptions.stripe_customer_id, '')
		FROM teams t
		LEFT JOIN subscriptions ON subscriptions.team_id = t.id
		WHERE t.id = ?
	`, teamID).Scan(&account.TeamID, &account.TeamName, &email, &account.CustomerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("lifecycle: account %d does not exist", teamID)
	}
	if err != nil {
		return Account{}, fmt.Errorf("lifecycle: load account %d for deletion: %w", teamID, err)
	}

	state, err := s.Load(ctx, teamID)
	if err != nil {
		return Account{}, err
	}
	account.State = state
	account.Email = email.String

	return account, nil
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
		SELECT template FROM lifecycle_outbox
		WHERE team_id = ? AND started_at = ? AND completed_at IS NOT NULL
	`, teamID, startedAt.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("lifecycle: sent emails %d: %w", teamID, err)
	}
	defer func() { _ = rows.Close() }()

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

// EmailClaim is one worker's leased ownership of an outbox row. MessageKey is
// stable across attempts and becomes the mail Message-ID where supported.
type EmailClaim struct {
	Token      string
	MessageKey string
	Notice     Notice
}

// ClaimEmail leases one incomplete message. A live lease excludes concurrent
// workers; an expired lease is replaced so a crash before send cannot strand
// the warning forever.
func (s *Store) ClaimEmail(ctx context.Context, teamID int64, startedAt time.Time, template, recipient string, now time.Time) (EmailClaim, bool, error) {
	return s.claimEmail(ctx, teamID, startedAt, template, recipient, Notice{
		TeamID: teamID, To: recipient, Template: template,
	}, now)
}

// ClaimNotice persists the complete immutable notice on its first attempt. A
// retry uses the original recipient, dates, copy inputs, and URLs under the same
// Message-ID even when account data or the wall clock has since changed.
func (s *Store) ClaimNotice(ctx context.Context, startedAt time.Time, notice Notice, now time.Time) (EmailClaim, bool, error) {
	return s.claimEmail(ctx, notice.TeamID, startedAt, notice.Template, notice.To, notice, now)
}

// claimEmail implements the leased insert/reclaim shared by production notices
// and lower-level outbox tests.
func (s *Store) claimEmail(ctx context.Context, teamID int64, startedAt time.Time, template, recipient string, notice Notice, now time.Time) (EmailClaim, bool, error) {
	token, err := emailToken()
	if err != nil {
		return EmailClaim{}, false, err
	}

	started := startedAt.UTC().Unix()
	stamp := now.UTC().Unix()
	messageKey := fmt.Sprintf("lifecycle-%d-%d-%s", teamID, started, template)
	payload, err := json.Marshal(notice)
	if err != nil {
		return EmailClaim{}, false, fmt.Errorf("lifecycle: encode %s for %d: %w", template, teamID, err)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO lifecycle_outbox
			(team_id, started_at, template, recipient, message_key, payload, lease_token,
			 lease_expires_at, attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (team_id, started_at, template) DO UPDATE SET
			lease_token = excluded.lease_token,
			lease_expires_at = excluded.lease_expires_at,
			attempts = lifecycle_outbox.attempts + 1,
			payload = CASE WHEN lifecycle_outbox.payload = '' THEN excluded.payload ELSE lifecycle_outbox.payload END,
			updated_at = excluded.updated_at
		WHERE lifecycle_outbox.completed_at IS NULL
		  AND lifecycle_outbox.lease_expires_at <= ?
	`, teamID, started, template, recipient, messageKey, string(payload), token,
		now.UTC().Add(emailLeaseDuration).Unix(), stamp, stamp, stamp)
	if err != nil {
		return EmailClaim{}, false, fmt.Errorf("lifecycle: claim %s for %d: %w", template, teamID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return EmailClaim{}, false, fmt.Errorf("lifecycle: claim %s for %d: %w", template, teamID, err)
	}

	if affected == 0 {
		return EmailClaim{}, false, nil
	}

	claim := EmailClaim{Token: token, MessageKey: messageKey}
	var storedPayload string
	if err := s.db.QueryRowContext(ctx, `
		SELECT message_key, payload FROM lifecycle_outbox
		WHERE team_id = ? AND started_at = ? AND template = ? AND lease_token = ?
	`, teamID, started, template, token).Scan(&claim.MessageKey, &storedPayload); err != nil {
		return EmailClaim{}, false, fmt.Errorf("lifecycle: read claimed %s for %d: %w", template, teamID, err)
	}
	if err := json.Unmarshal([]byte(storedPayload), &claim.Notice); err != nil {
		return EmailClaim{}, false, fmt.Errorf("lifecycle: decode claimed %s for %d: %w", template, teamID, err)
	}

	return claim, true, nil
}

// FinishEmail durably completes an accepted message while the caller still
// owns its lease. The deletion confirmation also marks its immutable audit row
// in the same transaction, so a crash cannot leave those two records split.
func (s *Store) FinishEmail(ctx context.Context, teamID int64, startedAt time.Time, template string, claim EmailClaim, outcome string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: finish %s for %d: %w", template, teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	result, err := tx.ExecContext(ctx, `
		UPDATE lifecycle_outbox
		SET completed_at = ?, outcome = ?, lease_token = '', lease_expires_at = 0, updated_at = ?
		WHERE team_id = ? AND started_at = ? AND template = ?
		  AND completed_at IS NULL AND lease_token = ?
	`, now.UTC().Unix(), outcome, now.UTC().Unix(), teamID, startedAt.UTC().Unix(), template, claim.Token)
	if err != nil {
		return fmt.Errorf("lifecycle: finish %s for %d: %w", template, teamID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: finish %s for %d: %w", template, teamID, err)
	}
	if affected != 1 {
		return fmt.Errorf("lifecycle: finish %s for %d: email lease was replaced", template, teamID)
	}

	if template == TemplateAccountDeleted {
		if _, err := tx.ExecContext(ctx, `
			UPDATE account_deletions
			SET notified_at = ?,
			    team_name = '',
			    contact_email = '',
			    stripe_customer_id = '',
			    notes = 'live account data removed; deletion confirmation sent'
			WHERE team_id = ? AND notified_at IS NULL
		`, now.UTC().Unix(), teamID); err != nil {
			return fmt.Errorf("lifecycle: finish deletion confirmation %d: %w", teamID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: finish %s for %d: %w", template, teamID, err)
	}

	return nil
}

// FailEmail records a failed attempt and releases its lease immediately. The
// row remains incomplete, so the next sweep retries without waiting five
// minutes for a failure the worker already observed.
func (s *Store) FailEmail(ctx context.Context, teamID int64, startedAt time.Time, template string, claim EmailClaim, outcome string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE lifecycle_outbox
		SET outcome = ?, lease_token = '', lease_expires_at = 0, updated_at = ?
		WHERE team_id = ? AND started_at = ? AND template = ?
		  AND completed_at IS NULL AND lease_token = ?
	`, outcome, now.UTC().Unix(), teamID, startedAt.UTC().Unix(), template, claim.Token)
	if err != nil {
		return fmt.Errorf("lifecycle: fail %s for %d: %w", template, teamID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: fail %s for %d: %w", template, teamID, err)
	}
	if affected != 1 {
		return fmt.Errorf("lifecycle: fail %s for %d: email lease was replaced", template, teamID)
	}

	return nil
}

// emailToken returns an opaque outbox lease token.
func emailToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("lifecycle: generate email lease: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
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
		DELETE FROM lifecycle_outbox WHERE team_id = ? AND completed_at IS NULL
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
	defer func() { _ = rows.Close() }()

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

// LockedTeams lists the accounts whose dashboard is blocked by the lifecycle,
// each with the phase it is blocked in. The access gate holds this in memory
// rather than querying per request, for the same reason the routing map is a
// snapshot.
//
// The phase comes back alongside the id because the two blocked phases owe the
// customer different words: an account in Locked is still being collected and
// an account in Dormant is not, and telling a dormant account that we are still
// recording its traffic is a promise it discovers is false on the day it pays.
func (s *Store) LockedTeams(ctx context.Context, now time.Time) (map[int64]Phase, error) {
	accounts, err := s.Running(ctx)
	if err != nil {
		return nil, err
	}

	locked := map[int64]Phase{}

	for _, account := range accounts {
		phase := account.State.At(now)

		if !Capabilities(phase).Dashboard {
			locked[account.TeamID] = phase
		}
	}

	return locked, nil
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
