//
// catalog.go
// The two prices this product sells, in one place.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

// PlanPrice is what one of the two plans costs and how that is shown to a
// customer. Preflight checks the configured Stripe prices against these amounts
// and the billing screen labels a subscription from them, so the check and the
// copy cannot disagree about what an account is paying.
type PlanPrice struct {
	// Key is the plan name used in URLs, the mirror and the claim table.
	Key string

	// Interval is Stripe's recurring interval the configured price must have.
	Interval string

	// Amount is the price in minor units, before tax, that the configured
	// Stripe price must charge.
	Amount int64

	// Price is the amount as customers read it.
	Price string
}

// Label renders the plan for a panel: "$9.99 / month".
func (p PlanPrice) Label() string {
	return p.Price + " / " + p.Interval
}

// Monthly and Yearly are the catalogue. The yearly plan is priced as ten
// months, which is the saving the pricing page advertises.
var (
	Monthly = PlanPrice{Key: "monthly", Interval: "month", Amount: 999, Price: "$9.99"}
	Yearly  = PlanPrice{Key: "yearly", Interval: "year", Amount: 10000, Price: "$100"}
)

// Plan is a price id resolved against the catalogue, as a page or a mirror
// row wants to show it.
type Plan struct {
	Key      string
	Label    string
	PriceID  string
	Amount   int64
	Interval string
}

// Describe turns a price id read back from the provider into a plan without a
// second API call on every page render. An id that matches neither configured
// price — somebody moved the subscription to a custom price in the provider's
// dashboard — is described honestly as custom rather than guessed at.
func (p Plans) Describe(priceID string) Plan {
	switch priceID {
	case "":
		return Plan{}
	case p.Monthly:
		return Plan{Key: Monthly.Key, Label: Monthly.Label(), PriceID: priceID, Amount: Monthly.Amount, Interval: Monthly.Interval}
	case p.Yearly:
		return Plan{Key: Yearly.Key, Label: Yearly.Label(), PriceID: priceID, Amount: Yearly.Amount, Interval: Yearly.Interval}
	default:
		return Plan{Key: "custom", Label: "Custom plan", PriceID: priceID}
	}
}
