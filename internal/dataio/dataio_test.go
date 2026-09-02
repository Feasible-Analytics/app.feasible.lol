//
// dataio_test.go
// The round trip: export a site, import the archive, get the same numbers back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// fixtureNow is the instant the round trip resolves its date range against.
var fixtureNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// plausibleFixture is a minimal, complete native Plausible export. Its headers
// mirror the real Optimus Cafe archive, including every provider-specific
// column that a generic CSV importer would otherwise reject or misinterpret.
var plausibleFixture = map[string]string{
	"imported_browsers_20260828_20260828.csv":          "date,browser,browser_version,visitors,visits,visit_duration,bounces,pageviews\n2026-08-28,Chrome,140,2,2,90,1,5\n",
	"imported_custom_events_20260828_20260828.csv":     "date,name,link_url,path,visitors,events\n2026-08-28,Outbound Link: Click,https://example.org,/pricing,2,3\n",
	"imported_custom_props_20260828_20260828.csv":      "date,property,value,visitors,events\n2026-08-28,position,hero,2,4\n",
	"imported_devices_20260828_20260828.csv":           "date,device,visitors,visits,visit_duration,bounces,pageviews\n2026-08-28,Desktop,2,2,90,1,5\n",
	"imported_entry_pages_20260828_20260828.csv":       "date,entry_page,visitors,entrances,visit_duration,bounces,pageviews\n2026-08-28,/,2,2,90,1,5\n",
	"imported_exit_pages_20260828_20260828.csv":        "date,exit_page,visitors,visit_duration,exits,bounces,pageviews\n2026-08-28,/pricing,2,90,2,1,5\n",
	"imported_locations_20260828_20260828.csv":         "date,country,region,city,visitors,visits,visit_duration,bounces,pageviews\n2026-08-28,US,US-OR,5739936,2,2,90,1,5\n",
	"imported_operating_systems_20260828_20260828.csv": "date,operating_system,operating_system_version,visitors,visits,visit_duration,bounces,pageviews\n2026-08-28,Mac,15,2,2,90,1,5\n",
	"imported_pages_20260828_20260828.csv":             "date,hostname,page,visits,visitors,pageviews,total_scroll_depth,total_scroll_depth_visits,total_time_on_page,total_time_on_page_visits\n2026-08-28,example.com,/pricing,2,2,5,120,2,60,2\n",
	"imported_sources_20260828_20260828.csv":           "date,source,referrer,utm_source,utm_medium,utm_campaign,utm_content,utm_term,pageviews,visitors,visits,visit_duration,bounces\n2026-08-28,Google,google.com,newsletter,email,launch,hero,analytics,5,2,2,90,1\n",
	"imported_visitors_20260828_20260828.csv":          "date,visitors,pageviews,bounces,visits,visit_duration\n2026-08-28,2,5,1,2,90\n",
}

// at builds a timestamp inside the fixture's window.
func at(day, hour int) int64 {
	return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC).Unix()
}

// visit is one whole session and its pageviews, written the way the ingest path
// would write them. Sessions never cross midnight in this fixture, because a
// visit that spans two local days is a real edge case with its own tests and
// would fail this one for a reason that is not about the round trip.
type visit struct {
	id        int64
	user      int64
	startedAt int64
	duration  int
	bounce    int
	pages     []string
	source    string
	country   string
	browser   string
	device    string
}

// fixtureVisits is the site the round trip exports and re-imports.
var fixtureVisits = []visit{
	{id: 1, user: 101, startedAt: at(28, 10), duration: 120, bounce: 0,
		pages: []string{"/home", "/pricing", "/pricing"}, source: "Google", country: "US", browser: "Chrome", device: "Desktop"},
	{id: 2, user: 102, startedAt: at(28, 11), duration: 0, bounce: 1,
		pages: []string{"/pricing"}, source: "Google", country: "GB", browser: "Firefox", device: "Mobile"},
	{id: 3, user: 101, startedAt: at(29, 9), duration: 60, bounce: 0,
		pages: []string{"/home", "/about"}, source: "Twitter", country: "US", browser: "Chrome", device: "Desktop"},
	{id: 4, user: 103, startedAt: at(29, 10), duration: 0, bounce: 1,
		pages: []string{"/home"}, source: "", country: "CA", browser: "Safari", device: "Mobile"},
	{id: 5, user: 104, startedAt: at(29, 14), duration: 300, bounce: 0,
		pages: []string{"/blog", "/blog/one", "/home"}, source: "Google", country: "US", browser: "Chrome", device: "Tablet"},
}

// TestCSVRoundTrip is the acceptance test for both halves of the feature: an
// export has to carry enough to reconstruct the numbers, and an import has to
// put them back where a report can find them.
//
// The reconstructed site is a different site id in the same account, so the two
// are read by the same engine and the comparison cannot be fooled by a shared
// row somewhere.
func TestCSVRoundTrip(t *testing.T) {
	ctx := context.Background()

	manager := accounts.NewManager(t.TempDir())
	defer func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	}()

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	seedSite(t, account, 1)

	archivePath := filepath.Join(t.TempDir(), "export.zip")

	if _, err := BuildExport(ctx, account.Reader(), 1, time.UTC, archivePath); err != nil {
		t.Fatal(err)
	}

	assertArchiveShape(t, archivePath)

	// Site 2 has no traffic of its own, so every number it reports comes from
	// the import.
	record, err := CreateImport(ctx, account.Writer(), 2, SourceCSV, "export.zip", fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	sources, closeArchive, err := SourcesFromUpload(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeArchive(); err != nil {
			t.Errorf("close uploaded archive: %v", err)
		}
	}()

	err = ImportCSV(ctx, account.Writer(), account.Intern, record, sources, time.UTC,
		func() time.Time { return fixtureNow })
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := GetImport(ctx, account.Reader(), 2, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if reloaded.Status != StatusCompleted {
		t.Fatalf("import status = %q (%s), want completed", reloaded.Status, reloaded.Failure)
	}

	if reloaded.RowsWritten == 0 {
		t.Fatal("the import wrote no rows")
	}

	engine := query.New(account.Reader())
	engine.Now = func() time.Time { return fixtureNow }

	// The totals, day by day, and then the same numbers broken down by every
	// dimension the export carries.
	compare(t, engine, "daily totals", nil, nil)
	compare(t, engine, "by source", []string{"visit:source"}, nil)
	compare(t, engine, "by country", []string{"visit:country"}, nil)
	compare(t, engine, "by browser", []string{"visit:browser"}, nil)
	compare(t, engine, "by page", []string{"event:page"}, nil)
	compare(t, engine, "by entry page", []string{"visit:entry_page"}, nil)

	// And a filtered read, which is the case the incumbent's import cannot
	// answer at all.
	compare(t, engine, "filtered by source", nil,
		[]query.Filter{{Operator: query.OpIs, Dimension: "visit:source", Values: []string{"Google"}}})
}

// TestPlausibleArchiveMigration proves the native eleven-file archive is
// detected and imported without editing, while preserving Plausible-only
// custom properties, outbound URLs, engagement denominators and scroll depth.
func TestPlausibleArchiveMigration(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "plausible-export.zip")
	writeArchive(t, archivePath, plausibleFixture)

	source, err := ClassifyUpload(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if source != SourcePlausible {
		t.Fatalf("source = %q, want %q", source, SourcePlausible)
	}

	record, account := importPlausibleArchive(t, archivePath)
	if record.ProgressDone != len(plausibleFixture) || record.ProgressTotal != len(plausibleFixture) {
		t.Fatalf("progress = %d/%d, want %d/%d", record.ProgressDone, record.ProgressTotal,
			len(plausibleFixture), len(plausibleFixture))
	}

	var propertyKey, propertyValue string
	var engagement, engagementVisits, scrollTotal, scrollVisits int64
	if err := account.Reader().QueryRow(`
		SELECT property_key, property_value, engagement_total, engagement_visits,
		       scroll_depth_total, scroll_depth_visits
		FROM imported_rollups
		WHERE import_id = ? AND property_key = 'position'`, record.ID).Scan(
		&propertyKey, &propertyValue, &engagement, &engagementVisits, &scrollTotal, &scrollVisits); err != nil {
		t.Fatal(err)
	}
	if propertyKey != "position" || propertyValue != "hero" {
		t.Fatalf("property = %q:%q, want position:hero", propertyKey, propertyValue)
	}

	if err := account.Reader().QueryRow(`
		SELECT engagement_total, engagement_visits, scroll_depth_total, scroll_depth_visits
		FROM imported_rollups
		WHERE import_id = ? AND engagement_visits > 0`, record.ID).Scan(
		&engagement, &engagementVisits, &scrollTotal, &scrollVisits); err != nil {
		t.Fatal(err)
	}
	if engagement != 60_000 || engagementVisits != 2 || scrollTotal != 120 || scrollVisits != 2 {
		t.Fatalf("Plausible metrics = engagement %d/%d scroll %d/%d, want 60000/2 and 120/2",
			engagement, engagementVisits, scrollTotal, scrollVisits)
	}

	engine := query.New(account.Reader())
	worker := Workers{Now: func() time.Time { return fixtureNow }}
	if err := worker.registerImportedProperties(context.Background(), account.Writer(), record); err != nil {
		t.Fatal(err)
	}
	if err := worker.registerImportedGoals(context.Background(), account.Writer(), record); err != nil {
		t.Fatal(err)
	}

	allowed, err := goals.Allowed(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 1 || allowed[0].Name != "position" || allowed[0].Scope != goals.ScopeEvent {
		t.Fatalf("registered Plausible properties = %+v, want event-scoped position", allowed)
	}

	goalList, err := goals.List(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(goalList) != 1 || goalList[0].EventName != "Outbound Link: Click" || goalList[0].CreatedAt != record.RangeStart {
		t.Fatalf("registered Plausible goals = %+v, want imported outbound-link goal", goalList)
	}

	goalResult, err := goals.Report(context.Background(), account.Reader(), engine, goals.ReportRequest{
		SiteID: 1,
		DateRange: query.DateRange{Preset: query.RangeCustom,
			Start: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(goalResult.Rows) != 1 || goalResult.Rows[0].TotalConversions != 3 {
		t.Fatalf("imported Plausible goal report = %+v, want three conversions", goalResult.Rows)
	}

	base := query.Query{
		SiteIDs: []int64{1}, Include: query.Include{Imports: true},
		DateRange: query.DateRange{Preset: query.RangeCustom,
			Start: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)},
		Pagination: query.Pagination{Limit: 100},
	}

	pageQuery := base
	pageQuery.Metrics = []string{"time_on_page", "scroll_depth"}
	pageQuery.Dimensions = []string{"event:page"}
	pageResult, err := engine.Run(context.Background(), pageQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(pageResult.Results) != 1 || pageResult.Results[0].Metrics[0] != 30 || pageResult.Results[0].Metrics[1] != 60 {
		t.Fatalf("page engagement result = %+v, want 30 seconds and 60%% scroll depth", pageResult.Results)
	}

	channelQuery := base
	channelQuery.Metrics = []string{"visitors"}
	channelQuery.Dimensions = []string{"visit:channel"}
	channelResult, err := engine.Run(context.Background(), channelQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(channelResult.Results) != 1 || channelResult.Results[0].Dimensions[0] != "Email" || channelResult.Results[0].Metrics[0] != 2 {
		t.Fatalf("derived Plausible channel result = %+v, want Email with two visitors", channelResult.Results)
	}

	propertyQuery := base
	propertyQuery.Metrics = []string{"events"}
	propertyQuery.Dimensions = []string{"event:props:position"}
	propertyResult, err := engine.Run(context.Background(), propertyQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(propertyResult.Results) != 1 || propertyResult.Results[0].Dimensions[0] != "hero" || propertyResult.Results[0].Metrics[0] != 4 {
		t.Fatalf("property result = %+v, want hero with 4 events", propertyResult.Results)
	}

	urlQuery := base
	urlQuery.Metrics = []string{"events"}
	urlQuery.Dimensions = []string{"event:props:url"}
	urlResult, err := engine.Run(context.Background(), urlQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(urlResult.Results) != 1 || urlResult.Results[0].Dimensions[0] != "https://example.org" || urlResult.Results[0].Metrics[0] != 3 {
		t.Fatalf("outbound URL result = %+v, want https://example.org with 3 events", urlResult.Results)
	}

	var before int
	if err := account.Reader().QueryRow("SELECT COUNT(*) FROM imported_rollups WHERE import_id = ?", record.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	retrySources, closeRetry, err := SourcesFromUpload(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportCSV(context.Background(), account.Writer(), account.Intern, record, retrySources,
		time.UTC, func() time.Time { return fixtureNow }); err != nil {
		t.Fatal(err)
	}
	if err := closeRetry(); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := account.Reader().QueryRow("SELECT COUNT(*) FROM imported_rollups WHERE import_id = ?", record.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("retry left %d roll-ups, want the original %d", after, before)
	}
}

// TestUnsafeZIPArchivesAreRejected covers two archive-level attacks before a
// worker reads a CSV: duplicate case-folded names that would double-count one
// table, and extreme expansion that turns a tiny upload into excessive work.
func TestUnsafeZIPArchivesAreRejected(t *testing.T) {
	t.Run("duplicate names", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "duplicates.zip")
		writeArchive(t, path, map[string]string{
			"imported_visitors_20260828_20260828.csv": "date,visitors\n2026-08-28,1\n",
			"IMPORTED_VISITORS_20260828_20260828.CSV": "date,visitors\n2026-08-28,1\n",
		})

		if _, err := ClassifyUpload(path); err == nil || !strings.Contains(err.Error(), "more than one CSV") {
			t.Fatalf("duplicate archive error = %v", err)
		}
	})

	t.Run("extreme expansion", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "expansion.zip")
		writeArchive(t, path, map[string]string{
			"imported_visitors_20260828_20260828.csv": strings.Repeat("0", 4<<20),
		})

		if _, err := ClassifyUpload(path); err == nil || !strings.Contains(err.Error(), "unsafe archive") {
			t.Fatalf("expanding archive error = %v", err)
		}
	})
}

// TestFailedImportRemovesPartialRows ensures a bad later CSV cannot leave the
// successfully committed prefix of an archive visible in customer reports.
func TestFailedImportRemovesPartialRows(t *testing.T) {
	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.CloseAll() })
	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	record, err := CreateImport(context.Background(), account.Writer(), 1, SourcePlausible, "broken.zip", fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewWriter(account.Writer(), account.Intern, record.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(context.Background(), Row{
		Timestamp:  time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC).Unix(),
		Dimensions: map[string]string{}, Metrics: map[string]int64{FieldPageviews: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := FailImport(context.Background(), account.Writer(), record.ID, "later.csv: invalid", fixtureNow); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := account.Reader().QueryRow("SELECT COUNT(*) FROM imported_rollups WHERE import_id = ?", record.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("failed import retained %d partial rows", rows)
	}
}

// TestOptimusCafePlausibleArchive exercises the importer against the supplied
// production-shaped export when its path is provided by the developer. It is
// optional in CI so the customer's analytics archive is never copied into the
// repository or made a build dependency.
func TestOptimusCafePlausibleArchive(t *testing.T) {
	path := os.Getenv("FEASIBLE_PLAUSIBLE_ARCHIVE")
	if path == "" {
		t.Skip("FEASIBLE_PLAUSIBLE_ARCHIVE is not set")
	}

	source, err := ClassifyUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	if source != SourcePlausible {
		t.Fatalf("source = %q, want %q", source, SourcePlausible)
	}

	record, _ := importPlausibleArchive(t, path)
	if record.Status != StatusCompleted || record.ProgressDone != 11 || record.RowsWritten == 0 {
		t.Fatalf("real archive result = status %q, progress %d, rows %d",
			record.Status, record.ProgressDone, record.RowsWritten)
	}
}

// importPlausibleArchive opens an isolated account database and runs one
// Plausible ZIP through the same parser and roll-up writer as the background
// job, returning the completed record for focused assertions.
func importPlausibleArchive(t *testing.T, archivePath string) (*Import, *accounts.Account) {
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

	record, err := CreateImport(context.Background(), account.Writer(), 1, SourcePlausible,
		filepath.Base(archivePath), fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	sources, closeArchive, err := SourcesFromUpload(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeArchive(); err != nil {
			t.Errorf("close Plausible archive: %v", err)
		}
	})

	if err := ImportCSV(context.Background(), account.Writer(), account.Intern, record, sources,
		time.UTC, func() time.Time { return fixtureNow }); err != nil {
		t.Fatal(err)
	}

	completed, err := GetImport(context.Background(), account.Reader(), 1, record.ID)
	if err != nil {
		t.Fatal(err)
	}

	return completed, account
}

// writeArchive creates a deterministic ZIP fixture from named CSV bodies.
func writeArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	archive := zip.NewWriter(file)
	for name, body := range files {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// compare runs the same question against the native site and the reconstructed
// one and insists on identical answers.
func compare(t *testing.T, engine *query.Engine, label string, dimensions []string, filters []query.Filter) {
	t.Helper()

	metrics := []string{"visitors", "visits", "pageviews"}

	// Every comparison is made at day grain, because a day is the grain a
	// roll-up holds. Distinct visitors cannot be re-derived from daily totals —
	// somebody who came on Tuesday and again on Wednesday is one visitor across
	// the week and two rows in any roll-up of days, here or anywhere else — so
	// asking across days would be asking the export to carry something no
	// export of this shape can carry.
	grouped := append([]string{"time:day"}, dimensions...)

	ask := func(siteID int64, imports bool) *query.Result {
		result, err := engine.Run(context.Background(), query.Query{
			SiteIDs:    []int64{siteID},
			Metrics:    metrics,
			Dimensions: grouped,
			Filters:    filters,
			DateRange: query.DateRange{
				Preset: query.RangeCustom,
				Start:  time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC),
				End:    time.Date(2026, 8, 30, 23, 0, 0, 0, time.UTC),
			},
			Include:    query.Include{Imports: imports},
			Pagination: query.Pagination{Limit: 100},
		})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}

		return result
	}

	native := ask(1, false)
	imported := ask(2, true)

	nativeRows := index(native)
	importedRows := index(imported)

	if len(nativeRows) != len(importedRows) {
		t.Fatalf("%s: the export produced %d rows and the import %d — %+v vs %+v",
			label, len(nativeRows), len(importedRows), native.Results, imported.Results)
	}

	for key, want := range nativeRows {
		got, ok := importedRows[key]
		if !ok {
			t.Errorf("%s: %q is missing from the re-imported site", label, key)
			continue
		}

		for i, name := range metrics {
			if got[i] != want[i] {
				t.Errorf("%s: %q %s = %v after the round trip, want %v", label, key, name, got[i], want[i])
			}
		}
	}
}

// index keys a result's rows by their dimension labels.
func index(result *query.Result) map[string][]float64 {
	rows := map[string][]float64{}

	for _, row := range result.Results {
		key := strings.Join(row.Dimensions, "|")

		// A gap-filled empty day exists on both sides or neither; skipping the
		// empty ones keeps the comparison about days that had traffic.
		empty := true
		for _, value := range row.Metrics {
			if value != 0 {
				empty = false
				break
			}
		}

		if empty {
			continue
		}

		rows[key] = row.Metrics
	}

	return rows
}

// assertArchiveShape checks the export contains all ten formats plus the raw
// events, which is the promise the settings page makes.
func assertArchiveShape(t *testing.T, path string) {
	t.Helper()

	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := archive.Close(); err != nil {
			t.Errorf("close export archive: %v", err)
		}
	}()

	found := map[string]bool{}
	for _, entry := range archive.File {
		found[entry.Name] = true
	}

	for _, sheet := range Sheets {
		if !found[sheet.Name+".csv"] {
			t.Errorf("the export is missing %s.csv", sheet.Name)
		}
	}

	if !found[RawEventsSheet+".csv"] {
		t.Error("the export is missing the raw events, which are included in every build rather than gated")
	}
}

// TestUnknownColumnIsRefused checks the import fails loudly. A column silently
// dropped is an import whose numbers are quietly too small, and nobody ever
// finds out.
func TestUnknownColumnIsRefused(t *testing.T) {
	_, err := planColumns("imported_pages.csv", []string{"date", "page", "wibble"})
	if err == nil {
		t.Fatal("an unrecognised column was accepted")
	}

	if !strings.Contains(err.Error(), "wibble") {
		t.Fatalf("the error does not name the column: %v", err)
	}

	if !strings.Contains(err.Error(), DateHeader) {
		t.Fatalf("the error does not list what is understood: %v", err)
	}
}

// TestHeaderSpellingsAreTolerated covers the files people actually upload:
// through a spreadsheet, with a byte order mark, capitals and spaces.
func TestHeaderSpellingsAreTolerated(t *testing.T) {
	_, err := planColumns("imported_pages.csv", []string{"\ufeffDate", "Page", "Page Views"})
	if err == nil {
		// "Page Views" is not one of ours, so this file is correctly refused —
		// the point of the case is that the date and page columns were read.
		t.Fatal("expected the unknown column to be refused")
	}

	plan, err := planColumns("imported_pages.csv", []string{"\ufeffDate", "Page", "Pageviews"})
	if err != nil {
		t.Fatal(err)
	}

	if plan.dateIndex != 0 {
		t.Fatalf("the date column was found at %d, want 0", plan.dateIndex)
	}

	if len(plan.dimensions) != 1 || plan.dimensions[0] != "event:page" {
		t.Fatalf("dimensions = %v, want event:page", plan.dimensions)
	}
}

// TestPlausibleSourcesDeriveTheirMissingChannel checks the provider's source
// shape grows the dimension its dashboard makes the default tab, including a
// spelled-out Direct label and campaign-tagged email traffic.
func TestPlausibleSourcesDeriveTheirMissingChannel(t *testing.T) {
	plan, err := planColumns("imported_sources_20260828_20260828.csv",
		[]string{"date", "source", "referrer", "utm_source", "utm_medium", "utm_campaign", "visitors"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		fields []string
		want   string
	}{
		{[]string{"2026-08-28", "Google", "google.com", "", "", "", "2"}, "Organic Search"},
		{[]string{"2026-08-28", "Google", "google.com", "newsletter", "email", "launch", "2"}, "Email"},
		{[]string{"2026-08-28", "Microsoft Copilot", "", "copilot.com", "", "", "2"}, "AI Assistants"},
		{[]string{"2026-08-28", "ChatGPT", "", "chatgpt.com", "", "", "2"}, "AI Assistants"},
		{[]string{"2026-08-28", "Google Gemini", "gemini.google.com", "", "", "", "2"}, "AI Assistants"},
		{[]string{"2026-08-28", "X (Twitter)", "t.co", "", "", "", "2"}, "Organic Social"},
		{[]string{"2026-08-28", "Brave", "search.brave.com", "", "", "", "2"}, "Organic Search"},
		{[]string{"2026-08-28", "Direct", "", "", "", "", "2"}, "Direct"},
	}

	for _, tc := range cases {
		row, err := plan.row(tc.fields, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if got := row.Dimensions["visit:channel"]; got != tc.want {
			t.Errorf("source row %v derived channel %q, want %q", tc.fields, got, tc.want)
		}
	}
}

// TestUploadsAreCopiedNeverRenamed is a regression guard for a real failure.
// rename(2) returns EXDEV across a device boundary, which is the normal shape
// of a Docker bind mount and of a data directory on a NAS; an incumbent shipped
// a rename here and it broke for exactly those installs.
func TestUploadsAreCopiedNeverRenamed(t *testing.T) {
	source := filepath.Join(t.TempDir(), "upload.csv")
	destination := filepath.Join(t.TempDir(), "imports", "000001-upload.csv")

	if err := os.WriteFile(source, []byte("date,pageviews\n2026-08-28,10\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := MoveFile(source, destination); err != nil {
		t.Fatal(err)
	}

	moved, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}

	if string(moved) != "date,pageviews\n2026-08-28,10\n" {
		t.Fatalf("the copied file reads %q", moved)
	}

	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("the original file was left behind")
	}

	// The behaviour above would also pass with a rename on a machine where both
	// paths happen to share a filesystem, which is every developer's laptop and
	// none of the installs that broke. The only reliable guard is that the call
	// is not in the source at all.
	body, err := os.ReadFile("upload.go")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(body), "os.Rename") {
		t.Fatal("upload.go calls os.Rename — it fails with a cross-device error on a bind mount or a NAS")
	}
}

// TestSafeFilenameCannotEscape covers a filename a browser will happily send.
func TestSafeFilenameCannotEscape(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", "/etc/passwd", "..", ".", ""} {
		got := SafeFilename(name)

		if strings.Contains(got, "/") || got == ".." || got == "." || got == "" {
			t.Errorf("SafeFilename(%q) = %q, which can still escape its directory", name, got)
		}
	}
}

// seedSite writes the fixture as real events and sessions.
func seedSite(t *testing.T, account *accounts.Account, siteID int64) {
	t.Helper()

	ctx := context.Background()

	id := func(dimension intern.Dimension, value string) int64 {
		got, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	pageview := id(intern.EventName, "pageview")
	eventID := int64(0)

	for _, session := range fixtureVisits {
		entry := session.pages[0]
		exit := session.pages[len(session.pages)-1]

		_, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
				pageviews, events, entry_page_id, exit_page_id, entry_hostname_id, exit_hostname_id,
				source_id, country_id, browser_id, device_type_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			session.id, siteID, session.user, session.startedAt, session.startedAt+int64(session.duration),
			session.duration, session.bounce, len(session.pages), len(session.pages),
			id(intern.Pathname, entry), id(intern.Pathname, exit),
			id(intern.Hostname, "example.com"), id(intern.Hostname, "example.com"),
			id(intern.Source, session.source), id(intern.Country, session.country),
			id(intern.Browser, session.browser), id(intern.DeviceType, session.device),
		)
		if err != nil {
			t.Fatal(err)
		}

		for i, page := range session.pages {
			eventID++

			_, err := account.Writer().ExecContext(ctx, `
				INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id,
					hostname_id, pathname_id, source_id, country_id, browser_id, device_type_id,
					scroll_depth, engagement_time)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,255,0)`,
				eventID, siteID, session.startedAt+int64(i), pageview, session.user, session.id,
				id(intern.Hostname, "example.com"), id(intern.Pathname, page),
				id(intern.Source, session.source), id(intern.Country, session.country),
				id(intern.Browser, session.browser), id(intern.DeviceType, session.device),
			)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestBotSessionsAreLeftOutOfAnExport covers a mixed bot/human session beside a
// human session across every roll-up. Human events from the mixed session stay
// exportable, but the whole session is excluded from session-grain metrics; the
// neighboring human session remains present everywhere it applies.
func TestBotSessionsAreLeftOutOfAnExport(t *testing.T) {
	ctx := context.Background()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	seedSite(t, account, 1)
	seedBotExportVisits(t, account, 1)
	var detailedEventID int64
	if err := account.Reader().QueryRow(`
		SELECT e.id FROM events e
		JOIN dim_pathname path ON path.id = e.pathname_id
		WHERE e.site_id = 1 AND path.value = '/neighbor-page'
	`).Scan(&detailedEventID); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().Exec(`
		INSERT INTO event_details
			(event_id, props, revenue_amount, revenue_currency, utm_content, utm_term, full_url)
		VALUES (?, '{"plan":"yearly"}', 1299, 'USD', 'hero', 'privacy analytics',
		        'https://example.com/neighbor-page?campaign=launch')
	`, detailedEventID); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "export.zip")

	if _, err := BuildExport(ctx, account.Reader(), 1, time.UTC, archivePath); err != nil {
		t.Fatal(err)
	}

	neighborMarkers := map[string]string{
		"imported_sources":           "Neighbor Source",
		"imported_pages":             "/neighbor-page",
		"imported_entry_pages":       "/neighbor-page",
		"imported_exit_pages":        "/neighbor-page",
		"imported_locations":         "NZ",
		"imported_devices":           "Neighbor Device",
		"imported_browsers":          "Neighbor Browser",
		"imported_operating_systems": "Neighbor OS",
		"imported_custom_events":     "Neighbor Goal",
	}

	for _, sheet := range Sheets {
		body := sheetBody(t, archivePath, sheet.Name+".csv")
		if marker := neighborMarkers[sheet.Name]; marker != "" && !strings.Contains(body, marker) {
			t.Errorf("%s lost the neighboring human session marker %q", sheet.Name, marker)
		}
	}

	for _, check := range []struct {
		sheet string
		value string
	}{
		{sheet: "imported_pages", value: "/mixed-bot-entry"},
		{sheet: "imported_entry_pages", value: "/mixed-bot-entry"},
		{sheet: "imported_exit_pages", value: "/mixed-human-event"},
	} {
		if body := sheetBody(t, archivePath, check.sheet+".csv"); strings.Contains(body, check.value) {
			t.Errorf("%s carries mixed session-grain bot data %q", check.sheet, check.value)
		}
	}

	for _, check := range []struct {
		sheet  string
		marker string
	}{
		{sheet: "imported_sources", marker: "Mixed Source"},
		{sheet: "imported_locations", marker: "MX"},
		{sheet: "imported_devices", marker: "Mixed Device"},
		{sheet: "imported_browsers", marker: "Mixed Browser"},
		{sheet: "imported_operating_systems", marker: "Mixed OS"},
	} {
		records := sheetRecords(t, archivePath, check.sheet+".csv")
		record := recordContaining(t, records, check.marker)
		if got := recordValue(t, records[0], record, FieldBounces); got != "0" {
			t.Errorf("%s mixed-session bounces = %s, want 0", check.sheet, got)
		}
		if got := recordValue(t, records[0], record, FieldDuration); got != "0" {
			t.Errorf("%s mixed-session duration = %s, want 0", check.sheet, got)
		}
	}

	if body := sheetBody(t, archivePath, "imported_pages.csv"); !strings.Contains(body, "/mixed-human-event") {
		t.Error("the human event inside the mixed session was removed from the event-grain page sheet")
	}
	if body := sheetBody(t, archivePath, "imported_custom_events.csv"); !strings.Contains(body, "Mixed Human Goal") {
		t.Error("the human event inside the mixed session was removed from the custom-event sheet")
	}

	totals := sheetRecords(t, archivePath, "imported_visitors.csv")
	day30 := recordContaining(t, totals, "2026-08-30")
	for field, want := range map[string]string{
		FieldVisitors:  "2",
		FieldVisits:    "2",
		FieldPageviews: "1",
		FieldEvents:    "3",
		FieldBounces:   "0",
		FieldDuration:  "60",
	} {
		if got := recordValue(t, totals[0], day30, field); got != want {
			t.Errorf("day-30 %s = %s, want %s", field, got, want)
		}
	}

	// The raw events file is the deliberate exception: it is everything, with a
	// bot_reason column so the reader can tell which rows are which.
	records := sheetRecords(t, archivePath, RawEventsSheet+".csv")
	pageColumn, botColumn := -1, -1
	for i, name := range records[0] {
		switch name {
		case "page":
			pageColumn = i
		case "bot_reason":
			botColumn = i
		}
	}
	if pageColumn < 0 || botColumn < 0 {
		t.Fatalf("raw event columns = %v, want page and bot_reason", records[0])
	}
	header := records[0]
	detailed := recordContaining(t, records, "/neighbor-page")
	for column, want := range map[string]string{
		"custom_properties_json": `{"plan":"yearly"}`,
		"revenue_amount":         "1299", "revenue_currency": "USD", "utm_content": "hero",
		"utm_term": "privacy analytics", "full_url": "https://example.com/neighbor-page?campaign=launch",
	} {
		if got := recordValue(t, header, detailed, column); got != want {
			t.Errorf("raw event %s = %q, want %q", column, got, want)
		}
	}

	found := map[string]bool{}
	for _, record := range records[1:] {
		page := record[pageColumn]
		wantReason, ok := map[string]string{
			"/mixed-bot-entry":   "bot",
			"/mixed-human-event": "",
			"/neighbor-page":     "",
		}[page]
		if !ok {
			continue
		}

		found[page+record[botColumn]] = true
		if record[botColumn] != wantReason {
			t.Errorf("raw event %s has bot_reason %q, want %q", page, record[botColumn], wantReason)
		}
	}
	if !found["/mixed-bot-entrybot"] {
		t.Error("the raw events file dropped a bot event instead of labelling it")
	}
	if !found["/mixed-human-event"] || !found["/neighbor-page"] {
		t.Error("the raw events file lost or mislabeled a human event beside bot traffic")
	}
}

// seedBotExportVisits writes one mixed bot/human session and one neighboring
// human session with unique values for every exported dimension.
func seedBotExportVisits(t *testing.T, account *accounts.Account, siteID int64) {
	t.Helper()

	ctx := context.Background()

	id := func(dimension intern.Dimension, value string) int64 {
		got, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	host := id(intern.Hostname, "example.com")
	pageview := id(intern.EventName, "pageview")
	bot := id(intern.BotReason, "bot")

	type dimensions struct {
		source, referrer, utmSource, utmMedium, utmCampaign int64
		country, region, city, device, screen               int64
		browser, browserVersion, os, osVersion, language    int64
	}

	// values interns one complete, uniquely named dimension set so every
	// applicable export sheet can prove which session contributed its row.
	values := func(prefix, country string) dimensions {
		return dimensions{
			source: id(intern.Source, prefix+" Source"), referrer: id(intern.Referrer, "https://"+strings.ToLower(prefix)+".example/start"),
			utmSource: id(intern.UTMSource, prefix+" UTM Source"), utmMedium: id(intern.UTMMedium, prefix+" UTM Medium"),
			utmCampaign: id(intern.UTMCampaign, prefix+" Campaign"), country: id(intern.Country, country),
			region: id(intern.Region, prefix+" Region"), city: id(intern.City, prefix+" City"),
			device: id(intern.DeviceType, prefix+" Device"), screen: id(intern.ScreenSize, prefix+" Screen"),
			browser: id(intern.Browser, prefix+" Browser"), browserVersion: id(intern.BrowserVersion, prefix+" Browser Version"),
			os: id(intern.OS, prefix+" OS"), osVersion: id(intern.OSVersion, prefix+" OS Version"),
			language: id(intern.Language, strings.ToLower(prefix)),
		}
	}

	mixed := values("Mixed", "MX")
	neighbor := values("Neighbor", "NZ")
	mixedEntry := id(intern.Pathname, "/mixed-bot-entry")
	mixedExit := id(intern.Pathname, "/mixed-human-event")
	neighborPage := id(intern.Pathname, "/neighbor-page")

	// insertSession writes a session carrying the supplied dimension set and
	// entry/exit paths without relying on the ingest session folder.
	insertSession := func(sessionID, userID, startedAt, lastSeenAt int64, duration, pageviews, events int, entryPage, exitPage int64, d dimensions) {
		t.Helper()

		_, err := account.Writer().ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
			pageviews, events, entry_page_id, exit_page_id, entry_hostname_id, exit_hostname_id,
			referrer_id, source_id, utm_source_id, utm_medium_id, utm_campaign_id,
			country_id, region_id, city_id, device_type_id, screen_size_id,
			browser_id, browser_version_id, os_id, os_version_id, language_id)
		VALUES (?,?,?,?,?,?,0, ?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?)`,
			sessionID, siteID, userID, startedAt, lastSeenAt, duration,
			pageviews, events, entryPage, exitPage, host, host,
			d.referrer, d.source, d.utmSource, d.utmMedium, d.utmCampaign,
			d.country, d.region, d.city, d.device, d.screen,
			d.browser, d.browserVersion, d.os, d.osVersion, d.language)
		if err != nil {
			t.Fatal(err)
		}
	}

	// insertEvent writes one raw event whose bot reason and dimensions can be
	// asserted independently from the session-level export filters.
	insertEvent := func(eventID, timestamp, userID, sessionID, nameID, pageID, botReason int64, d dimensions) {
		t.Helper()

		_, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id,
			hostname_id, pathname_id, referrer_id, source_id, utm_source_id, utm_medium_id, utm_campaign_id,
			country_id, region_id, city_id, device_type_id, screen_size_id,
			browser_id, browser_version_id, os_id, os_version_id, language_id,
			bot_reason_id, scroll_depth, engagement_time)
		VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,0,0)`,
			eventID, siteID, timestamp, nameID, userID, sessionID,
			host, pageID, d.referrer, d.source, d.utmSource, d.utmMedium, d.utmCampaign,
			d.country, d.region, d.city, d.device, d.screen,
			d.browser, d.browserVersion, d.os, d.osVersion, d.language, botReason)
		if err != nil {
			t.Fatal(err)
		}
	}

	insertSession(90, 900, at(30, 10), at(30, 10)+60, 60, 1, 2, mixedEntry, mixedExit, mixed)
	insertEvent(9000, at(30, 10), 900, 90, pageview, mixedEntry, bot, mixed)
	insertEvent(9001, at(30, 10)+60, 900, 90, id(intern.EventName, "Mixed Human Goal"), mixedExit, 0, mixed)

	insertSession(91, 901, at(30, 11), at(30, 11)+60, 60, 1, 2, neighborPage, neighborPage, neighbor)
	insertEvent(9100, at(30, 11), 901, 91, pageview, neighborPage, 0, neighbor)
	insertEvent(9101, at(30, 11)+60, 901, 91, id(intern.EventName, "Neighbor Goal"), neighborPage, 0, neighbor)
}

// recordContaining returns the first CSV record carrying a value.
func recordContaining(t *testing.T, records [][]string, value string) []string {
	t.Helper()

	for _, record := range records[1:] {
		for _, field := range record {
			if field == value {
				return record
			}
		}
	}

	t.Fatalf("CSV records do not contain %q: %v", value, records)

	return nil
}

// recordValue reads one named field from a CSV record.
func recordValue(t *testing.T, header, record []string, name string) string {
	t.Helper()

	for index, field := range header {
		if field == name {
			return record[index]
		}
	}

	t.Fatalf("CSV header does not contain %q: %v", name, header)

	return ""
}

// sheetRecords parses one CSV entry out of an export archive.
func sheetRecords(t *testing.T, path, name string) [][]string {
	t.Helper()

	records, err := csv.NewReader(strings.NewReader(sheetBody(t, path, name))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatalf("%s is empty", name)
	}

	return records
}

// sheetBody reads one entry out of an export archive.
func sheetBody(t *testing.T, path, name string) string {
	t.Helper()

	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := archive.Close(); err != nil {
			t.Errorf("close export archive: %v", err)
		}
	}()

	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}

		file, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				t.Errorf("close export entry: %v", err)
			}
		}()

		body, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}

		return string(body)
	}

	t.Fatalf("the export has no %s", name)

	return ""
}
