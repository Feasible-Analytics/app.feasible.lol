//
// report_test.go
// Every conversion number on the goals report, against hand-computed values.
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

// report runs the goals report over the fixture's window, or fails the test.
func report(t *testing.T, db *sql.DB, engine *query.Engine, req ReportRequest) *ReportResult {
	t.Helper()

	req.SiteID = siteID
	req.DateRange = fixtureRange()
	req.Timezone = "UTC"

	result, err := Report(context.Background(), db, engine, req)
	if err != nil {
		t.Fatalf("goals report failed: %v", err)
	}

	return result
}

// rowFor finds one goal's line by its label.
func rowFor(t *testing.T, result *ReportResult, label string) ReportRow {
	t.Helper()

	for _, row := range result.Rows {
		if row.Goal.Label() == label {
			return row
		}
	}

	t.Fatalf("no row for %q in %d rows", label, len(result.Rows))

	return ReportRow{}
}

// TestUniqueAndTotalConversionsDifferForARepeatedGoal is the support-ticket
// test. Visit 1 signs up twice: that is one unique conversion and two total
// ones, and showing only one of the two numbers is why everybody who tests a
// goal by clicking it repeatedly concludes it is broken.
func TestUniqueAndTotalConversionsDifferForARepeatedGoal(t *testing.T) {
	db, engine := newFixture(t)

	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Signed up"})

	row := rowFor(t, report(t, db, engine, ReportRequest{}), "Signed up")

	if row.UniqueConversions != 1 {
		t.Errorf("unique conversions = %d, want 1 — a goal converts at most once per visit", row.UniqueConversions)
	}

	if row.TotalConversions != 2 {
		t.Errorf("total conversions = %d, want 2 — both signups in the visit count", row.TotalConversions)
	}

	if row.ConvertedVisitors != 1 {
		t.Errorf("converted visitors = %d, want 1", row.ConvertedVisitors)
	}
}

// TestConversionRateDividesByEveryVisitorInThePeriod pins the divisor. One of
// the fixture's four visitors signed up, so the rate is 25% — and it is 25%
// whichever goal is being measured, which is what makes two goals comparable.
func TestConversionRateDividesByEveryVisitorInThePeriod(t *testing.T) {
	db, engine := newFixture(t)

	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Signed up"})
	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/order/complete", DisplayName: "Ordered"})

	result := report(t, db, engine, ReportRequest{})

	if result.Visitors != 4 {
		t.Fatalf("the period has %d visitors, want 4", result.Visitors)
	}

	signup := rowFor(t, result, "Signed up")
	if signup.ConversionRate != 25 {
		t.Errorf("signup conversion rate = %v, want 25 (1 of 4 visitors)", signup.ConversionRate)
	}

	// Three visits reached the confirmation page, made by three different
	// visitors, so the rate is three quarters.
	ordered := rowFor(t, result, "Ordered")

	if ordered.UniqueConversions != 3 {
		t.Errorf("order unique conversions = %d, want 3", ordered.UniqueConversions)
	}

	if ordered.ConvertedVisitors != 3 {
		t.Errorf("order converted visitors = %d, want 3", ordered.ConvertedVisitors)
	}

	if ordered.ConversionRate != 75 {
		t.Errorf("order conversion rate = %v, want 75", ordered.ConversionRate)
	}
}

// TestAGoalWithNoConversionsIsStillARow checks the empty state. A goal
// somebody created deliberately has to be visible at zero, or the only way to
// find out it never fires is to notice it is missing.
func TestAGoalWithNoConversionsIsStillARow(t *testing.T) {
	db, engine := newFixture(t)

	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Refund Requested", DisplayName: "Refunds"})

	row := rowFor(t, report(t, db, engine, ReportRequest{}), "Refunds")

	if row.UniqueConversions != 0 || row.TotalConversions != 0 || row.ConversionRate != 0 {
		t.Errorf("a goal nothing matched reported %+v, want zeroes", row)
	}
}

// TestGoalsDoNotBackfill is the second support-ticket behaviour. Conversions
// count from the goal's creation forward, so a goal created after the traffic
// counts none of it — and the row says which instant it started from rather
// than leaving somebody to guess why the number is zero.
func TestGoalsDoNotBackfill(t *testing.T) {
	db, engine := newFixture(t)

	ctx := context.Background()

	// Created after both signups, which happened on the 29th.
	late := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	if _, err := Create(ctx, db, Goal{
		SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Signed up",
	}, late); err != nil {
		t.Fatal(err)
	}

	row := rowFor(t, report(t, db, engine, ReportRequest{}), "Signed up")

	if row.TotalConversions != 0 {
		t.Errorf("a goal created after the traffic counted %d conversions, want 0", row.TotalConversions)
	}

	if !row.Partial {
		t.Error("a goal created inside the report range must be marked partial")
	}

	if !row.From.Equal(late) {
		t.Errorf("row starts at %v, want %v", row.From, late)
	}
}

// TestAWildcardGoalMatchesWhereItShould checks the two wildcards against real
// rows rather than against the matcher alone: /checkout/** is the pages under
// checkout and /checkout* is the checkout page itself.
func TestAWildcardGoalMatchesWhereItShould(t *testing.T) {
	db, engine := newFixture(t)

	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/checkout/**", DisplayName: "Deep"})
	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/checkout*", DisplayName: "Shallow"})

	result := report(t, db, engine, ReportRequest{})

	// /checkout/payment was viewed in visits 3 and 6.
	deep := rowFor(t, result, "Deep")
	if deep.UniqueConversions != 2 || deep.TotalConversions != 2 {
		t.Errorf("/checkout/** = %d unique, %d total, want 2 and 2", deep.UniqueConversions, deep.TotalConversions)
	}

	// /checkout itself was viewed in visits 3, 4 and 6.
	shallow := rowFor(t, result, "Shallow")
	if shallow.UniqueConversions != 3 || shallow.TotalConversions != 3 {
		t.Errorf("/checkout* = %d unique, %d total, want 3 and 3", shallow.UniqueConversions, shallow.TotalConversions)
	}
}

// TestAPropertyConstraintNarrowsAGoal checks the join into the cold table.
// Visit 1 signed up twice on two different plans, so the constrained goal
// counts one of them and the unconstrained one counts both.
func TestAPropertyConstraintNarrowsAGoal(t *testing.T) {
	db, engine := newFixture(t)

	mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Any plan"})
	mustCreate(t, db, Goal{
		SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Growth only",
		Properties: []PropertyConstraint{{Name: "plan", Value: "growth"}},
	})

	result := report(t, db, engine, ReportRequest{})

	if got := rowFor(t, result, "Any plan").TotalConversions; got != 2 {
		t.Errorf("unconstrained signups = %d, want 2", got)
	}

	if got := rowFor(t, result, "Growth only").TotalConversions; got != 1 {
		t.Errorf("growth signups = %d, want 1", got)
	}
}

// TestTheAutomaticGoalHidesUntilItHasSomethingToShow is what makes creating
// four goals on every new site free. The fixture serves exactly one 404, so
// that goal appears and the other three do not.
func TestTheAutomaticGoalHidesUntilItHasSomethingToShow(t *testing.T) {
	db, engine := newFixture(t)

	if _, err := EnsureAutomatic(context.Background(), db, siteID, fixtureNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	result := report(t, db, engine, ReportRequest{})

	if len(result.Rows) != 1 {
		t.Fatalf("report has %d rows, want only the 404 goal", len(result.Rows))
	}

	row := result.Rows[0]

	if row.Goal.EventName != EventNotFound {
		t.Fatalf("the visible automatic goal is %q, want %q", row.Goal.EventName, EventNotFound)
	}

	if row.TotalConversions != 1 || row.ConvertedVisitors != 1 {
		t.Errorf("404 goal = %d conversions by %d visitors, want 1 and 1",
			row.TotalConversions, row.ConvertedVisitors)
	}

	// The settings screen has to see all four, which is the one place somebody
	// wants the empty ones.
	all := report(t, db, engine, ReportRequest{IncludeEmptyAutomatic: true})
	if len(all.Rows) != 4 {
		t.Errorf("settings view has %d rows, want all 4 automatic goals", len(all.Rows))
	}
}

// TestRevenueOnAGoalIsCountedInMinorUnits checks the money on the one purchase
// in the fixture: fifty dollars, stored as five thousand cents, never as a
// float.
func TestRevenueOnAGoalIsCountedInMinorUnits(t *testing.T) {
	db, engine := newFixture(t)

	mustCreate(t, db, Goal{
		SiteID: siteID, Kind: KindEvent, EventName: "Purchase", DisplayName: "Purchases",
		IsRevenue: true, Currency: "USD",
	})

	row := rowFor(t, report(t, db, engine, ReportRequest{}), "Purchases")

	if row.Revenue != 5000 {
		t.Errorf("revenue = %d, want 5000 minor units", row.Revenue)
	}

	if row.AverageRevenue != 5000 {
		t.Errorf("average revenue = %d, want 5000 — one purchase", row.AverageRevenue)
	}

	// Four visitors in the period, fifty dollars taken, so twelve fifty each.
	if row.RevenuePerVisit != 1250 {
		t.Errorf("revenue per visitor = %d, want 1250", row.RevenuePerVisit)
	}

	if row.Currency != "USD" {
		t.Errorf("currency = %q, want USD", row.Currency)
	}
}

// TestTheNoBackfillNoticeExists checks that the sentence the creation form has
// to show lives beside the behaviour it describes, rather than in a template
// somebody can edit without noticing what it promises.
func TestTheNoBackfillNoticeExists(t *testing.T) {
	if NoBackfillNotice == "" {
		t.Error("the creation form has no sentence to show about backfilling")
	}
}
