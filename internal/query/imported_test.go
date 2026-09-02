//
// imported_test.go
// The behaviour the incumbent gets wrong: imported data that survives a filter.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// importedDay is the local midnight an imported roll-up row is stamped at. It
// is the day before the fixture's own traffic so the two never overlap and an
// assertion can tell them apart.
var importedDay = time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

// seedImport writes one import and its roll-up rows. The dimension list is the
// shape of the sheet the rows came from — the thing the incumbent does not
// record, and the reason their imported data disappears under a filter.
func seedImport(t *testing.T, account *accounts.Account, importID int64, dimensions []string, rows []importedRow) {
	t.Helper()

	ctx := context.Background()

	_, err := account.Writer().ExecContext(ctx,
		"INSERT INTO imports (id, site_id, source, label, status, created_at) VALUES (?,1,'csv','fixture','completed',0)",
		importID)
	if err != nil {
		t.Fatal(err)
	}

	covered, err := ImportedCoverage(dimensions)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range rows {
		columns := "import_id, site_id, timestamp, covered, visitors, visits, pageviews, events, bounces, duration_total"
		values := []any{importID, 1, row.timestamp, int64(covered),
			row.visitors, row.visits, row.pageviews, row.events, row.bounces, row.duration}

		for name, value := range row.dimensions {
			column, ok := ImportedColumn(name)
			if !ok {
				t.Fatalf("%q is not an imported dimension", name)
			}

			dimension, _ := ImportedInterned(name)

			id, err := account.Intern.ID(ctx, dimension, value)
			if err != nil {
				t.Fatal(err)
			}

			columns += ", " + column
			values = append(values, id)
		}

		placeholders := ""
		for i := range values {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
		}

		if _, err := account.Writer().ExecContext(ctx,
			"INSERT INTO imported_rollups ("+columns+") VALUES ("+placeholders+")", values...); err != nil {
			t.Fatal(err)
		}
	}
}

// importedRow is one roll-up row of a fixture.
type importedRow struct {
	timestamp  int64
	dimensions map[string]string
	visitors   int64
	visits     int64
	pageviews  int64
	events     int64
	bounces    int64
	duration   int64
}

// importedRange covers the imported day and the fixture's own two days.
func importedRange() DateRange {
	return DateRange{
		Preset: RangeCustom,
		Start:  time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		End:    time.Date(2026, 8, 30, 23, 59, 59, 0, time.UTC),
	}
}

// TestImportedDataSurvivesAFilter is the whole point of storing imports as
// roll-up rows with a coverage mask rather than as per-dimension marginals.
//
// The incumbent imports marginals, so their stored rows carry no record of
// which dimensions they describe; applying any filter therefore matches none of
// them and the imported half of every number silently reads zero. A customer
// with sixty million pageviews called the feature useless in public over
// exactly this. Here a filter on a dimension the import carries narrows the
// imported rows the same way it narrows native ones.
func TestImportedDataSurvivesAFilter(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	seedImport(t, account, 1, []string{"visit:source"}, []importedRow{
		{timestamp: importedDay.Unix(), dimensions: map[string]string{"visit:source": "Google"},
			visitors: 40, visits: 50, pageviews: 100, events: 100, bounces: 10, duration: 600},
		{timestamp: importedDay.Unix(), dimensions: map[string]string{"visit:source": "Twitter"},
			visitors: 5, visits: 6, pageviews: 12, events: 12, bounces: 3, duration: 60},
	})

	filtered := Query{
		SiteIDs:   []int64{1},
		Metrics:   []string{"pageviews"},
		DateRange: importedRange(),
		Filters:   []Filter{{Operator: OpIs, Dimension: "visit:source", Values: []string{"Google"}}},
	}

	// Native only: the fixture has four Google pageviews across visits 1 and 2.
	native := run(t, engine, filtered)
	if got := native.Results[0].Metrics[0]; got != 4 {
		t.Fatalf("native pageviews under a source filter = %v, want 4", got)
	}

	filtered.Include.Imports = true

	withImports := run(t, engine, filtered)
	if got := withImports.Results[0].Metrics[0]; got != 104 {
		t.Fatalf("filtered pageviews with imports = %v, want 104 — the imported rows were dropped by the filter", got)
	}

	if len(withImports.Meta.ImportGaps) != 0 {
		t.Fatalf("a filter the import can answer reported gaps: %+v", withImports.Meta.ImportGaps)
	}

	// The other source is excluded rather than ignored: a filter has to narrow
	// imported data, not merely fail to erase it.
	filtered.Filters[0].Values = []string{"Twitter"}

	twitter := run(t, engine, filtered)
	if got := twitter.Results[0].Metrics[0]; got != 13 {
		t.Fatalf("Twitter pageviews with imports = %v, want 13 (12 imported + 1 native)", got)
	}
}

// TestImportedGapIsLabelledNotZero covers the other half of the promise. A
// filter on a dimension the import genuinely does not carry cannot be answered
// from imported rows — and the honest response is to say so with the volume
// attached, not to contribute a zero that looks like a real measurement.
func TestImportedGapIsLabelledNotZero(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	seedImport(t, account, 1, []string{"visit:source"}, []importedRow{
		{timestamp: importedDay.Unix(), dimensions: map[string]string{"visit:source": "Google"},
			visitors: 40, visits: 50, pageviews: 100, events: 100},
	})

	result := run(t, engine, Query{
		SiteIDs:   []int64{1},
		Metrics:   []string{"pageviews"},
		DateRange: importedRange(),
		Filters:   []Filter{{Operator: OpIs, Dimension: "visit:country", Values: []string{"US"}}},
		Include:   Include{Imports: true},
	})

	if len(result.Meta.ImportGaps) != 1 {
		t.Fatalf("import gaps = %+v, want exactly one naming visit:country", result.Meta.ImportGaps)
	}

	gap := result.Meta.ImportGaps[0]

	if gap.Dimension != "visit:country" {
		t.Fatalf("gap names %q, want visit:country", gap.Dimension)
	}

	if gap.Pageviews != 100 {
		t.Fatalf("gap volume = %v, want the 100 imported pageviews that are outside the answer", gap.Pageviews)
	}

	if gap.Reason == "" {
		t.Fatal("a gap with no reason is a number the reader cannot act on")
	}
}

// TestImportedSheetsAreNotDoubleCounted guards the arithmetic that makes a full
// export importable. A real export carries a totals sheet and a sources sheet
// describing the same days, and adding both together would double a customer's
// history the moment the import finished.
func TestImportedSheetsAreNotDoubleCounted(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	seedImport(t, account, 1, nil, []importedRow{
		{timestamp: importedDay.Unix(), visitors: 45, visits: 56, pageviews: 112, events: 112},
	})

	seedImport(t, account, 2, []string{"visit:source"}, []importedRow{
		{timestamp: importedDay.Unix(), dimensions: map[string]string{"visit:source": "Google"},
			visitors: 40, visits: 50, pageviews: 100, events: 100},
		{timestamp: importedDay.Unix(), dimensions: map[string]string{"visit:source": "Twitter"},
			visitors: 5, visits: 6, pageviews: 12, events: 12},
	})

	// Both imports describe the same day. An unfiltered total reads the least
	// detailed shape each one holds, so the two contribute 112 each and nothing
	// is counted twice inside either.
	result := run(t, engine, Query{
		SiteIDs:   []int64{1},
		Metrics:   []string{"pageviews"},
		DateRange: importedRange(),
		Include:   Include{Imports: true},
	})

	// Seven native pageviews in the fixture, plus 112 from each import.
	if got := result.Results[0].Metrics[0]; got != 231 {
		t.Fatalf("total pageviews = %v, want 231 (7 native + 112 + 112)", got)
	}
}

// TestImportedComparisonCoversBothPeriods is the +450% bug. An incumbent's
// period comparison put imported data in the headline and native-only data in
// the denominator, and once reported a 450 per cent rise where the truth was a
// 34 per cent fall. Both periods run through the same executor here, so an
// import that lands in the earlier window is counted in it.
func TestImportedComparisonCoversBothPeriods(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	// A hundred imported pageviews in the earlier period only.
	seedImport(t, account, 1, nil, []importedRow{
		{timestamp: time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC).Unix(), pageviews: 100, visitors: 40, visits: 50},
	})

	result := run(t, engine, Query{
		SiteIDs: []int64{1},
		Metrics: []string{"pageviews"},
		DateRange: DateRange{
			Preset: RangeCustom,
			Start:  time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
			End:    time.Date(2026, 8, 30, 23, 59, 59, 0, time.UTC),
		},
		Include: Include{
			Imports:     true,
			Comparisons: &Comparison{Mode: ComparePreviousPeriod},
		},
	})

	row := result.Results[0]

	if row.Comparison == nil {
		t.Fatal("no comparison came back")
	}

	if row.Comparison.Metrics[0] != 100 {
		t.Fatalf("the earlier period counted %v pageviews, want the 100 imported ones — "+
			"a comparison that reads native-only for the denominator reports a rise where there was a fall",
			row.Comparison.Metrics[0])
	}
}

// TestPathCleaningIsRetroactive is the feature the incumbent declined. The
// stored rows are untouched: the map turns one interned id into another at
// query time, so the same history reads differently the moment a rule is saved
// and reads the old way again the moment it is removed.
func TestPathCleaningIsRetroactive(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	ctx := context.Background()

	// Two distinct pages in the stored data, both of which are really one route.
	for _, path := range []string{"/users/aaaa", "/users/bbbb"} {
		id, err := account.Intern.ID(ctx, intern.Pathname, path)
		if err != nil {
			t.Fatal(err)
		}

		_, err = account.Writer().ExecContext(ctx, `
			INSERT INTO events (site_id, timestamp, name_id, user_id, session_id, pathname_id, scroll_depth)
			VALUES (1, ?, ?, ?, 1, ?, 255)`,
			at(30, 11, 0), mustID(t, account, intern.EventName, "pageview"), visitorA, id)
		if err != nil {
			t.Fatal(err)
		}
	}

	byPage := Query{
		SiteIDs:    []int64{1},
		Metrics:    []string{"pageviews"},
		Dimensions: []string{"event:page"},
		DateRange:  DateRange{Preset: RangeDay},
	}

	before := run(t, engine, byPage)
	if countRows(before, "/users/aaaa") != 1 || countRows(before, "/users/bbbb") != 1 {
		t.Fatalf("before cleaning the two paths should be two rows, got %+v", before.Results)
	}

	// The rules are materialised as an id-to-id map. Nothing stored is
	// rewritten, which is what makes removing a rule put the paths back.
	target := mustID(t, account, intern.Pathname, "/users/:id")

	for _, path := range []string{"/users/aaaa", "/users/bbbb"} {
		source := mustID(t, account, intern.Pathname, path)

		if _, err := account.Writer().ExecContext(ctx,
			"INSERT INTO path_clean_map (site_id, source_id, target_id) VALUES (1, ?, ?)", source, target); err != nil {
			t.Fatal(err)
		}
	}

	after := run(t, engine, byPage)

	if countRows(after, "/users/:id") != 2 {
		t.Fatalf("after cleaning the two paths should be one row of 2 pageviews, got %+v", after.Results)
	}

	if countRows(after, "/users/aaaa") != 0 {
		t.Fatal("the raw path is still being reported, so the rule is not being applied at query time")
	}

	// A filter has to see the cleaned path too, or a report groups by one thing
	// and filters on another.
	filtered := byPage
	filtered.Dimensions = nil
	filtered.Filters = []Filter{{Operator: OpIs, Dimension: "event:page", Values: []string{"/users/:id"}}}

	result := run(t, engine, filtered)
	if got := result.Results[0].Metrics[0]; got != 2 {
		t.Fatalf("filtering on the cleaned path counted %v pageviews, want 2", got)
	}
}

// countRows finds one label's first metric in a result, or zero.
func countRows(result *Result, label string) float64 {
	for _, row := range result.Results {
		if len(row.Dimensions) > 0 && row.Dimensions[0] == label {
			return row.Metrics[0]
		}
	}

	return 0
}

// mustID interns a value or fails the test.
func mustID(t *testing.T, account *accounts.Account, dimension intern.Dimension, value string) int64 {
	t.Helper()

	id, err := account.Intern.ID(context.Background(), dimension, value)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

// TestAGoalFilterIsAnImportGapNotAnError checks that a configured goal, which
// no daily total can express, is reported as the volume it leaves out rather
// than refused. Refusing it would make the goals list unusable the moment an
// account imported its history.
func TestAGoalFilterIsAnImportGapNotAnError(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	seedImport(t, account, 1, []string{"visit:source"}, []importedRow{
		{timestamp: importedDay.Unix(), dimensions: map[string]string{"visit:source": "Google"},
			visitors: 40, visits: 50, pageviews: 100, events: 100},
	})

	goalID := seedSignupGoal(t, account)

	result := run(t, engine, Query{
		SiteIDs:   []int64{1},
		Metrics:   []string{"pageviews"},
		DateRange: importedRange(),
		Filters:   []Filter{{Operator: OpIs, Dimension: "event:goal", Values: []string{strconv.FormatInt(goalID, 10)}}},
		Include:   Include{Imports: true},
	})

	if len(result.Meta.ImportGaps) != 1 || result.Meta.ImportGaps[0].Dimension != "event:goal" {
		t.Fatalf("import gaps = %+v, want exactly one naming event:goal", result.Meta.ImportGaps)
	}

	if result.Meta.ImportGaps[0].Pageviews != 100 {
		t.Fatalf("gap volume = %v, want the 100 imported pageviews outside the answer", result.Meta.ImportGaps[0].Pageviews)
	}
}
