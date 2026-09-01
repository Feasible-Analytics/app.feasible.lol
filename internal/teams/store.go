//
// store.go
// Membership, invitations and ownership transfer, against control.db.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package teams

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InvitationTTL is how long an invitation stays redeemable. Two days is long
// enough to survive a weekend and short enough that a link forwarded out of an
// inbox a month later is worthless — an invitation is a credential that grants
// standing in somebody's account, and an immortal one in a mail archive is a
// back door nobody remembers leaving open.
const InvitationTTL = 48 * time.Hour

// The failures a caller has to be able to tell apart. They are sentinels rather
// than strings because the HTTP layer maps each one to a different status, and
// matching on message text is how a 403 quietly becomes a 500.
var (
	// ErrForbidden means the actor's role does not permit the operation.
	ErrForbidden = errors.New("teams: this role may not do that")

	// ErrNotFound means the team, member or invitation does not exist. It is
	// also what an actor outside the team gets, so probing for a team id tells
	// somebody nothing they did not already know.
	ErrNotFound = errors.New("teams: not found")

	// ErrExpired means the invitation was real and is now too old to redeem.
	ErrExpired = errors.New("teams: this invitation has expired")

	// ErrLastOwner means the operation would leave a team with nobody able to
	// administer it — an account that can never be closed, re-billed or
	// recovered without us touching the database by hand.
	ErrLastOwner = errors.New("teams: a team must always have an owner")

	// ErrInvalidRole means a role string is not one of the seven.
	ErrInvalidRole = errors.New("teams: unknown role")

	// ErrWrongRecipient means a valid invitation was presented by a user whose
	// verified account does not own the address the invitation was sent to.
	ErrWrongRecipient = errors.New("teams: this invitation belongs to another email address")

	// ErrOperationInProgress means a durable reset, deletion or purge has
	// claimed the site or one of its teams. A transfer must wait for that
	// operation to finish or be retried so erased analytics cannot move to a new
	// owner behind the operation's authorization check.
	ErrOperationInProgress = errors.New("teams: a destructive operation is in progress")

	// ErrStaleTransfer means the caller confirmed a source team that no longer
	// owns the site. It is distinct from not found so an interactive caller can
	// ask the owner to reload instead of implying the site vanished.
	ErrStaleTransfer = errors.New("teams: the site owner changed; reload before transferring it again")
)

// Store is the control-database half of this package. Every method takes the
// acting user rather than trusting the caller to have checked a permission
// first: an authorisation check that lives at the call site is one that gets
// forgotten on the second call site.
type Store struct {
	db *sql.DB

	// Now is the clock invitations expire against, injectable so a test can ask
	// what happens 49 hours from now without waiting.
	Now func() time.Time
}

// NewStore builds a store over an open control database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the injected clock, falling back to the real one so a zero-value
// Store built by a test helper still works rather than panicking.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Member is one person in a team.
type Member struct {
	UserID    int64
	Email     string
	Name      string
	Role      Role
	CreatedAt int64
}

// Guest is one person's access to a single site of a team they are not in.
type Guest struct {
	UserID    int64
	Email     string
	Name      string
	SiteID    int64
	Domain    string
	Role      Role
	CreatedAt int64
}

// Invitation is an outstanding offer of a role. SiteID is zero for a team
// invitation and set for a guest one, which is the only difference between the
// two from here on.
type Invitation struct {
	ID        int64
	TeamID    int64
	SiteID    int64
	Email     string
	Role      Role
	InvitedBy int64
	CreatedAt int64
	ExpiresAt int64
}

// Expired reports whether an invitation is past its deadline. It takes the
// instant rather than reading a clock so that the answer a caller acts on and
// the answer a response reports cannot be two different answers.
func (i Invitation) Expired(now time.Time) bool {
	return now.Unix() >= i.ExpiresAt
}

// RoleOf returns somebody's role in a team, or ErrNotFound when they are not a
// member. It is the primitive every authorisation check in the product is built
// from, which is why it answers "not found" rather than an empty role: an empty
// role compared against a permission would silently be denied, and a bug that
// denies looks exactly like a bug that permits until somebody complains.
func (s *Store) RoleOf(ctx context.Context, teamID, userID int64) (Role, error) {
	var role string

	err := s.db.QueryRowContext(ctx, `
		SELECT role FROM team_memberships WHERE team_id = ? AND user_id = ?
	`, teamID, userID).Scan(&role)

	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("teams: read membership: %w", err)
	}

	return Role(role), nil
}

// SiteRole resolves somebody's effective role on one site. A team membership
// wins over a guest membership, because being in the team is the stronger
// relationship and a person who is both should not be demoted by the weaker of
// the two.
func (s *Store) SiteRole(ctx context.Context, siteID, userID int64) (Role, error) {
	var role string

	err := s.db.QueryRowContext(ctx, `
		SELECT team_memberships.role
		FROM sites
		JOIN team_memberships ON team_memberships.team_id = COALESCE(sites.owner_team_id, sites.account_id)
		WHERE sites.id = ? AND team_memberships.user_id = ?
	`, siteID, userID).Scan(&role)

	if err == nil {
		return Role(role), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("teams: read site role: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT role FROM guest_memberships WHERE site_id = ? AND user_id = ?
	`, siteID, userID).Scan(&role)

	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("teams: read guest role: %w", err)
	}

	return Role(role), nil
}

// Authorise resolves the actor's role and checks one permission in a single
// call. Callers use it instead of RoleOf plus Can so that the two halves cannot
// drift — a check that reads a role and then forgets to test it is the classic
// authorisation bug and this signature makes it unwriteable.
func (s *Store) Authorise(ctx context.Context, teamID, userID int64, permission Permission) (Role, error) {
	role, err := s.RoleOf(ctx, teamID, userID)
	if err != nil {
		return "", err
	}

	if !Can(role, permission) {
		return role, ErrForbidden
	}

	return role, nil
}

// AuthoriseSite is Authorise for a permission on one site, so that a guest is
// admitted on the site they were invited to and nowhere else.
func (s *Store) AuthoriseSite(ctx context.Context, siteID, userID int64, permission Permission) (Role, error) {
	role, err := s.SiteRole(ctx, siteID, userID)
	if err != nil {
		return "", err
	}

	if !Can(role, permission) {
		return role, ErrForbidden
	}

	return role, nil
}

// TeamIDs lists every team in which a user holds the requested permission.
// Callers use it only when a route does not name a site, so a multi-team user
// can be required to choose an explicit team instead of silently acting on the
// first membership returned by the database.
func (s *Store) TeamIDs(ctx context.Context, userID int64, permission Permission) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT team_id, role
		FROM team_memberships
		WHERE user_id = ?
		ORDER BY team_id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("teams: list memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var (
			teamID int64
			role   Role
		)

		if err := rows.Scan(&teamID, &role); err != nil {
			return nil, fmt.Errorf("teams: list memberships: %w", err)
		}

		if Can(role, permission) {
			ids = append(ids, teamID)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: list memberships: %w", err)
	}

	return ids, nil
}

// TeamIDForSite returns the team that currently owns a site. The control row
// is read at request time so an ownership transfer changes authorization on
// the next request instead of waiting for an in-memory routing refresh.
func (s *Store) TeamIDForSite(ctx context.Context, siteID int64) (int64, error) {
	var teamID int64

	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?`, siteID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("teams: read site's team: %w", err)
	}

	return teamID, nil
}

// Members lists a team, ordered by role and then by email so the settings
// screen is stable between loads rather than following an index scan.
func (s *Store) Members(ctx context.Context, teamID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT users.id, users.email, users.name, team_memberships.role, team_memberships.created_at
		FROM team_memberships
		JOIN users ON users.id = team_memberships.user_id
		WHERE team_memberships.team_id = ?
		ORDER BY users.email
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("teams: list members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var members []Member

	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.UserID, &member.Email, &member.Name, &member.Role, &member.CreatedAt); err != nil {
			return nil, fmt.Errorf("teams: list members: %w", err)
		}

		members = append(members, member)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: list members: %w", err)
	}

	sortByRank(members)

	return members, nil
}

// sortByRank puts owners first and viewers last, keeping the email ordering
// inside each role. A settings screen that lists the owner halfway down makes
// somebody scan for who to ask.
func sortByRank(members []Member) {
	for i := 1; i < len(members); i++ {
		for j := i; j > 0 && Rank(members[j].Role) > Rank(members[j-1].Role); j-- {
			members[j], members[j-1] = members[j-1], members[j]
		}
	}
}

// Guests lists the per-site guests of a team's sites.
func (s *Store) Guests(ctx context.Context, teamID int64) ([]Guest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT users.id, users.email, users.name, sites.id, sites.domain,
		       guest_memberships.role, guest_memberships.created_at
		FROM guest_memberships
		JOIN sites ON sites.id = guest_memberships.site_id
		JOIN users ON users.id = guest_memberships.user_id
		WHERE COALESCE(sites.owner_team_id, sites.account_id) = ?
		ORDER BY sites.domain, users.email
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("teams: list guests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var guests []Guest

	for rows.Next() {
		var guest Guest
		if err := rows.Scan(&guest.UserID, &guest.Email, &guest.Name, &guest.SiteID,
			&guest.Domain, &guest.Role, &guest.CreatedAt); err != nil {
			return nil, fmt.Errorf("teams: list guests: %w", err)
		}

		guests = append(guests, guest)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: list guests: %w", err)
	}

	return guests, nil
}

// SetRole changes somebody's role. Owner or Admin is required, and two further
// rules are enforced here rather than in the UI, because the UI is not the only
// caller and an unenforced rule is a suggestion:
//
// Nobody may re-role somebody who outranks them, so an admin cannot demote an
// owner and then take the account.
//
// Owner is not grantable through this path at all. Making a second owner is
// ownership transfer, which has its own method and its own consequence for the
// person doing it.
func (s *Store) SetRole(ctx context.Context, actorID, teamID, userID int64, role Role) error {
	if !IsTeamRole(role) {
		return ErrInvalidRole
	}

	if role == RoleOwner {
		return fmt.Errorf("%w: use ownership transfer to make somebody an owner", ErrForbidden)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("teams: set role: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	if _, err := tx.ExecContext(ctx, `UPDATE team_memberships SET created_at = created_at WHERE id = -1`); err != nil {
		return fmt.Errorf("teams: set role: %w", err)
	}

	actorRole, err := roleOfTx(ctx, tx, teamID, actorID)
	if err != nil {
		return err
	}
	if !Can(actorRole, PermManageMembers) {
		return ErrForbidden
	}
	target, err := roleOfTx(ctx, tx, teamID, userID)
	if err != nil {
		return err
	}

	if Rank(target) > Rank(actorRole) {
		return fmt.Errorf("%w: %s cannot change the role of %s", ErrForbidden, Label(actorRole), Label(target))
	}

	if target == RoleOwner {
		if err := refuseLastOwnerTx(ctx, tx, teamID, userID); err != nil {
			return err
		}
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE team_memberships SET role = ?
		WHERE team_id = ? AND user_id = ? AND role = ?
	`, string(role), teamID, userID, string(target))
	if err != nil {
		return fmt.Errorf("teams: set role: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("teams: set role: %w", err)
	}

	return nil
}

// RemoveMember takes somebody out of a team. Removing the last owner is refused
// for the same reason demoting them is: an account with no owner cannot be
// billed, closed or recovered without us editing the database by hand.
//
// Every API key that person created for this team stops working, because a key
// is authorised against a live membership rather than against the row that
// created it.
func (s *Store) RemoveMember(ctx context.Context, actorID, teamID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("teams: remove member: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	if _, err := tx.ExecContext(ctx, `UPDATE team_memberships SET created_at = created_at WHERE id = -1`); err != nil {
		return fmt.Errorf("teams: remove member: %w", err)
	}

	actorRole, err := roleOfTx(ctx, tx, teamID, actorID)
	if err != nil {
		return err
	}
	if !Can(actorRole, PermManageMembers) && actorID != userID {
		return ErrForbidden
	}
	target, err := roleOfTx(ctx, tx, teamID, userID)
	if err != nil {
		return err
	}

	// Leaving is always allowed; removing somebody more senior than you is not.
	if actorID != userID && Rank(target) > Rank(actorRole) {
		return fmt.Errorf("%w: %s cannot remove %s", ErrForbidden, Label(actorRole), Label(target))
	}

	if target == RoleOwner {
		if err := refuseLastOwnerTx(ctx, tx, teamID, userID); err != nil {
			return err
		}
	}

	result, err := tx.ExecContext(ctx, `
		DELETE FROM team_memberships WHERE team_id = ? AND user_id = ? AND role = ?
	`, teamID, userID, string(target))
	if err != nil {
		return fmt.Errorf("teams: remove member: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("teams: remove member: %w", err)
	}

	return nil
}

// refuseLastOwnerTx returns ErrLastOwner when the named user is the only owner
// left. It is a separate query rather than a constraint because SQLite cannot
// express "at least one row matching a predicate", and a trigger would make the
// failure arrive as an opaque database error at whichever call site touched it.
func refuseLastOwnerTx(ctx context.Context, tx *sql.Tx, teamID, userID int64) error {
	var others int

	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM team_memberships
		WHERE team_id = ? AND role = 'owner' AND user_id <> ?
	`, teamID, userID).Scan(&others)
	if err != nil {
		return fmt.Errorf("teams: count owners: %w", err)
	}

	if others == 0 {
		return ErrLastOwner
	}

	return nil
}

// Invite offers a role to an email address, whether or not anybody holds that
// address yet. The returned token is the only time the secret exists in a
// readable form: what is stored is its hash, so a stolen copy of control.db
// cannot be replayed into somebody's account.
//
// Re-inviting an address that already has an outstanding invitation replaces it
// and restarts the 48 hours, which is what "resend" means to the person
// clicking it.
func (s *Store) Invite(ctx context.Context, actorID int64, invitation Invitation) (string, Invitation, error) {
	if !Valid(invitation.Role) {
		return "", Invitation{}, ErrInvalidRole
	}

	// A guest role is meaningless without a site and a team role is dangerous
	// with one, and the database says so too — this check exists so the caller
	// gets a sentence rather than a constraint violation.
	if IsGuestRole(invitation.Role) != (invitation.SiteID != 0) {
		return "", Invitation{}, fmt.Errorf("%w: %s is invited to %s", ErrInvalidRole,
			Label(invitation.Role), guestTarget(invitation.Role))
	}

	email := normaliseEmail(invitation.Email)
	if email == "" {
		return "", Invitation{}, fmt.Errorf("%w: an invitation needs an email address", ErrInvalidRole)
	}

	token, hash, err := newToken()
	if err != nil {
		return "", Invitation{}, err
	}

	now := s.now()
	invitation.Email = email
	invitation.InvitedBy = actorID
	invitation.CreatedAt = now.Unix()
	invitation.ExpiresAt = now.Add(InvitationTTL).Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Invitation{}, fmt.Errorf("teams: create invitation: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	// Reserve SQLite's writer before reading the live ownership boundary. A
	// transfer and an invitation now have one serial order: either the transfer
	// wins and this authorization fails, or this insert wins and the transfer
	// removes it before publishing the new owner.
	if _, err := tx.ExecContext(ctx, `UPDATE team_invitations SET expires_at = expires_at WHERE id = -1`); err != nil {
		return "", Invitation{}, fmt.Errorf("teams: create invitation: %w", err)
	}

	if IsGuestRole(invitation.Role) {
		var teamID int64
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?
		`, invitation.SiteID).Scan(&teamID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", Invitation{}, ErrNotFound
		}
		if err != nil {
			return "", Invitation{}, fmt.Errorf("teams: authorize guest invitation: %w", err)
		}
		if teamID != invitation.TeamID {
			return "", Invitation{}, ErrNotFound
		}

		// Handing one client access to one site is site administration, not
		// team administration, so an Editor who runs that site can do it.
		actorRole, err := siteRoleTx(ctx, tx, invitation.SiteID, actorID)
		if err != nil {
			return "", Invitation{}, err
		}
		if !Can(actorRole, PermManageSiteSettings) {
			return "", Invitation{}, ErrForbidden
		}
	} else {
		actorRole, err := roleOfTx(ctx, tx, invitation.TeamID, actorID)
		if err != nil {
			return "", Invitation{}, err
		}
		if !Can(actorRole, PermManageMembers) {
			return "", Invitation{}, ErrForbidden
		}
		if invitation.Role == RoleOwner || Rank(invitation.Role) > Rank(actorRole) {
			return "", Invitation{}, fmt.Errorf("%w: %s cannot invite a %s", ErrForbidden,
				Label(actorRole), Label(invitation.Role))
		}
	}

	// One live invitation per address per target: the unique index says so, and
	// resending has to replace rather than fail.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM team_invitations
		WHERE team_id = ? AND email = ? AND COALESCE(site_id, 0) = ?
	`, invitation.TeamID, email, invitation.SiteID)
	if err != nil {
		return "", Invitation{}, fmt.Errorf("teams: replace invitation: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO team_invitations
			(team_id, site_id, email, role, token_hash, invited_by_user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, invitation.TeamID, nullableSite(invitation.SiteID), email, string(invitation.Role),
		hash, actorID, invitation.CreatedAt, invitation.ExpiresAt)
	if err != nil {
		return "", Invitation{}, fmt.Errorf("teams: create invitation: %w", err)
	}

	invitation.ID, _ = result.LastInsertId()
	if err := tx.Commit(); err != nil {
		return "", Invitation{}, fmt.Errorf("teams: create invitation: %w", err)
	}

	return token, invitation, nil
}

// guestTarget names what a role is invited to, for the error message above.
func guestTarget(role Role) string {
	if IsGuestRole(role) {
		return "one site and needs a site id"
	}

	return "the whole team and must not carry a site id"
}

// Invitations lists what is outstanding for a team, expired rows included. They
// are included on purpose: "I sent that yesterday and nothing happened" is
// answered by seeing the expired row and a Resend button, not by a list that
// silently omits it.
func (s *Store) Invitations(ctx context.Context, teamID int64) ([]Invitation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, team_id, COALESCE(site_id, 0), email, role,
		       COALESCE(invited_by_user_id, 0), created_at, expires_at
		FROM team_invitations
		WHERE team_id = ?
		ORDER BY created_at DESC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("teams: list invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Invitation

	for rows.Next() {
		var invitation Invitation
		if err := rows.Scan(&invitation.ID, &invitation.TeamID, &invitation.SiteID, &invitation.Email,
			&invitation.Role, &invitation.InvitedBy, &invitation.CreatedAt, &invitation.ExpiresAt); err != nil {
			return nil, fmt.Errorf("teams: list invitations: %w", err)
		}

		out = append(out, invitation)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: list invitations: %w", err)
	}

	return out, nil
}

// InvitationByToken resolves a bearer invitation without consuming it. The
// HTTP flow uses this only to choose sign-in or sign-up and stores the token in
// an HTTP-only cookie before redirecting to a tokenless URL.
func (s *Store) InvitationByToken(ctx context.Context, token string) (Invitation, error) {
	var invitation Invitation

	err := s.db.QueryRowContext(ctx, `
		SELECT id, team_id, COALESCE(site_id, 0), email, role,
		       COALESCE(invited_by_user_id, 0), created_at, expires_at
		FROM team_invitations WHERE token_hash = ?
	`, hashToken(token)).Scan(&invitation.ID, &invitation.TeamID, &invitation.SiteID, &invitation.Email,
		&invitation.Role, &invitation.InvitedBy, &invitation.CreatedAt, &invitation.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("teams: inspect invitation: %w", err)
	}

	if invitation.Expired(s.now()) {
		return invitation, ErrExpired
	}

	return invitation, nil
}

// Accept redeems an invitation for a user. It is the only place a membership is
// created from an invitation, and it deletes the row in the same transaction as
// it grants the role, so a token cannot be redeemed twice by two requests that
// arrive at once.
//
// An expired invitation is deleted rather than left behind. It can never be
// redeemed again, and leaving it in the table would keep the address blocked
// from being re-invited by the unique index.
func (s *Store) Accept(ctx context.Context, token string, userID int64) (Invitation, error) {
	hash := hashToken(token)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, fmt.Errorf("teams: accept: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	// Take SQLite's writer lock before inspecting the one-time invitation.
	// Independent database handles then serialize here rather than both reading
	// the token before either request deletes it.
	if _, err := tx.ExecContext(ctx, `UPDATE team_invitations SET expires_at = expires_at WHERE id = -1`); err != nil {
		return Invitation{}, fmt.Errorf("teams: accept: %w", err)
	}

	var invitation Invitation

	err = tx.QueryRowContext(ctx, `
		SELECT id, team_id, COALESCE(site_id, 0), email, role,
		       COALESCE(invited_by_user_id, 0), created_at, expires_at
		FROM team_invitations WHERE token_hash = ?
	`, hash).Scan(&invitation.ID, &invitation.TeamID, &invitation.SiteID, &invitation.Email,
		&invitation.Role, &invitation.InvitedBy, &invitation.CreatedAt, &invitation.ExpiresAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("teams: accept: %w", err)
	}

	if invitation.Expired(s.now()) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_invitations WHERE id = ?`, invitation.ID); err != nil {
			return Invitation{}, fmt.Errorf("teams: accept: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return Invitation{}, fmt.Errorf("teams: accept: %w", err)
		}

		return invitation, ErrExpired
	}

	var userEmail string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id = ?`, userID).Scan(&userEmail); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Invitation{}, ErrWrongRecipient
		}

		return Invitation{}, fmt.Errorf("teams: accept: read recipient: %w", err)
	}

	if normaliseEmail(userEmail) != invitation.Email {
		return Invitation{}, ErrWrongRecipient
	}
	if invitation.Role == RoleOwner {
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_invitations WHERE id = ?`, invitation.ID); err != nil {
			return Invitation{}, fmt.Errorf("teams: accept: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Invitation{}, fmt.Errorf("teams: accept: %w", err)
		}

		return invitation, fmt.Errorf("%w: ownership only changes through transfer", ErrForbidden)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM team_invitations WHERE id = ?`, invitation.ID); err != nil {
		return Invitation{}, fmt.Errorf("teams: accept: %w", err)
	}

	if invitation.SiteID == 0 {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO team_memberships (team_id, user_id, role, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (team_id, user_id) DO NOTHING
		`, invitation.TeamID, userID, string(invitation.Role), s.now().Unix())
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO guest_memberships (site_id, user_id, role, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (site_id, user_id) DO NOTHING
		`, invitation.SiteID, userID, string(invitation.Role), s.now().Unix())
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("teams: accept: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Invitation{}, fmt.Errorf("teams: accept: %w", err)
	}

	return invitation, nil
}

// RevokeInvitation withdraws an outstanding offer.
func (s *Store) RevokeInvitation(ctx context.Context, actorID, teamID, invitationID int64) error {
	var (
		siteID int64
		role   Role
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(site_id, 0), role
		FROM team_invitations
		WHERE id = ? AND team_id = ?
	`, invitationID, teamID).Scan(&siteID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("teams: revoke invitation: %w", err)
	}

	if IsGuestRole(role) {
		if _, err := s.AuthoriseSite(ctx, siteID, actorID, PermManageSiteSettings); err != nil {
			return err
		}
	} else if _, err := s.Authorise(ctx, teamID, actorID, PermManageMembers); err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx, `
		DELETE FROM team_invitations WHERE id = ? AND team_id = ?
	`, invitationID, teamID)
	if err != nil {
		return fmt.Errorf("teams: revoke invitation: %w", err)
	}

	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	return nil
}

// RevokeSiteInvitation withdraws a guest invitation only when its team and site
// both match the caller's explicit target. Authorization is checked against the
// live site owner before the scoped delete.
func (s *Store) RevokeSiteInvitation(ctx context.Context, actorID, teamID, siteID, invitationID int64) error {
	ownerTeamID, err := s.TeamIDForSite(ctx, siteID)
	if err != nil || ownerTeamID != teamID {
		return ErrNotFound
	}
	if _, err := s.AuthoriseSite(ctx, siteID, actorID, PermManageSiteSettings); err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx, `
		DELETE FROM team_invitations
		WHERE id = ? AND team_id = ? AND site_id = ?
		  AND role IN ('guest_editor', 'guest_viewer')
	`, invitationID, teamID, siteID)
	if err != nil {
		return fmt.Errorf("teams: revoke site invitation: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrNotFound
	}

	return nil
}

// PurgeExpiredInvitations deletes what can no longer be redeemed and reports
// how many rows went. The count is returned rather than logged so the job that
// runs this can say it did something, which is the whole difference between a
// cleanup that works and one that silently stopped running months ago.
func (s *Store) PurgeExpiredInvitations(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM team_invitations WHERE expires_at <= ?
	`, s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("teams: purge invitations: %w", err)
	}

	affected, _ := result.RowsAffected()

	return affected, nil
}

// TransferOwnership hands an account to another member and demotes the person
// doing it to Admin.
//
// The demotion is the point. Two owners is a state nobody can reason about —
// either can delete the account and neither can stop the other — so the
// transfer is a handover rather than a promotion, and the old owner keeps
// everything except the ability to close the account and take the billing.
func (s *Store) TransferOwnership(ctx context.Context, actorID, teamID, toUserID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("teams: transfer ownership: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	// Take the write lock before authorization. A concurrent transfer can no
	// longer change the actor's role after this check and before the handover.
	if _, err := tx.ExecContext(ctx, `UPDATE team_memberships SET role = role WHERE id = -1`); err != nil {
		return fmt.Errorf("teams: transfer ownership: %w", err)
	}

	actorRole, err := roleOfTx(ctx, tx, teamID, actorID)
	if err != nil {
		return err
	}
	if !Can(actorRole, PermTransferOwnership) {
		return ErrForbidden
	}
	if actorID == toUserID {
		return nil
	}

	targetRole, err := roleOfTx(ctx, tx, teamID, toUserID)
	if err != nil {
		return err
	}
	if targetRole == RoleOwner {
		return ErrForbidden
	}

	// Demote first because the unique partial index permits exactly one owner.
	// Both updates are conditional on the roles just authorized, so stale
	// callers cannot overwrite a completed transfer.
	demoted, err := tx.ExecContext(ctx, `
		UPDATE team_memberships SET role = 'admin'
		WHERE team_id = ? AND user_id = ? AND role = 'owner'
	`, teamID, actorID)
	if err != nil {
		return fmt.Errorf("teams: transfer ownership: %w", err)
	}
	changed, _ := demoted.RowsAffected()
	if changed != 1 {
		return ErrForbidden
	}

	promoted, err := tx.ExecContext(ctx, `
		UPDATE team_memberships SET role = 'owner'
		WHERE team_id = ? AND user_id = ? AND role = ?
	`, teamID, toUserID, string(targetRole))
	if err != nil {
		return fmt.Errorf("teams: transfer ownership: %w", err)
	}
	changed, _ = promoted.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("teams: transfer ownership: %w", err)
	}

	return nil
}

// roleOfTx reads a membership from a transaction that already owns the writer
// lock, keeping authorization and the protected mutation in one serial order.
func roleOfTx(ctx context.Context, tx *sql.Tx, teamID, userID int64) (Role, error) {
	var role Role
	err := tx.QueryRowContext(ctx, `
		SELECT role FROM team_memberships WHERE team_id = ? AND user_id = ?
	`, teamID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("teams: read membership: %w", err)
	}

	return role, nil
}

// siteRoleTx resolves a site's strongest live role through a transaction that
// already owns the writer lock, so a transfer cannot change the owning team
// between authorization and the protected mutation.
func siteRoleTx(ctx context.Context, tx *sql.Tx, siteID, userID int64) (Role, error) {
	var role Role
	err := tx.QueryRowContext(ctx, `
		SELECT team_memberships.role
		FROM sites
		JOIN team_memberships ON team_memberships.team_id = COALESCE(sites.owner_team_id, sites.account_id)
		WHERE sites.id = ? AND team_memberships.user_id = ?
	`, siteID, userID).Scan(&role)
	if err == nil {
		return role, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("teams: read site role: %w", err)
	}

	err = tx.QueryRowContext(ctx, `
		SELECT role FROM guest_memberships WHERE site_id = ? AND user_id = ?
	`, siteID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("teams: read guest role: %w", err)
	}

	return role, nil
}

// TransferSite moves one site to another team. The actor must own the source
// and be able to manage sites in the destination, because a transfer is
// simultaneously a removal from one account and an addition to another and
// either half alone is not consent.
//
// The site's events stay where they are. account_id is the immutable analytics
// database location and owner_team_id is the live authorization boundary, so
// changing ownership neither copies nor orphans history. Guests, shared links
// and the old team's folder are revoked because they were grants made by the
// previous owner rather than properties of the historical data.
func (s *Store) TransferSite(ctx context.Context, actorID, siteID, toTeamID int64) error {
	var expectedFromTeamID int64

	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?`, siteID).
		Scan(&expectedFromTeamID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}

	return s.TransferSiteFrom(ctx, actorID, siteID, expectedFromTeamID, toTeamID)
}

// TransferSiteFrom moves a site only if the caller's confirmed source team is
// still its owner. The ownership read, both live role checks, destructive claim
// check and compare-and-swap all run under one SQLite writer reservation.
func (s *Store) TransferSiteFrom(ctx context.Context, actorID, siteID, expectedFromTeamID, toTeamID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `UPDATE sites SET updated_at = updated_at WHERE id = -1`); err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}

	var fromTeamID int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?`, siteID).
		Scan(&fromTeamID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}
	if fromTeamID != expectedFromTeamID {
		return ErrStaleTransfer
	}

	fromRole, err := roleOfTx(ctx, tx, fromTeamID, actorID)
	if err != nil {
		return err
	}
	if fromRole != RoleOwner {
		return ErrForbidden
	}
	if fromTeamID == toTeamID {
		return nil
	}

	toRole, err := roleOfTx(ctx, tx, toTeamID, actorID)
	if err != nil {
		return err
	}
	if !Can(toRole, PermManageSites) {
		return ErrForbidden
	}

	var blocked bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM destructive_operations
			WHERE (resource_type = 'site' AND resource_id = ?)
			   OR (resource_type = 'team' AND resource_id IN (?, ?))
		)
	`, siteID, fromTeamID, toTeamID).Scan(&blocked); err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}
	if blocked {
		return ErrOperationInProgress
	}
	if err := validateSiteTransferSchema(ctx, tx); err != nil {
		return err
	}

	updated, err := tx.ExecContext(ctx, `
		UPDATE sites SET owner_team_id = ?, folder_id = NULL, is_public = 0, updated_at = ?
		WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?`,
		toTeamID, s.now().Unix(), siteID, fromTeamID)
	if err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}
	changed, _ := updated.RowsAffected()
	if changed != 1 {
		return ErrStaleTransfer
	}

	// Every old-owner credential, recipient, webhook, pending snapshot and job
	// is revoked under the ownership compare-and-swap. Configuration that
	// describes the tracker and analytics remains attached to the site.
	for _, table := range transferRevokedSiteTables {
		query := "DELETE FROM " + quoteIdentifier(table) + " WHERE site_id = ?"
		if _, err := tx.ExecContext(ctx, query, siteID); err != nil {
			return fmt.Errorf("teams: transfer site: revoke %s: %w", table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("teams: transfer site: %w", err)
	}

	return nil
}

// siteTransferPolicy classifies every direct site-scoped control table. The
// transfer fails closed when a migration adds one without choosing whether its
// rows are old-owner authority or site configuration.
var siteTransferPolicy = map[string]bool{
	"guest_memberships":      true,
	"team_invitations":       true,
	"shared_links":           true,
	"site_tracker_config":    false,
	"site_custom_properties": false,
	"webhook_endpoints":      true,
	"site_allowed_hostnames": false,
	"saved_segments":         false,
	"report_subscriptions":   true,
	"alert_rules":            true,
	"notifications_sent":     true,
	"notification_claims":    true,
	"jobs":                   true,
}

// transferRevokedSiteTables orders direct deletions whose descendants cascade.
// Keeping this separate from the policy makes review of the destructive list
// straightforward while validateSiteTransferSchema checks completeness.
var transferRevokedSiteTables = []string{
	"notification_claims",
	"notifications_sent",
	"report_subscriptions",
	"alert_rules",
	"webhook_endpoints",
	"team_invitations",
	"guest_memberships",
	"shared_links",
	"jobs",
}

// validateSiteTransferSchema checks structured SQLite column metadata rather
// than parsing SQL. Any future direct site_id table blocks transfer until its
// ownership semantics are classified above.
func validateSiteTransferSchema(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'sites'
		ORDER BY name
	`)
	if err != nil {
		return fmt.Errorf("teams: inspect transfer schema: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("teams: inspect transfer schema: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("teams: inspect transfer schema: %w", err)
	}

	for _, name := range names {
		columns, err := tx.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(name)+")")
		if err != nil {
			return fmt.Errorf("teams: inspect %s: %w", name, err)
		}
		hasSiteID := false
		for columns.Next() {
			var cid, notNull, primary int
			var column, columnType string
			var defaultValue any
			if err := columns.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primary); err != nil {
				_ = columns.Close()
				return fmt.Errorf("teams: inspect %s: %w", name, err)
			}
			hasSiteID = hasSiteID || column == "site_id"
		}
		if err := columns.Close(); err != nil {
			return fmt.Errorf("teams: inspect %s: %w", name, err)
		}
		if hasSiteID {
			if _, classified := siteTransferPolicy[name]; !classified {
				return fmt.Errorf("teams: unclassified site transfer table %s", name)
			}
		}
	}

	return nil
}

// quoteIdentifier quotes a schema-owned SQLite identifier.
func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// nullableSite turns a zero site id into a SQL NULL, because the table's CHECK
// constraint distinguishes "no site" from "site zero" and only one of them is
// expressible as an integer.
func nullableSite(siteID int64) any {
	if siteID == 0 {
		return nil
	}

	return siteID
}

// normaliseEmail lower-cases and trims an address. The column is NOCASE, so
// this is about what gets displayed and compared in Go rather than about the
// index — but an address stored with a trailing space is one that never matches
// the person typing it.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// newToken mints an invitation secret and its stored hash. Thirty-two random
// bytes is far past the point where guessing is the attack anybody would try,
// and base64url keeps the token pasteable into a URL without escaping.
func newToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("teams: generate token: %w", err)
	}

	token = base64.RawURLEncoding.EncodeToString(raw)

	return token, hashToken(token), nil
}

// hashToken is what the database stores. A single SHA-256 is correct here and
// would not be for a password: the input is 256 bits of our own randomness, so
// there is no dictionary to run and no work factor worth paying.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}
