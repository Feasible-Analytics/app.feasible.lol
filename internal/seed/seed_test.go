//
// seed_test.go
// The generator: determinism, the shape of the data, and the deliberate oddities.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// fixedNow pins the history so a test never depends on the day it runs. The
// instant is mid-morning on a Tuesday, which puts a partial day at the end of
// the run exactly as a real one has.
func fixedNow() time.Time {
	return time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC)
}

// tinyRun seeds a small dataset into a temporary directory and returns it. It is
// deliberately small: the properties worth asserting in a unit test are the ones
// that hold at any size, and the ones that do not are checked by the shape
// report on a real run.
func tinyRun(t *testing.T, seed int64) (string, *Result) {
	t.Helper()
	return runSeed(t, seed, 4000)
}

// smallRun seeds a fraction of tinyRun's volume, for the one test that runs the
// generator three times. Determinism is a property rather than a quantity — a
// seed either reproduces its rows or it does not, and it decides that in the
// first handful of events. Three full runs under the race detector took this
// package past the ten-minute per-package timeout, which left `-race` reporting
// nothing at all for the concurrent code it exists to check. The day and site
// counts are unchanged, because those drive real branches in the generator.
func smallRun(t *testing.T, seed int64) (string, *Result) {
	t.Helper()
	return runSeed(t, seed, 400)
}

// runSeed generates a dataset into a temporary directory. The volume is a
// parameter because the tests want two different things from it: most assert
// properties that hold at any size, while the awkward-cases test needs enough
// events for the rare shapes — a third currency, a visitor behind a VPN — to
// actually occur.
func runSeed(t *testing.T, seed int64, pageviews int64) (string, *Result) {
	t.Helper()

	dir := t.TempDir()

	result, err := Run(context.Background(), Options{
		DataDir:   dir,
		Pageviews: pageviews,
		Days:      14,
		Sites:     5,
		Seed:      seed,
		Now:       fixedNow,
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}

	return dir, result
}

// TestRunIsDeterministic is the test the whole "fixed seed" promise rests on.
// Two runs with one seed have to produce the same rows down to the visitor ids,
// or a performance measurement taken a week apart is comparing two datasets
// rather than two queries.
func TestRunIsDeterministic(t *testing.T) {
	firstDir, first := smallRun(t, 99)
	secondDir, second := smallRun(t, 99)

	if first.Events != second.Events || first.Sessions != second.Sessions {
		t.Fatalf("two runs of one seed disagree: %d/%d events, %d/%d sessions",
			first.Events, second.Events, first.Sessions, second.Sessions)
	}

	firstDigest := digest(t, firstDir)
	secondDigest := digest(t, secondDir)

	if firstDigest != secondDigest {
		t.Fatalf("two runs of one seed produced different rows: %s and %s", firstDigest, secondDigest)
	}

	// A different seed has to produce different data, or the seed is not being
	// used and every run is the same dataset by accident.
	otherDir, _ := smallRun(t, 100)

	if digest(t, otherDir) == firstDigest {
		t.Fatal("two different seeds produced identical data")
	}
}

// digest hashes every event and session row of the first account. Hashing the
// rows rather than the file is deliberate: SQLite is free to lay out pages
// differently, and it is the data that has to be reproducible rather than the
// bytes on disk.
func digest(t *testing.T, dataDir string) string {
	t.Helper()

	db, err := store.Open(accounts.Path(dataDir, 1))
	if err != nil {
		t.Fatalf("open account database: %v", err)
	}
	defer db.Close()

	hash := sha256.New()

	rows, err := db.Query(`
		SELECT site_id, timestamp, name_id, user_id, session_id, pathname_id,
		       source_id, channel_id, country_id, browser_id, scroll_depth, engagement_time
		FROM events ORDER BY id`)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			siteID, timestamp, nameID, userID, sessionID, pathnameID int64
			sourceID, channelID, countryID, browserID                int64
			scroll, engagement                                       int64
		)

		if err := rows.Scan(&siteID, &timestamp, &nameID, &userID, &sessionID, &pathnameID,
			&sourceID, &channelID, &countryID, &browserID, &scroll, &engagement); err != nil {
			t.Fatalf("scan event: %v", err)
		}

		fmt.Fprintf(hash, "%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d\n",
			siteID, timestamp, nameID, userID, sessionID, pathnameID,
			sourceID, channelID, countryID, browserID, scroll, engagement)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}

	return hex.EncodeToString(hash.Sum(nil))
}

// TestRunProducesRealSessions checks the two numbers that say whether the
// session fold was exercised at all. A bounce rate of nought or a hundred, or a
// views-per-visit of exactly one, means every event opened its own session and
// the rules that must be byte-exact never ran.
func TestRunProducesRealSessions(t *testing.T) {
	_, result := tinyRun(t, 7)

	if len(result.Report.Sites) == 0 {
		t.Fatal("the run reported no sites")
	}

	primary := result.Report.Sites[0]

	if primary.BounceRate <= 0.1 || primary.BounceRate >= 0.95 {
		t.Errorf("bounce rate is %.2f, which means the session fold never saw a real visit", primary.BounceRate)
	}

	if primary.ViewsPerVisit <= 1.2 {
		t.Errorf("views per visit is %.2f, so nearly every event opened its own session", primary.ViewsPerVisit)
	}

	if primary.LongestSession < 20 {
		t.Errorf("the longest visit is %d pageviews, so the tail of the length distribution is missing", primary.LongestSession)
	}

	if primary.SinglePageShare < 0.35 || primary.SinglePageShare > 0.85 {
		t.Errorf("%.0f%% of visits are a single pageview, which is not a realistic spread", primary.SinglePageShare*100)
	}
}

// TestRunProducesPowerLawPages is the assertion that the data is not uniformly
// random. Uniform over two thousand pages would put the ten busiest at half a
// per cent between them, and every query against it would be measuring a
// distribution no site has.
func TestRunProducesPowerLawPages(t *testing.T) {
	_, result := tinyRun(t, 11)

	primary := result.Report.Sites[0]

	if primary.TopPageShare < 0.3 || primary.TopPageShare > 0.8 {
		t.Errorf("the ten busiest pages take %.0f%% of pageviews, which is not a power law", primary.TopPageShare*100)
	}

	if primary.DistinctPages < 50 {
		t.Errorf("only %d distinct pages were generated", primary.DistinctPages)
	}

	if primary.DistinctCountries < 5 {
		t.Errorf("only %d distinct countries were generated", primary.DistinctCountries)
	}
}

// TestRunGeneratesTheAwkwardCases checks the rows that are there on purpose.
// Tidy seed data hides precisely the bugs a seed exists to catch, so their
// absence is a failure rather than a curiosity.
func TestRunGeneratesTheAwkwardCases(t *testing.T) {
	dir, result := tinyRun(t, 13)

	for _, check := range result.Report.Checks {
		// The distribution checks are not enforced at this size — four thousand
		// pageviews cannot be meaningfully power-law — but the oddities are
		// injected deliberately and have to be present at any size.
		switch check.Name {
		case "a page has exactly one pageview",
			"revenue arrives in three currencies",
			"an event carries the property cap",
			"a visitor is bucketed as Anonymous VPN Service",
			"unvalidated hostname traffic is rejected",
			"a site has no data at all",
			"a locked account still has data",
			"a dormant account stopped receiving traffic":

			if !check.OK {
				t.Errorf("%s: %s", check.Name, check.Detail)
			}
		}
	}

	// Deliberate policy rejections, including dormant-account traffic and the
	// unvalidated-hostname fixture, must be visible in the run statistics.
	if result.Dropped == 0 {
		t.Error("no events were dropped, so deliberate policy rejections were accepted")
	}

	// Every account database has to exist, including the one whose account is
	// past its ingestion deadline.
	for id := int64(1); id <= 3; id++ {
		if _, err := store.Open(accounts.Path(dir, id)); err != nil {
			t.Errorf("account %d has no database: %v", id, err)
		}
	}
}

// TestInsertsCoverEveryColumn is the guard against the bulk writer drifting
// away from the schema. The generator writes its own inserts so it can prepare
// them once per batch, and a column added to the schema that this forgets would
// be a column every report silently reads as zero.
func TestInsertsCoverEveryColumn(t *testing.T) {
	dir := t.TempDir()

	if _, err := Run(context.Background(), Options{
		DataDir:   dir,
		Pageviews: 200,
		Days:      3,
		Sites:     1,
		Seed:      3,
		Now:       fixedNow,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	db, err := store.Open(accounts.Path(dir, 1))
	if err != nil {
		t.Fatalf("open account database: %v", err)
	}
	defer db.Close()

	for _, item := range []struct {
		table    string
		inserted int
		// autoIncrement is any column the insert deliberately leaves to SQLite.
		// The seed allocates both ids itself — a session so it can be updated
		// in place across batches, an event so a multi-row insert still knows
		// which cold row belongs to which event — so nothing is left out today.
		autoIncrement int
	}{
		{table: "events", inserted: 30, autoIncrement: 0},
		{table: "sessions", inserted: 31, autoIncrement: 0},
		{table: "event_details", inserted: 7, autoIncrement: 0},
	} {
		columns, err := columnCount(db, item.table)
		if err != nil {
			t.Fatalf("read %s columns: %v", item.table, err)
		}

		if columns != item.inserted+item.autoIncrement {
			t.Errorf("%s has %d columns and the seed writes %d — the bulk insert has drifted from the schema",
				item.table, columns, item.inserted)
		}
	}
}

// columnCount reads how many columns a table has.
func columnCount(db *sql.DB, table string) (int, error) {
	rows, err := db.Query("SELECT COUNT(*) FROM pragma_table_info(?)", table)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var count int
	if rows.Next() {
		if err := rows.Scan(&count); err != nil {
			return 0, err
		}
	}

	return count, rows.Err()
}

// BenchmarkRun is how the two-minute budget for a million pageviews is checked
// without waiting two minutes. It seeds a fortieth of that and reports the rate,
// which is the number the budget is really about.
func BenchmarkRun(b *testing.B) {
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()

		result, err := Run(context.Background(), Options{
			DataDir:   dir,
			Pageviews: 25_000,
			Days:      7,
			Sites:     1,
			Seed:      DefaultSeed,
			Now:       fixedNow,
		})
		if err != nil {
			b.Fatalf("seed run: %v", err)
		}

		b.ReportMetric(float64(result.Events)/result.Duration.Seconds(), "events/s")
	}
}
