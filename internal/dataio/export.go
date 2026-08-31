//
// export.go
// A full site export: the ten roll-up tables and the raw events, as one ZIP.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"encoding/csv"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// ExportDir is where prepared archives live, under the data directory.
const ExportDir = "exports"

// ExportPath is where one export's archive is written.
func ExportPath(dataDir string, exportID int64) string {
	return filepath.Join(dataDir, ExportDir, fmt.Sprintf("export-%06d.zip", exportID))
}

// counters is one day-and-dimension-combination's totals while an export is
// being assembled. It is keyed the same way the importer reads a row back, so
// what comes out of an export goes back in unchanged.
type counters struct {
	visitors   int64
	visits     int64
	pageviews  int64
	events     int64
	exits      int64
	bounces    int64
	duration   int64
	engagement int64
}

// bucket is one row of a sheet before it is rendered: the local day, the
// dimension values in sheet order, and the counters.
type bucket struct {
	day    string
	values []string
	counts counters
}

// BuildExport writes the whole archive and returns its size. Every sheet is
// built from SQL aggregates rather than from the query engine, because the
// files have to carry totals — bounces and seconds — where a report carries
// rates and averages, and a rate cannot be added to another rate on re-import.
//
// The raw events file is included in every build. Data somebody generated on
// their own site is theirs, and putting the only complete copy of it behind a
// plan is how a customer discovers they cannot leave.
func BuildExport(ctx context.Context, db *sql.DB, siteID int64, location *time.Location, destination string) (int64, error) {
	if location == nil {
		location = time.UTC
	}

	from, to, err := siteRange(ctx, db, siteID)
	if err != nil {
		return 0, err
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return 0, fmt.Errorf("dataio: create export directory: %w", err)
	}

	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("dataio: create %s: %w", destination, err)
	}
	defer file.Close()

	archive := zip.NewWriter(file)

	for _, sheet := range Sheets {
		if err := writeSheet(ctx, db, archive, siteID, location, from, to, sheet); err != nil {
			archive.Close()
			return 0, err
		}
	}

	if err := writeRawEvents(ctx, db, archive, siteID, location); err != nil {
		archive.Close()
		return 0, err
	}

	if err := archive.Close(); err != nil {
		return 0, fmt.Errorf("dataio: finish %s: %w", destination, err)
	}

	// The sync is what makes "the row says the file is ready" true after a
	// power cut, rather than a download link pointing at a truncated archive.
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("dataio: sync %s: %w", destination, err)
	}

	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("dataio: measure %s: %w", destination, err)
	}

	return info.Size(), nil
}

// siteRange finds the window an export covers. It is the site's whole history,
// widened by a day either side so the timezone conversion inside the day
// bucketing has margin at both ends.
func siteRange(ctx context.Context, db *sql.DB, siteID int64) (time.Time, time.Time, error) {
	var first, last sql.NullInt64

	err := db.QueryRowContext(ctx,
		"SELECT MIN(timestamp), MAX(timestamp) FROM events WHERE site_id = ?", siteID).Scan(&first, &last)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("dataio: read export range: %w", err)
	}

	if !first.Valid {
		now := time.Now().UTC()

		return now, now, nil
	}

	return time.Unix(first.Int64, 0).UTC().Add(-24 * time.Hour),
		time.Unix(last.Int64, 0).UTC().Add(24 * time.Hour), nil
}

// writeSheet renders one of the ten formats into the archive.
func writeSheet(ctx context.Context, db *sql.DB, archive *zip.Writer, siteID int64, location *time.Location, from, to time.Time, sheet Sheet) error {
	buckets := map[string]*bucket{}

	if sheet.Grain&grainEvent != 0 {
		if err := collect(ctx, db, siteID, location, from, to, sheet, grainEvent, buckets); err != nil {
			return err
		}
	}

	if sheet.Grain&grainSession != 0 {
		if err := collect(ctx, db, siteID, location, from, to, sheet, grainSession, buckets); err != nil {
			return err
		}
	}

	entry, err := archive.Create(sheet.Name + ".csv")
	if err != nil {
		return fmt.Errorf("dataio: write %s: %w", sheet.Name, err)
	}

	writer := csv.NewWriter(entry)

	if err := writer.Write(sheet.Header()); err != nil {
		return fmt.Errorf("dataio: write %s: %w", sheet.Name, err)
	}

	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		row := buckets[key]

		record := make([]string, 0, 1+len(sheet.Columns)+len(sheet.Metrics))
		record = append(record, row.day)
		record = append(record, row.values...)

		for _, field := range sheet.Metrics {
			record = append(record, strconv.FormatInt(row.counts.field(field), 10))
		}

		if err := writer.Write(record); err != nil {
			return fmt.Errorf("dataio: write %s: %w", sheet.Name, err)
		}
	}

	writer.Flush()

	return writer.Error()
}

// field reads one counter by its CSV field name.
func (c counters) field(name string) int64 {
	switch name {
	case FieldVisitors:
		return c.visitors
	case FieldVisits:
		return c.visits
	case FieldPageviews:
		return c.pageviews
	case FieldEvents:
		return c.events
	case FieldExits:
		return c.exits
	case FieldBounces:
		return c.bounces
	case FieldDuration:
		return c.duration
	case FieldEngagement:
		return c.engagement
	}

	return 0
}

// collect runs one grouped read and merges it into the sheet's buckets. The
// dimension values come back as strings by joining the dimension tables, which
// is affordable here and nowhere else: an export is one query over a whole
// site's history, not a request somebody is waiting on.
func collect(ctx context.Context, db *sql.DB, siteID int64, location *time.Location, from, to time.Time, sheet Sheet, g grain, buckets map[string]*bucket) error {
	table, timeColumn := "events e", "e.timestamp"
	if g == grainSession {
		table, timeColumn = "sessions e", "e.started_at"
	}

	dayExpr, dayArgs := query.LocalDaySQL(timeColumn, location, from, to)

	var (
		selects []string
		joins   []string
		args    []any
	)

	selects = append(selects, dayExpr)
	args = append(args, dayArgs...)

	for i, name := range sheet.Dimensions {
		column, dimension, ok := exportColumn(name, g)
		if !ok {
			// The sheet's grain does not carry this dimension — a page has no
			// session column, an entry page has no event column. The other
			// grain fills it in, and an empty string here would overwrite what
			// that one found.
			return nil
		}

		alias := fmt.Sprintf("t%d", i)
		joins = append(joins, "LEFT JOIN "+dimension.Table()+" "+alias+" ON "+alias+".id = e."+column)
		selects = append(selects, "COALESCE("+alias+".value, '')")
	}

	names, aggregates, err := exportAggregates(ctx, db, g, sheet.Grain&grainEvent == 0)
	if err != nil {
		return err
	}

	selects = append(selects, aggregates...)

	group := make([]string, 0, 1+len(sheet.Dimensions))
	for i := 1; i <= 1+len(sheet.Dimensions); i++ {
		group = append(group, strconv.Itoa(i))
	}

	statement := "SELECT " + strings.Join(selects, ", ") +
		" FROM " + table + " " + strings.Join(joins, " ") +
		" WHERE e.site_id = ? AND e.is_imported = 0"

	// Bot traffic is left out of an export for the same reason it is left out
	// of every report: it is not the customer's traffic, and re-importing it
	// somewhere else would carry a wrong number across the move.
	if g == grainEvent {
		statement += " AND e.bot_reason_id = 0"
	}

	statement += " GROUP BY " + strings.Join(group, ", ")

	args = append(args, siteID)

	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("dataio: read %s: %w", sheet.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		day := ""
		values := make([]string, len(sheet.Dimensions))
		numbers := make([]int64, len(names))

		scanTo := make([]any, 0, 1+len(values)+len(numbers))
		scanTo = append(scanTo, &day)
		for i := range values {
			scanTo = append(scanTo, &values[i])
		}
		for i := range numbers {
			scanTo = append(scanTo, &numbers[i])
		}

		if err := rows.Scan(scanTo...); err != nil {
			return fmt.Errorf("dataio: read %s: %w", sheet.Name, err)
		}

		key := day + "\x1f" + strings.Join(values, "\x1f")

		entry, ok := buckets[key]
		if !ok {
			entry = &bucket{day: day, values: values}
			buckets[key] = entry
		}

		for i, name := range names {
			entry.counts.set(name, numbers[i])
		}
	}

	return rows.Err()
}

// set records one counter. Assignment rather than addition is safe because
// every field is produced by exactly one pass: the two grains are given
// non-overlapping aggregate lists precisely so that a hit count and a visit
// count can never be added together into a number that is neither.
func (c *counters) set(name string, value int64) {
	switch name {
	case FieldVisitors:
		c.visitors = value
	case FieldVisits:
		c.visits = value
	case FieldPageviews:
		c.pageviews = value
	case FieldEvents:
		c.events = value
	case FieldExits:
		c.exits = value
	case FieldBounces:
		c.bounces = value
	case FieldDuration:
		c.duration = value
	case FieldEngagement:
		c.engagement = value
	}
}

// exportColumn resolves a dimension to the column one grain holds it in.
func exportColumn(name string, g grain) (string, intern.Dimension, bool) {
	dimension, ok := query.ImportedInterned(name)
	if !ok {
		return "", "", false
	}

	column, ok := query.ImportedColumn(name)
	if !ok {
		return "", "", false
	}

	if g == grainSession {
		switch name {
		case "event:page", "event:page_title", "event:name":
			// A visit has no single page, title or event name. The event pass
			// carries these sheets on its own.
			return "", "", false
		case "event:hostname":
			return "entry_hostname_id", dimension, true
		}

		return column, dimension, true
	}

	switch name {
	case "visit:entry_page", "visit:exit_page":
		// An event has no entry or exit page: those are properties of the whole
		// visit, and the session pass carries them.
		return "", "", false
	}

	return column, dimension, true
}

// exportAggregates returns the counter expressions for one grain, and the field
// each one fills.
//
// The two grains are given non-overlapping field lists on purpose. A sheet that
// reads both tables takes its visitor, visit and pageview counts from the event
// pass — those are hit-level facts and the event table is where they are exact
// — and takes only bounces and visit duration from the session pass, which is
// the only place they exist. A session-only sheet has no event pass to defer
// to, so it fills everything itself.
func exportAggregates(ctx context.Context, db *sql.DB, g grain, sessionOnly bool) ([]string, []string, error) {
	if g == grainSession {
		if !sessionOnly {
			return []string{FieldBounces, FieldDuration},
				[]string{
					"COALESCE(SUM(e.is_bounce), 0)",
					"COALESCE(SUM(e.duration), 0)",
				}, nil
		}

		return []string{FieldVisitors, FieldVisits, FieldExits, FieldPageviews, FieldBounces, FieldDuration},
			[]string{
				"COUNT(DISTINCT e.user_id)",
				"COUNT(*)",
				"COUNT(*)",
				"COALESCE(SUM(e.pageviews), 0)",
				"COALESCE(SUM(e.is_bounce), 0)",
				"COALESCE(SUM(e.duration), 0)",
			}, nil
	}

	pageview, engagement, err := eventNameIDs(ctx, db)
	if err != nil {
		return nil, nil, err
	}

	return []string{FieldVisitors, FieldVisits, FieldPageviews, FieldEvents, FieldEngagement},
		[]string{
			"COUNT(DISTINCT e.user_id)",
			"COUNT(DISTINCT e.session_id)",
			"COALESCE(SUM(CASE WHEN e.name_id = " + strconv.FormatInt(pageview, 10) + " THEN 1 ELSE 0 END), 0)",
			"COALESCE(SUM(CASE WHEN e.name_id <> " + strconv.FormatInt(engagement, 10) + " THEN 1 ELSE 0 END), 0)",
			"COALESCE(SUM(e.engagement_time), 0)",
		}, nil
}

// eventNameIDs reads the two interned event names the aggregates key off. A
// name this account has never recorded resolves to -1 so it matches no row —
// id 0 is the empty string, and matching that would count every event with no
// name at all.
func eventNameIDs(ctx context.Context, db *sql.DB) (int64, int64, error) {
	pageview, engagement := int64(-1), int64(-1)

	rows, err := db.QueryContext(ctx,
		"SELECT id, value FROM "+intern.EventName.Table()+" WHERE value IN ('pageview', 'engagement')")
	if err != nil {
		return 0, 0, fmt.Errorf("dataio: read event names: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var value string

		if err := rows.Scan(&id, &value); err != nil {
			return 0, 0, fmt.Errorf("dataio: read event names: %w", err)
		}

		switch value {
		case "pageview":
			pageview = id
		case "engagement":
			engagement = id
		}
	}

	return pageview, engagement, rows.Err()
}

// rawEventColumns is the raw export's header, and the order the query below
// returns. Keeping the two beside each other is what stops a new column being
// added to one and not the other.
var rawEventColumns = []string{
	"timestamp", "event_name", "visitor_id", "session_id",
	"hostname", "page", "page_title",
	"referrer", "source", "channel", "utm_source", "utm_medium", "utm_campaign",
	"country", "region", "city",
	"device", "screen_size", "browser", "browser_version",
	"operating_system", "operating_system_version", "language",
	"scroll_depth", "engagement_time_ms", "bot_reason",
}

// writeRawEvents streams every event into the archive, one row each.
//
// This is the file an incumbent puts behind their most expensive plan. It is
// here in every build, because the alternative is a customer discovering at the
// moment they want to leave that the complete copy of their own data is the one
// thing they cannot have.
func writeRawEvents(ctx context.Context, db *sql.DB, archive *zip.Writer, siteID int64, location *time.Location) error {
	entry, err := archive.Create(RawEventsSheet + ".csv")
	if err != nil {
		return fmt.Errorf("dataio: write raw events: %w", err)
	}

	writer := csv.NewWriter(entry)

	if err := writer.Write(rawEventColumns); err != nil {
		return fmt.Errorf("dataio: write raw events: %w", err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT e.timestamp, COALESCE(n.value, ''), e.user_id, e.session_id,
		       COALESCE(h.value, ''), COALESCE(p.value, ''), COALESCE(t.value, ''),
		       COALESCE(r.value, ''), COALESCE(s.value, ''), COALESCE(ch.value, ''),
		       COALESCE(us.value, ''), COALESCE(um.value, ''), COALESCE(uc.value, ''),
		       COALESCE(co.value, ''), COALESCE(re.value, ''), COALESCE(ci.value, ''),
		       COALESCE(dt.value, ''), COALESCE(ss.value, ''), COALESCE(br.value, ''), COALESCE(bv.value, ''),
		       COALESCE(os.value, ''), COALESCE(ov.value, ''), COALESCE(la.value, ''),
		       e.scroll_depth, e.engagement_time, COALESCE(bt.value, '')
		FROM events e
		LEFT JOIN dim_event_name n ON n.id = e.name_id
		LEFT JOIN dim_hostname h ON h.id = e.hostname_id
		LEFT JOIN dim_pathname p ON p.id = e.pathname_id
		LEFT JOIN dim_page_title t ON t.id = e.page_title_id
		LEFT JOIN dim_referrer r ON r.id = e.referrer_id
		LEFT JOIN dim_source s ON s.id = e.source_id
		LEFT JOIN dim_channel ch ON ch.id = e.channel_id
		LEFT JOIN dim_utm_source us ON us.id = e.utm_source_id
		LEFT JOIN dim_utm_medium um ON um.id = e.utm_medium_id
		LEFT JOIN dim_utm_campaign uc ON uc.id = e.utm_campaign_id
		LEFT JOIN dim_country co ON co.id = e.country_id
		LEFT JOIN dim_region re ON re.id = e.region_id
		LEFT JOIN dim_city ci ON ci.id = e.city_id
		LEFT JOIN dim_device_type dt ON dt.id = e.device_type_id
		LEFT JOIN dim_screen_size ss ON ss.id = e.screen_size_id
		LEFT JOIN dim_browser br ON br.id = e.browser_id
		LEFT JOIN dim_browser_version bv ON bv.id = e.browser_version_id
		LEFT JOIN dim_os os ON os.id = e.os_id
		LEFT JOIN dim_os_version ov ON ov.id = e.os_version_id
		LEFT JOIN dim_language la ON la.id = e.language_id
		LEFT JOIN dim_bot_reason bt ON bt.id = e.bot_reason_id
		WHERE e.site_id = ?
		ORDER BY e.timestamp, e.id`, siteID)
	if err != nil {
		return fmt.Errorf("dataio: read raw events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			timestamp, userID, sessionID, scrollDepth, engagementTime int64
			text                                                      [20]string
			botReason                                                 string
		)

		scanTo := []any{&timestamp, &text[0], &userID, &sessionID}
		for i := 1; i < 20; i++ {
			scanTo = append(scanTo, &text[i])
		}
		scanTo = append(scanTo, &scrollDepth, &engagementTime, &botReason)

		if err := rows.Scan(scanTo...); err != nil {
			return fmt.Errorf("dataio: read raw events: %w", err)
		}

		record := []string{
			time.Unix(timestamp, 0).In(location).Format(time.RFC3339),
			text[0],
			strconv.FormatInt(userID, 10),
			strconv.FormatInt(sessionID, 10),
		}
		record = append(record, text[1:20]...)
		record = append(record,
			strconv.FormatInt(scrollDepth, 10),
			strconv.FormatInt(engagementTime, 10),
			botReason,
		)

		if err := writer.Write(record); err != nil {
			return fmt.Errorf("dataio: write raw events: %w", err)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("dataio: read raw events: %w", err)
	}

	writer.Flush()

	return writer.Error()
}
