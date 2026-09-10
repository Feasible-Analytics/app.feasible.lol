//
// importer.go
// Walking GA4 and Search Console a day at a time, resumably.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// reportShape is one GA4 report: the dimensions to ask Google for, and the
// query dimensions they land in. Several shapes are requested rather than one
// wide report because the Reporting API caps how many dimensions a single query
// may carry — which is the constraint that pushed the incumbent into storing
// per-dimension marginals in the first place.
//
// The difference is what happens next. Each shape is written as its own set of
// roll-up rows carrying a record of exactly which dimensions it holds, so a
// filtered query reads the shape that can answer it and reports the others as a
// labelled gap. Marginals have no such record, which is why a filter zeroes
// them out.
type reportShape struct {
	// Name is what a progress line calls this pass.
	Name string

	// GA4 is the dimension list sent to Google, in order.
	GA4 []string

	// Ours is the query dimension each of those maps to, in the same order.
	Ours []string
}

// ga4Shapes are the reports an import runs, one per sheet the CSV import
// understands, so history brought in from Google and history brought in from a
// CSV are the same rows in the same table.
var ga4Shapes = []reportShape{
	{Name: "totals"},
	{Name: "pages", GA4: []string{"hostName", "pagePath"}, Ours: []string{"event:hostname", "event:page"}},
	{Name: "sources", GA4: []string{"sessionSource", "sessionMedium", "sessionCampaignName", "sessionDefaultChannelGroup"}, Ours: []string{"visit:source", "visit:utm_medium", "visit:utm_campaign", "visit:channel"}},
	{Name: "locations", GA4: []string{"countryId", "region", "city"}, Ours: []string{"visit:country", "visit:region", "visit:city"}},
	{Name: "devices", GA4: []string{"deviceCategory"}, Ours: []string{"visit:device"}},
	{Name: "browsers", GA4: []string{"browser"}, Ours: []string{"visit:browser"}},
	{Name: "operating_systems", GA4: []string{"operatingSystem"}, Ours: []string{"visit:os"}},
	{Name: "landing_pages", GA4: []string{"landingPage"}, Ours: []string{"visit:entry_page"}},
}

// ga4Metrics are the figures every shape asks for, in bind order.
var ga4Metrics = []string{"totalUsers", "sessions", "screenPageViews", "engagedSessions", "userEngagementDuration"}

// GA4Import runs one property's history into imported roll-up rows.
//
// Each attempt replaces its own rows before replaying the requested window.
// This matches StartImport and prevents interrupted imports from losing or
// duplicating previously written months.
func (a *App) GA4Import(ctx context.Context, db *sql.DB, cache *intern.Cache, record *dataio.Import,
	connection *Connection, from, to time.Time, location *time.Location, now func() time.Time) error {

	if connection.Property == "" || from.After(to) {
		return errors.New("google: choose a GA4 property and a valid date range")
	}
	months := monthsBetween(from, to)

	if err := dataio.StartImport(ctx, db, record.ID, len(months), now()); err != nil {
		return err
	}

	covered := map[string]bool{}
	var rowsWritten int64

	for index, month := range months {
		for _, shape := range ga4Shapes {
			written, err := a.importShape(ctx, db, cache, record, connection, shape, month, location, now)
			if err != nil {
				return err
			}

			rowsWritten += written

			for _, name := range shape.Ours {
				covered[name] = true
			}
		}

		if err := dataio.SetProgress(ctx, db, record.ID, index+1, rowsWritten, month.label); err != nil {
			return err
		}
	}

	names := make([]string, 0, len(covered))
	for name := range covered {
		names = append(names, name)
	}

	sort.Strings(names)

	// Before the import is marked complete, so a reader never sees a finished
	// import whose wide summaries are still being written.
	if err := dataio.SummariseImport(ctx, db, record.ID, record.SiteID, location); err != nil {
		return err
	}

	return dataio.CompleteImport(ctx, db, record.ID, names, from.Unix(), to.AddDate(0, 0, 1).Unix()-1, rowsWritten, now())
}

// importShape runs one report for one month and writes its rows.
func (a *App) importShape(ctx context.Context, db *sql.DB, cache *intern.Cache, record *dataio.Import,
	connection *Connection, shape reportShape, month monthRange, location *time.Location, now func() time.Time) (int64, error) {

	writer, err := dataio.NewWriter(db, cache, record.ID, record.SiteID, shape.Ours)
	if err != nil {
		return 0, err
	}

	offset := 0
	for {
		report, err := a.runReportPage(ctx, db, connection, shape, month, now(), offset)
		if err != nil {
			return 0, err
		}

		for _, row := range report.Rows {
			// The first dimension is always the date, so a month is one request
			// rather than thirty.
			if len(row.DimensionValues) != 1+len(shape.Ours) || len(row.MetricValues) != len(ga4Metrics) {
				return 0, fmt.Errorf("google: malformed %s report row", shape.Name)
			}

			timestamp, err := parseGA4Date(row.DimensionValues[0].Value, location)
			if err != nil {
				return 0, fmt.Errorf("the %s report returned %q where a date was expected", shape.Name, row.DimensionValues[0].Value)
			}

			parsed := dataio.Row{
				Timestamp:  timestamp,
				Dimensions: map[string]string{},
				Metrics:    map[string]int64{},
			}

			for i, name := range shape.Ours {
				parsed.Dimensions[name] = row.DimensionValues[i+1].Value
			}

			for i, name := range ga4Metrics {
				value, err := strconv.ParseFloat(row.MetricValues[i].Value, 64)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= float64(math.MaxInt64)/1000 {
					return 0, fmt.Errorf("google: invalid %s metric in %s report", name, shape.Name)
				}

				switch name {
				case "totalUsers":
					parsed.Metrics[dataio.FieldVisitors] = int64(value)
				case "sessions":
					parsed.Metrics[dataio.FieldVisits] = int64(value)
				case "screenPageViews":
					parsed.Metrics[dataio.FieldPageviews] = int64(value)
				case "engagedSessions":
					parsed.Metrics[dataio.FieldBounces] = max(0, parsed.Metrics[dataio.FieldVisits]-int64(value))
					parsed.Metrics[dataio.FieldEngagementVisits] = int64(value)
				case "userEngagementDuration":
					// GA4 reports engagement in seconds; our duration column is
					// seconds and our engagement column is milliseconds.
					parsed.Metrics[dataio.FieldDuration] = int64(value)
					parsed.Metrics[dataio.FieldEngagement] = int64(value * 1000)
				}
			}

			if err := writer.Add(ctx, parsed); err != nil {
				return 0, err
			}
		}

		if err := writer.Flush(ctx); err != nil {
			return 0, err
		}

		offset += len(report.Rows)
		if offset >= report.RowCount {
			break
		}
		if len(report.Rows) == 0 {
			return 0, errors.New("google: incomplete GA4 report pagination")
		}
	}
	return writer.Written(), nil
}

// ga4Row is one row of a report response.
type ga4Row struct {
	DimensionValues []struct {
		Value string `json:"value"`
	} `json:"dimensionValues"`
	MetricValues []struct {
		Value string `json:"value"`
	} `json:"metricValues"`
}

// ga4Report is the response shape.
type ga4Report struct {
	Rows     []ga4Row `json:"rows"`
	RowCount int      `json:"rowCount"`
	Error    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// ga4Order fixes pagination order across rows with equal dates by including every
// requested dimension, preventing unstable page boundaries from losing rows.
func ga4Order(dimensions []map[string]string) []map[string]any {
	orders := make([]map[string]any, 0, len(dimensions))
	for _, dimension := range dimensions {
		orders = append(orders, map[string]any{"dimension": map[string]string{"dimensionName": dimension["name"]}})
	}
	return orders
}

// runReportPage calls the Data API for a bounded page of one monthly report.
func (a *App) runReportPage(ctx context.Context, db *sql.DB, connection *Connection, shape reportShape, month monthRange, now time.Time, offset int) (report *ga4Report, err error) {
	token, err := a.AccessToken(ctx, db, connection, now)
	if err != nil {
		return nil, err
	}

	dimensions := make([]map[string]string, 0, 1+len(shape.GA4))
	dimensions = append(dimensions, map[string]string{"name": "date"})
	for _, name := range shape.GA4 {
		dimensions = append(dimensions, map[string]string{"name": name})
	}

	metrics := make([]map[string]string, 0, len(ga4Metrics))
	for _, name := range ga4Metrics {
		metrics = append(metrics, map[string]string{"name": name})
	}

	body, err := json.Marshal(map[string]any{
		"dateRanges": []map[string]string{{"startDate": month.start, "endDate": month.end}},
		"dimensions": dimensions,
		"metrics":    metrics,
		"limit":      "100000",
		"offset":     strconv.Itoa(offset),
		"orderBys":   ga4Order(dimensions),
	})
	if err != nil {
		return nil, fmt.Errorf("google: build report request: %w", err)
	}

	endpoint := AnalyticsAPI + "/v1beta/properties/" + escapePathSegment(connection.Property) + ":runReport"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("google: build report request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	response, err := a.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("google: the %s report could not be fetched: %w", shape.Name, err)
	}
	defer closeResource(response.Body, &err, shape.Name+" report response")

	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("google: reading the %s report: %w", shape.Name, err)
	}

	var found ga4Report
	if err := json.Unmarshal(raw, &found); err != nil {
		return nil, fmt.Errorf("google: the %s report was not JSON: %w", shape.Name, err)
	}

	if found.Error != nil {
		if found.Error.Code == http.StatusUnauthorized || found.Error.Code == http.StatusForbidden {
			if markErr := MarkNeedsReconnect(ctx, db, connection.SiteID, connection.Provider,
				"Google refused the request for this property — reconnect the account and check it can read the property", now); markErr != nil {
				return nil, markErr
			}

			return nil, errors.New("google refused the request for this property — reconnect the account")
		}

		return nil, fmt.Errorf("google answered: %s", found.Error.Message)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google: the %s report answered %d", shape.Name, response.StatusCode)
	}

	return &found, nil
}

// SearchConsoleImport reads search queries into their own table.
//
// It is a separate table from the roll-ups because a search query is not one of
// our dimensions and never will be: it is Google's data about Google's index,
// not a property of a visit anything here measured.
func (a *App) SearchConsoleImport(ctx context.Context, db *sql.DB, record *dataio.Import,
	connection *Connection, from, to time.Time, location *time.Location, now func() time.Time) error {

	if strings.TrimSpace(connection.Property) == "" {
		return errors.New("google: this site has no Search Console property chosen yet — pick one on the imports screen")
	}

	days := daysBetween(from, to)

	if err := dataio.StartImport(ctx, db, record.ID, len(days), now()); err != nil {
		return err
	}

	var rowsWritten int64

	for index, day := range days {
		if record.Cursor != "" && day <= record.Cursor {
			continue
		}

		written, err := a.importSearchDay(ctx, db, record.SiteID, connection, day, location, now())
		if err != nil {
			return err
		}

		rowsWritten += written

		if err := dataio.SetProgress(ctx, db, record.ID, index+1, rowsWritten, day); err != nil {
			return err
		}
	}

	// Search Console rows live in their own table and contribute nothing to a
	// report over imported_rollups, so there is nothing to sum. It is still
	// marked summarised: an import left unmarked reads as one whose summaries
	// are missing, and turns them off for every other import on the site.
	if err := dataio.MarkNothingToSummarise(ctx, db, record.ID, location); err != nil {
		return err
	}

	return dataio.CompleteImport(ctx, db, record.ID, nil, from.Unix(), to.Unix(), rowsWritten, now())
}

// SearchConsoleRefresh re-reads a short recent window without creating an
// import the customer has to look at.
//
// It is separate from SearchConsoleImport because the two are different
// promises. A backfill is a one-off the customer started and watches finish, so
// it earns a row on the imports screen. The nightly top-up is maintenance: a
// row a night would bury the customer's own imports under a year of noise
// within a year, and none of those rows would ever be read.
func (a *App) SearchConsoleRefresh(ctx context.Context, db *sql.DB, connection *Connection,
	from, to time.Time, location *time.Location, now time.Time) (int64, error) {

	if strings.TrimSpace(connection.Property) == "" {
		return 0, errors.New("google: this site has no Search Console property chosen yet — pick one on the imports screen")
	}

	var written int64

	for _, day := range daysBetween(from, to) {
		rows, err := a.importSearchDay(ctx, db, connection.SiteID, connection, day, location, now)
		if err != nil {
			return written, err
		}

		written += rows
	}

	return written, nil
}

// searchResponse is the Search Analytics response shape.
type searchResponse struct {
	Rows []struct {
		Keys        []string `json:"keys"`
		Clicks      float64  `json:"clicks"`
		Impressions float64  `json:"impressions"`
		Position    float64  `json:"position"`
	} `json:"rows"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// importSearchDay reads one day of search performance.
func (a *App) importSearchDay(ctx context.Context, db *sql.DB, siteID int64, connection *Connection, day string, location *time.Location, now time.Time) (written int64, err error) {
	token, err := a.AccessToken(ctx, db, connection, now)
	if err != nil {
		return 0, err
	}

	body, err := json.Marshal(map[string]any{
		"startDate":  day,
		"endDate":    day,
		"dimensions": []string{"query", "page", "country", "device"},
		"rowLimit":   25000,
	})
	if err != nil {
		return 0, fmt.Errorf("google: build search request: %w", err)
	}

	endpoint := SearchAPI + "/webmasters/v3/sites/" + escapePathSegment(connection.Property) + "/searchAnalytics/query"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("google: build search request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	response, err := a.client().Do(request)
	if err != nil {
		return 0, fmt.Errorf("google: Search Console could not be reached: %w", err)
	}
	defer closeResource(response.Body, &err, "Search Console response")

	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return 0, fmt.Errorf("google: reading Search Console: %w", err)
	}

	var parsed searchResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("google: Search Console did not answer with JSON: %w", err)
	}

	if parsed.Error != nil {
		return 0, fmt.Errorf("google answered: %s", parsed.Error.Message)
	}

	timestamp, err := parseISODate(day, location)
	if err != nil {
		return 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("google: write search rows: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	// The upsert is what makes a re-run of the same day idempotent. Search
	// Console revises its own numbers for a few days after the fact, so a day
	// is fetched more than once by design and must not accumulate.
	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO search_console_daily
			(site_id, timestamp, query, page, country, device, clicks, impressions, position_x1000_total)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(site_id, timestamp, query, page, country, device) DO UPDATE SET
			clicks = excluded.clicks,
			impressions = excluded.impressions,
			position_x1000_total = excluded.position_x1000_total`)
	if err != nil {
		return 0, fmt.Errorf("google: write search rows: %w", err)
	}
	defer closeResource(statement, &err, "Search Console statement")

	for _, row := range parsed.Rows {
		keys := make([]string, 4)
		copy(keys, row.Keys)

		// Position is stored multiplied by a thousand and summed against
		// impressions, so two days can be added together without averaging two
		// averages — which is wrong the moment the days have different volumes.
		position := int64(row.Position * 1000 * row.Impressions)

		if _, err := statement.ExecContext(ctx, siteID, timestamp, keys[0], keys[1], keys[2], keys[3],
			int64(row.Clicks), int64(row.Impressions), position); err != nil {
			return 0, fmt.Errorf("google: write search rows: %w", err)
		}

		written++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("google: write search rows: %w", err)
	}

	return written, nil
}

// monthRange is one calendar month of an import, with the labels the API and
// the cursor use.
type monthRange struct {
	label string
	start string
	end   string
}

// monthsBetween splits a range into whole calendar months. A month is the unit
// the cursor advances by: small enough that a failure loses little work, large
// enough that a year is twelve requests per report shape rather than 365.
func monthsBetween(from, to time.Time) []monthRange {
	var months []monthRange

	cursor := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC)

	for !cursor.After(last) {
		next := cursor.AddDate(0, 1, 0)
		end := next.AddDate(0, 0, -1)

		if end.After(to) {
			end = to
		}

		start := cursor
		if start.Before(from) {
			start = from
		}

		months = append(months, monthRange{
			label: cursor.Format("2006-01"),
			start: start.Format("2006-01-02"),
			end:   end.Format("2006-01-02"),
		})

		cursor = next
	}

	return months
}

// daysBetween lists the days in a range as ISO dates.
func daysBetween(from, to time.Time) []string {
	var days []string

	cursor := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	last := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)

	for !cursor.After(last) {
		days = append(days, cursor.Format("2006-01-02"))
		cursor = cursor.AddDate(0, 0, 1)
	}

	return days
}

// parseGA4Date reads the YYYYMMDD form the Data API returns.
func parseGA4Date(value string, location *time.Location) (int64, error) {
	if location == nil {
		location = time.UTC
	}

	parsed, err := time.ParseInLocation("20060102", strings.TrimSpace(value), location)
	if err != nil {
		return 0, err
	}

	return parsed.Unix(), nil
}

// parseISODate reads a YYYY-MM-DD date at the site's local midnight.
func parseISODate(value string, location *time.Location) (int64, error) {
	if location == nil {
		location = time.UTC
	}

	parsed, err := time.ParseInLocation("2006-01-02", value, location)
	if err != nil {
		return 0, err
	}

	return parsed.Unix(), nil
}

// escapePathSegment escapes a path segment. A Search Console property is a URL
// with slashes and a colon in it, and pasting one into a path unescaped points
// the request at an endpoint that does not exist.
func escapePathSegment(value string) string {
	return strings.NewReplacer(
		"%", "%25",
		":", "%3A",
		"/", "%2F",
		"?", "%3F",
		"#", "%23",
	).Replace(value)
}
