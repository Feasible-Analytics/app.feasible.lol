//
// users.go
// People, their teams, and the whole of deleting an account.
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
	"strings"
	"time"
)

// TrialDays is how long a new team has before it needs a subscription. No card
// is taken up front, so this pair of columns is the entire trial — there is no
// provider record to ask about a trial that never became a subscription.
const TrialDays = 30

// User is one person who can sign in. The password hash and the Google subject
// are both optional and either one alone is a complete identity: somebody who
// only ever clicks "sign in with Google" has no password to forget, and
// somebody who never links Google has no third party in their login path.
type User struct {
	ID              int64
	Email           string
	Name            string
	PasswordHash    string
	GoogleSub       string
	EmailVerifiedAt int64
	Theme           string
	TOTPSecret      string
	TOTPRecovery    string
	TOTPEnabledAt   int64
	TOTPLastStep    int64
	CreatedAt       int64
	UpdatedAt       int64
	LastSeenAt      int64
}

// Verified reports whether this address has been proven. Google linking and
// team invitations both key off it, and treating an unverified address as
// proven is how an account gets taken over by whoever registered the address
// first without ever reading the mailbox.
func (u *User) Verified() bool {
	return u.EmailVerifiedAt > 0
}

// TwoFactorEnabled reports whether TOTP is switched on. It is the enabled
// timestamp rather than the presence of a secret, because a half-finished
// enrolment stores a secret that must not yet be demanded at sign-in.
func (u *User) TwoFactorEnabled() bool {
	return u.TOTPEnabledAt > 0
}

// DisplayName is what the interface calls someone. Falling back to the local
// part of the address means a header never reads "Welcome, " with nothing after
// it for the majority of users who never fill in a name.
func (u *User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}

	if at := strings.Index(u.Email, "@"); at > 0 {
		return u.Email[:at]
	}

	return u.Email
}

// Team is an account. teams.id is the account id and names the per-account
// analytics database, so creating a team is what creates a customer.
type Team struct {
	ID                 int64
	Name               string
	TrialEndsAt        int64
	AcceptTrafficUntil int64
	Require2FA         bool
	CreatedAt          int64
	UpdatedAt          int64
}

// NormaliseEmail puts an address into the one form the unique index and every
// lookup agree on. The column is COLLATE NOCASE so the database would catch a
// case difference, but the address is also hashed into rate-limit keys and
// compared against the Google claim, and those have no collation to save them.
func NormaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CreateUser inserts a person and the team they own, in one transaction.
//
// A user without a team cannot do anything at all — there is no site to create,
// no database to create it in, and no subscription to bill — so the two rows
// are made together. Splitting them would leave a failure window whose only
// possible outcome is an account that looks signed up and then 500s on every
// page.
func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash, googleSub string) (*User, *Team, error) {
	return s.createUser(ctx, email, name, passwordHash, googleSub, true, false)
}

// CreateOperatorUser inserts a verified self-hosted owner without starting a
// commercial trial. The CLI is the trust boundary here: only an operator with
// filesystem access can call it, so sending a verification email would add no
// proof and could make a log-only installation impossible to enter.
func (s *Store) CreateOperatorUser(ctx context.Context, email, name, passwordHash string) (*User, *Team, error) {
	return s.createUser(ctx, email, name, passwordHash, "", false, true)
}

// createUser performs the atomic identity, team and ownership insert. Hosted
// signups receive the commercial trial window; operator-created self-hosted
// accounts store no lifecycle deadlines at all.
func (s *Store) createUser(ctx context.Context, email, name, passwordHash, googleSub string, commercialTrial, verified bool) (*User, *Team, error) {
	email = NormaliseEmail(email)
	if email == "" {
		return nil, nil, fmt.Errorf("auth: an email address is required")
	}

	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: create user: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	// A NULL google_sub rather than an empty string: the column is UNIQUE, and
	// SQLite treats every NULL as distinct while it would reject a second
	// account that also stored "".
	var sub any
	if googleSub != "" {
		sub = googleSub
	}
	var verifiedAt any
	if verified {
		verifiedAt = now.Unix()
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO users (email, name, password_hash, google_sub, email_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, email, name, passwordHash, sub, verifiedAt, now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, nil, ErrEmailTaken
		}
		return nil, nil, fmt.Errorf("auth: create user: %w", err)
	}

	userID, err := result.LastInsertId()
	if err != nil {
		return nil, nil, fmt.Errorf("auth: create user: %w", err)
	}

	teamName := name
	if teamName == "" {
		teamName = email
	}

	var trialEnds, acceptTrafficUntil any
	if commercialTrial {
		trial := now.AddDate(0, 0, TrialDays)
		trialEnds = trial.Unix()
		acceptTrafficUntil = trial.AddDate(0, 0, 30).Unix()
	}

	// SQLite may reuse the largest deleted INTEGER PRIMARY KEY. The sequence is
	// independent of team rows, so every deletion path permanently reserves ids.
	var teamID int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE team_id_sequence
		SET last_id = MAX(
			last_id,
			COALESCE((SELECT MAX(id) FROM teams), 0),
			COALESCE((SELECT MAX(team_id) FROM account_deletions), 0)
		) + 1
		WHERE singleton = 1
		RETURNING last_id
	`).Scan(&teamID); err != nil {
		return nil, nil, fmt.Errorf("auth: allocate team id: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
			INSERT INTO teams (id, name, trial_ends_at, accept_traffic_until, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, teamID, teamName, trialEnds, acceptTrafficUntil, now.Unix(), now.Unix())
	if err != nil {
		return nil, nil, fmt.Errorf("auth: create team: %w", err)
	}
	if !commercialTrial {
		// The schema trigger enrolls every ordinary team so future creation paths
		// cannot forget billing. Operator-created teams are the deliberate
		// exception, removed again inside the same transaction before they are
		// ever observable.
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_lifecycle WHERE team_id = ?`, teamID); err != nil {
			return nil, nil, fmt.Errorf("auth: clear operator lifecycle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE teams SET trial_ends_at = NULL, accept_traffic_until = NULL WHERE id = ?
		`, teamID); err != nil {
			return nil, nil, fmt.Errorf("auth: clear operator trial: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at)
		VALUES (?, ?, 'owner', ?)
	`, teamID, userID, now.Unix()); err != nil {
		return nil, nil, fmt.Errorf("auth: create membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("auth: create user: %w", err)
	}

	user := &User{
		ID:              userID,
		Email:           email,
		Name:            name,
		PasswordHash:    passwordHash,
		GoogleSub:       googleSub,
		EmailVerifiedAt: nullUnix(verifiedAt),
		Theme:           "system",
		CreatedAt:       now.Unix(),
		UpdatedAt:       now.Unix(),
	}

	team := &Team{
		ID:                 teamID,
		Name:               teamName,
		TrialEndsAt:        nullUnix(trialEnds),
		AcceptTrafficUntil: nullUnix(acceptTrafficUntil),
		CreatedAt:          now.Unix(),
		UpdatedAt:          now.Unix(),
	}

	return user, team, nil
}

// nullUnix returns a unix timestamp stored in an optional SQL argument, or
// zero when the row deliberately has no timestamp.
func nullUnix(value any) int64 {
	if timestamp, ok := value.(int64); ok {
		return timestamp
	}

	return 0
}

// userColumns is the select list every user read shares, so a column added to
// the struct is added in one place rather than in six queries that will
// otherwise drift.
const userColumns = `id, email, name, password_hash, COALESCE(google_sub, ''),
	email_verified_at, theme, totp_secret, totp_recovery_codes, totp_enabled_at,
	totp_last_used_step, created_at, updated_at, last_seen_at`

// scanUser reads one row in the shape userColumns produces.
func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var (
		u          User
		verifiedAt sql.NullInt64
		totpAt     sql.NullInt64
		lastSeen   sql.NullInt64
	)

	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.GoogleSub,
		&verifiedAt, &u.Theme, &u.TOTPSecret, &u.TOTPRecovery, &totpAt,
		&u.TOTPLastStep, &u.CreatedAt, &u.UpdatedAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read user: %w", err)
	}

	u.EmailVerifiedAt = nullInt64(verifiedAt)
	u.TOTPEnabledAt = nullInt64(totpAt)
	u.LastSeenAt = nullInt64(lastSeen)

	return &u, nil
}

// UserByEmail finds someone by address.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE email = ?", NormaliseEmail(email)))
}

// UserByID finds someone by id, which is what every request does once per page
// load to turn a session cookie into a person.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE id = ?", id))
}

// UserByGoogleSub finds someone by their Google subject id.
//
// The subject is what is stored and matched, never the email. Google's own
// documentation is explicit that an address can be reassigned and that `sub` is
// the only stable identifier; matching on email means a returning user whose
// address changed silently becomes a stranger, and a reassigned address
// silently becomes the previous owner.
func (s *Store) UserByGoogleSub(ctx context.Context, sub string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE google_sub = ?", sub))
}

// UpdateProfile changes the display name and the theme preference.
func (s *Store) UpdateProfile(ctx context.Context, userID int64, name, theme string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET name = ?, theme = ?, updated_at = ? WHERE id = ?
	`, name, theme, s.now().Unix(), userID)
	if err != nil {
		return fmt.Errorf("auth: update profile: %w", err)
	}

	return nil
}

// LinkGoogle attaches a Google subject id to an existing account. It is only
// ever called after the address on the Google profile has been confirmed as
// verified, both by Google and by us — an unverified claim would let anyone who
// can create a Google account with a matching address take over a password
// account.
func (s *Store) LinkGoogle(ctx context.Context, userID int64, sub string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users SET google_sub = ?, updated_at = ? WHERE id = ?
	`, sub, s.now().Unix(), userID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("auth: that Google account is already linked to another user")
		}
		return fmt.Errorf("auth: link google: %w", err)
	}

	return nil
}

// TouchUser records that somebody was active. It is written on session refresh
// rather than on every request so that reading a dashboard does not put a write
// on the app shard system database once per XHR.
func (s *Store) TouchUser(ctx context.Context, userID int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE users SET last_seen_at = ? WHERE id = ?", at.Unix(), userID); err != nil {
		return fmt.Errorf("auth: touch user: %w", err)
	}

	return nil
}

// TeamForUser returns the team a person owns or belongs to.
//
// Ownership is preferred over any other membership because everything this
// package does — create a site, change billing, delete the account — is a thing
// you do to your own team, and a guest membership in someone else's must never
// become the team a "create site" form writes into.
func (s *Store) TeamForUser(ctx context.Context, userID int64) (*Team, error) {
	var (
		t          Team
		trialEnds  sql.NullInt64
		acceptTill sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT teams.id, teams.name, teams.trial_ends_at, teams.accept_traffic_until,
		       teams.require_2fa, teams.created_at, teams.updated_at
		FROM teams
		JOIN team_memberships ON team_memberships.team_id = teams.id
		WHERE team_memberships.user_id = ?
		ORDER BY CASE team_memberships.role WHEN 'owner' THEN 0 ELSE 1 END, teams.id
		LIMIT 1
	`, userID).Scan(&t.ID, &t.Name, &trialEnds, &acceptTill, &t.Require2FA, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read team: %w", err)
	}

	t.TrialEndsAt = nullInt64(trialEnds)
	t.AcceptTrafficUntil = nullInt64(acceptTill)

	return &t, nil
}

// BillingTeamForUser returns an account on which the user may view or change
// billing. A requested id is honored only through the membership row; zero
// selects the user's preferred owner, admin, or billing membership.
func (s *Store) BillingTeamForUser(ctx context.Context, userID, requestedTeamID int64) (*Team, string, error) {
	var (
		t          Team
		role       string
		trialEnds  sql.NullInt64
		acceptTill sql.NullInt64
	)

	query := `
		SELECT teams.id, teams.name, teams.trial_ends_at, teams.accept_traffic_until,
		       teams.require_2fa, teams.created_at, teams.updated_at, team_memberships.role
		FROM teams
		JOIN team_memberships ON team_memberships.team_id = teams.id
		WHERE team_memberships.user_id = ?
		  AND team_memberships.role IN ('owner', 'admin', 'billing')
	`
	args := []any{userID}
	if requestedTeamID > 0 {
		query += " AND teams.id = ?"
		args = append(args, requestedTeamID)
	}
	query += " ORDER BY CASE team_memberships.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, teams.id LIMIT 1"

	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&t.ID, &t.Name, &trialEnds, &acceptTill, &t.Require2FA, &t.CreatedAt, &t.UpdatedAt, &role,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("auth: read billing team: %w", err)
	}

	t.TrialEndsAt = nullInt64(trialEnds)
	t.AcceptTrafficUntil = nullInt64(acceptTill)

	return &t, role, nil
}

// TeamByID reads one team after a membership-aware authorization check has
// selected it. It deliberately performs no access check of its own.
func (s *Store) TeamByID(ctx context.Context, teamID int64) (*Team, error) {
	var (
		team       Team
		trialEnds  sql.NullInt64
		acceptTill sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, trial_ends_at, accept_traffic_until, require_2fa, created_at, updated_at
		FROM teams WHERE id = ?
	`, teamID).Scan(&team.ID, &team.Name, &trialEnds, &acceptTill, &team.Require2FA,
		&team.CreatedAt, &team.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read team: %w", err)
	}

	team.TrialEndsAt = nullInt64(trialEnds)
	team.AcceptTrafficUntil = nullInt64(acceptTill)

	return &team, nil
}

// SetRequire2FA flips the team-wide two-factor policy. Turning it on does not
// enrol anybody: it makes the next page load of every member who has not
// enrolled land on the enrolment screen, which is the only version of this that
// does not lock a team out of its own account.
func (s *Store) SetRequire2FA(ctx context.Context, teamID int64, required bool) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE teams SET require_2fa = ?, updated_at = ? WHERE id = ?
	`, required, s.now().Unix(), teamID); err != nil {
		return fmt.Errorf("auth: set two-factor policy: %w", err)
	}

	return nil
}

// UpdateTeamName renames an account.
func (s *Store) UpdateTeamName(ctx context.Context, teamID int64, name string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE teams SET name = ?, updated_at = ? WHERE id = ?
	`, name, s.now().Unix(), teamID); err != nil {
		return fmt.Errorf("auth: rename team: %w", err)
	}

	return nil
}

// isUniqueViolation recognises SQLite's uniqueness error. The driver returns it
// as a string rather than a typed error, and matching on the text is the only
// option — so it is done once here rather than at every insert.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "unique constraint failed") || strings.Contains(msg, "constraint failed: unique")
}
