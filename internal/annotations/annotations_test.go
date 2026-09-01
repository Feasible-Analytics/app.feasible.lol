//
// annotations_test.go
// Dated notes, stored against a day rather than an instant.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package annotations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// fixture is an account manager over a temporary data directory.
type fixture struct {
	store     *Store
	accountID int64
	siteID    int64
	now       time.Time
}

// newFixture builds one.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	f := &fixture{
		accountID: 1,
		siteID:    7,
		now:       time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}

	f.store = NewStore(manager)
	f.store.Now = func() time.Time { return f.now }

	return f
}

// TestANoteIsStoredAgainstItsLocalDay is the design decision, asserted.
//
// An annotation is about a day — "we launched", "the outage" — not a moment.
// Storing an instant and converting it back would move somebody's marker by a
// day depending on which continent they read the dashboard from.
func TestANoteIsStoredAgainstItsLocalDay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := f.store.Create(ctx, f.accountID, Annotation{
		SiteID:     f.siteID,
		ShownOn:    "2026-08-14",
		Body:       "Launched the new pricing page",
		AuthorName: "Sam",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	read, err := f.store.Get(ctx, f.accountID, f.siteID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if read.ShownOn != "2026-08-14" {
		t.Fatalf("the date came back as %q", read.ShownOn)
	}

	if read.AuthorName != "Sam" {
		t.Fatalf("the author came back as %q", read.AuthorName)
	}
}

// TestListingIsInclusiveAtBothEnds checks the range the graph asks for.
func TestListingIsInclusiveAtBothEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, date := range []string{"2026-07-31", "2026-08-01", "2026-08-15", "2026-08-31", "2026-09-01"} {
		if _, err := f.store.Create(ctx, f.accountID, Annotation{
			SiteID: f.siteID, ShownOn: date, Body: "note for " + date,
		}); err != nil {
			t.Fatalf("create %s: %v", date, err)
		}
	}

	found, err := f.store.List(ctx, f.accountID, f.siteID, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(found) != 3 {
		t.Fatalf("the range returned %d notes, want 3 (both bounds inclusive)", len(found))
	}

	if found[0].ShownOn != "2026-08-01" || found[2].ShownOn != "2026-08-31" {
		t.Fatalf("the range is wrong: %+v", found)
	}
}

// TestAnOpenEndedRangeReturnsEverything checks the case where the dashboard has
// not resolved its dates yet. Answering with everything is far better than
// answering with nothing.
func TestAnOpenEndedRangeReturnsEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, date := range []string{"2020-01-01", "2026-08-15", "2030-12-31"} {
		if _, err := f.store.Create(ctx, f.accountID, Annotation{
			SiteID: f.siteID, ShownOn: date, Body: "x",
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	found, err := f.store.List(ctx, f.accountID, f.siteID, "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(found) != 3 {
		t.Fatalf("an open range returned %d notes, want 3", len(found))
	}
}

// TestNotesAreScopedToOneSite checks that two sites in one account database do
// not see each other's markers.
func TestNotesAreScopedToOneSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.store.Create(ctx, f.accountID, Annotation{SiteID: f.siteID, ShownOn: "2026-08-14", Body: "mine"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := f.store.Create(ctx, f.accountID, Annotation{SiteID: 99, ShownOn: "2026-08-14", Body: "theirs"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	found, err := f.store.List(ctx, f.accountID, f.siteID, "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(found) != 1 || found[0].Body != "mine" {
		t.Fatalf("the site's notes are %+v", found)
	}
}

// TestAnIdFromAnotherSiteCannotDeleteThisOnes checks the predicate that keeps
// one site's id from reaching another site's row.
func TestAnIdFromAnotherSiteCannotDeleteThisOnes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	other, err := f.store.Create(ctx, f.accountID, Annotation{SiteID: 99, ShownOn: "2026-08-14", Body: "theirs"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.store.Delete(ctx, f.accountID, f.siteID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting another site's note = %v, want ErrNotFound", err)
	}

	if _, err := f.store.Get(ctx, f.accountID, 99, other.ID); err != nil {
		t.Fatalf("the other site's note was deleted: %v", err)
	}
}

// TestABadDateIsRefusedWithASentence checks that a date the graph could never
// match is caught at write time rather than becoming a marker on nothing.
func TestABadDateIsRefusedWithASentence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, date := range []string{"", "2026-8-1", "14/08/2026", "2026-13-01", "2026-02-30"} {
		_, err := f.store.Create(ctx, f.accountID, Annotation{
			SiteID: f.siteID, ShownOn: date, Body: "x",
		})

		if !errors.Is(err, ErrInvalid) {
			t.Errorf("the date %q was accepted: %v", date, err)
		}
	}
}

// TestAnEmptyOrOverlongNoteIsRefused checks the body's bounds.
func TestAnEmptyOrOverlongNoteIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for name, body := range map[string]string{
		"empty":      "",
		"whitespace": "   \n  ",
		"too long":   strings.Repeat("a", MaxBodyLength+1),
	} {
		if _, err := f.store.Create(ctx, f.accountID, Annotation{
			SiteID: f.siteID, ShownOn: "2026-08-14", Body: body,
		}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted with %v", name, err)
		}
	}
}

// TestUpdatingKeepsTheAuthor checks that the person who wrote a note is a fact
// about the note rather than about whoever last fixed a typo.
func TestUpdatingKeepsTheAuthor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := f.store.Create(ctx, f.accountID, Annotation{
		SiteID: f.siteID, ShownOn: "2026-08-14", Body: "Lanuched", AuthorName: "Sam", AuthorUserID: 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	f.now = f.now.Add(time.Hour)

	if err := f.store.Update(ctx, f.accountID, Annotation{
		ID: created.ID, SiteID: f.siteID, ShownOn: "2026-08-15", Body: "Launched",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	read, err := f.store.Get(ctx, f.accountID, f.siteID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if read.Body != "Launched" || read.ShownOn != "2026-08-15" {
		t.Fatalf("the edit did not land: %+v", read)
	}

	if read.AuthorName != "Sam" || read.AuthorUserID != 3 {
		t.Fatalf("the author changed on edit: %+v", read)
	}

	if read.UpdatedAt <= read.CreatedAt {
		t.Fatal("the edit did not move updated_at")
	}
}

// TestDeletingRemovesTheMarker checks the last operation.
func TestDeletingRemovesTheMarker(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := f.store.Create(ctx, f.accountID, Annotation{
		SiteID: f.siteID, ShownOn: "2026-08-14", Body: "temporary",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.store.Delete(ctx, f.accountID, f.siteID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := f.store.Get(ctx, f.accountID, f.siteID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted note still reads: %v", err)
	}
}
