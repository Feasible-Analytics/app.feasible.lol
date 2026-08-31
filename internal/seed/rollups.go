//
// rollups.go
// Where the roll-up rebuild goes once there are roll-up tables to rebuild.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// buildRollups rebuilds an account's pre-aggregated tables at the end of a run.
//
// It does nothing yet, and that is the point of it existing: the roll-up tables
// are a later milestone, and the first dashboard load against a freshly seeded
// database must not be the thing that builds them. A seed that leaves the
// aggregates empty measures the raw-scan path and reports it as the roll-up
// path, which is the wrong number by two orders of magnitude.
//
// When the roll-up schema lands, the rebuild for every seeded site goes here —
// one call, over the range the run just generated.
func buildRollups(_ context.Context, _ *accounts.Account) error {
	return nil
}
