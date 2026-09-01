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

// TestAStrictFunnelRequiresConsecutiveSteps checks exact-consecutive mode. One
// visit has cart immediately followed by checkout; intervening visitor events
// interrupt every longer sequence in this fixture.
func TestAStrictFunnelCountsTheStepsInOrder(t *testing.T) {
	db, engine := newFixture(t)

	result := runFunnel(t, db, engine, buildFunnel(t, db, true))

	wantVisitors := []int64{1, 1, 0, 0}
	wantDropOff := []int64{0, 0, 1, 0}
	wantDropRate := []float64{0, 0, 100, 0}

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

	if got := result.Steps[3].ConversionRate; got != 0 {
		t.Errorf("funnel conversion rate = %v, want 0", got)
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

	wantVisits := []int64{1, 1, 0, 0}

	for i, step := range result.Steps {
		if step.Visits != wantVisits[i] {
			t.Errorf("step %d visits = %d, want %d", i+1, step.Visits, wantVisits[i])
		}
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
