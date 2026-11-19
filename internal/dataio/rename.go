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

// storedCombination is one distinct combination of dimension ids an import's
// rows hold, in renameColumns order, and how many rows hold it.
type storedCombination struct {
	ids  []int64
	rows int64
}

// renamedCombination is a stored combination whose values Normalise changes,
// with every value after the change in renameColumns order.
type renamedCombination struct {
	ids    []int64
	values []string
}

// renameCombinationsPerTransaction bounds how many combinations one write
// transaction rewrites, which bounds how long the account's writer is held.
const renameCombinationsPerTransaction = 50

// renameImport rewrites one import. It works on distinct combinations of
// values rather than on rows, which keeps memory bounded by the variety of an
// import rather than its size. Every read finishes before any write starts,
// because interning a new value and the update transaction both need the
// account's single writer connection.
func renameImport(ctx context.Context, db *sql.DB, cache *intern.Cache, record Import, location *time.Location, apply bool) (RenameResult, error) {
	result := RenameResult{ImportID: record.ID, Source: record.Source}

	combinations, err := readStoredCombinations(ctx, db, record.ID)
	if err != nil {
		return result, err
	}

	values, err := readStoredValues(ctx, db, combinations)
	if err != nil {
		return result, err
	}

	counts := map[[3]string]int64{}
	var renamed []renamedCombination

	for _, combination := range combinations {
		changed, after := normaliseStored(record.Source, combination.ids, values)
		if !changed {
			continue
		}

		renamed = append(renamed, renamedCombination{ids: combination.ids, values: after})
		result.Rows += combination.rows

		for i, column := range renameColumns {
			from := values[i][combination.ids[i]]
			if after[i] != from {
				counts[[3]string{column.dimension, from, after[i]}] += combination.rows
			}
		}
	}

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

	if !apply || len(renamed) == 0 {
		return result, nil
	}

	// Rows are matched by their old values, so a combination renamed into
	// another one's old values would be rewritten twice. Normalise is
	// idempotent, which rules that out; this refuses to write if it ever is not.
	old := make(map[string]bool, len(renamed))
	for _, combination := range renamed {
		old[fmt.Sprint(combination.ids)] = true
	}

	// Each update is the new ids, then the import id, then the old ids, in the
	// order the statement binds them.
	updates := make([][]any, 0, len(renamed))
	for _, combination := range renamed {
		args := make([]any, 0, 2*len(renameColumns)+1)
		ids := make([]int64, 0, len(renameColumns))

		for c, column := range renameColumns {
			id, err := cache.ID(ctx, column.table, combination.values[c])
			if err != nil {
				return result, fmt.Errorf("dataio: rename import %d: %w", record.ID, err)
			}

			args = append(args, id)
			ids = append(ids, id)
		}

		if old[fmt.Sprint(ids)] {
			return result, fmt.Errorf("dataio: rename import %d: %v renames into values another combination is renamed from; nothing was written",
				record.ID, combination.values)
		}

		args = append(args, record.ID)
		for _, id := range combination.ids {
			args = append(args, id)
		}

		updates = append(updates, args)
	}

	if err := writeRenamedCombinations(ctx, db, record.ID, updates); err != nil {
		return result, err
	}

	if err := SummariseImport(ctx, db, record.ID, record.SiteID, location); err != nil {
		return result, err
	}

	return result, nil
}

// normaliseStored runs one combination of stored ids through Normalise and
// reports whether any value changed, with every value after it in
// renameColumns order.
func normaliseStored(origin string, ids []int64, values []map[int64]string) (bool, []string) {
	dimensions := make(map[string]string, len(renameColumns))
	for i, column := range renameColumns {
		if ids[i] != intern.EmptyID {
			dimensions[column.dimension] = values[i][ids[i]]
		}
	}

	normalised := Normalise(origin, dimensions)

	changed := false
	after := make([]string, len(renameColumns))

	for i, column := range renameColumns {
		after[i] = normalised[column.dimension]
		if after[i] != values[i][ids[i]] {
			changed = true
		}
	}

	return changed, after
}

// renameColumnNames lists the dimension columns in renameColumns order.
func renameColumnNames() []string {
	names := make([]string, 0, len(renameColumns))
	for _, column := range renameColumns {
		names = append(names, column.column)
	}

	return names
}

// readStoredCombinations reads every distinct combination of dimension ids one
// import's rows hold, with the number of rows holding each.
func readStoredCombinations(ctx context.Context, db *sql.DB, importID int64) ([]storedCombination, error) {
	columns := strings.Join(renameColumnNames(), ", ")

	rows, err := db.QueryContext(ctx,
		"SELECT "+columns+", COUNT(*) FROM "+query.ImportedTable+" WHERE import_id = ? GROUP BY "+columns, importID)
	if err != nil {
		return nil, fmt.Errorf("dataio: read import %d values: %w", importID, err)
	}
	defer func() { _ = rows.Close() }()

	var combinations []storedCombination

	for rows.Next() {
		combination := storedCombination{ids: make([]int64, len(renameColumns))}

		targets := make([]any, 0, len(renameColumns)+1)
		for i := range combination.ids {
			targets = append(targets, &combination.ids[i])
		}
		targets = append(targets, &combination.rows)

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("dataio: read import %d values: %w", importID, err)
		}

		combinations = append(combinations, combination)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataio: read import %d values: %w", importID, err)
	}

	return combinations, nil
}

// readStoredValues looks up the value behind every id the combinations use,
// one map per column in renameColumns order. Id 0 is the empty value in every
// table.
func readStoredValues(ctx context.Context, db *sql.DB, combinations []storedCombination) ([]map[int64]string, error) {
	values := make([]map[int64]string, len(renameColumns))

	for i, column := range renameColumns {
		values[i] = map[int64]string{intern.EmptyID: ""}

		for _, combination := range combinations {
			id := combination.ids[i]
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

// writeRenamedCombinations rewrites every row holding each renamed combination
// of one import, a bounded number of combinations per transaction.
func writeRenamedCombinations(ctx context.Context, db *sql.DB, importID int64, updates [][]any) error {
	names := renameColumnNames()

	assignments := make([]string, 0, len(names))
	matches := make([]string, 0, len(names))
	for _, name := range names {
		assignments = append(assignments, name+" = ?")
		matches = append(matches, name+" = ?")
	}

	statement := "UPDATE " + query.ImportedTable + " SET " + strings.Join(assignments, ", ") +
		" WHERE import_id = ? AND " + strings.Join(matches, " AND ")

	for start := 0; start < len(updates); start += renameCombinationsPerTransaction {
		end := min(start+renameCombinationsPerTransaction, len(updates))

		if err := writeRenamedBatch(ctx, db, importID, statement, updates[start:end]); err != nil {
			return err
		}
	}

	return nil
}

// writeRenamedBatch applies one batch of updates in one transaction.
func writeRenamedBatch(ctx context.Context, db *sql.DB, importID int64, statement string, updates [][]any) (err error) {
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

	for _, args := range updates {
		if _, err := prepared.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("dataio: rename import %d: %w", importID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dataio: rename import %d: %w", importID, err)
	}

	return nil
}
