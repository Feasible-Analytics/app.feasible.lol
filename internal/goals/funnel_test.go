//
// funnel_test.go
// Every step and every drop-off, counted by hand from the fixture first.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// checkoutSteps is the funnel the fixture was built around: the cart, the
// checkout, the payment page and the confirmation.
var checkoutSteps = []string{"/cart", "/checkout", "/checkout/payment", "/order/complete"}

// buildFunnel creates the four page goals and the funnel over them.
func buildFunnel(t *testing.T, db *sql.DB, strict bool) Funnel {
	t.Helper()

	steps := make([]Step, 0, len(checkoutSteps))

	for _, path := range checkoutSteps {
		goal := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: path})
		steps = append(steps, Step{GoalID: goal.ID})
	}

	funnel, err := CreateFunnel(context.Background(), db, Funnel{
		SiteID: siteID, Name: "Checkout", StrictOrder: strict, Steps: steps,
	}, goalCreated)
	if err != nil {
		t.Fatal(err)
	}

	return funnel
}

// runFunnel measures a funnel over the fixture's window.
func runFunnel(t *testing.T, db *sql.DB, engine *query.Engine, funnel Funnel) *FunnelResult {
	t.Helper()

	result, err := RunFunnel(context.Background(), db, engine, FunnelRequest{
		FunnelID:  funnel.ID,
		DateRange: fixtureRange(),
		Timezone:  "UTC",
	})
	if err != nil {
		t.Fatalf("funnel failed: %v", err)
	}

	return result
}

// TestAStrictFunnelRequiresConsecutiveSteps checks exact-consecutive mode. A
// later unrelated event cannot erase a sequence that already completed; the
// fixture's complete checkout is therefore retained even though Purchase
// follows its final page.
func TestAStrictFunnelCountsTheStepsInOrder(t *testing.T) {
	db, engine := newFixture(t)

	result := runFunnel(t, db, engine, buildFunnel(t, db, true))

	wantVisitors := []int64{4, 2, 1, 1}
	wantDropOff := []int64{0, 2, 1, 0}
	wantDropRate := []float64{0, 50, 50, 0}

	if len(result.Steps) != len(wantVisitors) {
		t.Fatalf("funnel has %d steps, want %d", len(result.Steps), len(wantVisitors))
	}

	for i, step := range result.Steps {
		if step.Visitors != wantVisitors[i] {
			t.Errorf("step %d visitors = %d, want %d", i+1, step.Visitors, wantVisitors[i])
		}

		if step.DropOff != wantDropOff[i] {
			t.Errorf("step %d drop-off = %d, want %d", i+1, step.DropOff, wantDropOff[i])
		}

		if step.DropOffRate != wantDropRate[i] {
			t.Errorf("step %d drop-off rate = %v, want %v", i+1, step.DropOffRate, wantDropRate[i])
		}
	}

	if got := result.Steps[3].ConversionRate; got != 25 {
		t.Errorf("funnel conversion rate = %v, want 25", got)
	}
}

// TestASequentialFunnelAllowsUnrelatedEvents is the other half of the option.
// Steps still have to be ordered, while unrelated activity between them is
// ignored.
func TestALooseFunnelCountsTheStepsInAnyOrder(t *testing.T) {
	db, engine := newFixture(t)

	result := runFunnel(t, db, engine, buildFunnel(t, db, false))

	wantVisitors := []int64{4, 2, 1, 1}

	for i, step := range result.Steps {
		if step.Visitors != wantVisitors[i] {
			t.Errorf("step %d visitors = %d, want %d", i+1, step.Visitors, wantVisitors[i])
		}
	}
}

// TestFunnelVisitsAndVisitorsAreCountedSeparately checks the two units. Every
// visitor in the fixture reached the cart in exactly one visit, so the two
// agree here; the columns are separate because a returning visitor makes them
// disagree, and a funnel is a thing that happens inside one visit.
func TestFunnelVisitsAndVisitorsAreCountedSeparately(t *testing.T) {
	db, engine := newFixture(t)

	result := runFunnel(t, db, engine, buildFunnel(t, db, true))

	wantVisits := []int64{4, 2, 1, 1}

	for i, step := range result.Steps {
		if step.Visits != wantVisits[i] {
			t.Errorf("step %d visits = %d, want %d", i+1, step.Visits, wantVisits[i])
		}
	}
}

// TestScrollStepsWorkInBothFunnelModes ensures engagement measurements that
// satisfy a scroll goal remain visible to the ordered walk while ordinary
// heartbeat pings do not interrupt a strict funnel.
func TestScrollStepsWorkInBothFunnelModes(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential", true: "strict"}[strict], func(t *testing.T) {
			db, engine := newFixture(t)
			writeScrollMeasurement(t, db)
			page := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/pricing"})
			scroll := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindScroll, PagePattern: "/pricing", ScrollDepth: 50})
			funnel, err := CreateFunnel(context.Background(), db, Funnel{
				SiteID: siteID, Name: "Read pricing", StrictOrder: strict,
				Steps: []Step{{GoalID: page.ID}, {GoalID: scroll.ID}},
			}, goalCreated)
			if err != nil {
				t.Fatal(err)
			}
			result := runFunnel(t, db, engine, funnel)
			if result.Steps[0].Visitors != 2 || result.Steps[1].Visitors != 1 {
				t.Fatalf("scroll funnel visitors = %d -> %d, want 2 -> 1", result.Steps[0].Visitors, result.Steps[1].Visitors)
			}
		})
	}
}

// writeScrollMeasurement adds the real engagement measurement used by the
// scroll funnel test without changing the shared package fixture's event
// totals. It follows the pricing page immediately, which lets strict mode
// prove that this matching measurement is an action while other heartbeats
// remain invisible.
func writeScrollMeasurement(t *testing.T, db *sql.DB) {
	t.Helper()
	engagementID := internID(t, db, "dim_event_name", ingest.EventEngagement)
	pricingID := internID(t, db, "dim_pathname", "/pricing")
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id, scroll_depth)
		VALUES (30, ?, ?, ?, ?, 1, ?, 80)`, siteID, at(29, 10, 1)+1, engagementID, visitorA, pricingID); err != nil {
		t.Fatal(err)
	}
}

// TestAFunnelStartsWhenItsNewestGoalDid checks the window a funnel measures.
// A step added last week would otherwise show every visit before it as a
// drop-off — a cliff on the chart that nothing in the customer's product
// caused.
func TestAFunnelStartsWhenItsNewestGoalDid(t *testing.T) {
	db, engine := newFixture(t)

	ctx := context.Background()

	// The first three steps are older than the traffic; the last one is
	// created after every event in the fixture.
	steps := make([]Step, 0, len(checkoutSteps))

	for i, path := range checkoutSteps {
		created := goalCreated
		if i == len(checkoutSteps)-1 {
			created = time.Date(2026, 8, 30, 23, 0, 0, 0, time.UTC)
		}

		goal, err := Create(ctx, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: path}, created)
		if err != nil {
			t.Fatal(err)
		}

		steps = append(steps, Step{GoalID: goal.ID})
	}

	funnel, err := CreateFunnel(ctx, db, Funnel{
		SiteID: siteID, Name: "Checkout", StrictOrder: true, Steps: steps,
	}, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	result := runFunnel(t, db, engine, funnel)

	if !result.Partial {
		t.Error("a funnel whose newest goal postdates the range must say so")
	}

	// Nothing in the fixture happens after 23:00 on the 30th, so every step is
	// empty rather than showing three steps and a fourth that fell off a cliff.
	for i, step := range result.Steps {
		if step.Visitors != 0 {
			t.Errorf("step %d has %d visitors, want 0", i+1, step.Visitors)
		}
	}
}

// TestAFunnelNeedsTwoSteps pins the limits at both ends.
func TestAFunnelNeedsTwoSteps(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	goal := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/cart"})

	if _, err := CreateFunnel(ctx, db, Funnel{
		SiteID: siteID, Name: "One", Steps: []Step{{GoalID: goal.ID}},
	}, fixtureNow); err == nil {
		t.Error("a one-step funnel must be refused")
	}

	many := make([]Step, 0, MaxFunnelSteps+1)
	for i := 0; i <= MaxFunnelSteps; i++ {
		many = append(many, Step{GoalID: goal.ID})
	}

	if _, err := CreateFunnel(ctx, db, Funnel{
		SiteID: siteID, Name: "Nine", Steps: many,
	}, fixtureNow); err == nil {
		t.Errorf("a %d-step funnel must be refused", len(many))
	}
}

// TestFunnelStepsKeepTheirOrder checks that positions come from the order the
// caller wrote them in rather than from whatever the caller put in the field:
// two steps claiming position three is a chart with a hole in it.
func TestFunnelStepsKeepTheirOrder(t *testing.T) {
	db, _ := newFixture(t)

	funnel := buildFunnel(t, db, true)

	for i, step := range funnel.Steps {
		if step.Position != i+1 {
			t.Errorf("step %d has position %d", i+1, step.Position)
		}

		if step.Goal.PagePattern != checkoutSteps[i] {
			t.Errorf("step %d is %q, want %q", i+1, step.Goal.PagePattern, checkoutSteps[i])
		}
	}
}

// TestAHeartbeatDoesNotBreakAStrictRun pins the promise the funnel form makes
// to the person choosing this setting: turning off "allow other activity"
// demands consecutive events, and an engagement ping is not one of them.
//
// Without this, strict mode would be unsatisfiable on any page a visitor
// spends more than a few seconds on, and the form would be telling them
// something untrue.
func TestAHeartbeatDoesNotBreakAStrictRun(t *testing.T) {
	db, engine := newFixture(t)

	// Visit 3 walks the whole checkout in order. One heartbeat lands between
	// the cart and the checkout page, where a strict run is at its most
	// fragile.
	writeHeartbeat(t, db, nextEventID(t, db), 3, visitorC, at(30, 9, 0)+1, "/cart")

	result := runFunnel(t, db, engine, buildFunnel(t, db, true))

	if got := result.Steps[len(result.Steps)-1].Visitors; got != 1 {
		t.Errorf("%d visitors finished the strict funnel with a heartbeat mid-run, want 1", got)
	}
}

// TestABrokenStrictRunReportsTheFurthestAttempt is the other promise: a
// visitor is credited with how far they ever got, not with wherever they
// happened to stop.
//
// The visit reaches the second step, does something unrelated, and comes back
// to the first — so its last attempt is shallower than its first. Reporting the
// last one would make a funnel look worse the harder a visitor tried.
//
// It is a visitor the fixture does not otherwise have, so the assertion is that
// they are counted rather than that two numbers match. Two numbers matching is
// also what a test whose rows never arrived looks like.
func TestABrokenStrictRunReportsTheFurthestAttempt(t *testing.T) {
	db, engine := newFixture(t)
	before := runFunnel(t, db, engine, buildFunnel(t, db, true)).Steps[1].Visitors

	db, engine = newFixture(t)

	const (
		session = 99
		visitor = 1099
	)

	writePageview(t, db, nextEventID(t, db), session, visitor, at(30, 14, 0), "/cart")
	writePageview(t, db, nextEventID(t, db), session, visitor, at(30, 14, 1), "/checkout")

	// The break, and then a restart that gets no further than the first step.
	writePageview(t, db, nextEventID(t, db), session, visitor, at(30, 14, 2), "/pricing")
	writePageview(t, db, nextEventID(t, db), session, visitor, at(30, 14, 3), "/cart")

	after := runFunnel(t, db, engine, buildFunnel(t, db, true)).Steps[1].Visitors

	if after != before+1 {
		t.Errorf("%d visitors reached the second step, want %d — the visitor who got there and "+
			"then restarted shallower is not being credited with their deepest attempt",
			after, before+1)
	}
}

// nextEventID reserves an event id past everything the fixture holds, so a test
// that adds a row cannot collide with the next helper that does.
func nextEventID(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var highest int64
	if err := db.QueryRowContext(context.Background(),
		"SELECT COALESCE(MAX(id), 0) FROM events").Scan(&highest); err != nil {
		t.Fatal(err)
	}

	return highest + 1
}

// writeHeartbeat adds one engagement ping, which is a measurement rather than
// something the visitor did.
func writeHeartbeat(t *testing.T, db *sql.DB, id, session, user, timestamp int64, page string) {
	t.Helper()

	writeStepEvent(t, db, id, session, user, timestamp, ingest.EventEngagement, page)
}

// writePageview adds one ordinary pageview, which is something the visitor did
// and so can break a strict run.
func writePageview(t *testing.T, db *sql.DB, id, session, user, timestamp int64, page string) {
	t.Helper()

	writeStepEvent(t, db, id, session, user, timestamp, ingest.EventPageview, page)
}

// writeStepEvent inserts one event by its columns, which is what these tests
// have: a database rather than the account handle writeEvent takes.
func writeStepEvent(t *testing.T, db *sql.DB, id, session, user, timestamp int64, name, page string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, siteID, timestamp,
		internID(t, db, "dim_event_name", name), user, session,
		internID(t, db, "dim_pathname", page)); err != nil {
		t.Fatal(err)
	}
}
