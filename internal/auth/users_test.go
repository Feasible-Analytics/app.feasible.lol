//
// users_test.go
// People, their teams, and deleting an account for real.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"testing"
)

// TestCreateUserMakesATeam checks that signing up produces a person, an account
// and the membership joining them. A user without a team can do nothing at all,
// so the three rows have to arrive together or not at all.
func TestCreateUserMakesATeam(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	user, team, err := s.CreateUser(ctx, "Person@Example.com", "Person", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if user.Email != "person@example.com" {
		t.Errorf("email was not normalised: got %q", user.Email)
	}

	if team.TrialEndsAt <= s.Now().Unix() {
		t.Errorf("trial should end in the future, got %d", team.TrialEndsAt)
	}

	var role string
	if err := db.QueryRow("SELECT role FROM team_memberships WHERE team_id = ? AND user_id = ?",
		team.ID, user.ID).Scan(&role); err != nil {
		t.Fatalf("read membership: %v", err)
	}

	if role != "owner" {
		t.Errorf("the person who signed up should own the team, got %q", role)
	}
}

// TestCreateUserNeverReusesADeletedTeamID protects a permanent analytics
// tombstone from being mistaken for a later signup, including the immediate
// settings deletion path that does not retain a lifecycle audit.
func TestCreateUserNeverReusesADeletedTeamID(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, deleted, err := s.CreateUser(ctx, "deleted@example.com", "Deleted", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTeamRows(ctx, deleted.ID, user.ID); err != nil {
		t.Fatal(err)
	}

	_, replacement, err := s.CreateUser(ctx, "replacement@example.com", "Replacement", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID <= deleted.ID {
		t.Fatalf("replacement team id is %d, want greater than deleted id %d", replacement.ID, deleted.ID)
	}
}

// TestCreateUserRejectsADuplicateEmail checks the driver's uniqueness error is
// turned into the sentinel the registration form branches on, and that a
// difference in case still collides — the column is COLLATE NOCASE and an
// address that differs only in case is the same mailbox.
func TestCreateUserRejectsADuplicateEmail(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", ""); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, _, err := s.CreateUser(ctx, "A@Example.com", "", "hash", ""); err != ErrEmailTaken {
		t.Errorf("want ErrEmailTaken, got %v", err)
	}
}

// TestDisplayNameFallsBackToTheAddress checks the header never reads "Welcome,"
// with nothing after it for the majority of people who never fill in a name.
func TestDisplayNameFallsBackToTheAddress(t *testing.T) {
	if got := (&User{Email: "sam@example.com"}).DisplayName(); got != "sam" {
		t.Errorf("want %q, got %q", "sam", got)
	}

	if got := (&User{Email: "sam@example.com", Name: "Sam"}).DisplayName(); got != "Sam" {
		t.Errorf("want %q, got %q", "Sam", got)
	}
}

// TestUnlinkGoogleRefusesToLockSomebodyOut checks the guard on the one action
// in this package that can leave an account with no way in at all.
func TestUnlinkGoogleRefusesToLockSomebodyOut(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "", "google-sub")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.UnlinkGoogle(ctx, user.ID); err == nil {
		t.Fatal("unlinking the only sign-in method should be refused")
	}

	if err := s.SetPassword(ctx, user.ID, "a long enough password", 0); err != nil {
		t.Fatalf("set password: %v", err)
	}

	if err := s.UnlinkGoogle(ctx, user.ID); err != nil {
		t.Errorf("unlinking should be allowed once a password exists: %v", err)
	}
}

// TestRequire2FAIsATeamSetting checks the policy round-trips, since it is what
// the sign-in gate reads on every request.
func TestRequire2FAIsATeamSetting(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.SetRequire2FA(ctx, team.ID, true); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	reloaded, err := s.TeamForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("read team: %v", err)
	}

	if !reloaded.Require2FA {
		t.Error("the team-wide two-factor policy did not stick")
	}
}

// TestDeleteTeamRemovesEverything checks that deleting an account really
// deletes. A privacy product whose deletion leaves hidden rows has no honest
// answer to "what do you still hold about me".
func TestDeleteTeamRemovesEverything(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	user, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := s.CreateSite(ctx, team.ID, "a.example.com", "", "Etc/UTC"); err != nil {
		t.Fatalf("create site: %v", err)
	}

	if _, _, err := s.CreateSession(ctx, user.ID, "Chrome on macOS"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := s.DeleteTeamRows(ctx, team.ID, user.ID); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	for _, query := range []string{
		"SELECT COUNT(*) FROM users",
		"SELECT COUNT(*) FROM teams",
		"SELECT COUNT(*) FROM sites",
		"SELECT COUNT(*) FROM user_sessions",
		"SELECT COUNT(*) FROM team_memberships",
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("%s: %v", query, err)
		}

		if count != 0 {
			t.Errorf("%s left %d rows behind", query, count)
		}
	}
}
