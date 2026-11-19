//
// rename_test.go
// Tests for rewriting the values of imports already on disk.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// storedImportRow is one roll-up row written straight to disk, bypassing the
// importer's normalisation, the way rows written before it existed look.
type storedImportRow struct {
	day      int
	values   map[intern.Dimension]string
	visitors int64
}

// writeStoredImport records a completed import holding the given rows, with
// its week and month summaries built, and returns its id.
func writeStoredImport(t *testing.T, account *accounts.Account, source, status string, covered []string, rows []storedImportRow) int64 {
	t.Helper()

	ctx := context.Background()

	record, err := CreateImport(ctx, account.Writer(), 1, source, source+".zip", fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	writer, err := NewWriter(account.Writer(), account.Intern, record, covered)
	if err != nil {
		t.Fatal(err)
	}

	columns := map[intern.Dimension]string{
		intern.OS: "os_id", intern.OSVersion: "os_version_id", intern.Browser: "browser_id",
		intern.Source: "source_id", intern.UTMMedium: "utm_medium_id",
	}

	for _, row := range rows {
		ids := map[string]int64{}
		for dimension, value := range row.values {
			id, err := account.Intern.ID(ctx, dimension, value)
			if err != nil {
				t.Fatal(err)
			}
			ids[columns[dimension]] = id
		}

		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO imported_rollups
				(import_id, site_id, timestamp, covered, os_id, os_version_id, browser_id, source_id, utm_medium_id,
				 visitors, visits, pageviews)
			VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ID, time.Date(2026, 8, row.day, 0, 0, 0, 0, time.UTC).Unix(), int64(writer.covered),
			ids["os_id"], ids["os_version_id"], ids["browser_id"], ids["source_id"], ids["utm_medium_id"],
			row.visitors, row.visitors, row.visitors); err != nil {
			t.Fatal(err)
		}
	}

	switch status {
	case StatusCompleted:
		if err := SummariseImport(ctx, account.Writer(), record.ID, 1, time.UTC); err != nil {
			t.Fatal(err)
		}
		if err := CompleteImport(ctx, account.Writer(), record.ID, covered, 0, 0, int64(len(rows)), fixtureNow); err != nil {
			t.Fatal(err)
		}
	case StatusRunning:
		if _, err := account.Writer().ExecContext(ctx, "UPDATE imports SET status = ? WHERE id = ?", StatusRunning, record.ID); err != nil {
			t.Fatal(err)
		}
	}

	return record.ID
}

// openRenameAccount opens an isolated account database.
func openRenameAccount(t *testing.T) *accounts.Account {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	return account
}

// wideVisitors sums the month-summary visitors one import holds for each value
// of one dimension column.
func wideVisitors(t *testing.T, db *sql.DB, importID int64, column, table string) map[string]int64 {
	t.Helper()

	rows, err := db.Query("SELECT COALESCE(d.value, ''), SUM(w.visitors) FROM imported_wide w "+
		"LEFT JOIN "+table+" d ON d.id = w."+column+" "+
		"WHERE w.import_id = ? AND w.grain = (SELECT MAX(grain) FROM imported_wide WHERE import_id = ?) "+
		"GROUP BY 1", importID, importID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var value string
		var visitors int64
		if err := rows.Scan(&value, &visitors); err != nil {
			t.Fatal(err)
		}
		out[value] = visitors
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

// TestRenameImports checks a dry run writes nothing, a real run renames only
// completed imports' rows and rebuilds their summaries so two spellings of one
// system become one row, and a second run finds nothing left to do.
func TestRenameImports(t *testing.T) {
	ctx := context.Background()
	account := openRenameAccount(t)
	db := account.Writer()

	systems := []string{"visit:os", "visit:os_version"}

	archive := writeStoredImport(t, account, SourcePlausible, StatusCompleted, systems, []storedImportRow{
		{day: 3, values: map[intern.Dimension]string{intern.OS: "Mac", intern.OSVersion: "10.15"}, visitors: 3},
		{day: 3, values: map[intern.Dimension]string{intern.OS: "macOS", intern.OSVersion: "10.15"}, visitors: 2},
		{day: 4, values: map[intern.Dimension]string{intern.OS: "Windows", intern.OSVersion: "10.0"}, visitors: 4},
	})

	report := writeStoredImport(t, account, SourceGA4, StatusCompleted, []string{"visit:source", "visit:utm_medium"}, []storedImportRow{
		{day: 5, values: map[intern.Dimension]string{intern.Source: "(direct)", intern.UTMMedium: "(none)"}, visitors: 6},
		{day: 5, values: map[intern.Dimension]string{intern.Source: "google", intern.UTMMedium: "organic"}, visitors: 7},
		{day: 6, values: map[intern.Dimension]string{intern.Source: "Newsletter", intern.UTMMedium: "email"}, visitors: 1},
	})

	running := writeStoredImport(t, account, SourcePlausible, StatusRunning, []string{"visit:browser"}, []storedImportRow{
		{day: 7, values: map[intern.Dimension]string{intern.Browser: "Samsung Browser"}, visitors: 1},
	})

	dry, err := RenameImports(ctx, db, account.Intern, 1, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(dry) != 2 {
		t.Fatalf("dry run covered %d imports, want the 2 completed ones: %+v", len(dry), dry)
	}

	want := map[int64][]Renamed{
		archive: {
			{"visit:os", "Mac", "macOS", 1},
			{"visit:os_version", "10.0", "10", 1},
		},
		report: {
			{"visit:source", "(direct)", "", 1},
			{"visit:source", "google", "Google", 1},
			{"visit:utm_medium", "(none)", "", 1},
			{"visit:utm_medium", "organic", "", 1},
		},
	}

	for _, result := range dry {
		expected := want[result.ImportID]
		if len(result.Renamed) != len(expected) {
			t.Fatalf("import %d would rename %+v, want %+v", result.ImportID, result.Renamed, expected)
		}
		for i := range expected {
			if result.Renamed[i] != expected[i] {
				t.Errorf("import %d rename %d = %+v, want %+v", result.ImportID, i, result.Renamed[i], expected[i])
			}
		}
		if result.Rows != 2 {
			t.Errorf("import %d would change %d rows, want 2", result.ImportID, result.Rows)
		}
	}

	if got := wideVisitors(t, db, archive, "os_id", "dim_os"); got["Mac"] != 3 || got["macOS"] != 2 {
		t.Fatalf("a dry run changed the summaries: %v", got)
	}

	applied, err := RenameImports(ctx, db, account.Intern, 1, time.UTC, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || applied[0].Rows != 2 || applied[1].Rows != 2 {
		t.Fatalf("applied = %+v", applied)
	}

	if got := wideVisitors(t, db, archive, "os_id", "dim_os"); len(got) != 2 || got["macOS"] != 5 || got["Windows"] != 4 {
		t.Errorf("summaries after renaming = %v, want macOS 5 and Windows 4", got)
	}
	if got := wideVisitors(t, db, archive, "os_version_id", "dim_os_version"); got["10"] != 4 || got["10.0"] != 0 {
		t.Errorf("system versions after renaming = %v", got)
	}
	if got := wideVisitors(t, db, report, "source_id", "dim_source"); got[""] != 6 || got["Google"] != 7 || got["Newsletter"] != 1 {
		t.Errorf("sources after renaming = %v", got)
	}
	if got := wideVisitors(t, db, report, "utm_medium_id", "dim_utm_medium"); got[""] != 13 || got["email"] != 1 {
		t.Errorf("mediums after renaming = %v", got)
	}

	if got := storedValue(t, db, running, time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC).Unix(), "visit:browser"); got != "Samsung Browser" {
		t.Errorf("a running import's row was renamed to %q", got)
	}

	if got := countRows(t, db, "SELECT COUNT(*) FROM imported_rollups"); got != 7 {
		t.Errorf("renaming left %d roll-up rows, want the 7 written", got)
	}

	again, err := RenameImports(ctx, db, account.Intern, 1, time.UTC, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range again {
		if result.Rows != 0 || len(result.Renamed) != 0 {
			t.Errorf("a second run renamed %+v in import %d", result.Renamed, result.ImportID)
		}
	}
}
