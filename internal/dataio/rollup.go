//
// rollup.go
// Turning a parsed row into an imported roll-up, and writing them in batches.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// batchRows is how many roll-ups are collected before a transaction opens.
// SQLite caps out in the low hundreds of un-batched writes a second, and a few
// thousand rows a transaction is where the per-commit cost disappears while the
// memory held between commits is still small.
const batchRows = 5000

// Row is one parsed CSV line: a day, the dimension values the file carried, and
// the counters on it. Dimensions are keyed by query dimension name rather than
// by column name so that the same struct serves the CSV importer and the GA4
// one, which report the same things under different labels.
type Row struct {
	// Timestamp is unix seconds at the site's local midnight. Storing local
	// midnight rather than UTC midnight is what makes an imported day land in
	// the same bucket the customer saw it in before they moved.
	Timestamp int64

	Dimensions map[string]string
	Metrics    map[string]int64
}

// rollupColumns is the roll-up table in bind order. The dimension block comes
// from the query package's own registry so a column can never be filled from
// the wrong dimension.
var rollupColumns = []string{
	"import_id", "site_id", "timestamp", "covered",
	"name_id", "hostname_id", "pathname_id", "entry_page_id", "exit_page_id", "page_title_id",
	"referrer_id", "source_id", "channel_id", "utm_source_id", "utm_medium_id", "utm_campaign_id",
	"country_id", "region_id", "city_id",
	"device_type_id", "screen_size_id", "browser_id", "browser_version_id",
	"os_id", "os_version_id", "language_id",
	"visitors", "visits", "pageviews", "events", "exits", "bounces",
	"duration_total", "engagement_total",
}

// Writer turns parsed rows into roll-up rows. It holds the interning cache
// because every dimension value has to become an integer before it can be
// written, and interning may insert — which has to happen outside the write
// transaction, since an account's writer is a pool of exactly one connection.
type Writer struct {
	db    *sql.DB
	cache *intern.Cache

	importID int64
	siteID   int64

	// covered is the bitmask every row this writer produces carries: the
	// dimensions the source genuinely reported. It is fixed per file, because a
	// file's columns are fixed, and it is the thing that lets a filtered query
	// tell "does not match" from "cannot answer".
	covered uint64

	// dimensions is the same information in the form the import record stores,
	// so the settings page can say what an import covers.
	dimensions []string

	pending [][]any
	written int64
}

// NewWriter builds a writer for one file's worth of rows. The dimension list is
// validated up front so an unrecognised column fails before anything is
// written, rather than half way through a year of history.
func NewWriter(db *sql.DB, cache *intern.Cache, importID, siteID int64, dimensions []string) (*Writer, error) {
	ordered := append([]string(nil), dimensions...)
	sort.Strings(ordered)

	covered, err := query.ImportedCoverage(ordered)
	if err != nil {
		return nil, err
	}

	return &Writer{
		db: db, cache: cache,
		importID: importID, siteID: siteID,
		covered: covered, dimensions: ordered,
	}, nil
}

// Dimensions returns what this writer's rows carry.
func (w *Writer) Dimensions() []string {
	return w.dimensions
}

// Written reports how many roll-up rows have been committed.
func (w *Writer) Written() int64 {
	return w.written
}

// Add queues one parsed row. Interning happens here rather than at flush time
// because it can write, and a write issued while the flush transaction holds
// the single connection would wait for a connection only that transaction can
// release.
func (w *Writer) Add(ctx context.Context, row Row) error {
	ids := map[string]int64{}

	for name, value := range row.Dimensions {
		column, ok := query.ImportedColumn(name)
		if !ok {
			return fmt.Errorf("%q is not a dimension imported data can carry — known ones are %s",
				name, strings.Join(query.ImportedDimensionNames(), ", "))
		}

		dimension, ok := query.ImportedInterned(name)
		if !ok {
			return fmt.Errorf("%q has no dimension table to intern into", name)
		}

		id, err := w.cache.ID(ctx, dimension, strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("dataio: intern %s: %w", name, err)
		}

		ids[column] = id
	}

	args := make([]any, 0, len(rollupColumns))
	args = append(args, w.importID, w.siteID, row.Timestamp, int64(w.covered))

	for _, column := range rollupColumns[4:26] {
		args = append(args, ids[column])
	}

	args = append(args,
		row.Metrics[FieldVisitors],
		row.Metrics[FieldVisits],
		row.Metrics[FieldPageviews],
		row.Metrics[FieldEvents],
		row.Metrics[FieldExits],
		row.Metrics[FieldBounces],
		row.Metrics[FieldDuration],
		row.Metrics[FieldEngagement],
	)

	w.pending = append(w.pending, args)

	if len(w.pending) >= batchRows {
		return w.Flush(ctx)
	}

	return nil
}

// Flush writes everything queued in one transaction.
func (w *Writer) Flush(ctx context.Context) (err error) {
	if len(w.pending) == 0 {
		return nil
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dataio: write roll-ups: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	statement := "INSERT INTO " + query.ImportedTable + " (" + strings.Join(rollupColumns, ", ") +
		") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(rollupColumns)), ",") + ")"

	prepared, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return fmt.Errorf("dataio: write roll-ups: %w", err)
	}
	defer closeResource(prepared, &err, "roll-up statement")

	for _, args := range w.pending {
		if _, err := prepared.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("dataio: write roll-ups: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dataio: write roll-ups: %w", err)
	}

	w.written += int64(len(w.pending))
	w.pending = w.pending[:0]

	return nil
}
