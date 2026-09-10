//
// report.go
// Reading imported Search Console rows back out for the dashboard.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The dimensions a search report can group by. They are the four Search Console
// itself returns, and the allow-list is the only thing between a caller-supplied
// string and a column name in the SQL below.
const (
	DimensionQuery   = "query"
	DimensionPage    = "page"
	DimensionCountry = "country"
	DimensionDevice  = "device"
)

// searchColumns maps a dimension to its column. A dimension that is not a key
// here is refused rather than defaulted, because defaulting would answer a
// question nobody asked with numbers that look right.
var searchColumns = map[string]string{
	DimensionQuery:   "query",
	DimensionPage:    "page",
	DimensionCountry: "country",
	DimensionDevice:  "device",
}

// DefaultSearchLimit is how many rows a report returns. It matches what the
// card and its drawer can show without turning a report into a data dump.
const DefaultSearchLimit = 100

// MaxSearchLimit bounds what a caller may ask for.
const MaxSearchLimit = 1000

// SearchReportRequest is one grouped read of the stored Search Console rows.
type SearchReportRequest struct {
	SiteID    int64
	Dimension string

	// Start is inclusive and End exclusive, both as the site-local midnights
	// the rows are stamped with.
	Start time.Time
	End   time.Time

	// Search narrows to rows whose value contains this text, which is what
	// makes a keyword list usable once a site has thousands of them.
	Search string

	Limit int

	// Location is the site's timezone, which the stored days were stamped in.
	Location *time.Location
}

// SearchRow is one grouped row, with the two figures Google measures and the
// two derived from them.
type SearchRow struct {
	Value       string `json:"value"`
	Clicks      int64  `json:"clicks"`
	Impressions int64  `json:"impressions"`

	// CTR is a fraction, not a percentage: formatting a ratio is the reader's
	// side of the wire, and a number that is 4.2 in one field and 0.042 in the
	// next is how a dashboard ends up off by a hundred.
	CTR float64 `json:"ctr"`

	// Position is the impression-weighted average rank. It is weighted rather
	// than a mean of daily averages because a day with ten impressions and a
	// day with ten thousand are not worth the same.
	Position float64 `json:"position"`
}

// SearchReport is what the dashboard card draws.
type SearchReport struct {
	// Status tells the card which of three screens to draw without a second
	// request: the feature is unavailable on this install, the site has not
	// connected it, or here are the numbers.
	Status string `json:"status"`

	Dimension string      `json:"dimension"`
	Rows      []SearchRow `json:"rows"`
	Totals    SearchRow   `json:"totals"`

	// Property is the Search Console property these figures came from, shown
	// because a site with both a domain property and a URL prefix will get
	// different numbers depending on which one somebody chose.
	Property string `json:"property,omitempty"`

	// UpdatedThrough is the last day with any stored row, as an ISO date. It is
	// what lets the card say why the last day or two of a range look empty
	// instead of leaving the reader to conclude their traffic collapsed.
	UpdatedThrough string `json:"updated_through,omitempty"`
}

// Report statuses.
const (
	// StatusUnavailable means this install has no Google application
	// configured, so the feature can never work here and the card hides itself.
	StatusUnavailable = "unavailable"

	// StatusNotConnected means the feature works but this site has not
	// authorised it.
	StatusNotConnected = "not_connected"

	// StatusNoProperty means the grant exists but nobody has chosen which
	// Search Console property it reads.
	StatusNoProperty = "no_property"

	// StatusReady means the rows are real.
	StatusReady = "ready"
)

// SearchReportRun groups the stored rows one way and totals them.
//
// The totals are the sum of the rows we hold, not a figure Google published
// separately. Google withholds queries that too few people searched, so a
// keyword list always adds up to less than the site's real click count — and
// showing a Google-published total beside rows that cannot reach it produces a
// gap every customer reads as lost data.
func SearchReportRun(ctx context.Context, db *sql.DB, request SearchReportRequest) (*SearchReport, error) {
	column, ok := searchColumns[request.Dimension]
	if !ok {
		return nil, fmt.Errorf("google: %q is not a search dimension — use query, page, country or device", request.Dimension)
	}

	limit := request.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	report := &SearchReport{Status: StatusReady, Dimension: request.Dimension, Rows: []SearchRow{}}

	arguments := []any{request.SiteID, request.Start.Unix(), request.End.Unix()}
	where := "site_id = ? AND timestamp >= ? AND timestamp < ?"

	if search := strings.TrimSpace(request.Search); search != "" {
		where += " AND " + column + " LIKE ? ESCAPE '\\'"
		arguments = append(arguments, "%"+escapeLike(search)+"%")
	}

	rows, err := db.QueryContext(ctx, `
		SELECT `+column+`,
		       SUM(clicks), SUM(impressions), SUM(position_x1000_total)
		FROM search_console_daily
		WHERE `+where+`
		GROUP BY `+column+`
		ORDER BY SUM(clicks) DESC, SUM(impressions) DESC
		LIMIT ?`, append(arguments, limit)...)
	if err != nil {
		return nil, fmt.Errorf("google: read search report: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			value                        string
			clicks, impressions, ranking int64
		)

		if err := rows.Scan(&value, &clicks, &impressions, &ranking); err != nil {
			return nil, fmt.Errorf("google: read search report: %w", err)
		}

		report.Rows = append(report.Rows, SearchRow{
			Value:       presentValue(request.Dimension, value),
			Clicks:      clicks,
			Impressions: impressions,
			CTR:         ratio(clicks, impressions),
			Position:    averagePosition(ranking, impressions),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("google: read search report: %w", err)
	}

	// Totalled over the same window rather than over the returned rows: a
	// hundred-row page of a thousand-keyword site would otherwise report a
	// total that shrinks as the reader pages through it.
	if err := totalSearch(ctx, db, where, arguments, report); err != nil {
		return nil, err
	}

	through, err := LatestSearchDay(ctx, db, request.SiteID, request.Location)
	if err != nil {
		return nil, err
	}
	report.UpdatedThrough = through

	return report, nil
}

// totalSearch fills in the whole-window figures.
func totalSearch(ctx context.Context, db *sql.DB, where string, arguments []any, report *SearchReport) error {
	var clicks, impressions, ranking sql.NullInt64

	err := db.QueryRowContext(ctx, `
		SELECT SUM(clicks), SUM(impressions), SUM(position_x1000_total)
		FROM search_console_daily WHERE `+where, arguments...).
		Scan(&clicks, &impressions, &ranking)
	if err != nil {
		return fmt.Errorf("google: total search report: %w", err)
	}

	report.Totals = SearchRow{
		Clicks:      clicks.Int64,
		Impressions: impressions.Int64,
		CTR:         ratio(clicks.Int64, impressions.Int64),
		Position:    averagePosition(ranking.Int64, impressions.Int64),
	}

	return nil
}

// LatestSearchDay is the most recent day the site holds any search row for, as
// an ISO date, or empty when it holds none.
//
// The location matters. Rows are stamped with the site's own local midnight, so
// reading one back in UTC names the day before for every site east of Greenwich
// — and the card would then tell a customer in Tokyo that Google is a day
// further behind than it is.
func LatestSearchDay(ctx context.Context, db *sql.DB, siteID int64, location *time.Location) (string, error) {
	var latest sql.NullInt64

	err := db.QueryRowContext(ctx,
		"SELECT MAX(timestamp) FROM search_console_daily WHERE site_id = ?", siteID).Scan(&latest)
	if err != nil {
		return "", fmt.Errorf("google: latest search day: %w", err)
	}

	if !latest.Valid {
		return "", nil
	}

	if location == nil {
		location = time.UTC
	}

	return time.Unix(latest.Int64, 0).In(location).Format("2006-01-02"), nil
}

// EarliestSearchDay is the first day the site holds any search row for. It is
// what an "all time" range resolves against, so the range covers the imported
// history rather than only the days since the site started sending events.
func EarliestSearchDay(ctx context.Context, db *sql.DB, siteID int64) (time.Time, error) {
	var earliest sql.NullInt64

	err := db.QueryRowContext(ctx,
		"SELECT MIN(timestamp) FROM search_console_daily WHERE site_id = ?", siteID).Scan(&earliest)
	if err != nil {
		return time.Time{}, fmt.Errorf("google: earliest search day: %w", err)
	}

	if !earliest.Valid {
		return time.Time{}, nil
	}

	return time.Unix(earliest.Int64, 0).UTC(), nil
}

// ratio divides safely. A dimension value with impressions but no clicks is
// ordinary; one with neither is a row that should not exist, and either way
// zero is the honest answer rather than a NaN the front end has to guard.
func ratio(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}

	return float64(part) / float64(whole)
}

// averagePosition undoes the impression weighting the rows are stored with.
func averagePosition(weighted, impressions int64) float64 {
	if impressions <= 0 {
		return 0
	}

	return float64(weighted) / float64(impressions) / 1000
}

// presentValue rewrites a stored value into the vocabulary the rest of the
// dashboard already uses, so a device row on this card and a device row on the
// Devices card are the same word and the same flag.
func presentValue(dimension, value string) string {
	switch dimension {
	case DimensionDevice:
		return deviceName(value)
	case DimensionCountry:
		return CountryAlpha2(value)
	default:
		return value
	}
}

// deviceName turns Google's shouted category into ours.
func deviceName(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DESKTOP":
		return "Desktop"
	case "MOBILE":
		return "Mobile"
	case "TABLET":
		return "Tablet"
	default:
		return value
	}
}

// escapeLike neutralises the wildcards in a reader's search text, so somebody
// looking for a literal underscore does not silently match every character.
func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}
