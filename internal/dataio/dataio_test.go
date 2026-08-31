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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// fixtureNow is the instant the round trip resolves its date range against.
var fixtureNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

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
	defer manager.CloseAll()

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
	defer closeArchive()

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
	defer archive.Close()

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
	plan, err := planColumns("imported_pages.csv", []string{"\ufeffDate", "Page", "Page Views"})
	if err == nil {
		// "Page Views" is not one of ours, so this file is correctly refused —
		// the point of the case is that the date and page columns were read.
		t.Fatal("expected the unknown column to be refused")
	}

	plan, err = planColumns("imported_pages.csv", []string{"\ufeffDate", "Page", "Pageviews"})
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
