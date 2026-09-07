//
// wide.go
// Week and month summaries of an import's daily rows.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// wideKeys are the columns a summary row is grouped by: everything that says
// which shape and which values a row is about, rather than how much of it there
// was.
var wideKeys = []string{
	"covered",
	"name_id", "hostname_id", "pathname_id", "entry_page_id", "exit_page_id", "page_title_id",
	"referrer_id", "source_id", "channel_id",
	"utm_source_id", "utm_medium_id", "utm_campaign_id",
	"country_id", "region_id", "city_id",
	"device_type_id", "screen_size_id",
	"browser_id", "browser_version_id", "os_id", "os_version_id", "language_id",
	"property_key", "property_value",
}

// wideMetrics are the columns a summary row adds up.
//
// Every one of them is a total rather than an average, which is what makes a
// wider bucket a plain sum. There is no carried-visitor correction: a daily
// visitor count is all an import supplies, and nobody who appeared on two of
// its days can be un-counted from data we were never given.
var wideMetrics = []string{
	"visitors", "visits", "pageviews", "events", "exits", "bounces",
	"duration_total", "engagement_total", "engagement_visits",
	"scroll_depth_total", "scroll_depth_visits",
}

// wideGrains are the widths an import is summarised into.
func wideGrains() []query.Grain { return []query.Grain{query.GrainWeek, query.GrainMonth} }

// SummariseImport writes an import's week and month rows and records that it
// has them.
//
// It is a plain sum over rows already held, so it needs no source data and no
// re-import.
func SummariseImport(ctx context.Context, db *sql.DB, importID, siteID int64, location *time.Location) error {
	if location == nil {
		location = time.UTC
	}

	days, err := importDays(ctx, db, importID)
	if err != nil {
		return err
	}

	var built int64

	for _, grain := range wideGrains() {
		if err := summariseGrain(ctx, db, importID, siteID, grain, days, location); err != nil {
			return err
		}

		built |= grain.GrainBit()
	}

	// Recorded last and in one write, so a reader either sees a finished
	// summary or none: a partial one would answer a wide report with a bucket
	// that is missing days.
	//
	// The zone goes with it. A week is a week in one timezone and a different
	// seven days in another, so a summary read under a zone it was not cut in
	// reports one month's traffic as the next one's.
	if _, err := db.ExecContext(ctx,
		"UPDATE imports SET wide_grains = ?, wide_timezone = ? WHERE id = ?",
		built, location.String(), importID); err != nil {
		return fmt.Errorf("dataio: record wide grains for import %d: %w", importID, err)
	}

	return nil
}

// MarkNothingToSummarise records an import as summarised without writing any
// rows, for a source whose data does not live in imported_rollups at all.
//
// The mark is not bookkeeping: a reader treats an unmarked import as one whose
// summaries are missing and falls back to the daily rows for the whole site, so
// one Search Console connection would otherwise turn the summaries off for
// every archive beside it.
func MarkNothingToSummarise(ctx context.Context, db *sql.DB, importID int64, location *time.Location) error {
	if location == nil {
		location = time.UTC
	}

	var built int64
	for _, grain := range wideGrains() {
		built |= grain.GrainBit()
	}

	if _, err := db.ExecContext(ctx,
		"UPDATE imports SET wide_grains = ?, wide_timezone = ? WHERE id = ?",
		built, location.String(), importID); err != nil {
		return fmt.Errorf("dataio: record wide grains for import %d: %w", importID, err)
	}

	return nil
}

// importDays lists the local days an import holds rows for, which is what the
// bucket each of them belongs to is computed from.
func importDays(ctx context.Context, db *sql.DB, importID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT DISTINCT timestamp FROM imported_rollups WHERE import_id = ? ORDER BY timestamp", importID)
	if err != nil {
		return nil, fmt.Errorf("dataio: read import %d days: %w", importID, err)
	}

	defer func() { _ = rows.Close() }()

	var days []int64

	for rows.Next() {
		var day int64
		if err := rows.Scan(&day); err != nil {
			return nil, fmt.Errorf("dataio: read import %d days: %w", importID, err)
		}

		days = append(days, day)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataio: read import %d days: %w", importID, err)
	}

	return days, nil
}

// summariseGrain rewrites one grain's rows for one import.
func summariseGrain(ctx context.Context, db *sql.DB, importID, siteID int64, grain query.Grain, days []int64, location *time.Location) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = fmt.Errorf("dataio: close summary connection: %w", closeErr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	// Which bucket a day belongs to is calendar arithmetic — a month is not a
	// fixed number of days and a week can cross a daylight saving change — so it
	// is computed in Go and handed to SQLite rather than divided out in SQL.
	if err := writeDayMap(ctx, tx, grain, days, location); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM imported_wide WHERE import_id = ? AND grain = ?", importID, int64(grain)); err != nil {
		return fmt.Errorf("dataio: clear import %d %s rows: %w", importID, grain, err)
	}

	columns := append([]string{"import_id", "site_id", "grain", "timestamp"}, wideKeys...)
	values := []string{"?", "?", "?", "m.bucket"}

	for _, key := range wideKeys {
		values = append(values, "d."+key)
	}

	for _, metric := range wideMetrics {
		columns = append(columns, metric)
		values = append(values, "SUM(d."+metric+")")
	}

	group := []string{"m.bucket"}
	for _, key := range wideKeys {
		group = append(group, "d."+key)
	}

	statement := "INSERT INTO imported_wide (" + strings.Join(columns, ", ") + ") " +
		"SELECT " + strings.Join(values, ", ") + " " +
		"FROM imported_rollups d JOIN imported_day_map m ON m.day = d.timestamp " +
		"WHERE d.import_id = ? " +
		"GROUP BY " + strings.Join(group, ", ")

	if _, err := tx.ExecContext(ctx, statement, importID, siteID, int64(grain), importID); err != nil {
		return fmt.Errorf("dataio: summarise import %d at %s: %w", importID, grain, err)
	}

	return tx.Commit()
}

// writeDayMap records which bucket each of an import's days belongs to.
func writeDayMap(ctx context.Context, tx *sql.Tx, grain query.Grain, days []int64, location *time.Location) error {
	if _, err := tx.ExecContext(ctx,
		"CREATE TEMP TABLE IF NOT EXISTS imported_day_map (day INTEGER PRIMARY KEY, bucket INTEGER NOT NULL)"); err != nil {
		return fmt.Errorf("dataio: day map: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM imported_day_map"); err != nil {
		return fmt.Errorf("dataio: day map: %w", err)
	}

	insert, err := tx.PrepareContext(ctx, "INSERT INTO imported_day_map (day, bucket) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("dataio: day map: %w", err)
	}

	defer insert.Close() //nolint:errcheck // the statement dies with the transaction

	for _, day := range days {
		// The stored day is a wall-clock reading in the site's zone, so it is
		// read back in that zone before the bucket it falls in is taken.
		at := time.Unix(day, 0).In(location)
		bucket := query.RollupBucketStart(at, grain, location)

		if _, err := insert.ExecContext(ctx, day, bucket.Unix()); err != nil {
			return fmt.Errorf("dataio: day map: %w", err)
		}
	}

	return nil
}

// SummariseSite builds the summaries for every one of a site's imports that
// lacks them or holds them in another timezone, and says how many it built.
//
// A site's zone decides which days a week and a month hold, so a summary cut
// under a different one has to be thrown away rather than corrected.
func SummariseSite(ctx context.Context, db *sql.DB, siteID int64, location *time.Location) (int, error) {
	var want int64
	for _, grain := range wideGrains() {
		want |= grain.GrainBit()
	}

	rows, err := db.QueryContext(ctx,
		"SELECT id FROM imports WHERE site_id = ? AND ((wide_grains & ?) <> ? OR wide_timezone <> ?) ORDER BY id",
		siteID, want, want, location.String())
	if err != nil {
		return 0, fmt.Errorf("dataio: read imports to summarise: %w", err)
	}

	var pending []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()

			return 0, fmt.Errorf("dataio: read imports to summarise: %w", err)
		}

		pending = append(pending, id)
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()

		return 0, fmt.Errorf("dataio: read imports to summarise: %w", err)
	}

	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("dataio: read imports to summarise: %w", err)
	}

	for _, id := range pending {
		if err := SummariseImport(ctx, db, id, siteID, location); err != nil {
			return 0, err
		}
	}

	return len(pending), nil
}
