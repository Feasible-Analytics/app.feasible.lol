//
// harness_test.go
// A real control database, a real account database and a real key, per test.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/webhooks"
)

// The harness builds the real thing rather than a set of fakes: a migrated
// control database, a migrated account database with events in it, and a key
// created through the same code the CLI uses. The whole promise of this package
// is "no 500s and the shims agree with v2", and neither of those can be proved
// against mocks — a mock cannot return the driver error that would have been a
// 500, and two fakes always agree.

// testNow is the clock every test resolves its dates against.
var testNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// teamID and otherTeamID are the two tenants. The second one exists so that
// every authorisation check has something to fail against: a test suite with one
// tenant proves nothing about isolation.
const (
	teamID      = 7
	otherTeamID = 8
	siteID      = 3
	otherSiteID = 4
)

// harness is one test's whole world.
type harness struct {
	API     *API
	Server  *httptest.Server
	Control *sql.DB
	Key     string
	Other   string
}

// newHarness builds everything and tears it down after the test.
func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(ctx, control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	seedControl(t, control)

	manager := accounts.NewManager(dir)
	t.Cleanup(func() { manager.CloseAll() })

	account, err := manager.Open(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}

	seedEvents(t, account)

	cache := sites.New(control)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	keys := apikeys.NewStore(control)

	_, plaintext, err := keys.Create(ctx, teamID, 1, "test", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, otherPlaintext, err := keys.Create(ctx, otherTeamID, 2, "other", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	hooks := webhooks.NewStore(control)

	// A real gate with nothing in it. Every test starts against an account that
	// is paying, and the ones that care lock it with Set rather than by walking
	// a ninety-day clock.
	api := &API{
		Keys:       keys,
		Limiter:    apikeys.NewLimiter(0),
		Access:     access.New(nil, nil, nil, nil),
		Sites:      cache,
		Control:    NewControlStore(control),
		Accounts:   manager,
		Webhooks:   hooks,
		Dispatcher: webhooks.NewDispatcher(hooks),
		BaseURL:    "https://example.test",
		Now:        func() time.Time { return testNow },
	}

	server := httptest.NewServer(api.Routes())
	t.Cleanup(server.Close)

	return &harness{API: api, Server: server, Control: control, Key: plaintext, Other: otherPlaintext}
}

// seedControl writes the teams, users and sites the tests act on.
func seedControl(t *testing.T, control *sql.DB) {
	t.Helper()

	now := testNow.Unix()

	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Test', ?, ?)`, []any{teamID, now, now}},
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Other', ?, ?)`, []any{otherTeamID, now, now}},
		{`INSERT INTO users (id, email, name, created_at, updated_at) VALUES (1, 'owner@example.test', 'Owner', ?, ?)`, []any{now, now}},
		{`INSERT INTO users (id, email, name, created_at, updated_at) VALUES (2, 'other@example.test', 'Other', ?, ?)`, []any{now, now}},
		{`INSERT INTO users (id, email, name, created_at, updated_at) VALUES (3, 'guest@example.test', 'Guest', ?, ?)`, []any{now, now}},
		{`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, 1, 'owner', ?)`, []any{teamID, now}},
		{`INSERT INTO sites (id, account_id, domain, display_name, timezone, created_at, updated_at)
		  VALUES (?, ?, 'example.com', 'Example', 'UTC', ?, ?)`, []any{siteID, teamID, now, now}},
		{`INSERT INTO sites (id, account_id, domain, display_name, timezone, created_at, updated_at)
		  VALUES (?, ?, 'notyours.com', 'Not Yours', 'UTC', ?, ?)`, []any{otherSiteID, otherTeamID, now, now}},
	}

	for _, statement := range statements {
		if _, err := control.Exec(statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
}

// visit describes one seeded session, so the fixture reads as a table of
// traffic rather than as a wall of INSERT statements.
type visit struct {
	// day is how many days before the test's "today" the visit happened.
	day int

	source string
	page   string

	// pageviews is how many events the session recorded. Every seeded session
	// has more than one so that nothing is a bounce, which keeps the bounce
	// rate a stable zero across the suite.
	pageviews int
}

// fixture is the traffic every test reads.
//
// It is split deliberately across two whole weeks, with one source that stops
// between them. The stop is what makes the explain-a-change behaviour testable
// at all: a source that goes to zero has no rows in the later period, so a test
// fixture where every source appears in both would pass against an
// implementation that never looks at the earlier period.
var fixture = []visit{
	// The last seven days: 24 to 30 August.
	{day: 2, source: "Google", page: "/home", pageviews: 2},
	{day: 2, source: "Google", page: "/home", pageviews: 2},
	{day: 2, source: "Google", page: "/home", pageviews: 2},
	{day: 1, source: "Twitter", page: "/pricing", pageviews: 2},
	{day: 1, source: "Twitter", page: "/pricing", pageviews: 2},

	// The seven days before that: 17 to 23 August.
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/home", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/home", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/home", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/home", pageviews: 2},
}

// The totals the fixture implies, written out so a test asserts against a
// number somebody worked out rather than against whatever the code returned.
const (
	currentVisitors  = 5
	currentPageviews = 10
	previousVisitors = 9
	allVisitors      = 14
)

// seedEvents writes the fixture into an account database.
func seedEvents(t *testing.T, account *accounts.Account) {
	t.Helper()

	ctx := context.Background()

	pageview, err := account.Intern.ID(ctx, intern.EventName, ingest.EventPageview)
	if err != nil {
		t.Fatal(err)
	}

	for index, entry := range fixture {
		// One visitor per session. Sharing ids between sessions would make the
		// visitor counts depend on the order rows were written, which is a
		// fixture nobody can reason about.
		user := int64(1000 + index)
		session := int64(index + 1)

		at := testNow.AddDate(0, 0, -entry.day).Truncate(time.Hour).Unix()

		page, err := account.Intern.ID(ctx, intern.Pathname, entry.page)
		if err != nil {
			t.Fatal(err)
		}

		source, err := account.Intern.ID(ctx, intern.Source, entry.source)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
			                      pageviews, events, entry_page_id, exit_page_id, source_id)
			VALUES (?, ?, ?, ?, ?, 60, 0, ?, ?, ?, ?, ?)`,
			session, siteID, user, at, at+60, entry.pageviews, entry.pageviews, page, page, source); err != nil {
			t.Fatal(err)
		}

		for i := 0; i < entry.pageviews; i++ {
			if _, err := account.Writer().ExecContext(ctx, `
				INSERT INTO events (site_id, timestamp, name_id, user_id, session_id, pathname_id, source_id, scroll_depth)
				VALUES (?, ?, ?, ?, ?, ?, ?, 255)`,
				siteID, at+int64(i), pageview, user, session, page, source); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// get sends an authenticated GET and returns the status and the raw body.
func (h *harness) get(t *testing.T, path string) (int, []byte) {
	t.Helper()

	return h.do(t, http.MethodGet, path, "", h.Key)
}

// post sends an authenticated POST.
func (h *harness) post(t *testing.T, path, body string) (int, []byte) {
	t.Helper()

	return h.do(t, http.MethodPost, path, body, h.Key)
}

// do sends one request with an explicit credential.
func (h *harness) do(t *testing.T, method, path, body, key string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	request, err := http.NewRequest(method, h.Server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}

	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	return response.StatusCode, answer
}

// decode reads a JSON body into a map, failing the test if it is not JSON.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()

	decoded := map[string]any{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("response was not a JSON object: %v (%s)", err, string(body))
	}

	return decoded
}
