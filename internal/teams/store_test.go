//
// store_test.go
// Membership, invitation expiry and the two transfers.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package teams

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/webhooks"
)

// fixture is a system database with two teams, four people and two sites, plus
// a store whose clock the test drives. It is a struct rather than a set of
// return values because almost every test here needs at least four of the ids.
type fixture struct {
	db    *sql.DB
	store *Store
	now   time.Time

	teamA, teamB           int64
	owner, admin, editor   int64
	viewer, outsider       int64
	siteA1, siteA2, siteB1 int64
}

// TestMembershipMutationsCannotUseStaleOwnerAuthorization races the two
// mutation paths against ownership handover on independent connections. Every
// serial outcome retains exactly one owner and stale old-owner writes fail.
func TestMembershipMutationsCannotUseStaleOwnerAuthorization(t *testing.T) {
	for _, mutation := range []string{"set-role", "remove"} {
		t.Run(mutation, func(t *testing.T) {
			for attempt := 0; attempt < 24; attempt++ {
				f := newFixture(t)
				otherDB := openFixtureConnection(t, f.db)
				other := NewStore(otherDB)
				start := make(chan struct{})
				results := make(chan error, 2)
				go func() {
					<-start
					results <- f.store.TransferOwnership(context.Background(), f.owner, f.teamA, f.admin)
				}()
				go func() {
					<-start
					if mutation == "set-role" {
						results <- other.SetRole(context.Background(), f.owner, f.teamA, f.admin, RoleViewer)
						return
					}
					results <- other.RemoveMember(context.Background(), f.owner, f.teamA, f.admin)
				}()
				close(start)
				for i := 0; i < 2; i++ {
					err := <-results
					if err != nil && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
						t.Fatalf("attempt %d returned %v", attempt, err)
					}
				}

				var owners int
				if err := f.db.QueryRow(`
					SELECT COUNT(*) FROM team_memberships WHERE team_id = ? AND role = 'owner'
				`, f.teamA).Scan(&owners); err != nil {
					t.Fatal(err)
				}
				if owners != 1 {
					t.Fatalf("attempt %d left %d owners", attempt, owners)
				}
			}
		})
	}
}

// newFixture builds the database and seeds it.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &fixture{db: db, now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	f.store = NewStore(db)
	f.store.Now = func() time.Time { return f.now }

	f.teamA = f.team(t, "Acme")
	f.teamB = f.team(t, "Beta")

	f.owner = f.user(t, "owner@example.com")
	f.admin = f.user(t, "admin@example.com")
	f.editor = f.user(t, "editor@example.com")
	f.viewer = f.user(t, "viewer@example.com")
	f.outsider = f.user(t, "outsider@example.com")

	f.member(t, f.teamA, f.owner, RoleOwner)
	f.member(t, f.teamA, f.admin, RoleAdmin)
	f.member(t, f.teamA, f.editor, RoleEditor)
	f.member(t, f.teamA, f.viewer, RoleViewer)
	f.member(t, f.teamB, f.owner, RoleOwner)

	f.siteA1 = f.site(t, f.teamA, "acme.example")
	f.siteA2 = f.site(t, f.teamA, "acme-two.example")
	f.siteB1 = f.site(t, f.teamB, "beta.example")

	return f
}

// team inserts a team and returns its id.
func (f *fixture) team(t *testing.T, name string) int64 {
	t.Helper()

	result, err := f.db.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES (?, ?, ?)`,
		name, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}

	id, _ := result.LastInsertId()

	return id
}

// user inserts a person and returns their id.
func (f *fixture) user(t *testing.T, email string) int64 {
	t.Helper()

	result, err := f.db.Exec(`INSERT INTO users (email, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		email, email, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	id, _ := result.LastInsertId()

	return id
}

// member joins a person to a team.
func (f *fixture) member(t *testing.T, teamID, userID int64, role Role) {
	t.Helper()

	if _, err := f.db.Exec(`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		teamID, userID, string(role), f.now.Unix()); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
}

// site inserts a site and returns its id.
func (f *fixture) site(t *testing.T, teamID int64, domain string) int64 {
	t.Helper()

	result, err := f.db.Exec(`
		INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)
	`, teamID, domain, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}

	id, _ := result.LastInsertId()

	return id
}

// TestAnInvitationExpiresAfter48Hours is the acceptance criterion, driven by
// moving the clock rather than by waiting two days.
func TestAnInvitationExpiresAfter48Hours(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, invitation, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA,
		Email:  "outsider@example.com",
		Role:   RoleEditor,
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	if want := f.now.Add(48 * time.Hour).Unix(); invitation.ExpiresAt != want {
		t.Fatalf("expires at %d, want %d — the TTL is not 48 hours", invitation.ExpiresAt, want)
	}

	// One second before the deadline it is still good.
	f.now = f.now.Add(48*time.Hour - time.Second)

	if _, err := f.store.Accept(ctx, token, f.outsider); err != nil {
		t.Fatalf("accept just inside the window: %v", err)
	}

	role, err := f.store.RoleOf(ctx, f.teamA, f.outsider)
	if err != nil || role != RoleEditor {
		t.Fatalf("role after accepting = %q, %v; want editor", role, err)
	}
}

// TestAnExpiredInvitationIsRefusedAndDeleted checks the other side of the
// deadline, and that the dead row does not stay behind blocking the address
// from ever being re-invited.
func TestAnExpiredInvitationIsRefusedAndDeleted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA,
		Email:  "outsider@example.com",
		Role:   RoleEditor,
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	f.now = f.now.Add(48 * time.Hour)

	if _, err := f.store.Accept(ctx, token, f.outsider); !errors.Is(err, ErrExpired) {
		t.Fatalf("accept at the deadline = %v, want ErrExpired", err)
	}

	if _, err := f.store.RoleOf(ctx, f.teamA, f.outsider); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired invitation granted a membership")
	}

	invitations, err := f.store.Invitations(ctx, f.teamA)
	if err != nil {
		t.Fatalf("list invitations: %v", err)
	}

	if len(invitations) != 0 {
		t.Fatalf("%d invitations remain after an expired one was redeemed", len(invitations))
	}
}

// TestAnInvitationCannotBeRedeemedTwice checks that accepting deletes the row
// in the same transaction as it grants the membership.
func TestAnInvitationCannotBeRedeemedTwice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA, Email: "outsider@example.com", Role: RoleViewer,
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	if _, err := f.store.Accept(ctx, token, f.outsider); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	if _, err := f.store.Accept(ctx, token, f.editor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second accept = %v, want ErrNotFound", err)
	}
}

// TestInvitationAcceptanceNeverChangesExistingMembership proves a stale or
// mistaken invitation cannot demote somebody who already belongs to the team.
func TestInvitationAcceptanceNeverChangesExistingMembership(t *testing.T) {
	f := newFixture(t)
	token, _, err := f.store.Invite(context.Background(), f.owner, Invitation{
		TeamID: f.teamA, Email: "editor@example.com", Role: RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Accept(context.Background(), token, f.editor); err != nil {
		t.Fatal(err)
	}
	role, err := f.store.RoleOf(context.Background(), f.teamA, f.editor)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleEditor {
		t.Fatalf("existing editor became %s after accepting viewer invitation", role)
	}
}

// TestConcurrentInvitationAcceptanceUsesOneIndependentConnectionWinner proves
// the bearer token is single-use across separate SQLite handles, not only
// goroutines queued behind one database/sql connection.
func TestConcurrentInvitationAcceptanceUsesOneIndependentConnectionWinner(t *testing.T) {
	f := newFixture(t)
	token, _, err := f.store.Invite(context.Background(), f.owner, Invitation{
		TeamID: f.teamA, Email: "outsider@example.com", Role: RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}

	otherDB := openFixtureConnection(t, f.db)
	other := NewStore(otherDB)
	other.Now = f.store.Now
	stores := []*Store{f.store, other}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	var wait sync.WaitGroup
	for _, candidate := range stores {
		wait.Add(1)
		go func(candidate *Store) {
			defer wait.Done()
			<-start
			_, err := candidate.Accept(context.Background(), token, f.outsider)
			results <- err
		}(candidate)
	}
	close(start)
	wait.Wait()
	close(results)

	succeeded, refused := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrNotFound) {
			refused++
		} else {
			t.Fatalf("concurrent accept returned %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("concurrent accept successes/refusals = %d/%d, want 1/1", succeeded, refused)
	}
}

// TestPurgeRemovesOnlyExpiredInvitations checks the cleanup job's predicate.
func TestPurgeRemovesOnlyExpiredInvitations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA, Email: "old@example.com", Role: RoleViewer,
	}); err != nil {
		t.Fatalf("invite: %v", err)
	}

	f.now = f.now.Add(49 * time.Hour)

	if _, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA, Email: "fresh@example.com", Role: RoleViewer,
	}); err != nil {
		t.Fatalf("invite: %v", err)
	}

	purged, err := f.store.PurgeExpiredInvitations(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}

	if purged != 1 {
		t.Fatalf("purged %d invitations, want 1", purged)
	}

	remaining, _ := f.store.Invitations(ctx, f.teamA)
	if len(remaining) != 1 || remaining[0].Email != "fresh@example.com" {
		t.Fatalf("purge removed the wrong row: %+v", remaining)
	}
}

// TestGuestInvitationsAreScopedToOneSite checks that accepting a guest
// invitation grants access to that site and to nothing else in the team.
func TestGuestInvitationsAreScopedToOneSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, _, err := f.store.Invite(ctx, f.editor, Invitation{
		TeamID: f.teamA,
		SiteID: f.siteA1,
		Email:  "outsider@example.com",
		Role:   RoleGuestViewer,
	})
	if err != nil {
		t.Fatalf("invite guest: %v", err)
	}

	if _, err := f.store.Accept(ctx, token, f.outsider); err != nil {
		t.Fatalf("accept: %v", err)
	}

	role, err := f.store.SiteRole(ctx, f.siteA1, f.outsider)
	if err != nil || role != RoleGuestViewer {
		t.Fatalf("site role on the invited site = %q, %v", role, err)
	}

	if _, err := f.store.SiteRole(ctx, f.siteA2, f.outsider); !errors.Is(err, ErrNotFound) {
		t.Fatal("a guest reached a second site in the same team")
	}

	if _, err := f.store.RoleOf(ctx, f.teamA, f.outsider); !errors.Is(err, ErrNotFound) {
		t.Fatal("a guest became a member of the team")
	}
}

// TestInvitationRedemptionIsBoundToTheRecipient checks that possession of a
// forwarded bearer link does not grant access to a different signed-in user.
func TestInvitationRedemptionIsBoundToTheRecipient(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	token, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA, Email: "outsider@example.com", Role: RoleViewer,
	})
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	if _, err := f.store.Accept(ctx, token, f.editor); !errors.Is(err, ErrWrongRecipient) {
		t.Fatalf("wrong recipient accepted invitation: %v", err)
	}

	if _, err := f.store.InvitationByToken(ctx, token); err != nil {
		t.Fatalf("a failed redemption consumed the invitation: %v", err)
	}

	if _, err := f.store.Accept(ctx, token, f.outsider); err != nil {
		t.Fatalf("recipient could not accept after forwarded-link attempt: %v", err)
	}
}

// TestGuestInvitationCannotNameAnotherTeamsSite checks the system-database
// boundary directly instead of relying on the site's dropdown in the form.
func TestGuestInvitationCannotNameAnotherTeamsSite(t *testing.T) {
	f := newFixture(t)

	if _, _, err := f.store.Invite(context.Background(), f.owner, Invitation{
		TeamID: f.teamA, SiteID: f.siteB1, Email: "outsider@example.com", Role: RoleGuestViewer,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-team guest invitation = %v, want ErrNotFound", err)
	}
}

// TestAGuestRoleNeedsASiteAndATeamRoleMustNotHaveOne checks both halves of the
// constraint, because either mistake grants access somebody did not consent to.
func TestAGuestRoleNeedsASiteAndATeamRoleMustNotHaveOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA, Email: "a@example.com", Role: RoleGuestEditor,
	}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("a guest invitation with no site = %v, want ErrInvalidRole", err)
	}

	if _, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA, SiteID: f.siteA1, Email: "b@example.com", Role: RoleAdmin,
	}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("a team invitation with a site = %v, want ErrInvalidRole", err)
	}
}

// TestInvitationCannotGrantOwnerOrOutrankInviter proves a crafted invitation
// cannot bypass the same hierarchy enforced by direct role changes.
func TestInvitationCannotGrantOwnerOrOutrankInviter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for name, actor := range map[string]int64{"owner": f.owner, "admin": f.admin} {
		if _, _, err := f.store.Invite(ctx, actor, Invitation{
			TeamID: f.teamA, Email: "outsider@example.com", Role: RoleOwner,
		}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s invited an owner: %v", name, err)
		}
	}

	if _, _, err := f.store.Invite(ctx, f.admin, Invitation{
		TeamID: f.teamA, Email: "outsider@example.com", Role: RoleEditor,
	}); err != nil {
		t.Fatalf("admin could not invite a lower role: %v", err)
	}
}

// TestOnlyOwnerOrAdminMayChangeRoles is the acceptance criterion for who
// administers a team.
func TestOnlyOwnerOrAdminMayChangeRoles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.SetRole(ctx, f.editor, f.teamA, f.viewer, RoleEditor); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an editor changed a role: %v", err)
	}

	if err := f.store.SetRole(ctx, f.viewer, f.teamA, f.editor, RoleViewer); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer changed a role: %v", err)
	}

	if err := f.store.SetRole(ctx, f.admin, f.teamA, f.viewer, RoleEditor); err != nil {
		t.Fatalf("an admin could not change a role: %v", err)
	}

	if err := f.store.SetRole(ctx, f.owner, f.teamA, f.editor, RoleBilling); err != nil {
		t.Fatalf("an owner could not change a role: %v", err)
	}
}

// TestAnAdminCannotDemoteAnOwner checks the rank rule that stops an admin
// taking the account.
func TestAnAdminCannotDemoteAnOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.SetRole(ctx, f.admin, f.teamA, f.owner, RoleViewer); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an admin demoted the owner: %v", err)
	}

	if err := f.store.RemoveMember(ctx, f.admin, f.teamA, f.owner); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an admin removed the owner: %v", err)
	}
}

// TestOwnerIsNotGrantableThroughSetRole checks that making a second owner has
// to go through the transfer path, which also demotes the person doing it.
func TestOwnerIsNotGrantableThroughSetRole(t *testing.T) {
	f := newFixture(t)

	if err := f.store.SetRole(context.Background(), f.owner, f.teamA, f.admin, RoleOwner); !errors.Is(err, ErrForbidden) {
		t.Fatalf("SetRole granted owner: %v", err)
	}
}

// TestTheLastOwnerCannotBeRemovedOrDemoted checks the rule that keeps an
// account administrable.
func TestTheLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.RemoveMember(ctx, f.owner, f.teamA, f.owner); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the last owner removed themselves: %v", err)
	}

	if err := f.store.SetRole(ctx, f.owner, f.teamA, f.owner, RoleAdmin); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("the last owner demoted themselves: %v", err)
	}
}

// TestTransferOwnershipDemotesThePreviousOwner checks that a handover is a
// handover rather than a promotion, so a team never has two owners.
func TestTransferOwnershipDemotesThePreviousOwner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.TransferOwnership(ctx, f.owner, f.teamA, f.admin); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if role, _ := f.store.RoleOf(ctx, f.teamA, f.admin); role != RoleOwner {
		t.Fatalf("the new owner has role %q", role)
	}

	if role, _ := f.store.RoleOf(ctx, f.teamA, f.owner); role != RoleAdmin {
		t.Fatalf("the previous owner has role %q, want admin", role)
	}

	owners := 0
	for _, member := range mustMembers(t, f, f.teamA) {
		if member.Role == RoleOwner {
			owners++
		}
	}

	if owners != 1 {
		t.Fatalf("the team has %d owners after a transfer", owners)
	}
}

// TestOnlyAnOwnerMayTransfer checks that an admin cannot hand the account on.
func TestOnlyAnOwnerMayTransfer(t *testing.T) {
	f := newFixture(t)

	if err := f.store.TransferOwnership(context.Background(), f.admin, f.teamA, f.editor); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an admin transferred ownership: %v", err)
	}
}

// TestConcurrentOwnershipTransfersCannotCreateTwoOwners races independent
// database handles and verifies one stale handover loses authorization.
func TestConcurrentOwnershipTransfersCannotCreateTwoOwners(t *testing.T) {
	f := newFixture(t)
	otherDB := openFixtureConnection(t, f.db)
	stores := []*Store{f.store, NewStore(otherDB)}
	targets := []int64{f.admin, f.editor}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results <- stores[index].TransferOwnership(context.Background(), f.owner, f.teamA, targets[index])
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrForbidden) {
			t.Fatalf("concurrent transfer returned %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent ownership transfers succeeded, want 1", succeeded)
	}

	var owners int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM team_memberships WHERE team_id = ? AND role = 'owner'`, f.teamA).
		Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Fatalf("team has %d owners after race, want 1", owners)
	}
}

// openFixtureConnection opens a genuinely independent handle to the fixture's
// SQLite path for race tests.
func openFixtureConnection(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	var sequence int
	var name, path string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })

	return other
}

// TestSiteTransferNeedsBothSides checks that moving a site requires the power
// to manage sites in the team it leaves and the team it joins.
func TestSiteTransferNeedsBothSides(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})
	account, err := manager.Open(ctx, f.teamA)
	if err != nil {
		t.Fatalf("open source analytics: %v", err)
	}
	if _, err := account.Writer().Exec(`
		INSERT INTO annotations
			(site_id, shown_on, body, author_user_id, author_name, created_at, updated_at)
		VALUES (?, '2026-08-30', 'historical note', ?, 'Owner', ?, ?)
	`, f.siteA1, f.owner, f.now.Unix(), f.now.Unix()); err != nil {
		t.Fatalf("insert historical data: %v", err)
	}

	// The admin runs team A but is not in team B at all.
	if err := f.store.TransferSite(ctx, f.admin, f.siteA1, f.teamB); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a transfer into a team the actor is not in = %v", err)
	}

	// The owner runs both.
	if err := f.store.TransferSite(ctx, f.owner, f.siteA1, f.teamB); err != nil {
		t.Fatalf("transfer site: %v", err)
	}

	var storageAccount, ownerTeam int64
	if err := f.db.QueryRow(`SELECT account_id, owner_team_id FROM sites WHERE id = ?`, f.siteA1).
		Scan(&storageAccount, &ownerTeam); err != nil {
		t.Fatalf("read site: %v", err)
	}

	if storageAccount != f.teamA || ownerTeam != f.teamB {
		t.Fatalf("site storage/owner = %d/%d, want %d/%d", storageAccount, ownerTeam, f.teamA, f.teamB)
	}
	for _, teamID := range []int64{f.teamA, f.teamB} {
		if _, err := f.db.Exec(`DELETE FROM teams WHERE id = ?`, teamID); err == nil {
			t.Fatalf("schema allowed deletion of transfer participant %d", teamID)
		}
	}

	if _, err := f.store.AuthoriseSite(ctx, f.siteA1, f.admin, PermViewDashboard); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old-team admin retained access after transfer: %v", err)
	}
	if _, err := f.store.AuthoriseSite(ctx, f.siteA1, f.owner, PermViewDashboard); err != nil {
		t.Fatalf("new-team owner cannot access transferred site: %v", err)
	}

	cache := sites.New(f.db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatalf("refresh site cache: %v", err)
	}
	cached, ok := cache.Lookup("acme.example")
	if !ok || cached.AccountID != f.teamA || cached.TeamID != f.teamB {
		t.Fatalf("cached transferred site = %+v, %v", cached, ok)
	}

	var body string
	if err := account.Reader().QueryRow(`SELECT body FROM annotations WHERE site_id = ?`, f.siteA1).Scan(&body); err != nil {
		t.Fatalf("read transferred history: %v", err)
	}
	if body != "historical note" {
		t.Fatalf("historical annotation changed to %q", body)
	}
}

// TestConcurrentSiteTransfersHaveOneIndependentConnectionWinner races stale
// transfers to different destination teams. The transfer that acquires the
// writer lock second must not overwrite the ownership committed by the first.
func TestConcurrentSiteTransfersHaveOneIndependentConnectionWinner(t *testing.T) {
	f := newFixture(t)
	teamC := f.team(t, "Gamma")
	f.member(t, teamC, f.owner, RoleOwner)
	otherDB := openFixtureConnection(t, f.db)
	stores := []*Store{f.store, NewStore(otherDB)}
	targets := []int64{f.teamB, teamC}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range stores {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results <- stores[index].TransferSiteFrom(context.Background(), f.owner, f.siteA1, f.teamA, targets[index])
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	succeeded, stale := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrStaleTransfer) {
			stale++
		} else {
			t.Fatalf("concurrent site transfer returned %v", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatalf("concurrent site transfer successes/stale refusals = %d/%d, want 1/1", succeeded, stale)
	}

	var storageID, ownerID int64
	if err := f.db.QueryRow(`SELECT account_id, owner_team_id FROM sites WHERE id = ?`, f.siteA1).
		Scan(&storageID, &ownerID); err != nil {
		t.Fatal(err)
	}
	if storageID != f.teamA || (ownerID != f.teamB && ownerID != teamC) {
		t.Fatalf("site storage/owner = %d/%d after race", storageID, ownerID)
	}
}

// TestSiteTransferRevokesEveryOldOwnerDestination checks credentials,
// publication, recipients, Slack, webhooks, snapshots and queued work while
// preserving tracker and analytics configuration.
func TestSiteTransferRevokesEveryOldOwnerDestination(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.db.Exec(`INSERT INTO guest_memberships (site_id, user_id, role, created_at) VALUES (?, ?, 'guest_viewer', ?)`,
		f.siteA1, f.outsider, f.now.Unix()); err != nil {
		t.Fatalf("insert guest: %v", err)
	}

	if _, err := f.db.Exec(`INSERT INTO shared_links (site_id, name, slug, created_at) VALUES (?, 'x', 'abc', ?)`,
		f.siteA1, f.now.Unix()); err != nil {
		t.Fatalf("insert link: %v", err)
	}
	token, _, err := f.store.Invite(ctx, f.owner, Invitation{
		TeamID: f.teamA,
		SiteID: f.siteA1,
		Email:  "outsider@example.com",
		Role:   RoleGuestViewer,
	})
	if err != nil {
		t.Fatalf("invite guest: %v", err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE sites SET is_public = 1 WHERE id = ?`, []any{f.siteA1}},
		{`INSERT INTO site_tracker_config (site_id, updated_at) VALUES (?, ?)`, []any{f.siteA1, f.now.Unix()}},
		{`INSERT INTO site_custom_properties (site_id, key, created_at) VALUES (?, 'plan', ?)`, []any{f.siteA1, f.now.Unix()}},
		{`INSERT INTO site_allowed_hostnames (site_id, hostname, created_at) VALUES (?, 'www.acme.example', ?)`, []any{f.siteA1, f.now.Unix()}},
		{`INSERT INTO saved_segments (site_id, name, created_at) VALUES (?, 'Paid', ?)`, []any{f.siteA1, f.now.Unix()}},
		{`INSERT INTO webhook_endpoints (id, team_id, site_id, url, secret, created_at, updated_at) VALUES (501, ?, ?, 'https://old.example/hook', 'secret', ?, ?)`, []any{f.teamA, f.siteA1, f.now.Unix(), f.now.Unix()}},
		{`INSERT INTO webhook_deliveries (endpoint_id, event_id, event_type, payload, created_at) VALUES (501, 'old-event', 'pageview', '{}', ?)`, []any{f.now.Unix()}},
		{`INSERT INTO report_subscriptions (site_id, kind, recipients, slack_webhook_url, created_at, updated_at) VALUES (?, 'weekly', '["old@example.com"]', 'https://hooks.slack.test/old', ?, ?)`, []any{f.siteA1, f.now.Unix(), f.now.Unix()}},
		{`INSERT INTO alert_rules (site_id, kind, threshold, recipients, slack_webhook_url, created_at, updated_at) VALUES (?, 'spike', 10, '["old@example.com"]', 'https://hooks.slack.test/old', ?, ?)`, []any{f.siteA1, f.now.Unix(), f.now.Unix()}},
		{`INSERT INTO notifications_sent (site_id, kind, recipients, sent_at) VALUES (?, 'drop', 1, ?)`, []any{f.siteA1, f.now.Unix()}},
		{`INSERT INTO notification_claims (id, site_id, kind, payload, created_at) VALUES (502, ?, 'spike', '{}', ?)`, []any{f.siteA1, f.now.Unix()}},
		{`INSERT INTO notification_destinations (notification_id, destination_key, channel, target) VALUES (502, 'email:old@example.com', 'email', 'old@example.com')`, nil},
		{`INSERT INTO jobs (queue, kind, args, scheduled_at, site_id) VALUES ('notifications', 'old.send', '{}', ?, ?)`, []any{f.now.Unix(), f.siteA1}},
	}
	for _, statement := range statements {
		if _, err := f.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed transfer state %q: %v", statement.query, err)
		}
	}

	if err := f.store.TransferSite(ctx, f.owner, f.siteA1, f.teamB); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	var guests, invitations, links, public int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM guest_memberships WHERE site_id = ?`, f.siteA1).Scan(&guests)
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM team_invitations WHERE site_id = ?`, f.siteA1).Scan(&invitations)
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM shared_links WHERE site_id = ?`, f.siteA1).Scan(&links)
	_ = f.db.QueryRow(`SELECT is_public FROM sites WHERE id = ?`, f.siteA1).Scan(&public)

	if guests != 0 {
		t.Errorf("%d guest memberships survived a site transfer", guests)
	}
	if invitations != 0 {
		t.Errorf("%d guest invitations survived a site transfer", invitations)
	}

	if links != 0 {
		t.Errorf("%d shared links survived a site transfer", links)
	}
	if public != 0 {
		t.Error("public dashboard survived a site transfer")
	}
	for _, table := range transferRevokedSiteTables {
		var count int
		if err := f.db.QueryRow("SELECT COUNT(*) FROM "+quoteIdentifier(table)+" WHERE site_id = ?", f.siteA1).Scan(&count); err != nil {
			t.Fatalf("count revoked %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s retained %d old-owner rows", table, count)
		}
	}
	for _, table := range []string{"site_tracker_config", "site_custom_properties", "site_allowed_hostnames", "saved_segments"} {
		var count int
		if err := f.db.QueryRow("SELECT COUNT(*) FROM "+quoteIdentifier(table)+" WHERE site_id = ?", f.siteA1).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%s configuration count = %d, want 1", table, count)
		}
	}
	for _, table := range []string{"notification_destinations", "webhook_deliveries"} {
		var count int
		if err := f.db.QueryRow("SELECT COUNT(*) FROM " + quoteIdentifier(table)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s descendants survived transfer", table)
		}
	}
	if _, err := f.store.Accept(ctx, token, f.outsider); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked invitation remained redeemable: %v", err)
	}
}

// TestSiteTransferAndStaleDestinationSavesHaveOneSerialOutcome proves old-owner
// requests cannot recreate recipients, webhooks or publication after cleanup.
func TestSiteTransferAndStaleDestinationSavesHaveOneSerialOutcome(t *testing.T) {
	f := newFixture(t)
	reportDB := openFixtureConnection(t, f.db)
	reportStore := reports.NewStore(reportDB)
	reportStore.Now = f.store.Now
	webhookDB := openFixtureConnection(t, f.db)
	webhookStore := webhooks.NewStore(webhookDB)
	webhookStore.Now = f.store.Now
	sharingDB := openFixtureConnection(t, f.db)
	sharingStore := sharing.NewStore(sharingDB)
	sharingStore.Now = f.store.Now
	start := make(chan struct{})
	results := make(chan error, 5)
	go func() {
		<-start
		results <- f.store.TransferSiteFrom(context.Background(), f.owner, f.siteA1, f.teamA, f.teamB)
	}()
	go func() {
		<-start
		results <- reportStore.SaveSubscriptionForOwner(context.Background(), reports.Subscription{
			SiteID: f.siteA1, Kind: reports.KindWeekly, Recipients: []string{"old@example.com"},
			SlackWebhookURL: "https://hooks.slack.test/old", Enabled: true,
		}, f.teamA)
	}()
	go func() {
		<-start
		_, err := webhookStore.Create(context.Background(), f.teamA, &f.siteA1,
			"https://old.example/hook", "stale", []string{webhooks.EventTrafficSpike})
		results <- err
	}()
	go func() {
		<-start
		results <- sharingStore.SetPublicForOwner(context.Background(), f.siteA1, f.teamA, true)
	}()
	go func() {
		<-start
		_, err := sharingStore.CreateLinkForOwner(context.Background(), f.siteA1, f.teamA,
			"stale", "", 0, f.owner)
		results <- err
	}()
	close(start)
	for i := 0; i < 5; i++ {
		err := <-results
		if err != nil && !errors.Is(err, reports.ErrSiteOwnerChanged) &&
			!errors.Is(err, sharing.ErrSiteOwnerChanged) && !errors.Is(err, webhooks.ErrNotFound) {
			t.Fatalf("transfer/destination race returned %v", err)
		}
	}

	var ownerID, public, reportsLeft, webhooksLeft, linksLeft int
	if err := f.db.QueryRow(`SELECT owner_team_id, is_public FROM sites WHERE id = ?`, f.siteA1).
		Scan(&ownerID, &public); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM report_subscriptions WHERE site_id = ?`, f.siteA1).Scan(&reportsLeft); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM webhook_endpoints WHERE site_id = ?`, f.siteA1).Scan(&webhooksLeft); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM shared_links WHERE site_id = ?`, f.siteA1).Scan(&linksLeft); err != nil {
		t.Fatal(err)
	}
	if ownerID != int(f.teamB) || public != 0 || reportsLeft != 0 || webhooksLeft != 0 || linksLeft != 0 {
		t.Fatalf("owner/public/reports/webhooks/links after race = %d/%d/%d/%d/%d, want %d/0/0/0/0",
			ownerID, public, reportsLeft, webhooksLeft, linksLeft, f.teamB)
	}
}

// TestSiteTransferFailsClosedForANewScopedTable protects the schema-complete
// transfer classification from silently carrying future old-owner state.
func TestSiteTransferFailsClosedForANewScopedTable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE future_site_destination (id INTEGER PRIMARY KEY, site_id INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	err := f.store.TransferSiteFrom(context.Background(), f.owner, f.siteA1, f.teamA, f.teamB)
	if err == nil || !strings.Contains(err.Error(), "unclassified site transfer table future_site_destination") {
		t.Fatalf("transfer with unclassified table = %v", err)
	}
}

// TestGuestInvitationAndTransferHaveOneSerialOutcome races independent SQLite
// connections and proves no old-team invitation survives whichever writer wins.
func TestGuestInvitationAndTransferHaveOneSerialOutcome(t *testing.T) {
	f := newFixture(t)
	otherDB := openFixtureConnection(t, f.db)
	other := NewStore(otherDB)
	other.Now = f.store.Now
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup

	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, _, err := f.store.Invite(context.Background(), f.owner, Invitation{
			TeamID: f.teamA,
			SiteID: f.siteA1,
			Email:  "outsider@example.com",
			Role:   RoleGuestViewer,
		})
		results <- err
	}()
	go func() {
		defer wait.Done()
		<-start
		results <- other.TransferSiteFrom(context.Background(), f.owner, f.siteA1, f.teamA, f.teamB)
	}()
	close(start)
	wait.Wait()
	close(results)

	succeeded, notFound := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrNotFound) {
			notFound++
		} else {
			t.Fatalf("invitation/transfer race returned %v", err)
		}
	}
	if succeeded != 2 && (succeeded != 1 || notFound != 1) {
		t.Fatalf("race successes/not-found refusals = %d/%d", succeeded, notFound)
	}

	var ownerTeamID, invitations int64
	if err := f.db.QueryRow(`SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?`, f.siteA1).
		Scan(&ownerTeamID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM team_invitations WHERE site_id = ?`, f.siteA1).
		Scan(&invitations); err != nil {
		t.Fatal(err)
	}
	if ownerTeamID != f.teamB || invitations != 0 {
		t.Fatalf("owner/invitations after race = %d/%d, want %d/0", ownerTeamID, invitations, f.teamB)
	}
}

// TestATeamMembershipBeatsAGuestMembership checks that somebody who is both is
// not demoted by the weaker of the two.
func TestATeamMembershipBeatsAGuestMembership(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.db.Exec(`INSERT INTO guest_memberships (site_id, user_id, role, created_at) VALUES (?, ?, 'guest_viewer', ?)`,
		f.siteA1, f.editor, f.now.Unix()); err != nil {
		t.Fatalf("insert guest: %v", err)
	}

	role, err := f.store.SiteRole(ctx, f.siteA1, f.editor)
	if err != nil {
		t.Fatalf("site role: %v", err)
	}

	if role != RoleEditor {
		t.Fatalf("site role = %q, want editor — the guest membership won", role)
	}
}

// TestUnlimitedMembersOnEveryPlan adds far more members than any incumbent tier
// allows and checks that nothing refuses them.
func TestUnlimitedMembersOnEveryPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		email := "person" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + "@example.com"

		token, _, err := f.store.Invite(ctx, f.owner, Invitation{
			TeamID: f.teamA, Email: email, Role: RoleViewer,
		})
		if err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}

		if _, err := f.store.Accept(ctx, token, f.user(t, email)); err != nil {
			t.Fatalf("accept %d: %v", i, err)
		}
	}

	members := mustMembers(t, f, f.teamA)
	if len(members) != 54 {
		t.Fatalf("the team has %d members, want 54 — something capped it", len(members))
	}
}

// TestMembersAreOrderedByRank checks the settings screen's ordering.
func TestMembersAreOrderedByRank(t *testing.T) {
	f := newFixture(t)

	members := mustMembers(t, f, f.teamA)

	for i := 1; i < len(members); i++ {
		if Rank(members[i-1].Role) < Rank(members[i].Role) {
			t.Fatalf("member %d (%s) outranks the one before it (%s)",
				i, members[i].Role, members[i-1].Role)
		}
	}
}

// mustMembers reads a team or fails the test.
func mustMembers(t *testing.T, f *fixture, teamID int64) []Member {
	t.Helper()

	members, err := f.store.Members(context.Background(), teamID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}

	return members
}
