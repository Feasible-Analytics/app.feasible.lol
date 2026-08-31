//
// revenue_test.go
// Money across two currencies, asserted to the minor unit.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// purchaseFilter selects the two purchases written below.
var purchaseFilter = Filter{Operator: OpIs, Dimension: "event:name", Values: []string{"Purchase"}}

// TestRevenueIsSummedInTheReportingCurrency is the arithmetic, by hand: fifty
// dollars and twenty euros at 1.10, so seventy-two dollars, held as 7200
// minor units and never as a float.
func TestRevenueIsSummedInTheReportingCurrency(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writePurchases(t, account)
	writeRate(t, account, "EUR", "USD", 1.10)

	q := baseQuery("total_revenue", "average_revenue", "revenue_per_visitor")
	q.Filters = []Filter{purchaseFilter}
	q.Currency = "USD"

	row := run(t, engine, q).Results[0]

	closeTo(t, "total_revenue", row.Metrics[0], 7200)

	// Two purchases, so the average is half the total.
	closeTo(t, "average_revenue", row.Metrics[1], 3600)

	// Three visitors could have paid, whether or not they did, so the money is
	// spread across all three.
	closeTo(t, "revenue_per_visitor", row.Metrics[2], 2400)
}

// TestRevenueBreaksDownBySource is the attribution test.
//
// The euro purchase happened in a visit that arrived from Twitter and the
// dollar one in a visit with no source, and each visit's money follows the
// source the visit started with. That is why a payment provider never appears
// here: a visitor coming back through one is still inside the visit that
// started at Twitter, and the source of a visit is fixed at its first event.
func TestRevenueBreaksDownBySource(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writePurchases(t, account)
	writeRate(t, account, "EUR", "USD", 1.10)

	q := baseQuery("total_revenue")
	q.Dimensions = []string{"visit:source"}
	q.Filters = []Filter{purchaseFilter}
	q.Currency = "USD"

	got := map[string]float64{}
	for _, row := range run(t, engine, q).Results {
		got[row.Dimensions[0]] = row.Metrics[0]
	}

	closeTo(t, "direct revenue", got[""], 5000)
	closeTo(t, "Twitter revenue", got["Twitter"], 2200)
}

// TestRevenueInAnUnknownCurrencyIsReportedRatherThanDropped checks the promise
// that nothing goes missing quietly. Asked for a currency nothing converts
// into, the total is zero and the warning names how many events were left out
// and in which currencies.
func TestRevenueInAnUnknownCurrencyIsReportedRatherThanDropped(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writePurchases(t, account)

	q := baseQuery("total_revenue")
	q.Filters = []Filter{purchaseFilter}
	q.Currency = "GBP"

	result := run(t, engine, q)

	closeTo(t, "total_revenue", result.Results[0].Metrics[0], 0)

	warning, ok := result.Meta.MetricWarnings["total_revenue"]
	if !ok || warning.Code != WarnMissingRate {
		t.Fatalf("revenue with no usable rate must warn, got %+v", warning)
	}

	if !strings.Contains(warning.Warning, "2 events") {
		t.Errorf("the warning must say how much was left out, got %q", warning.Warning)
	}
}

// TestASingleCurrencyNeedsNoConfiguration checks the case that has to work out
// of the box: one currency in the data and no currency in the request.
func TestASingleCurrencyNeedsNoConfiguration(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writePurchases(t, account)

	q := baseQuery("total_revenue")
	q.Filters = []Filter{
		purchaseFilter,
		{Operator: OpIs, Dimension: "visit:source", Values: []string{""}},
	}

	row := run(t, engine, q).Results[0]

	closeTo(t, "total_revenue", row.Metrics[0], 5000)
}

// TestMixedCurrenciesWithoutAChoiceAreRefused checks the refusal. Picking one
// silently would either drop money or add two currencies together, and both of
// those end up in a board deck.
func TestMixedCurrenciesWithoutAChoiceAreRefused(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writePurchases(t, account)

	q := baseQuery("total_revenue")
	q.Filters = []Filter{purchaseFilter}

	_, err := engine.Run(context.Background(), q)
	if err == nil {
		t.Fatal("a report over two currencies with no reporting currency must be refused")
	}

	if !strings.Contains(err.Error(), "EUR") || !strings.Contains(err.Error(), "USD") {
		t.Errorf("the refusal must name the currencies it found, got %q", err)
	}
}

// TestACurrencyThatIsNotACodeIsRefused checks the validation. A typo would
// match no stored rate and report every revenue figure as zero.
func TestACurrencyThatIsNotACodeIsRefused(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("total_revenue")
	q.Currency = "dollars"

	if _, err := engine.Run(context.Background(), q); err == nil {
		t.Fatal("a currency that is not a three-letter code must be refused")
	}
}

// TestRefundsAreNotClampedAway checks that money is allowed to be negative. A
// month with more refunds than sales is a real month, and reporting it as zero
// would hide the one number somebody needed to see.
func TestRefundsAreNotClampedAway(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writePurchases(t, account)
	writePurchase(t, account, 22, 3, visitorA, at(30, 9, 5), -9000, "USD")
	writeRate(t, account, "EUR", "USD", 1.10)

	q := baseQuery("total_revenue")
	q.Filters = []Filter{purchaseFilter}
	q.Currency = "USD"

	// Fifty dollars, twenty-two dollars of euros, and a ninety dollar refund.
	closeTo(t, "total_revenue", run(t, engine, q).Results[0].Metrics[0], -1800)
}

// writePurchases adds the two purchases every test above counts: fifty dollars
// in a visit with no source, and twenty euros in one from Twitter.
func writePurchases(t *testing.T, account *accounts.Account) {
	t.Helper()

	writePurchase(t, account, 20, 3, visitorA, at(30, 9, 3), 5000, "USD")
	writePurchase(t, account, 21, 4, visitorC, at(30, 10, 1), 2000, "EUR")
}

// writePurchase writes one revenue event, hot row and cold row together.
func writePurchase(t *testing.T, account *accounts.Account, id, session, user, timestamp, amount int64, currency string) {
	t.Helper()

	ctx := context.Background()

	name, err := account.Intern.ID(ctx, intern.EventName, "Purchase")
	if err != nil {
		t.Fatal(err)
	}

	// The source is denormalised onto the event exactly as the ingest writer
	// does it, because that copy is what makes revenue attribution follow the
	// visit's first touch without a join.
	source := int64(0)
	if session == 4 {
		if source, err = account.Intern.ID(ctx, intern.Source, "Twitter"); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, source_id, scroll_depth, has_details)
		VALUES (?, 1, ?, ?, ?, ?, ?, 255, 1)`,
		id, timestamp, name, user, session, source); err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO event_details (event_id, revenue_amount, revenue_currency) VALUES (?, ?, ?)",
		id, amount, currency); err != nil {
		t.Fatal(err)
	}
}

// writeRate stores one exchange rate. The goals package owns the refresh
// policy; the compiler only ever reads the row.
func writeRate(t *testing.T, account *accounts.Account, base, quote string, rate float64) {
	t.Helper()

	if _, err := account.Writer().ExecContext(context.Background(),
		"INSERT INTO currency_rates (base, quote, rate, fetched_at) VALUES (?,?,?,0)",
		base, quote, rate); err != nil {
		t.Fatal(err)
	}
}
