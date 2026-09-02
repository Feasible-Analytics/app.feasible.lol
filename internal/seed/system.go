//
// control.go
// The accounts, people and sites a seeded dataset needs before it has an event.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// seededSite is one site after it has an id.
type seededSite struct {
	Fixture   siteFixture
	ID        int64
	AccountID int64
}

// seededAccount is one account after it has an id, with its sites.
type seededAccount struct {
	Fixture accountFixture
	ID      int64
	Sites   []*seededSite
}

// trafficSites returns the sites of an account that will be given events.
func (a *seededAccount) trafficSites() []*seededSite {
	var sites []*seededSite

	for _, site := range a.Sites {
		if site.Fixture.Traffic {
			sites = append(sites, site)
		}
	}

	return sites
}

// ensureFixture writes the accounts, people and sites into system.db and
// returns them with their ids. It is written to be re-runnable: every insert is
// keyed on something unique and reads the id back, so seeding twice into one
// data directory adds traffic to the sites that are already there instead of
// failing on a duplicate domain.
func ensureFixture(ctx context.Context, db *sql.DB, fixtures []accountFixture, start, now time.Time) ([]*seededAccount, error) {
	created := now.Unix()

	// One extra person, so the first account is a team rather than an
	// individual. Memberships, invitations and per-role permissions cannot be
	// built or demonstrated against a team of one.
	mateID, err := ensureUser(ctx, db, teamMateEmail, teamMateName, created)
	if err != nil {
		return nil, err
	}

	accounts := make([]*seededAccount, 0, len(fixtures))

	for _, item := range fixtures {
		ownerID, err := ensureUser(ctx, db, item.OwnerEmail, item.OwnerName, created)
		if err != nil {
			return nil, err
		}

		accountID, err := ensureTeam(ctx, db, item, start, now)
		if err != nil {
			return nil, err
		}

		if err := ensureMembership(ctx, db, accountID, ownerID, "owner", created); err != nil {
			return nil, err
		}

		// The second person is on the first account only. An account with one
		// member and an account with two are different pages, and both have to
		// exist for either to be checked.
		if item.State == stateActive {
			if err := ensureMembership(ctx, db, accountID, mateID, "editor", created); err != nil {
				return nil, err
			}
		}

		if err := ensureSubscription(ctx, db, accountID, item.State, now); err != nil {
			return nil, err
		}

		account := &seededAccount{Fixture: item, ID: accountID}

		for i, site := range item.Sites {
			siteID, err := ensureSite(ctx, db, accountID, site, i == 0, created)
			if err != nil {
				return nil, err
			}

			account.Sites = append(account.Sites, &seededSite{Fixture: site, ID: siteID, AccountID: accountID})
		}

		accounts = append(accounts, account)
	}

	return accounts, nil
}

// ensureUser creates a person if their address is not already there. The
// password hash is left empty: passwords, invitations and sign-in are a later
// milestone, and inventing a hash format here is how the seed and the real one
// end up disagreeing.
func ensureUser(ctx context.Context, db *sql.DB, email, name string, created int64) (int64, error) {
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (email, name, email_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(email) DO NOTHING`,
		email, name, created, created, created,
	); err != nil {
		return 0, fmt.Errorf("seed user %s: %w", email, err)
	}

	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM users WHERE email = ?", email).Scan(&id); err != nil {
		return 0, fmt.Errorf("seed user %s: %w", email, err)
	}

	return id, nil
}

// ensureTeam creates an account in the billing state its fixture asks for. The
// two columns that matter are the trial end and the ingestion deadline: between
// them they produce an account that is fine, one that has lost the dashboard,
// and one that has stopped being accepted at the front door.
func ensureTeam(ctx context.Context, db *sql.DB, item accountFixture, start, now time.Time) (int64, error) {
	trialEnds := start.Add(14 * 24 * time.Hour).Unix()

	// A NULL deadline means no limit. It is the normal state and it is why the
	// column is nullable rather than a far-future timestamp nobody would notice
	// was wrong.
	var acceptUntil any

	switch item.State {
	case stateLocked:
		// The subscription is gone but the events keep landing for a while.
		// Dropping a paying customer's traffic the instant a card fails loses
		// data they can never get back.
		acceptUntil = now.Add(7 * 24 * time.Hour).Unix()
	case stateDormant:
		// Past the grace period. Its events are dropped with a reason, which is
		// what makes the site look like one that stopped rather than one that
		// was never there.
		acceptUntil = now.Add(-10 * 24 * time.Hour).Unix()
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO teams (name, trial_ends_at, accept_traffic_until, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM teams WHERE name = ?)`,
		item.Name, trialEnds, acceptUntil, start.Unix(), now.Unix(), item.Name,
	); err != nil {
		return 0, fmt.Errorf("seed team %s: %w", item.Name, err)
	}

	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM teams WHERE name = ?", item.Name).Scan(&id); err != nil {
		return 0, fmt.Errorf("seed team %s: %w", item.Name, err)
	}

	// A re-run has to move the deadline forward, or an account seeded as
	// dormant last week would be dormant for a different stretch of the history
	// this week.
	if _, err := db.ExecContext(ctx, "UPDATE teams SET accept_traffic_until = ?, updated_at = ? WHERE id = ?", acceptUntil, now.Unix(), id); err != nil {
		return 0, fmt.Errorf("seed team %s: %w", item.Name, err)
	}

	return id, nil
}

// ensureMembership puts a person on an account in a role.
func ensureMembership(ctx context.Context, db *sql.DB, teamID, userID int64, role string, created int64) error {
	if _, err := db.ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(team_id, user_id) DO NOTHING`,
		teamID, userID, role, created,
	); err != nil {
		return fmt.Errorf("seed membership: %w", err)
	}

	return nil
}

// ensureSubscription mirrors the billing state onto the row the dashboard
// reads. The payment provider is the source of truth in production; here the
// seed is, and the point is that every status the dashboard branches on has an
// account in it.
func ensureSubscription(ctx context.Context, db *sql.DB, teamID int64, state accountState, now time.Time) error {
	status, plan := "active", "growth"
	periodEnd := now.Add(21 * 24 * time.Hour).Unix()

	switch state {
	case stateLocked:
		status, plan = "canceled", "growth"
		periodEnd = now.Add(-2 * 24 * time.Hour).Unix()
	case stateDormant:
		status, plan = "canceled", "starter"
		periodEnd = now.Add(-40 * 24 * time.Hour).Unix()
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (team_id, status, plan, current_period_end, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(team_id) DO UPDATE SET
			status = excluded.status,
			plan = excluded.plan,
			current_period_end = excluded.current_period_end,
			updated_at = excluded.updated_at`,
		teamID, status, plan, periodEnd, now.Unix(), now.Unix(),
	); err != nil {
		return fmt.Errorf("seed subscription: %w", err)
	}

	return nil
}

// ensureSite creates a site if its domain is not already registered. The first
// site of each account is published, because a public dashboard and a private
// one are different pages and the difference is one column.
func ensureSite(ctx context.Context, db *sql.DB, accountID int64, site siteFixture, public bool, created int64) (int64, error) {
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sites (account_id, domain, display_name, timezone, is_public, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain) DO NOTHING`,
		accountID, site.Domain, site.DisplayName, site.Timezone, boolToInt(public), created, created,
	); err != nil {
		return 0, fmt.Errorf("seed site %s: %w", site.Domain, err)
	}

	var id int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM sites WHERE domain = ?", site.Domain).Scan(&id); err != nil {
		return 0, fmt.Errorf("seed site %s: %w", site.Domain, err)
	}

	return id, nil
}

// setStatsStart records the first day a site has data. Date pickers read it so
// that nobody is offered a range that predates the site and comes back empty,
// and a seeded site with six weeks of history and no start date would offer
// every month back to 1970.
func setStatsStart(ctx context.Context, db *sql.DB, siteID int64, start time.Time) error {
	if _, err := db.ExecContext(ctx, "UPDATE sites SET stats_start_date = ?, updated_at = ? WHERE id = ?",
		start.Unix(), time.Now().Unix(), siteID,
	); err != nil {
		return fmt.Errorf("seed stats start: %w", err)
	}

	return nil
}

// boolToInt renders a Go bool as the integer SQLite stores.
func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
