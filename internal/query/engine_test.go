//
// engine_test.go
// Every metric asserted against a hand-computed fixture.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// fixtureNow is the instant every test resolves its date range against. It is
// fixed because "today" is the hardest thing to test in an analytics product,
// and a suite whose answers change at midnight is a suite nobody trusts.
var fixtureNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// The fixture's visitors. They are fingerprints, so they are just integers.
const (
	visitorA int64 = 1001
	visitorB int64 = 1002
	visitorC int64 = 1003
)

// at builds a timestamp inside the fixture's window.
func at(day, hour, minute int) int64 {
	return time.Date(2026, 8, day, hour, minute, 0, 0, time.UTC).Unix()
}

// sessionRow is one row of the sessions table, written directly rather than
// folded by the ingest pipeline. Writing it directly is deliberate: these tests
// are about what the query engine counts, and a fixture that went through the
// pipeline would fail for two different reasons at once.
type sessionRow struct {
	id        int64
	user      int64
	startedAt int64
	duration  int
	bounce    int
	pageviews int
	events    int
	entryPage string
	exitPage  string
	source    string
	country   string
	browser   string
}

// eventRow is one row of the events table.
type eventRow struct {
	session    int64
	user       int64
	timestamp  int64
	name       string
	page       string
	title      string
	source     string
	country    string
	browser    string
	scroll     int
	engagement int64
	botReason  string
	props      map[string]string
}

// fixtureSessions is the visit-grain half of the fixture. Every number in every
// assertion below is worked out by hand from these four rows and the ten events
// beneath them.
var fixtureSessions = []sessionRow{
	{id: 1, user: visitorA, startedAt: at(29, 10, 0), duration: 120, bounce: 0, pageviews: 3, events: 3,
		entryPage: "/home", exitPage: "/pricing", source: "Google", country: "US", browser: "Chrome"},
	{id: 2, user: visitorB, startedAt: at(29, 11, 0), duration: 0, bounce: 1, pageviews: 1, events: 1,
		entryPage: "/pricing", exitPage: "/pricing", source: "Google", country: "US", browser: "Firefox"},
	{id: 3, user: visitorA, startedAt: at(30, 9, 0), duration: 60, bounce: 0, pageviews: 2, events: 3,
		entryPage: "/home", exitPage: "/about", source: "", country: "CA", browser: "Chrome"},
	{id: 4, user: visitorC, startedAt: at(30, 10, 0), duration: 0, bounce: 1, pageviews: 1, events: 1,
		entryPage: "/home", exitPage: "/home", source: "Twitter", country: "US", browser: "Chrome"},
}

// fixtureEvents is the hit-grain half.
var fixtureEvents = []eventRow{
	// Visit 1 — three pageviews and one engagement ping.
	{session: 1, user: visitorA, timestamp: at(29, 10, 0), name: ingest.EventPageview, page: "/home", title: "Home", source: "Google", country: "US", browser: "Chrome"},
	{session: 1, user: visitorA, timestamp: at(29, 10, 1), name: ingest.EventPageview, page: "/pricing", title: "Pricing", source: "Google", country: "US", browser: "Chrome"},
	{session: 1, user: visitorA, timestamp: at(29, 10, 2), name: ingest.EventPageview, page: "/pricing", title: "Pricing", source: "Google", country: "US", browser: "Chrome"},
	{session: 1, user: visitorA, timestamp: at(29, 10, 2), name: ingest.EventEngagement, page: "/pricing", source: "Google", country: "US", browser: "Chrome", scroll: 80, engagement: 45000},

	// Visit 2 — a bounce that still reported engagement.
	{session: 2, user: visitorB, timestamp: at(29, 11, 0), name: ingest.EventPageview, page: "/pricing", title: "Pricing", source: "Google", country: "US", browser: "Firefox"},
	{session: 2, user: visitorB, timestamp: at(29, 11, 0), name: ingest.EventEngagement, page: "/pricing", source: "Google", country: "US", browser: "Firefox", scroll: 30, engagement: 5000},

	// Visit 3 — today, and the only visit that converted.
	{session: 3, user: visitorA, timestamp: at(30, 9, 0), name: ingest.EventPageview, page: "/home", title: "Home", country: "CA", browser: "Chrome"},
	{session: 3, user: visitorA, timestamp: at(30, 9, 1), name: ingest.EventPageview, page: "/about", title: "About", country: "CA", browser: "Chrome"},
	{session: 3, user: visitorA, timestamp: at(30, 9, 2), name: "Signup", page: "/about", country: "CA", browser: "Chrome", props: map[string]string{"plan": "pro"}},

	// Visit 4 — today, a bounce.
	{session: 4, user: visitorC, timestamp: at(30, 10, 0), name: ingest.EventPageview, page: "/home", title: "Home", source: "Twitter", country: "US", browser: "Chrome"},
}

// newEngine builds an engine over a freshly migrated account database seeded
// with the fixture.
func newEngine(t *testing.T) *Engine {
	t.Helper()

	engine, _ := newEngineWithAccount(t)

	return engine
}

// newEngineWithAccount also hands back the account, for the few tests that add
// rows after the fixture has been seeded. The engine itself only ever sees the
// read-only handle, which is the same separation the serving path uses.
func newEngineWithAccount(t *testing.T) (*Engine, *accounts.Account) {
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

	seed(t, account)

	engine := New(account.Reader())
	engine.Now = func() time.Time { return fixtureNow }

	return engine, account
}

// newAccountThrough opens a test account at one real migration boundary. Large
// backfill fixtures stop at M9 version 10, write their historical facts, and
// then exercise sampling migration 0011 through the production runner.
func newAccountThrough(t testing.TB, version int) *accounts.Account {
	t.Helper()

	db, err := store.OpenDatabase(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close account database: %v", err)
		}
	})

	if _, err := migrate.Run(context.Background(), db.Writer(), migrate.UpTo(migrate.Account(), version)); err != nil {
		t.Fatal(err)
	}

	cache := intern.New(db.Writer())
	if err := cache.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}

	return &accounts.Account{ID: 1, DB: db, Intern: cache}
}

// installSamplingSchema upgrades a version-10 fixture through account 0011
// with the same migration runner production maintenance uses.
func installSamplingSchema(t testing.TB, db *sql.DB) {
	t.Helper()

	if _, err := migrate.Run(context.Background(), db, migrate.Account()); err != nil {
		t.Fatalf("install sampling test schema: %v", err)
	}
}

// seed writes the fixture, interning every dimension string exactly as the
// ingest path would.
func seed(t *testing.T, account *accounts.Account) {
	t.Helper()

	ctx := context.Background()

	id := func(dimension intern.Dimension, value string) int64 {
		got, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	for _, session := range fixtureSessions {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
				pageviews, events, entry_page_id, exit_page_id, source_id, country_id, browser_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			session.id, 1, session.user, session.startedAt, session.startedAt+int64(session.duration),
			session.duration, session.bounce, session.pageviews, session.events,
			id(intern.Pathname, session.entryPage), id(intern.Pathname, session.exitPage),
			id(intern.Source, session.source), id(intern.Country, session.country),
			id(intern.Browser, session.browser),
		); err != nil {
			t.Fatal(err)
		}
	}

	for i, event := range fixtureEvents {
		scroll := event.scroll
		if scroll == 0 {
			// 255 is the "never reported" marker the schema uses, and the one
			// the scroll-depth query has to exclude.
			scroll = 255
		}

		result, err := account.Writer().ExecContext(ctx, `
			INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id,
				pathname_id, page_title_id, source_id, country_id, browser_id,
				scroll_depth, engagement_time, bot_reason_id, has_details)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			i+1, 1, event.timestamp, id(intern.EventName, event.name), event.user, event.session,
			id(intern.Pathname, event.page), id(intern.PageTitle, event.title), id(intern.Source, event.source),
			id(intern.Country, event.country), id(intern.Browser, event.browser),
			scroll, event.engagement, id(intern.BotReason, event.botReason), boolInt(len(event.props) > 0),
		)
		if err != nil {
			t.Fatal(err)
		}

		if len(event.props) == 0 {
			continue
		}

		eventID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}

		encoded, err := json.Marshal(event.props)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := account.Writer().ExecContext(ctx,
			"INSERT INTO event_details (event_id, props) VALUES (?, ?)", eventID, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPageTitlesComposeWithVisitMetrics checks the query used by Top Pages.
// Titles enrich the path rather than becoming a grouping key: engagement,
// custom events and Vitals all carry the route with a blank title, and none may
// split a path into a second blank-titled row.
func TestPageTitlesComposeWithVisitMetrics(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	ctx := context.Background()
	for i, event := range []struct {
		name string
		page string
	}{
		{name: "Custom Route Event", page: "/about"},
		{name: "Web Vitals", page: "/home"},
	} {
		nameID, err := account.Intern.ID(ctx, intern.EventName, event.name)
		if err != nil {
			t.Fatal(err)
		}
		pageID, err := account.Intern.ID(ctx, intern.Pathname, event.page)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id)
			VALUES (?, 1, ?, ?, ?, 3, ?)`, 100+i, at(30, 9, 10+i), nameID, visitorA, pageID); err != nil {
			t.Fatal(err)
		}
	}

	q := baseQuery("visitors", "visits", "pageviews", "bounce_rate", "visit_duration")
	q.Dimensions = []string{"event:page"}
	q.Include.PageTitles = true

	result := run(t, engine, q)
	rows := map[string]Row{}
	for _, row := range result.Results {
		if _, duplicate := rows[row.Dimensions[0]]; duplicate {
			t.Fatalf("path %q was split into titled and blank rows: %+v", row.Dimensions[0], result.Results)
		}
		if len(row.Dimensions) != 1 {
			t.Fatalf("title became a grouping dimension for %q: %+v", row.Dimensions[0], row)
		}
		if row.Enrichments["page_title"] == "" {
			t.Fatalf("path %q lost its representative title: %+v", row.Dimensions[0], result.Results)
		}
		rows[row.Dimensions[0]] = row
	}
	if len(rows) != 3 {
		t.Fatalf("Top Pages returned %d path rows, want 3: %+v", len(rows), result.Results)
	}

	home, ok := rows["/home"]
	if !ok {
		t.Fatalf("home title was not returned alongside its path: %+v", result.Results)
	}
	if home.Enrichments["page_title"] != "Home" {
		t.Fatalf("home title = %q, want Home", home.Enrichments["page_title"])
	}

	closeTo(t, "home visits", home.Metrics[1], 3)
	closeTo(t, "home bounce rate", home.Metrics[3], 33.333)

	pricing, ok := rows["/pricing"]
	if !ok {
		t.Fatalf("pricing title was not returned alongside its path: %+v", result.Results)
	}

	// Visits beside pageviews are visits that touched the page; the visit-grain
	// bounce rate is entry-scoped and therefore uses only the pricing entrance.
	closeTo(t, "pricing visits", pricing.Metrics[1], 2)
	closeTo(t, "pricing bounce rate", pricing.Metrics[3], 100)
}

// TestPageTitleEnrichmentUsesCleanedSourcesAndThePathTimeIndex proves both
// lookup contracts: a cleaned target receives the latest title captured on its
// source path, and SQLite constrains each source by site, path and time through
// events_page rather than scanning the site's events.
func TestPageTitleEnrichmentUsesCleanedSourcesAndThePathTimeIndex(t *testing.T) {
	engine, account := newUnseededSamplingEngine(t)
	ctx := context.Background()

	source, err := account.Intern.ID(ctx, intern.Pathname, "/products/42")
	if err != nil {
		t.Fatal(err)
	}
	target, err := account.Intern.ID(ctx, intern.Pathname, "/products/:id")
	if err != nil {
		t.Fatal(err)
	}
	title, err := account.Intern.ID(ctx, intern.PageTitle, "Product detail")
	if err != nil {
		t.Fatal(err)
	}
	latestTitle, err := account.Intern.ID(ctx, intern.PageTitle, "Product detail updated")
	if err != nil {
		t.Fatal(err)
	}
	futureTitle, err := account.Intern.ID(ctx, intern.PageTitle, "Product detail future")
	if err != nil {
		t.Fatal(err)
	}
	pageview, err := account.Intern.ID(ctx, intern.EventName, "pageview")
	if err != nil {
		t.Fatal(err)
	}
	custom, err := account.Intern.ID(ctx, intern.EventName, "Signup")
	if err != nil {
		t.Fatal(err)
	}
	excludedTitle, err := account.Intern.ID(ctx, intern.PageTitle, "Excluded candidate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO path_clean_map (site_id, source_id, target_id) VALUES (1, ?, ?)", source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id, page_title_id)
		VALUES (1, 1, ?, ?, 1, 1, ?, ?)`, at(29, 0, 0), pageview, source, title); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events
			(id, site_id, timestamp, name_id, user_id, session_id, pathname_id, page_title_id, is_imported, bot_reason_id)
		VALUES (5, 1, ?, ?, 1, 1, ?, ?, 0, 0),
		       (6, 1, ?, ?, 1, 1, ?, ?, 1, 0),
		       (7, 1, ?, ?, 1, 1, ?, ?, 0, 1)`,
		at(30, 2, 0), custom, source, excludedTitle,
		at(30, 3, 0), pageview, source, excludedTitle,
		at(30, 4, 0), pageview, source, excludedTitle); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id, page_title_id)
		VALUES (2, 1, ?, ?, 1, 1, ?, ?),
		       (3, 1, ?, ?, 1, 1, ?, 0),
		       (4, 1, ?, ?, 1, 1, ?, ?)`,
		at(30, 0, 0), pageview, source, latestTitle,
		at(30, 1, 0), pageview, source,
		at(31, 0, 0), pageview, source, futureTitle); err != nil {
		t.Fatal(err)
	}

	q := baseQuery("events")
	q.Dimensions = []string{"event:page"}
	q.Include.PageTitles = true
	result := run(t, engine, q)
	if len(result.Results) != 1 || result.Results[0].Dimensions[0] != "/products/:id" {
		t.Fatalf("cleaned page rows = %+v", result.Results)
	}
	if got := result.Results[0].Enrichments["page_title"]; got != "Product detail updated" {
		t.Fatalf("cleaned page title = %q, want Product detail updated", got)
	}

	resolved, err := q.DateRange.Resolve(fixtureNow, time.UTC, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	q.Normalise()
	sqlText, args, err := pageTitleEnrichmentQuery([]int64{target}, []int64{1}, resolved, &q, pageview)
	if err != nil {
		t.Fatal(err)
	}
	plan := explainPlan(t, account, sqlText, args)
	if !strings.Contains(plan, "SEARCH candidate USING INDEX events_page") ||
		!strings.Contains(plan, "site_id=? AND pathname_id=? AND timestamp>?") {
		t.Fatalf("title enrichment is not a bounded path/time seek:\n%s", plan)
	}
	if !strings.Contains(plan, "path_clean_map_target") {
		t.Fatalf("title enrichment did not use the bounded reverse-clean-path index:\n%s", plan)
	}
	if !strings.Contains(sqlText, "candidate.name_id = ?") ||
		!strings.Contains(sqlText, "candidate.bot_reason_id = 0") ||
		!strings.Contains(sqlText, "candidate.is_imported = 0") {
		t.Fatalf("title candidates do not match event population:\n%s", sqlText)
	}

	defaultRate := q
	defaultRate.SampleRate = 0
	defaultSQL, defaultArgs, err := pageTitleEnrichmentQuery([]int64{target}, []int64{1}, resolved, &defaultRate, pageview)
	if err != nil {
		t.Fatal(err)
	}
	_ = explainPlan(t, account, defaultSQL, defaultArgs)

	q.SampleRate = 0.5
	sampledSQL, sampledArgs, err := pageTitleEnrichmentQuery([]int64{target}, []int64{1}, resolved, &q, pageview)
	if err != nil {
		t.Fatal(err)
	}
	sampledPlan := explainPlan(t, account, sampledSQL, sampledArgs)
	if !strings.Contains(sampledPlan, "event_sampling_seek") {
		t.Fatalf("sampled title candidates escaped sampled population:\n%s", sampledPlan)
	}
}

// BenchmarkPageTitleEnrichment measures the post-aggregation lookup over a
// 750k-row/100-path account. Runtime should scale with displayed source paths,
// not with one scalar lookup per aggregate fact row.
func BenchmarkPageTitleEnrichment(b *testing.B) {
	account := newAccountThrough(b, 10)

	ctx := context.Background()
	const paths = 100
	for i := 1; i <= paths; i++ {
		pathID, err := account.Intern.ID(ctx, intern.Pathname, fmt.Sprintf("/page/%03d", i))
		if err != nil {
			b.Fatal(err)
		}
		titleID, err := account.Intern.ID(ctx, intern.PageTitle, fmt.Sprintf("Page %03d", i))
		if err != nil {
			b.Fatal(err)
		}
		if pathID != int64(i) || titleID != int64(i) {
			b.Fatalf("fixture ids are path/title %d/%d, want %d", pathID, titleID, i)
		}
	}

	const events = 750_100
	if _, err := account.Writer().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, pathname_id, page_title_id)
		SELECT n, 1, ? + (n % 3600), 0, n, n, 1 + ((n - 1) % 100), 1 + ((n - 1) % 100) FROM seq`,
		events, at(29, 0, 0)); err != nil {
		b.Fatal(err)
	}
	installSamplingSchema(b, account.Writer())

	page, _ := resolveDimension("event:page")
	blueprint := &plan{Dimensions: []dimension{page}}
	rows := make([]finalRow, paths)
	for i := range rows {
		rows[i].raw = []any{int64(i + 1)}
	}
	executor := &executor{
		engine: New(account.Reader()),
		query:  &Query{SiteIDs: []int64{1}, Include: Include{PageTitles: true}},
		plan:   blueprint,
		resolved: Resolved{
			Start: time.Unix(at(28, 0, 0), 0),
			End:   time.Unix(at(30, 0, 0), 0),
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		titles, err := executor.pageTitleEnrichments(ctx, rows)
		if err != nil {
			b.Fatal(err)
		}
		if len(titles) != paths {
			b.Fatalf("title lookup returned %d paths, want %d", len(titles), paths)
		}
	}
	b.ReportMetric(paths, "path_seeks/op")
}

// boolInt renders a flag the way the schema stores it.
func boolInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

// run answers a query or fails the test, so that every assertion below reads as
// one line of expectation rather than three of error handling.
func run(t *testing.T, engine *Engine, q Query) *Result {
	t.Helper()

	result, err := engine.Run(context.Background(), q)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	return result
}

// baseQuery is the fixture's whole window with no filters.
func baseQuery(metrics ...string) Query {
	return Query{
		SiteIDs:   []int64{1},
		Metrics:   metrics,
		DateRange: DateRange{Preset: RangeLast7Days},
		Timezone:  "UTC",
	}
}

// TestGoalFilterResolvesTheStoredDefinition ensures a dashboard goal click is
// one canonical predicate rather than a lossy frontend approximation.
func TestGoalFilterResolvesTheStoredDefinition(t *testing.T) {
	engine, account := newEngineWithAccount(t)
	ctx := context.Background()
	result, err := account.Writer().ExecContext(ctx, `
		INSERT INTO goals (site_id, kind, page_pattern, created_at, signature)
		VALUES (1, 'page', '/pricing', 0, 'pricing')`)
	if err != nil {
		t.Fatal(err)
	}
	goalID, _ := result.LastInsertId()

	q := baseQuery("visitors", "visits", "events")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "event:goal", Values: []string{strconv.FormatInt(goalID, 10)}}}
	report := run(t, engine, q)
	if got := report.Results[0].Metrics; got[0] != 2 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("page goal filter metrics = %v, want 2 visitors, 2 visits, and 3 pageviews", got)
	}

	result, err = account.Writer().ExecContext(ctx, `
		INSERT INTO goals (site_id, kind, event_name, created_at, signature)
		VALUES (1, 'event', 'Signup', 0, 'signup-pro')`)
	if err != nil {
		t.Fatal(err)
	}
	propertyGoalID, _ := result.LastInsertId()
	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO goal_properties (goal_id, name, value) VALUES (?, 'plan', 'pro')", propertyGoalID); err != nil {
		t.Fatal(err)
	}
	q.Filters[0].Values = []string{strconv.FormatInt(propertyGoalID, 10)}
	report = run(t, engine, q)
	if got := report.Results[0].Metrics; got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("property goal filter metrics = %v, want one matching signup", got)
	}
}

// closeTo compares a metric against a hand-computed value.
func closeTo(t *testing.T, name string, got, want float64) {
	t.Helper()

	if math.Abs(got-want) > 0.005 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestEveryMetricAgainstHandComputedValues is the test the product depends on.
// Each expectation is worked out from the fixture by hand and written next to
// the definition it checks, because a metric that is subtly wrong is worse than
// a metric that is missing: nobody goes looking for the cause of a number that
// looks plausible.
func TestEveryMetricAgainstHandComputedValues(t *testing.T) {
	engine := newEngine(t)

	cases := []struct {
		metric string
		want   float64
		why    string
	}{
		{"visitors", 3, "three distinct fingerprints across four visits"},
		{"visits", 4, "four sessions"},
		{"pageviews", 7, "3 + 1 + 2 + 1"},
		{"events", 8, "ten rows less the two engagement pings"},
		{"bounce_rate", 50, "two of four visits bounced"},
		{"visit_duration", 45, "(120 + 0 + 60 + 0) / 4, bounces counted as zero"},
		{"views_per_visit", 1.75, "(3 + 1 + 2 + 1) / 4"},
		{"time_on_page", 25, "(45000 + 5000) ms over the two visits that reported engagement"},
		{"scroll_depth", 55, "the deepest point per visit, 80 and 30, averaged"},
		{"exit_rate", 57.143, "four exits over seven pageviews"},
	}

	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			result := run(t, engine, baseQuery(tc.metric))

			if len(result.Results) != 1 {
				t.Fatalf("got %d rows, want one aggregate row", len(result.Results))
			}

			closeTo(t, tc.metric+" ("+tc.why+")", result.Results[0].Metrics[0], tc.want)
		})
	}
}

// TestVisitDurationCountsBouncesAsZero pins the definition apart from the one
// that excludes them. Excluding bounces answers "how long did the visits that
// stayed last", which is always a larger number and a different question.
func TestVisitDurationCountsBouncesAsZero(t *testing.T) {
	engine := newEngine(t)

	result := run(t, engine, baseQuery("visit_duration"))

	// Excluding the two bounces would give (120 + 60) / 2 = 90.
	closeTo(t, "visit_duration", result.Results[0].Metrics[0], 45)
}

// TestScrollDepthIgnoresTheNeverReportedMarker checks that 255 stays out of the
// average. It is the schema's "no measurement" value, and averaging it in would
// report a scroll depth above 100% on any page nobody scrolled.
func TestScrollDepthIgnoresTheNeverReportedMarker(t *testing.T) {
	engine := newEngine(t)

	result := run(t, engine, baseQuery("scroll_depth"))

	closeTo(t, "scroll_depth", result.Results[0].Metrics[0], 55)
}

// TestBreakdownByPage checks an ordinary event-scoped breakdown, including that
// the interned ids come back as strings.
func TestBreakdownByPage(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews", "visitors")
	q.Dimensions = []string{"event:page"}

	result := run(t, engine, q)

	want := map[string][2]float64{
		"/home":    {3, 2},
		"/pricing": {3, 2},
		"/about":   {1, 1},
	}

	if len(result.Results) != len(want) {
		t.Fatalf("got %d rows, want %d", len(result.Results), len(want))
	}

	for _, row := range result.Results {
		expected, ok := want[row.Dimensions[0]]
		if !ok {
			t.Fatalf("unexpected page %q", row.Dimensions[0])
		}

		closeTo(t, row.Dimensions[0]+" pageviews", row.Metrics[0], expected[0])
		closeTo(t, row.Dimensions[0]+" visitors", row.Metrics[1], expected[1])
	}
}

// TestBounceRateUnderAPageBreakdownIsScopedToEntrances is the subtlety this
// whole package exists to get right. /pricing was touched by two visits, one of
// which bounced, so a naive answer is 50%. Only one visit *entered* on
// /pricing, and it bounced, so the correct answer is 100%.
func TestBounceRateUnderAPageBreakdownIsScopedToEntrances(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews", "bounce_rate")
	q.Dimensions = []string{"event:page"}

	result := run(t, engine, q)

	got := map[string]float64{}
	for _, row := range result.Results {
		got[row.Dimensions[0]] = row.Metrics[1]
	}

	closeTo(t, "/pricing bounce_rate", got["/pricing"], 100)
	closeTo(t, "/home bounce_rate", got["/home"], 33.333)

	warning, ok := result.Meta.MetricWarnings["bounce_rate"]
	if !ok {
		t.Fatal("a re-scoped bounce rate must announce itself in meta.metric_warnings")
	}

	if warning.Code != WarnEntryScoped {
		t.Errorf("warning code = %q, want %q", warning.Code, WarnEntryScoped)
	}
}

// TestSessionMetricUnderAnEventDimensionWithNoAnalogueIsRefused checks the
// other half of the guard rail: where there is no correctly-scoped answer, the
// query is refused rather than answered with a plausible wrong number.
func TestSessionMetricUnderAnEventDimensionWithNoAnalogueIsRefused(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("bounce_rate")
	q.Dimensions = []string{"event:name"}

	_, err := engine.Run(context.Background(), q)
	if err == nil {
		t.Fatal("a bounce rate per custom event name has no correct value and must be refused")
	}

	if _, ok := err.(*Error); !ok {
		t.Fatalf("want a caller-facing error, got %T", err)
	}
}

// TestSessionMetricUnderAnEventFilterSelectsWholeVisits checks the composable
// case: a filter on a custom event name selects the visits that contain one,
// and says so.
func TestSessionMetricUnderAnEventFilterSelectsWholeVisits(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("visits", "bounce_rate")
	q.Filters = []Filter{{Operator: OpIs, Dimension: "event:name", Values: []string{"Signup"}}}

	result := run(t, engine, q)

	// Only visit 3 contains a signup, and it did not bounce.
	closeTo(t, "visits", result.Results[0].Metrics[0], 1)
	closeTo(t, "bounce_rate", result.Results[0].Metrics[1], 0)

	if warning, ok := result.Meta.MetricWarnings["bounce_rate"]; !ok || warning.Code != WarnSessionScoped {
		t.Errorf("a visit-scoped metric under an event filter must announce its scope, got %+v", warning)
	}
}

// TestConversionRateStripsTheGoalFromItsDenominator checks that the second
// query really does drop the goal filter. One visitor of three signed up.
func TestConversionRateStripsTheGoalFromItsDenominator(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("conversion_rate")
	q.Filters = []Filter{{
		Operator: OpHasDone,
		Child:    &Filter{Operator: OpIs, Dimension: "event:name", Values: []string{"Signup"}},
	}}

	result := run(t, engine, q)

	closeTo(t, "conversion_rate", result.Results[0].Metrics[0], 33.333)
}

// TestConversionRateWithoutAGoalIsRefused checks that the metric cannot report
// a meaningless 100%.
func TestConversionRateWithoutAGoalIsRefused(t *testing.T) {
	engine := newEngine(t)

	if _, err := engine.Run(context.Background(), baseQuery("conversion_rate")); err == nil {
		t.Fatal("a conversion rate with nothing to convert must be refused")
	}
}

// TestExitRateIsMeasuredAgainstPageviews pins the denominator. /pricing ended
// two visits and was viewed three times, so the rate is two thirds — measuring
// it against the one visit that *entered* on /pricing would give 200%.
func TestExitRateIsMeasuredAgainstPageviews(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("exit_rate")
	q.Dimensions = []string{"event:page"}

	result := run(t, engine, q)

	got := map[string]float64{}
	for _, row := range result.Results {
		got[row.Dimensions[0]] = row.Metrics[0]
	}

	closeTo(t, "/pricing exit_rate", got["/pricing"], 66.667)
	closeTo(t, "/home exit_rate", got["/home"], 33.333)
	closeTo(t, "/about exit_rate", got["/about"], 100)
}

// TestTimeSeriesCoversGapsAndMarksThePresent checks the two pieces of metadata
// a graph cannot be drawn without.
func TestTimeSeriesCoversGapsAndMarksThePresent(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Dimensions = []string{"time:day"}

	result := run(t, engine, q)

	if len(result.Meta.TimeLabels) != 7 {
		t.Fatalf("got %d time labels, want 7", len(result.Meta.TimeLabels))
	}

	if result.Meta.TimeLabels[0] != "2026-08-24" || result.Meta.TimeLabels[6] != "2026-08-30" {
		t.Fatalf("time labels run %s..%s, want 2026-08-24..2026-08-30",
			result.Meta.TimeLabels[0], result.Meta.TimeLabels[6])
	}

	if result.Meta.PresentIndex == nil || *result.Meta.PresentIndex != 6 {
		t.Fatalf("present_index = %v, want 6 — the in-progress bucket is today", result.Meta.PresentIndex)
	}

	if len(result.Results) != 7 {
		t.Fatalf("got %d rows, want one per bucket including the empty ones", len(result.Results))
	}

	// Four quiet days, then four pageviews on the 29th and three today.
	want := []float64{0, 0, 0, 0, 0, 4, 3}
	for i, row := range result.Results {
		closeTo(t, "bucket "+result.Meta.TimeLabels[i], row.Metrics[0], want[i])
	}
}

// TestPresentIndexIsNullForAFinishedRange checks that a range in the past does
// not dash its last bucket.
func TestPresentIndexIsNullForAFinishedRange(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Dimensions = []string{"time:day"}
	q.DateRange = DateRange{Preset: RangeCustom, DateOnly: true,
		Start: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)}

	result := run(t, engine, q)

	if result.Meta.PresentIndex != nil {
		t.Fatalf("present_index = %v, want null for a finished range", *result.Meta.PresentIndex)
	}
}

// TestComparisonComparesTheSameNumberOfHours is the like-for-like rule. At
// noon, today's twelve hours are compared against the first twelve hours of
// yesterday, not against yesterday's whole twenty-four.
func TestComparisonComparesTheSameNumberOfHours(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.DateRange = DateRange{Preset: RangeDay}
	q.Include.Comparisons = &Comparison{Mode: ComparePreviousPeriod}

	result := run(t, engine, q)

	// Today has three pageviews. Yesterday's window is 00:00 to 12:00, which
	// covers the 10:00 and 11:00 visits — four pageviews.
	closeTo(t, "pageviews", result.Results[0].Metrics[0], 3)

	if result.Results[0].Comparison == nil {
		t.Fatal("a comparison was asked for and none came back")
	}

	closeTo(t, "comparison pageviews", result.Results[0].Comparison.Metrics[0], 4)

	if len(result.Meta.ComparisonDateRange) != 2 {
		t.Fatal("the comparison window must be echoed back")
	}

	if result.Meta.ComparisonDateRange[1] != "2026-08-29T12:00:00Z" {
		t.Errorf("comparison ends at %s, want 2026-08-29T12:00:00Z — the same twelve hours",
			result.Meta.ComparisonDateRange[1])
	}
}

// TestResolvedQueryIsEchoedBack checks that the client can read back exactly
// which window it was given.
func TestResolvedQueryIsEchoedBack(t *testing.T) {
	engine := newEngine(t)

	result := run(t, engine, baseQuery("visitors"))

	if len(result.Query.DateRange) != 2 {
		t.Fatal("the resolved date range must be echoed as two instants")
	}

	if result.Query.DateRange[0] != "2026-08-24T00:00:00Z" || result.Query.DateRange[1] != "2026-08-31T00:00:00Z" {
		t.Errorf("echoed range is %v, want 2026-08-24..2026-08-31", result.Query.DateRange)
	}

	if result.Query.Preset != RangeLast7Days {
		t.Errorf("echoed preset = %q", result.Query.Preset)
	}

	if result.Query.Pagination.Limit != DefaultLimit {
		t.Errorf("the echo must show the default page size that actually ran, got %d", result.Query.Pagination.Limit)
	}
}

// TestTimezoneMovesEventsIntoTheLocalDay checks that bucketing happens in the
// site's timezone rather than in UTC. The 09:00 UTC pageview on the 30th is
// 02:00 in Los Angeles on the same day, but the 29th's 10:00 UTC events are
// 03:00 local on the 29th — and an event just after UTC midnight would belong
// to the previous local day.
func TestTimezoneMovesEventsIntoTheLocalDay(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Dimensions = []string{"time:day"}
	q.Timezone = "America/Los_Angeles"

	result := run(t, engine, q)

	got := map[string]float64{}
	for i, row := range result.Results {
		got[result.Meta.TimeLabels[i]] = row.Metrics[0]
	}

	// 10:00 and 11:00 UTC on the 29th are 03:00 and 04:00 local on the 29th.
	closeTo(t, "2026-08-29 in Los Angeles", got["2026-08-29"], 4)

	// 09:00 and 10:00 UTC on the 30th are 02:00 and 03:00 local on the 30th.
	closeTo(t, "2026-08-30 in Los Angeles", got["2026-08-30"], 3)
}

// TestStablePaginationAcrossPages checks that page two does not repeat page
// one. Independently paginated sub-queries are how a breakdown ends up
// returning the same row twice with different columns filled in.
func TestStablePaginationAcrossPages(t *testing.T) {
	engine := newEngine(t)

	seen := map[string]bool{}

	for offset := 0; offset < 3; offset++ {
		q := baseQuery("pageviews", "bounce_rate")
		q.Dimensions = []string{"event:page"}
		q.Pagination = Pagination{Limit: 1, Offset: offset}

		result := run(t, engine, q)

		if len(result.Results) != 1 {
			t.Fatalf("offset %d returned %d rows", offset, len(result.Results))
		}

		page := result.Results[0].Dimensions[0]
		if seen[page] {
			t.Fatalf("page %q appeared on two pages of the same result set", page)
		}

		seen[page] = true
	}

	if len(seen) != 3 {
		t.Fatalf("saw %d distinct pages across three pages of one row each", len(seen))
	}
}

// TestTotalRowsCountsGroupsNotRows checks include.total_rows.
func TestTotalRowsCountsGroupsNotRows(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Dimensions = []string{"event:page"}
	q.Pagination = Pagination{Limit: 1}
	q.Include.TotalRows = true

	result := run(t, engine, q)

	if result.Meta.TotalRows == nil || *result.Meta.TotalRows != 3 {
		t.Fatalf("total_rows = %v, want 3", result.Meta.TotalRows)
	}
}

// TestBotTrafficIsExcludedUnlessAskedFor checks the default that keeps
// automated traffic out of every number, on both fact tables. Sessions carry no
// bot marker of their own, so the visit-grain answer has to reach through the
// session's events — and getting that wrong would make the two tables disagree
// about how many visits there were.
func TestBotTrafficIsExcludedUnlessAskedFor(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	ctx := context.Background()
	writer := account.Writer()

	reason, err := account.Intern.ID(ctx, intern.BotReason, "datacenter")
	if err != nil {
		t.Fatal(err)
	}

	pageview, err := account.Intern.ID(ctx, intern.EventName, ingest.EventPageview)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce, pageviews, events)
		VALUES (99, 1, 9999, ?, ?, 0, 1, 1, 1)`, at(30, 11, 0), at(30, 11, 0)); err != nil {
		t.Fatal(err)
	}

	if _, err := writer.ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, scroll_depth, bot_reason_id)
		VALUES (99, 1, ?, ?, 9999, 99, 255, ?)`, at(30, 11, 0), pageview, reason); err != nil {
		t.Fatal(err)
	}

	human := run(t, engine, baseQuery("pageviews", "visitors", "visits", "bounce_rate"))
	closeTo(t, "pageviews without bots", human.Results[0].Metrics[0], 7)
	closeTo(t, "visitors without bots", human.Results[0].Metrics[1], 3)
	closeTo(t, "visits without bots", human.Results[0].Metrics[2], 4)
	closeTo(t, "bounce_rate without bots", human.Results[0].Metrics[3], 50)

	q := baseQuery("pageviews", "visitors", "visits", "bounce_rate")
	q.Include.Bots = true

	all := run(t, engine, q)
	closeTo(t, "pageviews with bots", all.Results[0].Metrics[0], 8)
	closeTo(t, "visitors with bots", all.Results[0].Metrics[1], 4)
	closeTo(t, "visits with bots", all.Results[0].Metrics[2], 5)
	closeTo(t, "bounce_rate with bots", all.Results[0].Metrics[3], 60)
}

// TestComparisonLinesUpWithTheRowItIsAttachedTo checks that the earlier
// period's numbers land on the right breakdown row rather than on whichever row
// happened to be in the same position.
func TestComparisonLinesUpWithTheRowItIsAttachedTo(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Dimensions = []string{"visit:country"}
	q.DateRange = DateRange{Preset: RangeDay}
	q.Include.Comparisons = &Comparison{Mode: ComparePreviousPeriod}

	result := run(t, engine, q)

	rows := map[string]Row{}
	for _, row := range result.Results {
		rows[row.Dimensions[0]] = row
	}

	// Today: two pageviews from Canada and one from the United States.
	// The first twelve hours of yesterday: four from the United States, none
	// from Canada.
	us, ok := rows["US"]
	if !ok || us.Comparison == nil {
		t.Fatalf("no comparison for the United States: %+v", rows)
	}

	closeTo(t, "US today", us.Metrics[0], 1)
	closeTo(t, "US yesterday", us.Comparison.Metrics[0], 4)

	if us.Comparison.Change[0] == nil || *us.Comparison.Change[0] != -75 {
		t.Errorf("US change = %v, want -75", us.Comparison.Change[0])
	}

	ca, ok := rows["CA"]
	if !ok || ca.Comparison == nil {
		t.Fatalf("no comparison for Canada: %+v", rows)
	}

	closeTo(t, "CA yesterday", ca.Comparison.Metrics[0], 0)

	if ca.Comparison.Change[0] != nil {
		t.Errorf("change from nothing = %v, want null", *ca.Comparison.Change[0])
	}
}

// TestEventMetricBrokenDownByEntryPage checks the dimension that only exists on
// the sessions table. It is the one place an event query joins a fact table,
// and getting the join wrong would multiply every count.
func TestEventMetricBrokenDownByEntryPage(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews")
	q.Dimensions = []string{"visit:entry_page"}

	result := run(t, engine, q)

	got := map[string]float64{}
	for _, row := range result.Results {
		got[row.Dimensions[0]] = row.Metrics[0]
	}

	// Three visits entered on /home and saw six pages between them; one entered
	// on /pricing and saw one.
	closeTo(t, "/home entries", got["/home"], 6)
	closeTo(t, "/pricing entries", got["/pricing"], 1)
}

// TestSamplingScalesCountsAndNotRates checks that a sampled total is scaled
// back up and a sampled rate is left alone, and that the answer says it was
// sampled.
func TestSamplingScalesCountsAndNotRates(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("pageviews", "bounce_rate")
	q.SampleRate = 0.5

	result := run(t, engine, q)

	// Three pageviews landed in the selected event buckets and one bouncing
	// visit landed in the independently selected session buckets. The additive
	// count expands by two; the rate remains the directly observed percentage.
	closeTo(t, "sampled pageviews", result.Results[0].Metrics[0], 6)
	closeTo(t, "sampled bounce_rate", result.Results[0].Metrics[1], 100)

	if result.Meta.SampleRate != 0.5 {
		t.Errorf("meta.sample_rate = %v, want 0.5", result.Meta.SampleRate)
	}

	if warning, ok := result.Meta.MetricWarnings["pageviews"]; !ok || warning.Code != WarnSampled {
		t.Errorf("a sampled total must announce itself, got %+v", warning)
	}
}

// TestPartialCoverageIsReported checks the warning for a metric measured over
// less data than the reader will assume. Only two of the four visits reported
// engagement, so scroll depth covers half of them.
func TestPartialCoverageIsReported(t *testing.T) {
	engine := newEngine(t)

	result := run(t, engine, baseQuery("scroll_depth"))

	warning, ok := result.Meta.MetricWarnings["scroll_depth"]
	if !ok {
		t.Fatal("a metric measured over part of the data must say so")
	}

	if warning.Code != WarnPartialBucket {
		t.Errorf("warning code = %q, want %q", warning.Code, WarnPartialBucket)
	}
}

// TestATruncatedGroupSetSaysSo checks the ceiling on how many groups one query
// may pull into memory. Silently returning the first N groups of a breakdown is
// a wrong answer that looks like a complete one.
//
// Ordering by the page name is what takes the in-memory path: an interned
// dimension sorts by its label, and the label only exists once the ids have
// been resolved, so the database cannot do the ordering.
func TestATruncatedGroupSetSaysSo(t *testing.T) {
	engine := newEngine(t)
	engine.MaxGroups = 1

	q := baseQuery("pageviews", "bounce_rate")
	q.Dimensions = []string{"event:page"}
	q.OrderBy = []Order{{Key: "event:page"}}

	result := run(t, engine, q)

	warning, ok := result.Meta.MetricWarnings["pageviews"]
	if !ok || warning.Code != WarnGroupsTruncated {
		t.Fatalf("a capped group set must announce itself, got %+v", result.Meta.MetricWarnings)
	}

	if len(result.Results) == 0 {
		t.Fatal("a truncated answer is still an answer")
	}
}
