//
// goals_test.go
// The seeded goals, the funnel that has to lose people, and the goal that has nobody.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// shopDomain is the seeded site the checkout funnel belongs to.
const shopDomain = "shop.northwind.example"

// seededShop runs a small dataset and opens the shop's account database. The
// shop is the third traffic-carrying site in the fixture, so three sites is the
// smallest run that reaches it.
func seededShop(t *testing.T) (*sql.DB, int64) {
	t.Helper()

	dir := t.TempDir()

	if _, err := Run(context.Background(), Options{
		DataDir:           dir,
		Pageviews:         8000,
		Days:              14,
		Sites:             3,
		Seed:              7,
		Now:               fixedNow,
		ControlMigrations: migrate.Control(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	control, err := store.Open(filepath.Join(dir, config.ControlDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var siteID int64
	if err := control.QueryRow("SELECT id FROM sites WHERE domain = ?", shopDomain).Scan(&siteID); err != nil {
		t.Fatalf("find %s: %v", shopDomain, err)
	}

	db, err := store.Open(accounts.Path(dir, 1))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db, siteID
}

// engineFor builds a compiler over a seeded account, pinned to the same clock
// the run used so "all time" resolves to the history that was generated.
func engineFor(db *sql.DB) *query.Engine {
	engine := query.New(db)
	engine.Now = func() time.Time { return fixedNow() }

	return engine
}

// TestSeedCreatesGoals checks that a seeded site has something on its goals
// report at all. Until there was a schema for them the seed skipped goals
// entirely, which meant the one screen this milestone is about had no data to
// be built against.
func TestSeedCreatesGoals(t *testing.T) {
	db, siteID := seededShop(t)

	list, err := goals.List(context.Background(), db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) < 8 {
		t.Fatalf("the shop has %d goals, want the four automatic ones and its own", len(list))
	}

	var automatic, revenue, funnelSteps int

	for _, goal := range list {
		if goal.IsAutomatic {
			automatic++
		}

		if goal.IsRevenue {
			revenue++
		}

		for _, path := range checkoutPath {
			if goal.PagePattern == path {
				funnelSteps++
			}
		}
	}

	if automatic != 4 {
		t.Errorf("the shop has %d automatic goals, want 4", automatic)
	}

	if revenue == 0 {
		t.Error("no seeded goal carries revenue, so no revenue report has data")
	}

	if funnelSteps != len(checkoutPath) {
		t.Errorf("%d of the %d funnel step goals exist", funnelSteps, len(checkoutPath))
	}
}

// TestSeedCreatesAGoalNobodyConverts checks the empty state. A goal at zero is
// the row somebody has to be able to look at and understand, and it is the one
// case a generator will never produce by accident.
func TestSeedCreatesAGoalNobodyConverts(t *testing.T) {
	db, siteID := seededShop(t)

	result, err := goals.Report(context.Background(), db, engineFor(db), goals.ReportRequest{
		SiteID:    siteID,
		DateRange: query.DateRange{Preset: query.RangeAll},
		Timezone:  "UTC",
		Currency:  reportingCurrency,
	})
	if err != nil {
		t.Fatalf("goals report: %v", err)
	}

	var empty, converting int

	for _, row := range result.Rows {
		if row.TotalConversions == 0 {
			empty++
			continue
		}

		converting++
	}

	if empty == 0 {
		t.Error("every seeded goal converts — there is no empty row to build the empty state against")
	}

	if converting == 0 {
		t.Error("no seeded goal converts at all, so the goals report is blank")
	}
}

// TestSeedCreatesAFunnelWithARealDropOff is the point of generating checkout
// walks rather than leaving them to the page sampler. Sampling two thousand
// pages independently produces an ordered cart-to-confirmation sequence roughly
// never, and a funnel of four zeroes tests nothing.
func TestSeedCreatesAFunnelWithARealDropOff(t *testing.T) {
	db, siteID := seededShop(t)

	ctx := context.Background()

	funnels, err := goals.ListFunnels(ctx, db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	if len(funnels) != 1 {
		t.Fatalf("the shop has %d funnels, want 1", len(funnels))
	}

	funnel := funnels[0]

	if len(funnel.Steps) != len(checkoutPath) {
		t.Fatalf("the funnel has %d steps, want %d", len(funnel.Steps), len(checkoutPath))
	}

	result, err := goals.RunFunnel(ctx, db, engineFor(db), goals.FunnelRequest{
		FunnelID:  funnel.ID,
		DateRange: query.DateRange{Preset: query.RangeAll},
		Timezone:  "UTC",
	})
	if err != nil {
		t.Fatalf("run funnel: %v", err)
	}

	for i, step := range result.Steps {
		if step.Visitors == 0 {
			t.Fatalf("step %d has no visitors, so there is nothing to draw", i+1)
		}

		if i == 0 {
			continue
		}

		if step.Visitors > result.Steps[i-1].Visitors {
			t.Errorf("step %d has more visitors than step %d, which is not a funnel", i+1, i)
		}

		if step.DropOff == 0 {
			t.Errorf("step %d loses nobody — a funnel with no drop-off shows nothing", i+1)
		}
	}
}

// TestSeedRegistersPropertiesWithScopes checks the allow-list the seed writes,
// including the one session-scoped property. Without a property that describes
// the whole visit, nothing in a seeded dataset shows the difference a declared
// scope makes to a conversion rate.
func TestSeedRegistersPropertiesWithScopes(t *testing.T) {
	db, siteID := seededShop(t)

	allowed, err := goals.Allowed(context.Background(), db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	scopes := map[string]goals.Scope{}
	for _, property := range allowed {
		scopes[property.Name] = property.Scope
	}

	if scopes["plan"] != goals.ScopeEvent {
		t.Errorf("plan is scoped %q, want event", scopes["plan"])
	}

	if scopes["ab_test_group"] != goals.ScopeSession {
		t.Errorf("ab_test_group is scoped %q, want session", scopes["ab_test_group"])
	}
}

// TestSeedStoresExchangeRates checks that a seeded dataset can report revenue
// across the three currencies it generates. Without rates the cross-currency
// total is zero and a warning, which is correct and useless to build against.
func TestSeedStoresExchangeRates(t *testing.T) {
	db, _ := seededShop(t)

	rates, fetchedAt, err := goals.ReadRates(context.Background(), db, reportingCurrency)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]float64{}
	for _, rate := range rates {
		found[rate.Base] = rate.Rate
	}

	for _, currency := range []string{"EUR", "GBP", reportingCurrency} {
		if found[currency] <= 0 {
			t.Errorf("no rate stored for %s", currency)
		}
	}

	if fetchedAt.IsZero() {
		t.Error("the stored rates have no fetch time, so nothing can tell whether they are stale")
	}
}
