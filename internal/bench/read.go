//
// read.go
// The reports whose cost decides whether a dashboard needs summaries at all.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package bench

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// Dataset names one seeded site to measure against, and where its data lives.
type Dataset struct {
	DataDir   string
	AccountID int64
	SiteID    int64
	Domain    string
	Timezone  string
}

// OpenDataset finds the busiest site in a seeded data directory. Busiest rather
// than first, because a benchmark against the fixture's deliberately empty site
// would report that everything is instant.
func OpenDataset(ctx context.Context, dataDir string) (Dataset, error) {
	control, err := store.Open(filepath.Join(dataDir, "system.db"))
	if err != nil {
		return Dataset{}, err
	}
	defer control.Close()

	rows, err := control.QueryContext(ctx,
		"SELECT id, account_id, domain, COALESCE(timezone, 'UTC') FROM sites ORDER BY account_id, id")
	if err != nil {
		return Dataset{}, fmt.Errorf("bench: read sites: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []Dataset

	for rows.Next() {
		set := Dataset{DataDir: dataDir}
		if err := rows.Scan(&set.SiteID, &set.AccountID, &set.Domain, &set.Timezone); err != nil {
			return Dataset{}, fmt.Errorf("bench: read sites: %w", err)
		}

		candidates = append(candidates, set)
	}

	if err := rows.Err(); err != nil {
		return Dataset{}, fmt.Errorf("bench: read sites: %w", err)
	}

	if len(candidates) == 0 {
		return Dataset{}, fmt.Errorf("bench: %s holds no sites — run `make seed` against it first", dataDir)
	}

	return candidates[0], nil
}

// ReadCase is one report to time, and where it is allowed to read it from.
//
// The same report is run against both sources on purpose. The whole argument
// for keeping summary tables is the gap between the two numbers, and an
// estimate of that gap is not an argument.
type ReadCase struct {
	Name string

	// Rollups lets the query use the summary tables. False pins it to the raw
	// events and sessions, which is what a self-hoster who has never run the
	// worker is reading.
	Rollups bool

	Query query.Query
}

// ReadCases is the set of reports the plan's estimates were written about:
// the busiest table in the product over the two ranges that bracket it, the
// same table filtered, and today.
func ReadCases(set Dataset) []ReadCase {
	topPages := func(preset string, filters []query.Filter) query.Query {
		return query.Query{
			SiteIDs:    []int64{set.SiteID},
			Metrics:    []string{"visitors", "pageviews", "bounce_rate", "visit_duration"},
			Dimensions: []string{"event:page"},
			DateRange:  query.DateRange{Preset: preset},
			Filters:    filters,
			Timezone:   set.Timezone,
			OrderBy:    []query.Order{{Key: "visitors", Descending: true}},
			Pagination: query.Pagination{Limit: 100},
			Exact:      true,
		}
	}

	byCountry := []query.Filter{{
		Operator:  query.OpIs,
		Dimension: "visit:country",
		Values:    []string{"US"},
	}}

	return []ReadCase{
		{Name: "top_pages_28d_raw", Query: topPages(query.RangeLast28Days, nil)},
		{Name: "top_pages_12mo_raw", Query: topPages(query.RangeLast12Months, nil)},
		{Name: "top_pages_28d_rollups", Rollups: true, Query: topPages(query.RangeLast28Days, nil)},
		{Name: "top_pages_12mo_rollups", Rollups: true, Query: topPages(query.RangeLast12Months, nil)},
		{Name: "top_pages_28d_country_raw", Query: topPages(query.RangeLast28Days, byCountry)},
		{Name: "today_raw", Query: topPages(query.RangeDay, nil)},
	}
}

// RunRead answers one report and says how long it took and how many groups came
// back. The row count is reported because a query that got faster by returning
// nothing is not a query that got faster.
func RunRead(ctx context.Context, db *sql.DB, c ReadCase) (time.Duration, int, error) {
	engine := query.New(db)
	if !c.Rollups {
		engine.Router = query.RawRouter{}
	}

	// The benchmark measures the cost of the whole scan, so it must not be
	// sampled: a twelve-month case answered from a tenth of the visitors would
	// report a tenth of the time and look like a win nobody made.
	c.Query.Exact = true

	started := time.Now()

	result, err := engine.Run(ctx, c.Query)
	if err != nil {
		return 0, 0, fmt.Errorf("bench: %s: %w", c.Name, err)
	}

	return time.Since(started), len(result.Results), nil
}
