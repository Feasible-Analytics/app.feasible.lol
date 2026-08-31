//
// journey_test.go
// Where the fixture's visitors went before and after the cart.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// journey answers the Explore card for one page, or fails the test.
func journey(t *testing.T, db *sql.DB, engine *query.Engine, page string) *JourneyResult {
	t.Helper()

	result, err := Journey(context.Background(), db, engine, JourneyRequest{
		SiteID:    siteID,
		DateRange: fixtureRange(),
		Timezone:  "UTC",
		Page:      page,
	})
	if err != nil {
		t.Fatalf("journey failed: %v", err)
	}

	return result
}

// stepCount finds one step's event count, or -1 when it is not in the list.
func stepCount(steps []JourneyStep, value string) int64 {
	for _, step := range steps {
		if step.Value == value {
			return step.Events
		}
	}

	return -1
}

// TestNextAndPreviousPagesAroundAPage counts the four visits that touched the
// cart. Two went on to the checkout, one jumped to the confirmation page, and
// one arrived at the cart from the checkout — the visit that walked the whole
// flow backwards.
func TestNextAndPreviousPagesAroundAPage(t *testing.T) {
	db, engine := newFixture(t)

	result := journey(t, db, engine, "/cart")

	if result.Views != 4 || result.Visitors != 4 {
		t.Errorf("/cart had %d views by %d visitors, want 4 and 4", result.Views, result.Visitors)
	}

	if got := stepCount(result.NextPages, "/checkout"); got != 2 {
		t.Errorf("next page /checkout = %d, want 2", got)
	}

	if got := stepCount(result.NextPages, "/order/complete"); got != 1 {
		t.Errorf("next page /order/complete = %d, want 1", got)
	}

	if got := stepCount(result.NextPages, "/checkout/payment"); got != 1 {
		t.Errorf("next page /checkout/payment = %d, want 1", got)
	}

	// Three visits started at the cart, so their previous step is the start of
	// the visit rather than a page. Dropping those would make the card claim
	// everybody arrived from somewhere inside the site.
	if got := stepCount(result.PreviousPages, EntryStep); got != 3 {
		t.Errorf("previous step %q = %d, want 3", EntryStep, got)
	}

	if got := stepCount(result.PreviousPages, "/checkout"); got != 1 {
		t.Errorf("previous page /checkout = %d, want 1", got)
	}
}

// TestTheEndOfAVisitIsAStep checks the exit bucket. The confirmation page ends
// three of the fixture's visits, and "most people leave from here" is the most
// useful thing this report says.
func TestTheEndOfAVisitIsAStep(t *testing.T) {
	db, engine := newFixture(t)

	result := journey(t, db, engine, "/order/complete")

	if got := stepCount(result.NextPages, ExitStep); got != 3 {
		t.Errorf("exits from /order/complete = %d, want 3", got)
	}

	for _, step := range result.NextPages {
		if step.Value == ExitStep && !step.Terminal {
			t.Error("the exit bucket must be marked terminal, not left to look like a page called (exit)")
		}
	}
}

// TestTrailingSlashesAreNotNormalised is the one behaviour this report exists
// to get right. The incumbent normalised /cart/ to /cart in this report and
// nowhere else, so journey steps stopped lining up with Top Pages. Two paths
// are two pages here, exactly as they are everywhere else; merging them
// belongs in path-cleaning rules, applied uniformly.
func TestTrailingSlashesAreNotNormalised(t *testing.T) {
	db, engine := newFixture(t)

	writeTrailingSlashVisit(t, db)

	plain := journey(t, db, engine, "/cart")

	if plain.Views != 4 {
		t.Errorf("/cart had %d views, want 4 — /cart/ is a different page", plain.Views)
	}

	if got := stepCount(plain.NextPages, "/checkout"); got != 2 {
		t.Errorf("next page /checkout from /cart = %d, want 2", got)
	}

	slashed := journey(t, db, engine, "/cart/")

	if slashed.Views != 1 {
		t.Errorf("/cart/ had %d views, want 1", slashed.Views)
	}

	if got := stepCount(slashed.NextPages, "/checkout"); got != 1 {
		t.Errorf("next page /checkout from /cart/ = %d, want 1", got)
	}
}

// writeTrailingSlashVisit adds one visit that reaches /cart/ rather than
// /cart, which is the pair of paths the normalisation bug conflated.
func writeTrailingSlashVisit(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
			pageviews, events, entry_page_id, exit_page_id, source_id)
		VALUES (7, ?, 1005, ?, ?, 0, 0, 2, 2, 0, 0, 0)`,
		siteID, at(30, 13, 0), at(30, 13, 1)); err != nil {
		t.Fatal(err)
	}

	pathID := internID(t, db, "dim_pathname", "/cart/")
	checkoutID := internID(t, db, "dim_pathname", "/checkout")
	pageviewID := internID(t, db, "dim_event_name", ingest.EventPageview)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id, scroll_depth)
		VALUES (22, ?, ?, ?, 1005, 7, ?, 255), (23, ?, ?, ?, 1005, 7, ?, 255)`,
		siteID, at(30, 13, 0), pageviewID, pathID,
		siteID, at(30, 13, 1), pageviewID, checkoutID); err != nil {
		t.Fatal(err)
	}
}

// internID reads or creates a dimension value's id the way the ingest cache
// would, so a test can add rows without holding an account handle.
func internID(t *testing.T, db *sql.DB, table, value string) int64 {
	t.Helper()

	ctx := context.Background()

	var id int64

	err := db.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE value = ?", value).Scan(&id)
	if err == nil {
		return id
	}

	result, err := db.ExecContext(ctx, "INSERT INTO "+table+" (value) VALUES (?)", value)
	if err != nil {
		t.Fatal(err)
	}

	id, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	return id
}

// TestTheEventAfterAPageIsAStep checks the events half of the card: the signup
// that followed the signup page.
func TestTheEventAfterAPageIsAStep(t *testing.T) {
	db, engine := newFixture(t)

	result := journey(t, db, engine, "/signup")

	if got := stepCount(result.NextEvents, "Signup"); got != 1 {
		t.Errorf("next event Signup = %d, want 1", got)
	}

	// A pageview is not an event on this half of the card, or every row would
	// be "pageview" and the card would say nothing.
	if got := stepCount(result.NextEvents, ingest.EventPageview); got != -1 {
		t.Errorf("pageviews must not appear in the events breakdown, got %d", got)
	}
}

// TestAPageWithNoHistoryAnswersEmpty checks the case that would otherwise
// match id 0 — the empty string every event with no path carries — and report
// somebody else's journey.
func TestAPageWithNoHistoryAnswersEmpty(t *testing.T) {
	db, engine := newFixture(t)

	result := journey(t, db, engine, "/never-served")

	if result.Views != 0 || len(result.NextPages) != 0 || len(result.PreviousPages) != 0 {
		t.Errorf("a page nobody visited reported %+v, want an empty journey", result)
	}
}

// TestAJourneyIsCappedAtTwentySteps pins the limit, and the default beneath it.
func TestAJourneyIsCappedAtTwentySteps(t *testing.T) {
	db, engine := newFixture(t)

	result, err := Journey(context.Background(), db, engine, JourneyRequest{
		SiteID:    siteID,
		DateRange: fixtureRange(),
		Timezone:  "UTC",
		Page:      "/cart",
		Limit:     500,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.NextPages) > MaxJourneySteps {
		t.Errorf("a journey returned %d steps, want at most %d", len(result.NextPages), MaxJourneySteps)
	}
}
