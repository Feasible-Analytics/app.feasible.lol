//
// store_test.go
// Who is a candidate for the onboarding emails, and who is left out.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// start is when the onboarding emails began in every test database.
var start = time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)

// openSystem builds a migrated system database whose onboarding start is the
// fixed instant above rather than the moment the migration ran.
func openSystem(t *testing.T) (*sql.DB, string) {
	t.Helper()

	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	mustExec(t, db, `UPDATE onboarding_start SET started_at = ?`, start.Unix())

	return db, dir
}

// mustExec runs a statement that a test cannot continue without.
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// addTeam inserts a verified owner and the team they own, created at an
// instant. The schema trigger enrols the team in the billing lifecycle, as a
// real sign-up is.
func addTeam(t *testing.T, db *sql.DB, id int64, created time.Time) {
	t.Helper()

	stamp := created.Unix()

	mustExec(t, db, `
		INSERT INTO users (id, email, name, email_verified_at, created_at, updated_at)
		VALUES (?, ?, 'Sam Ellis', ?, ?, ?)
	`, id, "owner"+string(rune('0'+id))+"@example.com", stamp, stamp, stamp)
	mustExec(t, db, `INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Example', ?, ?)`, id, stamp, stamp)
	mustExec(t, db, `INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)`, id, id, stamp)
}

// candidateIDs lists the team ids the store would write to.
func candidateIDs(t *testing.T, s *Store, since time.Time) []int64 {
	t.Helper()

	teams, err := s.Candidates(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}

	ids := []int64{}
	for _, team := range teams {
		ids = append(ids, team.ID)
	}

	return ids
}

// TestStartIsStampedByTheMigration proves a fresh database records when the
// emails began, which is what "never backfill" is measured from.
func TestStartIsStampedByTheMigration(t *testing.T) {
	dir := t.TempDir()

	db, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	before := time.Now().UTC().Add(-time.Minute)

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	got, err := NewStore(db).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if got.Before(before) || got.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("start = %s, want the moment the migration ran", got)
	}

	mustExec(t, db, `DELETE FROM onboarding_start`)

	got, err = NewStore(db).Start(context.Background())
	if err != nil || !got.IsZero() {
		t.Fatalf("start with no row = %s, %v; want zero", got, err)
	}
}

// TestCandidatesLeavesOutEveryoneWhoShouldNotBeWrittenTo covers each exclusion
// with one team that trips exactly that rule.
func TestCandidatesLeavesOutEveryoneWhoShouldNotBeWrittenTo(t *testing.T) {
	db, _ := openSystem(t)
	s := NewStore(db)

	signedUp := start.Add(time.Hour)

	// 1: an ordinary new account.
	addTeam(t, db, 1, signedUp)

	// 2: signed up before the emails began.
	addTeam(t, db, 2, start.Add(-time.Second))

	// 3: complimentary access.
	addTeam(t, db, 3, signedUp)
	mustExec(t, db, `INSERT INTO account_comps (team_id, owner_email, comped_at) VALUES (3, 'owner3@example.com', ?)`, signedUp.Unix())

	// 4: asked for the emails to stop.
	addTeam(t, db, 4, signedUp)
	if stopped, err := s.OptOut(context.Background(), 4, signedUp); err != nil || !stopped {
		t.Fatalf("opt out = %v, %v", stopped, err)
	}

	// 5: the owner also belongs to team 1, so they signed up for an invitation.
	addTeam(t, db, 5, signedUp)
	mustExec(t, db, `INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (1, 5, 'viewer', ?)`, signedUp.Unix())

	// 6: being deleted.
	addTeam(t, db, 6, signedUp)
	mustExec(t, db, `UPDATE account_lifecycle SET deleted_at = ? WHERE team_id = 6`, signedUp.Unix())

	// 7: created by an operator, so it has no billing lifecycle at all.
	addTeam(t, db, 7, signedUp)
	mustExec(t, db, `DELETE FROM account_lifecycle WHERE team_id = 7`)

	// 8: the owner is a guest on somebody else's site.
	addTeam(t, db, 8, signedUp)
	mustExec(t, db, `INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', ?, ?)`, signedUp.Unix(), signedUp.Unix())
	mustExec(t, db, `INSERT INTO guest_memberships (site_id, user_id, role, created_at) VALUES (1, 8, 'guest_viewer', ?)`, signedUp.Unix())

	got := candidateIDs(t, s, start)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("candidates = %v, want only team 1", got)
	}
}

// TestCandidatesCarryTheOwner checks the address, the name and the verified
// flag the sweeper acts on.
func TestCandidatesCarryTheOwner(t *testing.T) {
	db, _ := openSystem(t)
	s := NewStore(db)

	signedUp := start.Add(time.Hour)
	addTeam(t, db, 1, signedUp)
	addTeam(t, db, 2, signedUp)
	mustExec(t, db, `UPDATE users SET email_verified_at = NULL WHERE id = 2`)

	teams, err := s.Candidates(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 2 {
		t.Fatalf("candidates = %d, want 2", len(teams))
	}

	first := teams[0]
	if first.OwnerEmail != "owner1@example.com" || first.OwnerName != "Sam Ellis" || !first.Verified || !first.CreatedAt.Equal(signedUp) {
		t.Fatalf("first candidate = %+v", first)
	}

	if teams[1].Verified {
		t.Fatal("an owner with no verified address was reported as verified")
	}
}

// TestSentReadsOnlyCompletedMarkersForThisSignUp proves a leased but unfinished
// row is not a sent marker.
func TestSentReadsOnlyCompletedMarkersForThisSignUp(t *testing.T) {
	db, _ := openSystem(t)
	s := NewStore(db)

	signedUp := start.Add(time.Hour)
	addTeam(t, db, 1, signedUp)

	done := signedUp.Add(5 * time.Minute)

	mustExec(t, db, `
		INSERT INTO lifecycle_outbox (team_id, started_at, template, recipient, message_key, completed_at, created_at, updated_at)
		VALUES (1, ?, ?, 'owner1@example.com', 'a', ?, ?, ?)
	`, signedUp.Unix(), TemplateWelcome, done.Unix(), done.Unix(), done.Unix())
	mustExec(t, db, `
		INSERT INTO lifecycle_outbox (team_id, started_at, template, recipient, message_key, created_at, updated_at)
		VALUES (1, ?, ?, 'owner1@example.com', 'b', ?, ?)
	`, signedUp.Unix(), TemplateNoDataDay1, done.Unix(), done.Unix())

	sent, err := s.Sent(context.Background(), Team{ID: 1, CreatedAt: signedUp})
	if err != nil {
		t.Fatal(err)
	}

	if len(sent) != 1 || !sent[TemplateWelcome].Equal(done) {
		t.Fatalf("sent = %v, want only the completed welcome at %s", sent, done)
	}
}

// TestFirstSiteIsTheOldest checks the site an email names.
func TestFirstSiteIsTheOldest(t *testing.T) {
	db, _ := openSystem(t)
	s := NewStore(db)

	signedUp := start.Add(time.Hour)
	addTeam(t, db, 1, signedUp)

	if _, found, err := s.FirstSite(context.Background(), 1); err != nil || found {
		t.Fatalf("first site with none = %v, %v", found, err)
	}

	mustExec(t, db, `INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (4, 1, 'first.example', 1, 1)`)
	mustExec(t, db, `INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (9, 1, 'second.example', 2, 2)`)

	site, found, err := s.FirstSite(context.Background(), 1)
	if err != nil || !found || site.ID != 4 || site.Domain != "first.example" {
		t.Fatalf("first site = %+v, %v, %v", site, found, err)
	}
}

// TestOptOutIsIdempotentAndIgnoresAMissingTeam covers a second click and a
// link clicked after the account was deleted.
func TestOptOutIsIdempotentAndIgnoresAMissingTeam(t *testing.T) {
	db, _ := openSystem(t)
	s := NewStore(db)

	addTeam(t, db, 1, start.Add(time.Hour))

	if stopped, err := s.OptOut(context.Background(), 1, start); err != nil || !stopped {
		t.Fatalf("first opt out = %v, %v", stopped, err)
	}
	if stopped, err := s.OptOut(context.Background(), 1, start); err != nil || stopped {
		t.Fatalf("second opt out = %v, %v; want no change", stopped, err)
	}
	if stopped, err := s.OptOut(context.Background(), 99, start); err != nil || stopped {
		t.Fatalf("opt out of a missing team = %v, %v; want no change", stopped, err)
	}
}
