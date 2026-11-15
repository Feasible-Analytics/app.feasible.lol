//
// traffic.go
// Whether an account has counted anything yet, read from its own database.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// AccountTraffic answers from the account database, where the sessions live.
type AccountTraffic struct {
	Accounts *accounts.Manager
}

// HasEvents reports whether any site on the account has recorded a session of
// its own. A session row exists from the first pageview, so one row is the
// whole answer. Imported history does not count: it says nothing about whether
// the snippet is installed.
func (t AccountTraffic) HasEvents(ctx context.Context, teamID int64) (bool, error) {
	lease, err := t.Accounts.AcquireForScan(ctx, teamID)
	if err != nil {
		return false, fmt.Errorf("onboarding: open account %d: %w", teamID, err)
	}
	defer lease.Release() //nolint:errcheck // the query result is more useful than an unlock error

	var found bool

	err = lease.Account.Reader().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE is_imported = 0)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("onboarding: read sessions %d: %w", teamID, err)
	}

	return found, nil
}
