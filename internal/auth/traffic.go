//
// traffic.go
// The two things the sites screens read out of an account's analytics database.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// Traffic reads the per-site numbers the sites list and the onboarding poll
// need. It is a thin type over the account manager rather than a package,
// because two queries do not justify one, and both of them are here so nothing
// else in this package has to know the analytics schema.
type Traffic struct {
	manager *accounts.Manager
}

// NewTraffic wraps the account manager.
func NewTraffic(manager *accounts.Manager) *Traffic {
	return &Traffic{manager: manager}
}

// Sparklines fills in the last SparklineDays of visits for every site in one
// account.
//
// It is one query for all of a team's sites rather than one per site: an agency
// with fifty sites would otherwise open fifty index scans to render one page,
// and they are the customer this screen exists for.
//
// Each site is bucketed in its own timezone, because a site's day is what every
// other number in the product is bucketed by, and a sparkline whose days do not
// line up with the dashboard's days is a chart that quietly disagrees with the
// page it links to.
func (t *Traffic) Sparklines(ctx context.Context, accountID int64, list []*Site, now time.Time) (err error) {
	if len(list) == 0 {
		return nil
	}

	lease, err := t.manager.Acquire(ctx, accountID)
	if err != nil {
		return fmt.Errorf("auth: sparklines: %w", err)
	}
	defer lease.Release() //nolint:errcheck // the query result is more useful than an unlock error
	account := lease.Account

	// The oldest instant any site could need. Sites in different timezones have
	// different local midnights, so the scan starts a day early and each site
	// buckets what it wants out of the same range.
	from := now.AddDate(0, 0, -(SparklineDays + 1)).Unix()

	byLocation := map[int64]*time.Location{}
	buckets := map[int64]map[int64]int64{}

	for _, site := range list {
		location, err := time.LoadLocation(site.Timezone)
		if err != nil {
			location = time.UTC
		}

		byLocation[site.ID] = location
		buckets[site.ID] = map[int64]int64{}
	}

	rows, err := account.Reader().QueryContext(ctx, `
		SELECT site_id, started_at FROM sessions WHERE started_at >= ?
	`, from)
	if err != nil {
		return fmt.Errorf("auth: sparklines: %w", err)
	}
	defer closeSQLRows(rows, &err, "sparklines")

	for rows.Next() {
		var siteID, startedAt int64

		if err := rows.Scan(&siteID, &startedAt); err != nil {
			return fmt.Errorf("auth: sparklines: %w", err)
		}

		location, ok := byLocation[siteID]
		if !ok {
			continue
		}

		day := time.Unix(startedAt, 0).In(location).Format("20060102")

		key, err := strconv.ParseInt(day, 10, 64)
		if err != nil {
			return fmt.Errorf("auth: sparklines: parse day %q: %w", day, err)
		}

		buckets[siteID][key]++
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("auth: sparklines: %w", err)
	}

	for _, site := range list {
		location := byLocation[site.ID]
		counts := buckets[site.ID]

		series := make([]int64, 0, SparklineDays)
		var total int64

		for offset := SparklineDays - 1; offset >= 0; offset-- {
			day := now.In(location).AddDate(0, 0, -offset).Format("20060102")

			key, err := strconv.ParseInt(day, 10, 64)
			if err != nil {
				return fmt.Errorf("auth: sparklines: parse day %q: %w", day, err)
			}

			series = append(series, counts[key])
			total += counts[key]
		}

		site.Sparkline = series
		site.Visitors = total
	}

	return nil
}

// SparklinesForSites fills a mixed-owner site list from each site's immutable
// storage account. Ownership transfers make the current team and analytics
// database different facts, so one team page may need more than one account.
func (t *Traffic) SparklinesForSites(ctx context.Context, list []*Site, now time.Time) error {
	byAccount := map[int64][]*Site{}
	for _, site := range list {
		byAccount[site.AccountID] = append(byAccount[site.AccountID], site)
	}

	for accountID, sites := range byAccount {
		if err := t.Sparklines(ctx, accountID, sites, now); err != nil {
			return err
		}
	}

	return nil
}

// SortByTraffic reorders a list by the sparkline total, busiest first, with
// pinned sites still held at the top. The sort happens here rather than in SQL
// because the counts live in a different database from the site rows.
func SortByTraffic(list []*Site) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Pinned() != list[j].Pinned() {
			return list[i].Pinned()
		}

		return list[i].Visitors > list[j].Visitors
	})
}

// FirstEventAt reports when a site last recorded anything, as unix seconds, or
// zero if it never has.
//
// This is the whole of the onboarding poll. It reads the session table rather
// than counting events because a session row exists from the first pageview and
// there is exactly one per visit, so the query touches one index and stops.
func (t *Traffic) FirstEventAt(ctx context.Context, accountID, siteID int64) (int64, error) {
	lease, err := t.manager.Acquire(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("auth: first event: %w", err)
	}
	defer lease.Release() //nolint:errcheck // the query result is more useful than an unlock error
	account := lease.Account

	var at sql.NullInt64

	err = account.Reader().QueryRowContext(ctx,
		"SELECT MIN(started_at) FROM sessions WHERE site_id = ?", siteID).Scan(&at)
	if err != nil {
		return 0, fmt.Errorf("auth: first event: %w", err)
	}

	return nullInt64(at), nil
}

// ResetStats deletes every recorded event for one site without deleting the
// site.
//
// It is scoped by site id in every statement. An account database holds every
// site a team owns, so a reset that dropped tables or cleared by anything other
// than the site id would take the customer's other sites with it — and there is
// no undo.
func (t *Traffic) ResetStats(ctx context.Context, accountID, siteID int64) error {
	lease, err := t.manager.Acquire(ctx, accountID)
	if err != nil {
		return fmt.Errorf("auth: reset stats: %w", err)
	}
	defer lease.Release() //nolint:errcheck // the transaction result is more useful than an unlock error
	account := lease.Account

	tx, err := account.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: reset stats: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	// event_details hangs off events and has no site column of its own, so it
	// is cleared through the events it belongs to, before those events go.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM event_details WHERE event_id IN (SELECT id FROM events WHERE site_id = ?)
	`, siteID); err != nil {
		return fmt.Errorf("auth: reset stats: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM events WHERE site_id = ?", siteID); err != nil {
		return fmt.Errorf("auth: reset stats: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE site_id = ?", siteID); err != nil {
		return fmt.Errorf("auth: reset stats: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: reset stats: %w", err)
	}

	return nil
}
