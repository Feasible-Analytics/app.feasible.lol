//
// rename.go
// Rewriting the values of imports already on disk into the names live collection stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// Renamed is one value an import's rows were rewritten from and to, and on how
// many rows.
type Renamed struct {
	Dimension string
	From      string
	To        string
	Rows      int64
}

// RenameResult is what renaming one import changed, or would change.
type RenameResult struct {
	ImportID int64
	Source   string

	// Rows is how many roll-up rows hold at least one renamed value.
	Rows int64

	// Renamed lists every rewrite, sorted by dimension and then by value.
	Renamed []Renamed
}

// renameColumn is one dimension column of the roll-up table, with the
// dimension table its ids point into.
type renameColumn struct {
	dimension string
	column    string
	table     intern.Dimension
}

// renameColumns are the dimension columns an import's values live in, in a
// fixed order.
var renameColumns = buildRenameColumns()

// buildRenameColumns resolves every imported dimension that has a column and a
// dimension table.
func buildRenameColumns() []renameColumn {
	var columns []renameColumn

	for _, name := range query.ImportedDimensionNames() {
		column, ok := query.ImportedColumn(name)
		if !ok {
			continue
		}

		table, ok := query.ImportedInterned(name)
		if !ok {
			continue
		}

		columns = append(columns, renameColumn{dimension: name, column: column, table: table})
	}

	return columns
}

// RenameImports rewrites the dimension values of every completed import on a
// site into the names live collection stores, using the same Normalise an
// import runs today, and rebuilds the week and month summaries of each import
// it changed. Only imported_rollups and imported_wide are written; collected
// events and sessions are never read or touched. With apply false it reports
// what would change and writes nothing.
//
// The location is the site's timezone, which the rebuilt summaries are cut in.
func RenameImports(ctx context.Context, db *sql.DB, cache *intern.Cache, siteID int64, location *time.Location, apply bool) ([]RenameResult, error) {
	imports, err := completedImports(ctx, db, siteID)
	if err != nil {
		return nil, err
	}

	var results []RenameResult

	for _, record := range imports {
		result, err := renameImport(ctx, db, cache, record, location, apply)
		if err != nil {
			return results, err
		}

		results = append(results, result)
	}

	return results, nil
}

// completedImports lists a site's finished imports. A pending or running
// import is skipped because its worker is still writing rows.
func completedImports(ctx context.Context, db *sql.DB, siteID int64) ([]Import, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, site_id, source FROM imports WHERE site_id = ? AND status = ? ORDER BY id",
		siteID, StatusCompleted)
	if err != nil {
		return nil, fmt.Errorf("dataio: list imports to rename: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var imports []Import

	for rows.Next() {
		var record Import
		if err := rows.Scan(&record.ID, &record.SiteID, &record.Source); err != nil {
			return nil, fmt.Errorf("dataio: list imports to rename: %w", err)
		}

		imports = append(imports, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataio: list imports to rename: %w", err)
	}

	return imports, nil
}

// storedRow is one roll-up row's id and its dimension ids, in renameColumns
// order.
type storedRow struct {
	id  int64
	ids []int64
}

// renameImport rewrites one import. Every read finishes before any write
// starts, because interning a new value and the update transaction both need
// the account's single writer connection.
func renameImport(ctx context.Context, db *sql.DB, cache *intern.Cache, record Import, location *time.Location, apply bool) (RenameResult, error) {
	result := RenameResult{ImportID: record.ID, Source: record.Source}

	rows, err := readStoredRows(ctx, db, record.ID)
	if err != nil {
		return result, err
	}

	values, err := readStoredValues(ctx, db, rows)
	if err != nil {
		return result, err
	}

	// Rows sharing a combination of ids normalise the same way, so each
	// combination is worked out once.
	outcomes := map[string]renamedRow{}
	counts := map[[3]string]int64{}
	changed := make([]storedRow, 0)
	newValues := make([][]string, 0)

	for _, row := range rows {
		key := idsKey(row.ids)

		found, seen := outcomes[key]
		if !seen {
			found = normaliseStored(record.Source, row.ids, values)
			outcomes[key] = found
		}

		if !found.changed {
			continue
		}

		changed = append(changed, row)
		newValues = append(newValues, found.values)

		for i, column := range renameColumns {
			from := values[i][row.ids[i]]
			if found.values[i] != from {
				counts[[3]string{column.dimension, from, found.values[i]}]++
			}
		}
	}

	result.Rows = int64(len(changed))
	for key, count := range counts {
		result.Renamed = append(result.Renamed, Renamed{Dimension: key[0], From: key[1], To: key[2], Rows: count})
	}
	sort.Slice(result.Renamed, func(i, j int) bool {
		a, b := result.Renamed[i], result.Renamed[j]
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		if a.From != b.From {
			return a.From < b.From
		}

		return a.To < b.To
	})

	if !apply || len(changed) == 0 {
		return result, nil
	}

	updates := make([][]int64, len(changed))
	for i, row := range changed {
		ids := make([]int64, len(renameColumns))

		for c, column := range renameColumns {
			id, err := cache.ID(ctx, column.table, newValues[i][c])
			if err != nil {
				return result, fmt.Errorf("dataio: rename import %d: %w", record.ID, err)
			}

			ids[c] = id
		}

		updates[i] = append(ids, row.id)
	}

	if err := writeRenamedRows(ctx, db, record.ID, updates); err != nil {
		return result, err
	}

	if err := SummariseImport(ctx, db, record.ID, record.SiteID, location); err != nil {
		return result, err
	}

	return result, nil
}

// renamedRow is one combination of stored ids after Normalise: whether any
// value changed, and every value in renameColumns order.
type renamedRow struct {
	changed bool
	values  []string
}

// normaliseStored runs one combination of stored ids through Normalise.
func normaliseStored(origin string, ids []int64, values []map[int64]string) (out renamedRow) {
	dimensions := make(map[string]string, len(renameColumns))
	for i, column := range renameColumns {
		if ids[i] != intern.EmptyID {
			dimensions[column.dimension] = values[i][ids[i]]
		}
	}

	normalised := Normalise(origin, dimensions)

	out.values = make([]string, len(renameColumns))
	for i, column := range renameColumns {
		out.values[i] = normalised[column.dimension]
		if out.values[i] != values[i][ids[i]] {
			out.changed = true
		}
	}

	return out
}

// idsKey renders a combination of ids as a map key.
func idsKey(ids []int64) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(strconv.FormatInt(id, 10))
		b.WriteByte(',')
	}

	return b.String()
}

// readStoredRows reads every roll-up row of one import.
func readStoredRows(ctx context.Context, db *sql.DB, importID int64) ([]storedRow, error) {
	names := make([]string, 0, len(renameColumns))
	for _, column := range renameColumns {
		names = append(names, column.column)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT id, "+strings.Join(names, ", ")+" FROM "+query.ImportedTable+" WHERE import_id = ? ORDER BY id", importID)
	if err != nil {
		return nil, fmt.Errorf("dataio: read import %d rows: %w", importID, err)
	}
	defer func() { _ = rows.Close() }()

	var stored []storedRow

	for rows.Next() {
		row := storedRow{ids: make([]int64, len(renameColumns))}

		targets := make([]any, 0, 1+len(renameColumns))
		targets = append(targets, &row.id)
		for i := range row.ids {
			targets = append(targets, &row.ids[i])
		}

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("dataio: read import %d rows: %w", importID, err)
		}

		stored = append(stored, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataio: read import %d rows: %w", importID, err)
	}

	return stored, nil
}

// readStoredValues looks up the value behind every id the rows use, one map
// per column in renameColumns order. Id 0 is the empty value in every table.
func readStoredValues(ctx context.Context, db *sql.DB, rows []storedRow) ([]map[int64]string, error) {
	values := make([]map[int64]string, len(renameColumns))

	for i, column := range renameColumns {
		values[i] = map[int64]string{intern.EmptyID: ""}

		for _, row := range rows {
			id := row.ids[i]
			if _, ok := values[i][id]; ok {
				continue
			}

			var value string
			err := db.QueryRowContext(ctx, "SELECT value FROM "+column.table.Table()+" WHERE id = ?", id).Scan(&value)
			if err != nil {
				return nil, fmt.Errorf("dataio: read %s value %d: %w", column.dimension, id, err)
			}

			values[i][id] = value
		}
	}

	return values, nil
}

// writeRenamedRows applies the new ids in batches, one transaction each. Each
// update is the row's new ids in renameColumns order followed by its row id.
func writeRenamedRows(ctx context.Context, db *sql.DB, importID int64, updates [][]int64) error {
	assignments := make([]string, 0, len(renameColumns))
	for _, column := range renameColumns {
		assignments = append(assignments, column.column+" = ?")
	}

	statement := "UPDATE " + query.ImportedTable + " SET " + strings.Join(assignments, ", ") +
		" WHERE id = ? AND import_id = ?"

	for start := 0; start < len(updates); start += batchRows {
		end := min(start+batchRows, len(updates))

		if err := writeRenamedBatch(ctx, db, importID, statement, updates[start:end]); err != nil {
			return err
		}
	}

	return nil
}

// writeRenamedBatch applies one batch of updates in one transaction.
func writeRenamedBatch(ctx context.Context, db *sql.DB, importID int64, statement string, updates [][]int64) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dataio: rename import %d: %w", importID, err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	prepared, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return fmt.Errorf("dataio: rename import %d: %w", importID, err)
	}
	defer closeResource(prepared, &err, "rename statement")

	for _, update := range updates {
		args := make([]any, 0, len(update)+1)
		for _, value := range update {
			args = append(args, value)
		}
		args = append(args, importID)

		if _, err := prepared.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("dataio: rename import %d: %w", importID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dataio: rename import %d: %w", importID, err)
	}

	return nil
}
