//
// rollup_test.go
// Proving that a number read from a summary is the number the raw rows hold.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The tests live in an external package because the biggest of them builds its
// fixture with the seed generator, and the seed generator rebuilds roll-ups —
// so an internal test would be an import cycle.
package rollup_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/seed"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// losAngeles is the timezone every test here uses, and it is not UTC on
// purpose. A visitor id is derived from a salt that rotates at UTC midnight, so
// on a site whose day starts anywhere else one visitor can be present in two
// local days — which is the whole reason the carry-over columns exist. A test
// suite written entirely in UTC would pass without them.
var losAngeles = mustLoad("America/Los_Angeles")

// mustLoad resolves a timezone or dies, because a build with no timezone
// database cannot test day boundaries at all.
func mustLoad(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}

	return location
}

// fixtureNow is the instant the hand-built fixture is read at: early afternoon
// in Los Angeles, so that today is genuinely half finished.
var fixtureNow = time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)

// testSite is the site every hand-built fixture writes to.
var testSite = rollup.Site{ID: 1, Domain: "example.test", Timezone: "America/Los_Angeles"}

// eventRow is one row of the events table, written directly. Writing it
// directly is deliberate: these tests are about what the builder aggregates,
// and a fixture that went through the ingest pipeline would fail for two
// different reasons at once.
type eventRow struct {
	session int64
	user    int64
	at      time.Time
	name    string
	page    string
	source  string
	country string
}

// sessionRow is one row of the sessions table.
type sessionRow struct {
	id        int64
	user      int64
	startedAt time.Time
	lastSeen  time.Time
	duration  int
	bounce    int
	pageviews int
	entryPage string
	exitPage  string
	source    string
	country   string
}

// local builds an instant from a wall-clock reading in Los Angeles.
func local(day, hour int) time.Time {
	return time.Date(2026, 8, day, hour, 0, 0, 0, losAngeles)
}

// openAccount creates an empty account database in a temporary directory.
func openAccount(t *testing.T) *accounts.Account {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.CloseAll() })

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	return account
}

// writeFixture puts the rows into an account database, interning every
// dimension string exactly as the ingest path would.
func writeFixture(t *testing.T, account *accounts.Account, sessions []sessionRow, events []eventRow) {
	t.Helper()

	ctx := context.Background()

	id := func(dimension intern.Dimension, value string) int64 {
		got, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	for _, session := range sessions {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
				pageviews, events, entry_page_id, exit_page_id, source_id, country_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			session.id, testSite.ID, session.user, session.startedAt.Unix(), session.lastSeen.Unix(),
			session.duration, session.bounce, session.pageviews, session.pageviews,
			id(intern.Pathname, session.entryPage), id(intern.Pathname, session.exitPage),
			id(intern.Source, session.source), id(intern.Country, session.country),
		); err != nil {
			t.Fatal(err)
		}
	}

	for i, event := range events {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id,
				pathname_id, source_id, country_id, scroll_depth)
			VALUES (?,?,?,?,?,?,?,?,?,255)`,
			i+1, testSite.ID, event.at.Unix(), id(intern.EventName, event.name), event.user, event.session,
			id(intern.Pathname, event.page), id(intern.Source, event.source), id(intern.Country, event.country),
		); err != nil {
			t.Fatal(err)
		}
	}
}

// buildAll rebuilds both grains over the whole fixture, covering everything up
// to the start of today — which is exactly what the worker does.
func buildAll(t *testing.T, account *accounts.Account, now time.Time) {
	t.Helper()

	builder := rollup.New(account.Writer())
	builder.Now = func() time.Time { return now }

	today := query.RollupBucketStart(now.In(losAngeles), query.GrainDay, losAngeles)
	from := today.AddDate(0, 0, -30)

	for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
		to := today
		if grain == query.GrainDay {
			to = today.AddDate(0, 0, 1)
		}

		if err := builder.Rebuild(context.Background(), rollup.Request{
			Site: testSite, Grain: grain, From: from, To: to, CoverThrough: today,
			FromBeginning: true,
		}); err != nil {
			t.Fatalf("rebuild %s: %v", grain, err)
		}
	}
}

// engines builds two engines over the same database: one that must read raw
// rows and one that is allowed to use the summary. Every assertion in this file
// is the two of them answering the same question.
func engines(account *accounts.Account, now time.Time) (*query.Engine, *query.Engine) {
	raw := query.New(account.Reader())
	raw.Router = query.RawRouter{}
	raw.Now = func() time.Time { return now }

	rolled := query.New(account.Reader())
	rolled.Now = func() time.Time { return now }

	return raw, rolled
}

// carryFixture is a visitor who comes back later the same UTC day but on the
// next local day, and another who does the same across the boundary between the
// summary and today.
//
// U1 visits at 20:00 on the 28th and 10:00 on the 29th. Both are inside the UTC
// day of the 29th, so both carry the same visitor id, and a summary that simply
// added its two daily rows would report two visitors where there is one.
//
// U2 does the same across the 29th and the 30th, which is the boundary between
// the last summarised day and today.
func carryFixture() ([]sessionRow, []eventRow) {
	sessions := []sessionRow{
		{id: 1, user: 1001, startedAt: local(28, 20), lastSeen: local(28, 20), duration: 0, bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"},
		{id: 2, user: 1001, startedAt: local(29, 10), lastSeen: local(29, 10), duration: 0, bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"},
		{id: 3, user: 1002, startedAt: local(29, 20), lastSeen: local(29, 20), duration: 0, bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"},
		{id: 4, user: 1002, startedAt: local(30, 9), lastSeen: local(30, 9), duration: 0, bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"},
		{id: 5, user: 1003, startedAt: local(29, 11), lastSeen: local(29, 11), duration: 30, bounce: 0, pageviews: 2,
			entryPage: "/home", exitPage: "/pricing", source: "Twitter", country: "CA"},
	}

	events := []eventRow{
		{session: 1, user: 1001, at: local(28, 20), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"},
		{session: 2, user: 1001, at: local(29, 10), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"},
		{session: 3, user: 1002, at: local(29, 20), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"},
		{session: 4, user: 1002, at: local(30, 9), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"},
		{session: 5, user: 1003, at: local(29, 11), name: ingest.EventPageview, page: "/home", source: "Twitter", country: "CA"},
		{session: 5, user: 1003, at: local(29, 11).Add(time.Minute), name: ingest.EventPageview, page: "/pricing", source: "Twitter", country: "CA"},
	}

	return sessions, events
}

// TestCarryOverMakesVisitorCountsReAggregate is the test the whole design turns
// on. Without the carry-over columns the summary reports four visitors where
// there are three, and the error grows with how loyal the audience is.
func TestCarryOverMakesVisitorCountsReAggregate(t *testing.T) {
	account := openAccount(t)

	sessions, events := carryFixture()
	writeFixture(t, account, sessions, events)
	buildAll(t, account, fixtureNow)

	raw, rolled := engines(account, fixtureNow)

	q := query.Query{
		SiteIDs:   []int64{testSite.ID},
		Metrics:   []string{"visitors", "visits", "pageviews", "bounce_rate", "visit_duration"},
		DateRange: query.DateRange{Preset: query.RangeLast7Days},
		Timezone:  testSite.Timezone,
	}

	fromRaw := answer(t, raw, q)
	fromRollup := answer(t, rolled, q)

	if len(fromRollup.Meta.Sources) != 2 {
		t.Fatalf("meta.sources = %v — the query should have read both a summary and today's raw rows", fromRollup.Meta.Sources)
	}

	// Three people: 1001 across the 28th and 29th, 1002 across the 29th and
	// today, and 1003 on the 29th.
	if got := fromRaw.Results[0].Metrics[0]; got != 3 {
		t.Fatalf("raw visitors = %v, want 3 — the fixture is not what the test thinks it is", got)
	}

	compare(t, "carry-over fixture", q.Metrics, fromRaw, fromRollup)
}

// TestDailyVisitorsAreNotTheSumOfHourlyVisitors pins the rule that the hourly
// buckets exist to be drawn and for nothing else.
//
// Within a day the salt does not rotate, so a visitor at 09:00 and again at
// 14:00 is the same id in two hourly buckets. Adding those buckets counts them
// twice. The test asserts both halves of that: the hourly rows really do
// double-count when summed, and the daily figure the engine returns does not.
func TestDailyVisitorsAreNotTheSumOfHourlyVisitors(t *testing.T) {
	account := openAccount(t)

	// One person, twice on the 29th, five hours apart.
	sessions := []sessionRow{
		{id: 1, user: 2001, startedAt: local(29, 9), lastSeen: local(29, 9), bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"},
		{id: 2, user: 2001, startedAt: local(29, 14), lastSeen: local(29, 14), bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"},
	}

	events := []eventRow{
		{session: 1, user: 2001, at: local(29, 9), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"},
		{session: 2, user: 2001, at: local(29, 14), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"},
	}

	writeFixture(t, account, sessions, events)
	buildAll(t, account, fixtureNow)

	hourly := sumHourlyVisitors(t, account, local(29, 0), local(30, 0))
	if hourly != 2 {
		t.Fatalf("the two hourly buckets hold %d visitors between them, want 2 — the fixture is wrong", hourly)
	}

	daily := dailyVisitors(t, account, local(29, 0))
	if daily != 1 {
		t.Errorf("the daily bucket holds %d visitors, want 1 — a daily row must be counted from raw, never summed from hours", daily)
	}

	raw, rolled := engines(account, fixtureNow)

	q := query.Query{
		SiteIDs:   []int64{testSite.ID},
		Metrics:   []string{"visitors"},
		DateRange: query.DateRange{Preset: query.RangeLast7Days},
		Timezone:  testSite.Timezone,
	}

	fromRollup := answer(t, rolled, q)

	if got := fromRollup.Results[0].Metrics[0]; got != 1 {
		t.Errorf("visitors over the range = %v, want 1 — the hourly buckets must not have been summed", got)
	}

	compare(t, "one visitor twice in a day", q.Metrics, answer(t, raw, q), fromRollup)
}

// sumHourlyVisitors adds up the hourly buckets, which is exactly the thing no
// report is allowed to do. The test does it to prove the trap is real.
func sumHourlyVisitors(t *testing.T, account *accounts.Account, from, to time.Time) int64 {
	t.Helper()

	var total sql.NullInt64

	err := account.Reader().QueryRow(`
		SELECT SUM(event_visitors) FROM rollup_visitors
		WHERE site_id = ? AND grain = ? AND bucket >= ? AND bucket < ?`,
		testSite.ID, int64(query.GrainHour),
		query.RollupLocalUnix(from, losAngeles), query.RollupLocalUnix(to, losAngeles)).Scan(&total)
	if err != nil {
		t.Fatal(err)
	}

	return total.Int64
}

// dailyVisitors reads one day's stored visitor count.
func dailyVisitors(t *testing.T, account *accounts.Account, day time.Time) int64 {
	t.Helper()

	var total sql.NullInt64

	err := account.Reader().QueryRow(`
		SELECT event_visitors FROM rollup_visitors
		WHERE site_id = ? AND grain = ? AND bucket = ?`,
		testSite.ID, int64(query.GrainDay), query.RollupLocalUnix(day, losAngeles)).Scan(&total)
	if err != nil {
		t.Fatal(err)
	}

	return total.Int64
}

// TestRebuildingABucketTwiceChangesNothing is the "a roll-up is a cache" rule
// as a test. A build that was not idempotent would double every number the
// second time the worker touched a day it had already sealed.
func TestRebuildingABucketTwiceChangesNothing(t *testing.T) {
	account := openAccount(t)

	sessions, events := carryFixture()
	writeFixture(t, account, sessions, events)

	buildAll(t, account, fixtureNow)
	first := snapshot(t, account)

	buildAll(t, account, fixtureNow)
	second := snapshot(t, account)

	if first != second {
		t.Errorf("rebuilding changed the totals:\n first  %s\n second %s", first, second)
	}
}

// snapshot renders every stored number as one string, so a difference anywhere
// fails the test with both values side by side.
func snapshot(t *testing.T, account *accounts.Account) string {
	t.Helper()

	var out string

	for _, table := range query.RollupTables() {
		var (
			rows      sql.NullInt64
			pageviews sql.NullInt64
			visitors  sql.NullInt64
			carried   sql.NullInt64
			visits    sql.NullInt64
		)

		err := account.Reader().QueryRow(
			"SELECT COUNT(*), SUM(pageviews), SUM(event_visitors), SUM(event_visitors_carried), SUM(visits) FROM "+table).
			Scan(&rows, &pageviews, &visitors, &carried, &visits)
		if err != nil {
			t.Fatal(err)
		}

		out += fmt.Sprintf("%s:%d/%d/%d/%d/%d ", table, rows.Int64, pageviews.Int64, visitors.Int64, carried.Int64, visits.Int64)
	}

	return out
}

// TestPruneDropsAgedHourlyBucketsAndNarrowsTheWindow checks the retention rule.
// Deleting the rows without moving the covered window would leave a reader
// trusting buckets that are gone and drawing a fortnight-old morning as an hour
// with no traffic.
func TestPruneDropsAgedHourlyBucketsAndNarrowsTheWindow(t *testing.T) {
	account := openAccount(t)

	writeFixture(t, account,
		[]sessionRow{{id: 1, user: 3001, startedAt: local(1, 9), lastSeen: local(1, 9), bounce: 1, pageviews: 1,
			entryPage: "/home", exitPage: "/home", source: "Google", country: "US"}},
		[]eventRow{{session: 1, user: 3001, at: local(1, 9), name: ingest.EventPageview, page: "/home", source: "Google", country: "US"}})

	buildAll(t, account, fixtureNow)

	builder := rollup.New(account.Writer())
	builder.Now = func() time.Time { return fixtureNow }

	if err := builder.Prune(context.Background(), testSite); err != nil {
		t.Fatal(err)
	}

	var hourly int
	if err := account.Reader().QueryRow(
		"SELECT COUNT(*) FROM rollup_visitors WHERE site_id = ? AND grain = ?",
		testSite.ID, int64(query.GrainHour)).Scan(&hourly); err != nil {
		t.Fatal(err)
	}

	if hourly != 0 {
		t.Errorf("%d hourly buckets survived a prune of everything older than a fortnight, want 0", hourly)
	}

	coverage, found, err := builder.Coverage(context.Background(), testSite.ID, query.GrainHour)
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("the hourly coverage row disappeared with the buckets")
	}

	cutoff := query.RollupLocalUnix(
		query.RollupBucketStart(fixtureNow.Add(-rollup.HourlyRetention).In(losAngeles), query.GrainHour, losAngeles), losAngeles)

	if coverage.From < cutoff {
		t.Errorf("hourly coverage still claims %d, which is before the retention cutoff %d", coverage.From, cutoff)
	}

	// Daily buckets are kept forever, so the same prune must not have touched
	// them.
	var daily int
	if err := account.Reader().QueryRow(
		"SELECT COUNT(*) FROM rollup_visitors WHERE site_id = ? AND grain = ?",
		testSite.ID, int64(query.GrainDay)).Scan(&daily); err != nil {
		t.Fatal(err)
	}

	if daily == 0 {
		t.Error("the prune removed the daily buckets, which are kept forever")
	}
}

// TestSeededDatabaseAnswersIdenticallyFromEitherSource is the correctness bar
// the whole milestone is measured against: on a database of realistic shape,
// every metric, every breakdown and every range has to give the same number
// whether it came out of the summary or off the raw rows.
//
// It is table-driven across the reports a dashboard actually fires, because a
// summary that is right for the headline numbers and wrong for one breakdown is
// a summary nobody can trust.
func TestSeededDatabaseAnswersIdenticallyFromEitherSource(t *testing.T) {
	if testing.Short() {
		t.Skip("generating a realistic dataset takes a few seconds")
	}

	account, site, now := seedDatabase(t)

	builder := rollup.New(account.Writer())
	builder.Now = func() time.Time { return now }

	location := site.Location()
	today := query.RollupBucketStart(now.In(location), query.GrainDay, location)

	for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
		to := today
		if grain == query.GrainDay {
			to = today.AddDate(0, 0, 1)
		}

		if err := builder.Rebuild(context.Background(), rollup.Request{
			Site: site, Grain: grain, From: today.AddDate(0, 0, -60), To: to, CoverThrough: today,
			FromBeginning: true,
		}); err != nil {
			t.Fatalf("rebuild %s: %v", grain, err)
		}
	}

	raw := query.New(account.Reader())
	raw.Router = query.RawRouter{}
	raw.Now = func() time.Time { return now }

	rolled := query.New(account.Reader())
	rolled.Now = func() time.Time { return now }

	headline := []string{"visitors", "visits", "pageviews", "bounce_rate", "visit_duration", "views_per_visit"}
	breakdown := []string{"visitors", "visits", "pageviews", "bounce_rate"}

	ranges := []query.DateRange{
		{Preset: query.RangeLast7Days},
		{Preset: query.RangeLast28Days},
		{Preset: query.RangeMonth},
		{Preset: query.RangeLast12Months},
		{Preset: query.RangeAll},

		// A window entirely in the past is the one case the summary answers on
		// its own, with no raw segment beside it — which is also the only case
		// where the database orders and pages the summary rows itself.
		{
			Preset: query.RangeCustom, DateOnly: true,
			Start: today.AddDate(0, 0, -8),
			End:   today.AddDate(0, 0, -3),
		},
	}

	dimensions := [][]string{
		nil,
		{"time"},
		{"time:day"},
		{"time:month"},
		{"event:page"},
		{"event:hostname"},
		{"event:name"},
		{"visit:entry_page"},
		{"visit:exit_page"},
		{"visit:source"},
		{"visit:channel"},
		{"visit:utm_campaign"},
		{"visit:country"},
		{"visit:region"},
		{"visit:city"},
		{"visit:device"},
		{"visit:browser"},
		{"visit:os"},
		{"visit:language"},
		{"visit:source", "time:day"},
	}

	summaryUsed := 0

	for _, dateRange := range ranges {
		for _, dimension := range dimensions {
			metrics := headline
			if len(dimension) > 0 {
				metrics = breakdown
			}

			q := query.Query{
				SiteIDs:    []int64{site.ID},
				Metrics:    metrics,
				Dimensions: dimension,
				DateRange:  dateRange,
				Timezone:   site.Timezone,

				// Everything, so that a tie at a page boundary cannot make the
				// two paths return different rows for reasons that have nothing
				// to do with whether the numbers agree.
				Pagination: query.Pagination{Limit: query.MaxLimit},

				// total_rows is answered by a statement of its own on the
				// push-down path, so it is its own chance to disagree.
				Include: query.Include{TotalRows: true},
			}

			fromRaw, rawErr := raw.Run(context.Background(), q)
			fromRollup, rollupErr := rolled.Run(context.Background(), q)

			// A combination the planner refuses has to be refused identically
			// by both, or the summary has quietly widened what is answerable.
			if (rawErr == nil) != (rollupErr == nil) {
				t.Errorf("%v %v: raw error %v, roll-up error %v", dateRange.Preset, dimension, rawErr, rollupErr)
				continue
			}

			if rawErr != nil {
				continue
			}

			name := fmt.Sprintf("%s %v", dateRange.Preset, dimension)

			served := false
			for _, source := range fromRollup.Meta.Sources {
				if source == "rollup" {
					served = true
				}
			}

			if served {
				summaryUsed++
			} else {
				// Logged rather than asserted: a report the summary cannot
				// answer is meant to be slow, not wrong, and knowing which ones
				// they are is the whole reason to look at this test's output.
				t.Logf("%s was answered from raw", name)
			}

			compare(t, name, metrics, fromRaw, fromRollup)
		}
	}

	if summaryUsed == 0 {
		t.Fatal("no query in the table was answered from a summary — the test proved nothing")
	}

	t.Logf("%d of %d reports were answered from the summary tables", summaryUsed, len(ranges)*len(dimensions))
}

// seedDatabase generates a realistic dataset and hands back the site it belongs
// to. The generator runs the real derive pipeline, so the visitor ids are
// hashed with a salt that really does rotate at UTC midnight — which is what
// makes this a test of the carry-over arithmetic rather than of tidy data.
func seedDatabase(t *testing.T) (*accounts.Account, rollup.Site, time.Time) {
	t.Helper()

	dir := t.TempDir()
	now := time.Date(2026, 8, 30, 19, 30, 0, 0, time.UTC)

	if _, err := seed.Run(context.Background(), seed.Options{
		DataDir:           dir,
		Pageviews:         8_000,
		Days:              9,
		Sites:             1,
		Seed:              414243,
		Now:               func() time.Time { return now },
		Out:               io.Discard,
		ControlMigrations: migrate.Control(),
	}); err != nil {
		t.Fatal(err)
	}

	control, err := store.Open(filepath.Join(dir, config.ControlDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var (
		site      rollup.Site
		accountID int64
	)

	if err := control.QueryRow(
		"SELECT id, account_id, domain, timezone FROM sites WHERE domain = ?", seed.PrimaryDomain()).
		Scan(&site.ID, &accountID, &site.Domain, &site.Timezone); err != nil {
		t.Fatal(err)
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = manager.CloseAll() })

	account, err := manager.Open(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}

	return account, site, now
}

// answer runs a query or fails the test.
func answer(t *testing.T, engine *query.Engine, q query.Query) *query.Result {
	t.Helper()

	result, err := engine.Run(context.Background(), q)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	return result
}

// compare asserts that two answers to the same question hold the same groups
// with the same numbers.
//
// The rows are matched by their labels rather than by position. Two groups with
// identical numbers are tied, and the database breaks a tie by the interned id
// while the in-memory path breaks it by the label — an ordering difference that
// says nothing about whether the numbers are right, which is what this is
// checking.
func compare(t *testing.T, name string, metrics []string, want, got *query.Result) {
	t.Helper()

	byKey := func(result *query.Result) map[string][]float64 {
		rows := map[string][]float64{}
		for _, row := range result.Results {
			rows[fmt.Sprint(row.Dimensions)] = row.Metrics
		}

		return rows
	}

	left, right := byKey(want), byKey(got)

	if len(left) != len(right) {
		t.Errorf("%s: raw returned %d groups, the summary returned %d", name, len(left), len(right))
	}

	if want.Meta.TotalRows != nil && got.Meta.TotalRows != nil && *want.Meta.TotalRows != *got.Meta.TotalRows {
		t.Errorf("%s: total_rows is %d from raw and %d from the summary",
			name, *want.Meta.TotalRows, *got.Meta.TotalRows)
	}

	for key, wantMetrics := range left {
		gotMetrics, ok := right[key]
		if !ok {
			t.Errorf("%s: the summary is missing the group %s", name, key)
			continue
		}

		for j := range metrics {
			if j >= len(wantMetrics) || j >= len(gotMetrics) {
				t.Errorf("%s: group %s has %d metrics from raw and %d from the summary",
					name, key, len(wantMetrics), len(gotMetrics))
				break
			}

			// A hair of tolerance, because two ratios of the same sums round at
			// the last decimal place in different orders.
			if math.Abs(wantMetrics[j]-gotMetrics[j]) > 0.011 {
				t.Errorf("%s: group %s %s = %v from raw, %v from the summary",
					name, key, metrics[j], wantMetrics[j], gotMetrics[j])
			}
		}
	}

	for key := range right {
		if _, ok := left[key]; !ok {
			t.Errorf("%s: the summary invented the group %s", name, key)
		}
	}
}

// benchDirEnv points the benchmark at a data directory that already holds a
// seeded database. It is an environment variable rather than a flag because the
// benchmark is a measurement rather than a check: the dataset takes minutes to
// generate, so it is generated once and pointed at many times.
const benchDirEnv = "FEASIBLE_ROLLUP_BENCH_DIR"

// TestReportTimings measures the reports a dashboard load fires, once against
// the raw tables and once against the summaries.
//
// It is the point of this milestone rather than a check on it, so it is skipped
// unless somebody asks for it:
//
//	feasible seed --data-dir /tmp/big --pageviews 1000000 --days 30 --sites 1 --fresh
//	FEASIBLE_ROLLUP_BENCH_DIR=/tmp/big go test ./internal/rollup -run TestReportTimings -v
func TestReportTimings(t *testing.T) {
	dir := os.Getenv(benchDirEnv)
	if dir == "" {
		t.Skipf("set %s to a seeded data directory to measure the reports", benchDirEnv)
	}

	control, err := store.Open(filepath.Join(dir, config.ControlDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var (
		site      rollup.Site
		accountID int64
	)

	if err := control.QueryRow(
		"SELECT id, account_id, domain, timezone FROM sites WHERE domain = ?", seed.PrimaryDomain()).
		Scan(&site.ID, &accountID, &site.Domain, &site.Timezone); err != nil {
		t.Fatal(err)
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = manager.CloseAll() })

	account, err := manager.Open(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}

	var events, visits int64
	if err := account.Reader().QueryRow("SELECT COUNT(*) FROM events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := account.Reader().QueryRow("SELECT COUNT(*) FROM sessions").Scan(&visits); err != nil {
		t.Fatal(err)
	}

	// The clock is the real one: the seeded history ends today, so measuring
	// against a pinned instant would measure a range with no traffic in it.
	now := time.Now().UTC()

	raw := query.New(account.Reader())
	raw.Router = query.RawRouter{}
	raw.Now = func() time.Time { return now }

	rolled := query.New(account.Reader())
	rolled.Now = func() time.Time { return now }

	headline := []string{"visitors", "visits", "pageviews", "bounce_rate", "visit_duration", "views_per_visit"}
	breakdown := []string{"visitors", "visits", "pageviews", "bounce_rate"}

	reports := []struct {
		name  string
		query query.Query
	}{
		{"Top stats, today", query.Query{Metrics: headline, DateRange: query.DateRange{Preset: query.RangeDay}}},
		{"Top stats, 7 days", query.Query{Metrics: headline, DateRange: query.DateRange{Preset: query.RangeLast7Days}}},
		{"Top stats, 28 days", query.Query{Metrics: headline, DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Top stats, all time", query.Query{Metrics: headline, DateRange: query.DateRange{Preset: query.RangeAll}}},
		{"Top pages, 28 days", query.Query{Metrics: breakdown, Dimensions: []string{"event:page"},
			DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Top pages, all time", query.Query{Metrics: breakdown, Dimensions: []string{"event:page"},
			DateRange: query.DateRange{Preset: query.RangeAll}}},
		{"Top sources, 28 days", query.Query{Metrics: breakdown, Dimensions: []string{"visit:source"},
			DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Top entry pages, 28 days", query.Query{Metrics: []string{"visitors", "visits", "bounce_rate", "visit_duration"},
			Dimensions: []string{"visit:entry_page"}, DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Countries, 28 days", query.Query{Metrics: breakdown, Dimensions: []string{"visit:country"},
			DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Browsers, 28 days", query.Query{Metrics: breakdown, Dimensions: []string{"visit:browser"},
			DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Main graph, 28 days by day", query.Query{Metrics: []string{"visitors", "pageviews"},
			Dimensions: []string{"time:day"}, DateRange: query.DateRange{Preset: query.RangeLast28Days}}},
		{"Main graph, 7 days by hour", query.Query{Metrics: []string{"visitors", "pageviews"},
			Dimensions: []string{"time:hour"}, DateRange: query.DateRange{Preset: query.RangeLast7Days}}},
		{"Realtime, 30 min", query.Query{Metrics: []string{"visitors"}, DateRange: query.DateRange{Preset: query.RangeRealtime}}},
		{"Filtered by country, 28 days", query.Query{Metrics: breakdown, Dimensions: []string{"event:page"},
			DateRange: query.DateRange{Preset: query.RangeLast28Days},
			Filters:   []query.Filter{{Operator: query.OpIs, Dimension: "visit:country", Values: []string{"US"}}}}},
		{"Filtered page contains /blog", query.Query{Metrics: breakdown,
			DateRange: query.DateRange{Preset: query.RangeLast28Days},
			Filters:   []query.Filter{{Operator: query.OpContains, Dimension: "event:page", Values: []string{"/blog"}}}}},
	}

	t.Logf("%d events / %d visits", events, visits)
	t.Logf("%-30s %12s %12s  %s", "Query", "raw", "roll-up", "read from")

	for _, report := range reports {
		q := report.query
		q.SiteIDs = []int64{site.ID}
		q.Timezone = site.Timezone
		q.Pagination = query.Pagination{Limit: 100}

		rawTime, _ := timeReport(t, raw, q)
		rolledTime, sources := timeReport(t, rolled, q)

		t.Logf("%-30s %10.1f ms %10.1f ms  %s", report.name,
			float64(rawTime.Microseconds())/1000, float64(rolledTime.Microseconds())/1000,
			strings.Join(sources, "+"))
	}
}

// timeReport runs a report three times and returns the middle time with where
// the numbers came from. Three runs because the first warms the page cache and
// the difference between the second and third is noise.
func timeReport(t *testing.T, engine *query.Engine, q query.Query) (time.Duration, []string) {
	t.Helper()

	var (
		runs    []time.Duration
		sources []string
	)

	for i := 0; i < 3; i++ {
		started := time.Now()

		result, err := engine.Run(context.Background(), q)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}

		runs = append(runs, time.Since(started))
		sources = result.Meta.Sources
	}

	sort.Slice(runs, func(i, j int) bool { return runs[i] < runs[j] })

	return runs[1], sources
}
