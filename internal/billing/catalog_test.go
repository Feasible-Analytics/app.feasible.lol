//
// catalog_test.go
// The catalogue is the one source every printed price reads from.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import "testing"

// TestDescribeResolvesConfiguredPricesAndAdmitsCustomOnes pins the mapping the
// billing screen and the mirror rely on: the two configured ids describe the
// catalogue, an empty id is no plan, and anything else is called custom rather
// than mislabelled with a price the customer is not paying.
func TestDescribeResolvesConfiguredPricesAndAdmitsCustomOnes(t *testing.T) {
	plans := Plans{Product: "prod_1", Monthly: "price_m", Yearly: "price_y"}

	monthly := plans.Describe("price_m")
	if monthly.Key != "monthly" || monthly.Label != "$9.99 / month" || monthly.Amount != 999 || monthly.Interval != "month" {
		t.Fatalf("monthly described as %+v", monthly)
	}

	yearly := plans.Describe("price_y")
	if yearly.Key != "yearly" || yearly.Label != "$100 / year" || yearly.Amount != 10000 || yearly.Interval != "year" {
		t.Fatalf("yearly described as %+v", yearly)
	}

	if none := plans.Describe(""); none != (Plan{}) {
		t.Fatalf("no price described as %+v", none)
	}

	custom := plans.Describe("price_other")
	if custom.Key != "custom" || custom.Label != "Custom plan" || custom.Amount != 0 {
		t.Fatalf("unknown price described as %+v", custom)
	}
}
