//
// goals.go
// The goals, funnels, properties and exchange rates a seeded site is given.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
)

// The share of a shop's visits that walk the checkout, and how many of them
// survive each step of it.
//
// The walk is generated deliberately rather than left to the page sampler.
// Sampling two thousand pages independently produces an ordered cart-checkout-
// payment-complete sequence roughly never, so a seeded funnel would be four
// zeroes — and a funnel with no drop-off in it tests nothing at all.
const (
	checkoutShare = 0.09

	// Each figure is the share that goes on to the next page. They are not
	// equal on purpose: a funnel whose drop-off is the same at every step
	// looks synthetic on a chart, and the payment step is where a real shop
	// loses people.
	cartToCheckout    = 0.62
	checkoutToPayment = 0.70
	paymentToComplete = 0.75
)

// checkoutPath is the shop's funnel, in order. Every page in it is in the
// shop's head-page catalogue, so the pages also appear in the top-pages report
// as themselves rather than as paths only the funnel has heard of.
var checkoutPath = []string{"/cart", "/checkout", "/checkout/payment", "/order/complete"}

// seedRates are the exchange rates a seeded dataset reports revenue with. They
// are fixed numbers rather than a live fetch: a seed has to produce the same
// database twice, and a rate that moved overnight would change every revenue
// figure a test asserted yesterday.
var seedRates = map[string]float64{
	"EUR": 1.08,
	"GBP": 1.27,
}

// reportingCurrency is what a seeded account totals its revenue in.
const reportingCurrency = "USD"

// ensureGoals gives one site its goals, its funnel and its property
// allow-list.
//
// It runs before any traffic is generated and stamps the goals with the first
// day of the history, because goals do not backfill: a goal created at the end
// of the run would count nothing, and a seeded dataset whose goals report is
// empty would look exactly like a broken goals report.
func (g *generator) ensureGoals(ctx context.Context, account *accountRun, site *siteRun, at time.Time) error {
	db := account.account.Writer()

	if _, err := goals.EnsureAutomatic(ctx, db, site.seeded.ID, at); err != nil {
		return fmt.Errorf("seed goals: %w", err)
	}

	for _, definition := range siteGoals(site.seeded.Fixture.Kind) {
		definition.SiteID = site.seeded.ID

		if _, err := goals.Create(ctx, db, definition, at); err != nil {
			return fmt.Errorf("seed goals: %w", err)
		}
	}

	for name, scope := range seededProperties {
		if _, err := goals.Allow(ctx, db, site.seeded.ID, name, scope, at); err != nil {
			return fmt.Errorf("seed properties: %w", err)
		}
	}

	if site.seeded.Fixture.Kind == kindShop {
		if err := g.ensureCheckoutFunnel(ctx, account, site, at); err != nil {
			return err
		}
	}

	if err := goals.StoreRates(ctx, db, reportingCurrency, seedRates, at); err != nil {
		return fmt.Errorf("seed rates: %w", err)
	}

	return nil
}

// seededProperties is the allow-list every seeded site gets, with the scope
// each property is registered under.
//
// The mix matters. Most of them describe one event, and ab_test_group
// describes the whole visit — which is the case where the declared scope
// changes the denominator of a conversion rate, and therefore the case a
// dataset has to contain for anybody to see the difference.
var seededProperties = map[string]goals.Scope{
	"plan":          goals.ScopeEvent,
	"seats":         goals.ScopeEvent,
	"sku":           goals.ScopeEvent,
	"placement":     goals.ScopeEvent,
	"topic":         goals.ScopeEvent,
	"ab_test_group": goals.ScopeSession,
}

// siteGoals is what each kind of site measures. Every kind gets one goal that
// nothing ever matches: a goal with no conversions is its own empty state, it
// is the row somebody has to be able to look at and understand, and it is the
// one case a generator will never produce by accident.
func siteGoals(kind siteKind) []goals.Goal {
	common := []goals.Goal{
		{
			Kind:        goals.KindEvent,
			EventName:   "Refund Requested",
			DisplayName: "Refunds requested",
		},
	}

	switch kind {
	case kindShop:
		return append(common,
			goals.Goal{
				Kind:        goals.KindPage,
				PagePattern: "/order/complete",
				DisplayName: "Completed orders",
			},
			goals.Goal{
				Kind:        goals.KindEvent,
				EventName:   "Purchase",
				DisplayName: "Purchases",
				IsRevenue:   true,
				Currency:    reportingCurrency,
			},
			goals.Goal{
				Kind:        goals.KindEvent,
				EventName:   "Add to Cart",
				DisplayName: "Added to cart",
			},
			goals.Goal{
				Kind:        goals.KindPage,
				PagePattern: "/collections/**",
				DisplayName: "Browsed a collection",
			},
		)

	case kindBlog:
		return append(common,
			goals.Goal{
				Kind:        goals.KindEvent,
				EventName:   "Newsletter Signup",
				DisplayName: "Newsletter signups",
			},
			goals.Goal{
				Kind:        goals.KindPage,
				PagePattern: "/topics/*",
				DisplayName: "Read a topic page",
			},
		)

	case kindDocs:
		return append(common,
			goals.Goal{
				Kind:        goals.KindPage,
				PagePattern: "/docs/**",
				DisplayName: "Read the documentation",
			},
			goals.Goal{
				Kind:        goals.KindEvent,
				EventName:   "Search",
				DisplayName: "Searched the docs",
			},
		)

	default:
		return append(common,
			goals.Goal{
				Kind:        goals.KindPage,
				PagePattern: "/signup",
				DisplayName: "Reached signup",
			},
			goals.Goal{
				Kind:        goals.KindEvent,
				EventName:   "Signup",
				DisplayName: "Signed up",
			},
			goals.Goal{
				// A goal with a property constraint, which is the shape that
				// exercises the join into the cold table.
				Kind:        goals.KindEvent,
				EventName:   "Signup",
				DisplayName: "Signed up on growth",
				Properties:  []goals.PropertyConstraint{{Name: "plan", Value: "growth"}},
			},
		)
	}
}

// ensureCheckoutFunnel builds the shop's funnel out of one page goal per step.
// The steps are page goals rather than events because that is what a shop
// actually has: a checkout is a sequence of pages, and every one of them is
// already in the site's catalogue.
func (g *generator) ensureCheckoutFunnel(ctx context.Context, account *accountRun, site *siteRun, at time.Time) error {
	db := account.account.Writer()

	steps := make([]goals.Step, 0, len(checkoutPath))

	for _, path := range checkoutPath {
		goal, err := goals.Create(ctx, db, goals.Goal{
			SiteID:      site.seeded.ID,
			Kind:        goals.KindPage,
			PagePattern: path,
			DisplayName: "Visit " + path,
		}, at)
		if err != nil {
			return fmt.Errorf("seed funnel goal %s: %w", path, err)
		}

		steps = append(steps, goals.Step{GoalID: goal.ID})
	}

	if _, err := goals.CreateFunnel(ctx, db, goals.Funnel{
		SiteID: site.seeded.ID,
		Name:   "Checkout",

		// Strict order, because the interesting number in a checkout is where
		// people stop rather than whether they eventually saw every page.
		StrictOrder: true,
		Steps:       steps,
	}, at); err != nil {
		return fmt.Errorf("seed funnel: %w", err)
	}

	return nil
}

// checkoutWalk decides whether a visit is a checkout attempt and how far down
// it gets. It returns nil for every other visit, which is most of them.
func (g *generator) checkoutWalk(site *siteRun) []string {
	if site.seeded.Fixture.Kind != kindShop {
		return nil
	}

	if g.rng.Float64() >= checkoutShare {
		return nil
	}

	reached := 1

	for _, survives := range []float64{cartToCheckout, checkoutToPayment, paymentToComplete} {
		if g.rng.Float64() >= survives {
			break
		}

		reached++
	}

	return checkoutPath[:reached]
}
