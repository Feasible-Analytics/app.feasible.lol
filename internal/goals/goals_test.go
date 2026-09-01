//
// goals_test.go
// The definitions, and the fixture every other test in this package counts.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// fixtureNow is the instant every test resolves its range against. It is fixed
// because "today" is the hardest thing to test in an analytics product, and a
// suite whose answers change at midnight is a suite nobody trusts.
var fixtureNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// The fixture's visitors. A visitor is a fingerprint, so it is an integer.
const (
	visitorA int64 = 1001
	visitorB int64 = 1002
	visitorC int64 = 1003
	visitorD int64 = 1004
)

// siteID is the one site every test writes to.
const siteID int64 = 1

// at builds a timestamp inside the fixture's window.
func at(day, hour, minute int) int64 {
	return time.Date(2026, 8, day, hour, minute, 0, 0, time.UTC).Unix()
}

// fixtureRange is the window the tests measure: the two days the fixture has
// events on, written as bare dates so the second day is covered to its end.
func fixtureRange() query.DateRange {
	return query.DateRange{
		Preset:   query.RangeCustom,
		Start:    time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		DateOnly: true,
	}
}

// sessionRow is one visit, written directly rather than folded by the ingest
// pipeline: these tests are about what the reports count, and a fixture that
// went through the pipeline would fail for two reasons at once.
type sessionRow struct {
	id        int64
	user      int64
	startedAt int64
	pageviews int
	entryPage string
	exitPage  string
	source    string
}

// eventRow is one hit.
type eventRow struct {
	id        int64
	session   int64
	user      int64
	timestamp int64
	name      string
	page      string
	source    string
	props     map[string]string
	revenue   int64
	currency  string
}

// fixtureSessions is the visit-grain half of the fixture. Four visitors across
// six visits, and every number asserted anywhere in this package is worked out
// by hand from these rows and the events beneath them.
var fixtureSessions = []sessionRow{
	{id: 1, user: visitorA, startedAt: at(29, 10, 0), pageviews: 3, entryPage: "/home", exitPage: "/signup", source: "Google"},
	{id: 2, user: visitorB, startedAt: at(29, 11, 0), pageviews: 2, entryPage: "/home", exitPage: "/pricing", source: "Google"},
	{id: 3, user: visitorC, startedAt: at(30, 9, 0), pageviews: 4, entryPage: "/cart", exitPage: "/order/complete", source: "Twitter"},
	{id: 4, user: visitorD, startedAt: at(30, 10, 0), pageviews: 2, entryPage: "/cart", exitPage: "/checkout"},
	{id: 5, user: visitorA, startedAt: at(30, 11, 0), pageviews: 2, entryPage: "/cart", exitPage: "/order/complete"},
	{id: 6, user: visitorB, startedAt: at(30, 12, 0), pageviews: 4, entryPage: "/checkout", exitPage: "/order/complete"},
}

// fixtureEvents is the hit-grain half.
//
// Visit 1 fires the same goal twice, which is the case that decides whether
// unique and total conversions differ. Visit 6 walks the checkout out of
// order, which is the case that separates a strict funnel from a loose one.
var fixtureEvents = []eventRow{
	// Visit 1 — a signup, twice, on two different plans.
	{id: 1, session: 1, user: visitorA, timestamp: at(29, 10, 0), name: ingest.EventPageview, page: "/home", source: "Google"},
	{id: 2, session: 1, user: visitorA, timestamp: at(29, 10, 1), name: ingest.EventPageview, page: "/pricing", source: "Google"},
	{id: 3, session: 1, user: visitorA, timestamp: at(29, 10, 2), name: ingest.EventPageview, page: "/signup", source: "Google"},
	{id: 4, session: 1, user: visitorA, timestamp: at(29, 10, 3), name: "Signup", page: "/signup", source: "Google", props: map[string]string{"plan": "growth"}},
	{id: 5, session: 1, user: visitorA, timestamp: at(29, 10, 4), name: "Signup", page: "/signup", source: "Google", props: map[string]string{"plan": "starter"}},

	// Visit 2 — the one 404 in the fixture, which is what makes the automatic
	// goal appear at all.
	{id: 6, session: 2, user: visitorB, timestamp: at(29, 11, 0), name: ingest.EventPageview, page: "/home", source: "Google"},
	{id: 7, session: 2, user: visitorB, timestamp: at(29, 11, 1), name: ingest.EventPageview, page: "/pricing", source: "Google"},
	{id: 8, session: 2, user: visitorB, timestamp: at(29, 11, 2), name: EventNotFound, page: "/missing", source: "Google"},

	// Visit 3 — the whole checkout, in order, ending in money.
	{id: 9, session: 3, user: visitorC, timestamp: at(30, 9, 0), name: ingest.EventPageview, page: "/cart", source: "Twitter"},
	{id: 10, session: 3, user: visitorC, timestamp: at(30, 9, 1), name: ingest.EventPageview, page: "/checkout", source: "Twitter"},
	{id: 11, session: 3, user: visitorC, timestamp: at(30, 9, 2), name: ingest.EventPageview, page: "/checkout/payment", source: "Twitter"},
	{id: 12, session: 3, user: visitorC, timestamp: at(30, 9, 3), name: ingest.EventPageview, page: "/order/complete", source: "Twitter"},
	{id: 13, session: 3, user: visitorC, timestamp: at(30, 9, 4), name: "Purchase", page: "/order/complete", source: "Twitter", revenue: 5000, currency: "USD"},

	// Visit 4 — abandoned at the checkout page.
	{id: 14, session: 4, user: visitorD, timestamp: at(30, 10, 0), name: ingest.EventPageview, page: "/cart"},
	{id: 15, session: 4, user: visitorD, timestamp: at(30, 10, 1), name: ingest.EventPageview, page: "/checkout"},

	// Visit 5 — the cart and then, somehow, the confirmation page.
	{id: 16, session: 5, user: visitorA, timestamp: at(30, 11, 0), name: ingest.EventPageview, page: "/cart"},
	{id: 17, session: 5, user: visitorA, timestamp: at(30, 11, 1), name: ingest.EventPageview, page: "/order/complete"},

	// Visit 6 — every step of the funnel, in the wrong order.
	{id: 18, session: 6, user: visitorB, timestamp: at(30, 12, 0), name: ingest.EventPageview, page: "/checkout"},
	{id: 19, session: 6, user: visitorB, timestamp: at(30, 12, 1), name: ingest.EventPageview, page: "/cart"},
	{id: 20, session: 6, user: visitorB, timestamp: at(30, 12, 2), name: ingest.EventPageview, page: "/checkout/payment"},
	{id: 21, session: 6, user: visitorB, timestamp: at(30, 12, 3), name: ingest.EventPageview, page: "/order/complete"},
}

// newFixture builds a migrated account database with the fixture in it, and an
// engine over its read-only handle — the same separation the serving path uses.
func newFixture(t *testing.T) (*sql.DB, *query.Engine) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	ctx := context.Background()

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

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
				pageviews, events, entry_page_id, exit_page_id, source_id)
			VALUES (?,?,?,?,?,0,0,?,?,?,?,?)`,
			session.id, siteID, session.user, session.startedAt, session.startedAt,
			session.pageviews, session.pageviews,
			id(intern.Pathname, session.entryPage), id(intern.Pathname, session.exitPage),
			id(intern.Source, session.source),
		); err != nil {
			t.Fatal(err)
		}
	}

	for _, event := range fixtureEvents {
		writeEvent(t, account, event)
	}

	engine := query.New(account.Reader())
	engine.Now = func() time.Time { return fixtureNow }

	return account.Writer(), engine
}

// writeEvent inserts one event and, when it carries anything cold, its
// event_details partner. The has_details flag and the detail row are written
// together so the flag can never claim a row that is not there.
func writeEvent(t *testing.T, account *accounts.Account, event eventRow) {
	t.Helper()

	ctx := context.Background()

	id := func(dimension intern.Dimension, value string) int64 {
		got, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	details := len(event.props) > 0 || event.revenue != 0

	flag := 0
	if details {
		flag = 1
	}

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id,
			pathname_id, source_id, scroll_depth, has_details)
		VALUES (?,?,?,?,?,?,?,?,255,?)`,
		event.id, siteID, event.timestamp, id(intern.EventName, event.name), event.user, event.session,
		id(intern.Pathname, event.page), id(intern.Source, event.source), flag,
	); err != nil {
		t.Fatal(err)
	}

	if !details {
		return
	}

	var (
		props    any
		amount   any
		currency any
	)

	if len(event.props) > 0 {
		encoded, err := json.Marshal(event.props)
		if err != nil {
			t.Fatal(err)
		}
		props = string(encoded)
	}

	if event.revenue != 0 {
		amount, currency = event.revenue, event.currency
	}

	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO event_details (event_id, props, revenue_amount, revenue_currency) VALUES (?,?,?,?)",
		event.id, props, amount, currency,
	); err != nil {
		t.Fatal(err)
	}
}

// goalCreated is when the tests' goals are made: before the fixture's first
// event. Goals do not backfill, so a goal created at the fixture's "now" would
// count none of the fixture's traffic — which is the behaviour under test in
// one place and pure noise in every other.
var goalCreated = time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)

// mustCreate creates a goal or fails the test, so every setup line below reads
// as one line of intent rather than three of error handling.
func mustCreate(t *testing.T, db *sql.DB, goal Goal) Goal {
	t.Helper()

	created, err := Create(context.Background(), db, goal, goalCreated)
	if err != nil {
		t.Fatalf("create goal %+v: %v", goal, err)
	}

	return created
}

// TestGoalPathsAreTrimmed pins the behaviour behind a bug report that read as
// "wildcards interfere with each other" and was actually a stray space. A
// leading or trailing space is invisible in a text box and silently prevents
// every match, so it is removed before the definition is stored.
func TestGoalPathsAreTrimmed(t *testing.T) {
	db, _ := newFixture(t)

	goal := mustCreate(t, db, Goal{
		SiteID:      siteID,
		Kind:        KindPage,
		PagePattern: "  /order/complete  ",
		DisplayName: "  Completed orders  ",
	})

	if goal.PagePattern != "/order/complete" {
		t.Errorf("page pattern = %q, want it trimmed to %q", goal.PagePattern, "/order/complete")
	}

	if goal.DisplayName != "Completed orders" {
		t.Errorf("display name = %q, want it trimmed", goal.DisplayName)
	}

	// The trimmed pattern has to actually match, which is the half of the bug
	// that costs somebody an afternoon.
	if !Matches(goal.PagePattern, "/order/complete") {
		t.Error("a trimmed pattern must match the path it names")
	}
}

// TestEventNamesAreTrimmed checks the same thing for the other kind of goal.
func TestEventNamesAreTrimmed(t *testing.T) {
	db, _ := newFixture(t)

	goal := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: " Signup "})

	if goal.EventName != "Signup" {
		t.Errorf("event name = %q, want %q", goal.EventName, "Signup")
	}
}

// TestCreatingTheSameGoalTwiceIsOneGoal checks that a duplicate definition does
// not become a second row. Two identical goals would count every conversion
// twice on a report where nobody could see why.
func TestCreatingTheSameGoalTwiceIsOneGoal(t *testing.T) {
	db, _ := newFixture(t)

	first := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup"})
	second := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup"})

	if first.ID != second.ID {
		t.Errorf("creating the same goal twice made two goals: %d and %d", first.ID, second.ID)
	}

	list, err := List(context.Background(), db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 1 {
		t.Errorf("site has %d goals, want 1", len(list))
	}
}

// TestRecreatingAGoalKeepsItsCreationTime is the other half of not backfilling.
// If re-running creation moved the instant conversions start counting from, a
// report would change under a customer who did nothing.
func TestRecreatingAGoalKeepsItsCreationTime(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	first, err := Create(ctx, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup"}, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	later, err := Create(ctx, db, Goal{SiteID: siteID, Kind: KindEvent, EventName: "Signup"}, fixtureNow.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if later.CreatedAt != first.CreatedAt {
		t.Errorf("creation time moved from %d to %d", first.CreatedAt, later.CreatedAt)
	}
}

// TestAutomaticGoalsExistOnEveryNewSite checks the four goals a site gets for
// free. The 404 one is the point: the commonest reason 404 tracking silently
// fails is pasting the snippet and never making the goal.
func TestAutomaticGoalsExistOnEveryNewSite(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	created, err := EnsureAutomatic(ctx, db, siteID, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	if len(created) != 4 {
		t.Fatalf("a new site got %d automatic goals, want 4", len(created))
	}

	want := map[string]bool{
		EventNotFound:       false,
		EventOutboundClick:  false,
		EventFileDownload:   false,
		EventFormSubmission: false,
	}

	for _, goal := range created {
		if !goal.IsAutomatic {
			t.Errorf("goal %q is not marked automatic", goal.EventName)
		}

		want[goal.EventName] = true
	}

	for name, found := range want {
		if !found {
			t.Errorf("no automatic goal for %q", name)
		}
	}

	// Running it again is what a site refresh does, and it must not duplicate
	// anything or move any creation time.
	if _, err := EnsureAutomatic(ctx, db, siteID, fixtureNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	list, err := List(ctx, db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 4 {
		t.Errorf("after two runs the site has %d goals, want 4", len(list))
	}
}

// TestAGoalNeedsSomethingToMatch checks the refusals. Every one of these would
// otherwise be a goal that silently counts nothing.
func TestAGoalNeedsSomethingToMatch(t *testing.T) {
	db, _ := newFixture(t)

	cases := []struct {
		name string
		goal Goal
	}{
		{"page goal with no path", Goal{SiteID: siteID, Kind: KindPage}},
		{"page goal with a relative path", Goal{SiteID: siteID, Kind: KindPage, PagePattern: "order/complete"}},
		{"event goal with no name", Goal{SiteID: siteID, Kind: KindEvent}},
		{"a goal that is neither", Goal{SiteID: siteID, Kind: "whatever", EventName: "Signup"}},
		{"revenue with no currency", Goal{SiteID: siteID, Kind: KindEvent, EventName: "Purchase", IsRevenue: true}},
		{"revenue with a bad currency", Goal{SiteID: siteID, Kind: KindEvent, EventName: "Purchase", IsRevenue: true, Currency: "dollars"}},
		{"a goal with no site", Goal{Kind: KindEvent, EventName: "Signup"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Create(context.Background(), db, tc.goal, fixtureNow); err == nil {
				t.Fatal("want a refusal")
			}
		})
	}
}

// TestAGoalCarriesAtMostThreeConstraints pins the limit. It is a product
// decision rather than a storage one, so it is enforced here or not at all.
func TestAGoalCarriesAtMostThreeConstraints(t *testing.T) {
	db, _ := newFixture(t)

	goal := Goal{
		SiteID:    siteID,
		Kind:      KindEvent,
		EventName: "Purchase",
		Properties: []PropertyConstraint{
			{Name: "plan", Value: "growth"},
			{Name: "seats", Value: "5"},
			{Name: "region", Value: "eu"},
			{Name: "channel", Value: "web"},
		},
	}

	if _, err := Create(context.Background(), db, goal, fixtureNow); err == nil {
		t.Fatalf("a goal with %d constraints must be refused", len(goal.Properties))
	}

	goal.Properties = goal.Properties[:MaxProperties]

	created := mustCreate(t, db, goal)
	if len(created.Properties) != MaxProperties {
		t.Errorf("goal kept %d constraints, want %d", len(created.Properties), MaxProperties)
	}
}

// TestDeletingAGoalUsedByAFunnelIsRefused checks that a chart cannot lose a
// step underneath it.
func TestDeletingAGoalUsedByAFunnelIsRefused(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	first := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/cart"})
	second := mustCreate(t, db, Goal{SiteID: siteID, Kind: KindPage, PagePattern: "/checkout"})

	if _, err := CreateFunnel(ctx, db, Funnel{
		SiteID: siteID, Name: "Checkout",
		Steps: []Step{{GoalID: first.ID}, {GoalID: second.ID}},
	}, fixtureNow); err != nil {
		t.Fatal(err)
	}

	if err := Delete(ctx, db, first.ID); err == nil {
		t.Fatal("deleting a goal a funnel depends on must be refused")
	}

	if err := Delete(ctx, db, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a goal that does not exist = %v, want ErrNotFound", err)
	}
}

// TestAGoalDescribesItself checks the label a report shows for a goal nobody
// named, because an empty cell in a table is worse than a generated sentence.
func TestAGoalDescribesItself(t *testing.T) {
	page := Goal{Kind: KindPage, PagePattern: "/blog/**"}
	if got := page.Label(); got != "Visit /blog/**" {
		t.Errorf("page goal label = %q", got)
	}

	event := Goal{Kind: KindEvent, EventName: "Signup"}
	if got := event.Label(); got != "Signup" {
		t.Errorf("event goal label = %q", got)
	}

	named := Goal{Kind: KindEvent, EventName: "Signup", DisplayName: "Trial started"}
	if got := named.Label(); got != "Trial started" {
		t.Errorf("named goal label = %q", got)
	}
}
