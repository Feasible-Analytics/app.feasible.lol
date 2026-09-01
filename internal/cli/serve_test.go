//
// serve_test.go
// Tests for the `serve` subcommand, including what a locked account may reach.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
)

// TestServeReportsResolvedConfig checks that serve resolves and reports the
// values the rest of the system is built on. Until the HTTP server exists this
// is the only place a wrong listen address or base URL becomes visible.
func TestServeReportsResolvedConfig(t *testing.T) {
	t.Setenv("FEASIBLE_APP_BASE_URL", "http://rager.example.ts.net:19300")
	t.Setenv("FEASIBLE_APP_TRANSPORT", "http")

	code, stdout, stderr := run(t, "serve", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	for _, want := range []string{
		"base_url=http://rager.example.ts.net:19300",
		"transport=http",
		"internal_listen=127.0.0.1:19401",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("serve did not report %q; got %q", want, stdout)
		}
	}
}

// TestServeListenFlagOverrides covers the flag overriding the environment, which
// is what makes a second instance on another port a one-word change.
func TestServeListenFlagOverrides(t *testing.T) {
	t.Setenv("FEASIBLE_APP_LISTEN", "127.0.0.1:19301")

	code, stdout, _ := run(t, "serve", "-check", "-listen", "127.0.0.1:29301")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "listen=127.0.0.1:29301") {
		t.Fatalf("flag did not override the environment: %q", stdout)
	}
}

// TestServeTraceEventsFlag is the plumbing check for --trace-events: the root
// flag has to reach the configuration the subcommand runs with, or turning it on
// would appear to do nothing.
func TestServeTraceEventsFlag(t *testing.T) {
	code, stdout, _ := run(t, "--trace-events", "serve", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "trace_events=true") {
		t.Fatalf("--trace-events did not reach the config: %q", stdout)
	}
}

// TestServeConfigErrorExitsOne separates a bad configuration from a bad command
// line. Both used to be exit 2 in a hundred other tools, and it makes a boot
// loop impossible to diagnose from a supervisor log.
func TestServeConfigErrorExitsOne(t *testing.T) {
	t.Setenv("FEASIBLE_APP_TRANSPORT", "carrier-pigeon")

	code, _, stderr := run(t, "serve", "-check")

	if code != ExitError {
		t.Fatalf("exit code %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "configuration error") {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}

// The lock is a property of the assembled process, not of any one package. Every
// package below can be right on its own and the product still ship with a hole,
// because what is actually gated is decided by which handler serveRoutes wraps —
// so the tests below drive the real route table, built the way serve builds it,
// and ask each path what it answers.

// lockedTeam is the account these tests put on the clock.
const lockedTeam = 1

// stack is one assembled process, with an account on the lifecycle clock and a
// working API key for it.
type stack struct {
	routes       http.Handler
	gate         *access.Gate
	key          string
	dataDir      string
	sessionToken string
	csrfToken    string
	csrfCookie   string
}

// newStack assembles everything serve assembles, over a temporary data
// directory, and hands back the same mux the listener would have been given.
func newStack(t *testing.T) *stack {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	cfg.App.DataDir = dir
	cfg.App.BaseURL = "http://feasible.test"
	cfg.App.Transport = config.TransportDirect
	cfg.App.MailTransport = config.MailTransportLog

	e := &env{
		cfg:    cfg,
		log:    logger.New(logger.Options{Level: "error", Output: os.Stderr}),
		stdout: io.Discard,
		stderr: io.Discard,
	}

	seedLapsedAccount(t, dir)

	service, control, manager, err := buildIngest(ctx, e, dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})
	t.Cleanup(func() { control.Close() })

	secret, err := tracker.LoadSecret(dir)
	if err != nil {
		t.Fatal(err)
	}

	mailer, err := buildMailer(e)
	if err != nil {
		t.Fatal(err)
	}

	com := buildCommerce(e, control, manager, service.Sites, mailer)

	app, err := buildApp(e, control, manager, service, secret, mailer, com.Gate, com.Purger)
	if err != nil {
		t.Fatal(err)
	}

	sessionToken, _, err := auth.NewStore(control).CreateSession(ctx, 1, "commerce integration test")
	if err != nil {
		t.Fatal(err)
	}

	csrfResponse := httptest.NewRecorder()
	csrfToken := app.FormToken(csrfResponse, httptest.NewRequest(http.MethodGet, "/pricing", nil))
	csrfCookie := ""
	for _, cookie := range csrfResponse.Result().Cookies() {
		if cookie.Name == "feasible_csrf" {
			csrfCookie = cookie.Name + "=" + cookie.Value
		}
	}
	if csrfToken == "" || csrfCookie == "" {
		t.Fatal("the commerce fixture could not mint a CSRF token")
	}

	public := buildPublic(e, control, service.Sites, manager, com.Gate)

	// The site configuration screens are mounted the way serve mounts them,
	// because what they are wrapped in is the thing under test here.
	site, err := buildSiteRules(ctx, e, service, manager)
	if err != nil {
		t.Fatal(err)
	}

	data := buildData(e, control, manager, service, site)

	_, key, err := public.Keys.Create(ctx, lockedTeam, 1, "test", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// The team, sharing, report and health screens are part of the assembled
	// route table, so they are built here too. A stack that left them out would
	// let this file's enumeration pass while the process it describes served
	// paths nobody had checked.
	extra := buildServices(e, control, manager, service, mailer)

	return &stack{
		routes:       serveRoutes(e, service, manager, secret, dir, app, public, com, data.settings, extra),
		gate:         com.Gate,
		key:          key,
		dataDir:      dir,
		sessionToken: sessionToken,
		csrfToken:    csrfToken,
		csrfCookie:   csrfCookie,
	}
}

// seedLapsedAccount writes one team, one owner and one site, and starts the
// lifecycle clock thirty-one days ago so the account is genuinely in the Locked
// phase rather than merely marked as being in it.
func seedLapsedAccount(t *testing.T, dir string) {
	t.Helper()

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	applyControlMigrations(t, control)

	now := time.Now().UTC().Unix()

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, name, email_verified_at, created_at, updated_at) VALUES (1, 'owner@example.com', 'Owner', ?, ?, ?)`, []any{now, now, now}},
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Example Co', ?, ?)`, []any{lockedTeam, now, now}},
		{`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, 1, 'owner', ?)`, []any{lockedTeam, now}},
		// Published, so the fixture covers the public dashboard as well as the
		// signed-in one. They are two doors onto the same numbers and the lock
		// has to mean the same thing at both.
		{`INSERT INTO sites (id, account_id, domain, timezone, is_public, created_at, updated_at) VALUES (1, ?, 'example.com', 'UTC', 1, ?, ?)`, []any{lockedTeam, now, now}},
	} {
		if _, err := control.Exec(statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}

	lapsed := lifecycle.State{
		Trigger:   lifecycle.TriggerLapse,
		StartedAt: time.Now().UTC().Add(-(lifecycle.GraceDays + 1) * lifecycle.Day),
	}

	if err := lifecycle.NewStore(control).Save(context.Background(), lockedTeam, lapsed); err != nil {
		t.Fatal(err)
	}
}

// clock moves the account's day 0 and rebuilds the locked set, which is what the
// gate's own ticker does every fifteen seconds.
func (s *stack) clock(t *testing.T, state lifecycle.State) {
	t.Helper()

	control, err := store.Open(filepath.Join(s.dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	if err := lifecycle.NewStore(control).Save(context.Background(), lockedTeam, state); err != nil {
		t.Fatal(err)
	}

	if err := s.gate.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// lock rebuilds the locked set and refuses to continue if the fixture is not
// actually locked, because a test that silently stopped locking would keep
// passing while proving nothing.
func (s *stack) lock(t *testing.T) {
	t.Helper()

	if err := s.gate.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, locked := s.gate.Locked(lockedTeam); !locked {
		t.Fatal("the fixture account is not locked, so this test proves nothing")
	}
}

// pay stops the clock and rebuilds, which is what a successful payment does.
func (s *stack) pay(t *testing.T) {
	t.Helper()

	s.clock(t, lifecycle.State{})
}

// send drives one request through the assembled route table.
func (s *stack) send(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, path, reader)
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	s.routes.ServeHTTP(recorder, request)

	return recorder
}

// bearer is the header an API caller sends.
func (s *stack) bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + s.key, "Content-Type": "application/json"}
}

// signedInForm returns the cookies and content type an authenticated browser
// sends to a server-rendered form. The caller still controls the submitted CSRF
// value so missing and mismatched cases exercise the real validator.
func (s *stack) signedInForm() map[string]string {
	return map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Cookie":       auth.SessionCookieName + "=" + s.sessionToken + "; " + s.csrfCookie,
	}
}

// TestCommerceRoutesUseAuthAccountAndCSRF drives the assembled mux that ships.
// It covers the original routing regression: public callers cannot select an
// account, verified owners resolve their own team, and billing POSTs use auth's
// signed CSRF cookie rather than trusting a hidden account id.
func TestCommerceRoutesUseAuthAccountAndCSRF(t *testing.T) {
	s := newStack(t)

	pricing := s.send(t, http.MethodGet, "/pricing", "", nil)
	if pricing.Code != http.StatusOK {
		t.Fatalf("public pricing answered %d", pricing.Code)
	}
	for _, want := range []string{
		"/register?next=%2Fpricing%3Fplan%3Dmonthly",
		"/login?next=%2Fpricing%3Fplan%3Dyearly",
	} {
		if !strings.Contains(pricing.Body.String(), want) {
			t.Errorf("signed-out pricing is missing %q", want)
		}
	}

	signedOut := s.send(t, http.MethodGet, "/billing?team=999", "", nil)
	if signedOut.Code != http.StatusFound || !strings.HasPrefix(signedOut.Header().Get("Location"), "/login?next=") {
		t.Fatalf("signed-out billing answered %d and redirected to %q", signedOut.Code, signedOut.Header().Get("Location"))
	}

	headers := s.signedInForm()
	forged := s.send(t, http.MethodGet, "/billing?team=999", "", headers)
	if forged.Code != http.StatusNotFound {
		t.Fatalf("forged billing account answered %d, want 404", forged.Code)
	}

	signedIn := s.send(t, http.MethodGet, "/billing", "", headers)
	if signedIn.Code != http.StatusOK {
		t.Fatalf("signed-in billing answered %d: %s", signedIn.Code, signedIn.Body.String())
	}
	if !strings.Contains(signedIn.Body.String(), "Example Co") || strings.Contains(signedIn.Body.String(), "Account 999") {
		t.Errorf("billing did not use the authenticated account: %s", signedIn.Body.String())
	}

	for name, token := range map[string]string{"missing": "", "mismatched": "not-the-token"} {
		t.Run(name+" csrf", func(t *testing.T) {
			body := "plan=monthly&team=1"
			if token != "" {
				body += "&csrf_token=" + token
			}

			response := s.send(t, http.MethodPost, "/billing/checkout", body, headers)
			if response.Code != http.StatusForbidden {
				t.Errorf("%s CSRF answered %d, want 403: %s", name, response.Code, response.Body.String())
			}
		})
	}

	valid := s.send(t, http.MethodPost, "/billing/checkout",
		"plan=monthly&team=1&csrf_token="+s.csrfToken, headers)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), "cannot take payments") {
		t.Fatalf("valid authenticated checkout answered %d: %s", valid.Code, valid.Body.String())
	}
}

// TestALockedAccountIsRefusedEveryReadPath walks every door this account's own
// numbers can come out of. One case per path, because the bug being fixed here
// was a gate that covered two of them and none of the rest.
func TestALockedAccountIsRefusedEveryReadPath(t *testing.T) {
	s := newStack(t)
	s.lock(t)

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
	}{
		{"the dashboard", http.MethodGet, "/dashboard/example.com", "", nil},
		{"the stats endpoint behind it", http.MethodPost, "/api/stats/example.com/query", `{"metrics":["visitors"],"date_range":"7d"}`, nil},
		{"the v2 query endpoint", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`, s.bearer()},
		{"the v1 aggregate shim", http.MethodGet, "/api/v1/stats/aggregate?site_id=example.com&metrics=visitors", "", s.bearer()},
		{"the v1 timeseries shim", http.MethodGet, "/api/v1/stats/timeseries?site_id=example.com&metrics=visitors", "", s.bearer()},
		{"the v1 breakdown shim", http.MethodGet, "/api/v1/stats/breakdown?site_id=example.com&property=event:page", "", s.bearer()},
		{"the v1 realtime shim", http.MethodGet, "/api/v1/stats/realtime/visitors?site_id=example.com", "", s.bearer()},
		{"the sites API", http.MethodGet, "/api/v1/sites", "", s.bearer()},
		{"the webhooks API", http.MethodGet, "/api/v1/webhooks", "", s.bearer()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := s.send(t, tc.method, tc.path, tc.body, tc.headers)

			if response.Code != http.StatusPaymentRequired {
				t.Fatalf("status %d, want 402: %s", response.Code, response.Body.String())
			}

			// A refusal nobody can read is a support ticket. Every one of these
			// has to name the state and the page that clears it.
			if !strings.Contains(response.Body.String(), "/billing") {
				t.Errorf("the refusal does not say where to go: %s", response.Body.String())
			}
		})
	}
}

// TestTheTeamScreensNeedASession is the check that stops the administration
// surface being open.
//
// These screens transfer an account's ownership, mint API keys and publish a
// site's traffic. They live in their own package, and a package that answered
// them without a session would hand all three to anybody who could reach the
// port — with no error anywhere, because from the server's side it looks like
// an ordinary request.
func TestTheTeamScreensNeedASession(t *testing.T) {
	s := newStack(t)

	for _, path := range []string{
		"/settings/members",
		"/settings/sites/example.com/sharing",
		"/settings/sites/example.com/reports",
		"/settings/sites/example.com/health",
	} {
		t.Run(path, func(t *testing.T) {
			response := s.send(t, http.MethodGet, path, "", nil)

			if response.Code == http.StatusNotFound {
				t.Fatalf("%s is not a route this process answers", path)
			}

			if response.Code != http.StatusFound {
				t.Fatalf("a signed-out request was answered %d, want a redirect to sign in: %s",
					response.Code, response.Body.String())
			}

			if location := response.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
				t.Fatalf("a signed-out request was sent to %q rather than to sign in", location)
			}
		})
	}
}

// TestALockedAccountsSharedDashboardCarriesNoNumbers covers the door a public
// dashboard opens.
//
// The shell itself is not refused: it is a page a stranger may have bookmarked,
// and answering them with this account's billing state would say more about the
// account than the account's owner asked us to. What it must not do is carry the
// numbers, and it does not — every figure on that page comes from the stats
// endpoint, which is gated on the same account.
//
// The assertion is on the stats endpoint reached through the shared page's own
// origin, because "the shell rendered" and "the numbers were served" are the two
// facts that have to differ here, and only one of them is visible in the HTML.
func TestALockedAccountsSharedDashboardCarriesNoNumbers(t *testing.T) {
	s := newStack(t)
	s.lock(t)

	if code := s.send(t, http.MethodGet, "/public/example.com", "", nil).Code; code == http.StatusNotFound {
		t.Fatal("/public/{domain} is not a route this process answers")
	}

	query := `{"metrics":["visitors"],"date_range":"7d"}`

	response := s.send(t, http.MethodPost, "/api/stats/example.com/query", query, nil)
	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("a locked account's public dashboard was served numbers: %d (%s)",
			response.Code, response.Body.String())
	}
}

// TestALockedAccountIsRefusedTheMCPEndpoint is the same refusal over the other
// protocol. It is asserted separately because JSON-RPC carries its failure in
// the body rather than in the status, so a status check alone would pass
// against a server that cheerfully answered the question.
func TestALockedAccountIsRefusedTheMCPEndpoint(t *testing.T) {
	s := newStack(t)
	s.lock(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`

	response := s.send(t, http.MethodPost, "/mcp", body, s.bearer())

	var answer struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %s", response.Body.String())
	}

	if answer.Error == nil {
		t.Fatalf("a locked account got an answer from the MCP endpoint: %s", response.Body.String())
	}
	if !strings.Contains(answer.Error.Message, "/billing") {
		t.Errorf("the refusal does not say where to go: %q", answer.Error.Message)
	}
}

// TestALockedAccountIsStillCollected is the whole point of the Locked phase and
// the one thing here that must never change. Losing somebody's dashboard over a
// failed card is acceptable; losing their data is not.
func TestALockedAccountIsStillCollected(t *testing.T) {
	s := newStack(t)
	s.lock(t)

	event := `{"n":"pageview","u":"https://example.com/pricing","d":"example.com"}`

	response := s.send(t, http.MethodPost, "/api/event", event, map[string]string{"Content-Type": "text/plain"})

	if response.Code != http.StatusAccepted {
		t.Fatalf("a locked account's event was answered %d, want 202: %s", response.Code, response.Body.String())
	}

	// The public API's wrapper carries the account lock now, so an event
	// endpoint that had drifted behind it would refuse every locked customer's
	// traffic — and do it quietly, as somebody else's console error.
	if got := response.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("the event endpoint challenged for a credential: %q", got)
	}
}

// TestSensitiveDataEndpointsRefuseDirectUnsignedRequests checks the assembled
// mux rather than only the inner handlers, because a missing wrapper is the
// bypass this test is meant to catch.
func TestSensitiveDataEndpointsRefuseDirectUnsignedRequests(t *testing.T) {
	s := newStack(t)

	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/stats/example.com/query", `{"metrics":["visitors"],"date_range":"7d"}`},
		{http.MethodGet, "/api/sites/example.com/annotations", ""},
		{http.MethodPost, "/api/sites/example.com/annotations", `{"shown_on":"2026-08-31","body":"x"}`},
		{http.MethodGet, "/api/sites/example.com/health", ""},
		{http.MethodPost, "/api/sites/example.com/health/allow-hostname", `{"hostname":"evil.example"}`},
	}

	for _, tc := range cases {
		response := s.send(t, tc.method, tc.path, tc.body, map[string]string{"Content-Type": "application/json"})
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d, want 401: %s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
}

// TestStatsCapabilitiesAreRevalidatedThroughTheAssembledRoute proves that the
// real stats mount cannot keep reading after a public toggle or link revocation,
// and that knowing a protected link's bearer slug is insufficient by itself.
func TestStatsCapabilitiesAreRevalidatedThroughTheAssembledRoute(t *testing.T) {
	s := newStack(t)
	s.pay(t)

	control, err := store.Open(filepath.Join(s.dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	ctx := context.Background()
	shares := sharing.NewStore(control)
	body := `{"metrics":["visitors"],"date_range":"7d"}`

	response := s.send(t, http.MethodPost, "/api/stats/example.com/query", body, map[string]string{
		"Content-Type":       "application/json",
		sharing.HeaderPublic: "public",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("published site stats answered %d: %s", response.Code, response.Body.String())
	}

	if err := shares.SetPublic(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	response = s.send(t, http.MethodPost, "/api/stats/example.com/query", body, map[string]string{
		"Content-Type":       "application/json",
		sharing.HeaderPublic: "public",
	})
	if response.Code != http.StatusNotFound {
		t.Fatalf("private site direct stats answered %d, want 404: %s", response.Code, response.Body.String())
	}

	link, err := shares.CreateLink(ctx, 1, "temporary", "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	response = s.send(t, http.MethodPost, "/api/stats/example.com/query", body, map[string]string{
		"Content-Type":      "application/json",
		sharing.HeaderShare: link.Slug,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("live shared stats answered %d: %s", response.Code, response.Body.String())
	}

	if err := shares.RevokeLink(ctx, 1, link.ID); err != nil {
		t.Fatal(err)
	}
	response = s.send(t, http.MethodPost, "/api/stats/example.com/query", body, map[string]string{
		"Content-Type":      "application/json",
		sharing.HeaderShare: link.Slug,
	})
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked link direct stats answered %d, want 404: %s", response.Code, response.Body.String())
	}

	protected, err := shares.CreateLink(ctx, 1, "protected", "hunter2", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	response = s.send(t, http.MethodPost, "/api/stats/example.com/query", body, map[string]string{
		"Content-Type":      "application/json",
		sharing.HeaderShare: protected.Slug,
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("password-protected direct stats answered %d, want 401: %s", response.Code, response.Body.String())
	}
}

// TestBillingRoutesRejectCallerSelectedTeamsWithoutASession is the assembled
// regression for the commerce mount. Query and form values are attacker input;
// neither may select an account before the authentication and role guard runs.
func TestBillingRoutesRejectCallerSelectedTeamsWithoutASession(t *testing.T) {
	s := newStack(t)

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/billing?team=1&team_id=1", ""},
		{http.MethodGet, "/billing/upgrade?team=1&team_id=1", ""},
		{http.MethodGet, "/billing/export?team=1&team_id=1", ""},
		{http.MethodPost, "/billing/checkout", "team=1&team_id=1&plan=monthly"},
		{http.MethodPost, "/billing/portal", "team=1&team_id=1"},
	} {
		response := s.send(t, tc.method, tc.path, tc.body,
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if response.Code != http.StatusFound {
			t.Errorf("%s %s answered %d, want sign-in redirect: %s", tc.method, tc.path, response.Code, response.Body.String())
			continue
		}
		if location := response.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
			t.Errorf("%s %s redirected to %q, want login", tc.method, tc.path, location)
		}
	}
}

// TestALockedAccountCanStillReachEverythingItNeedsToPay enumerates what stays
// open, and is the test that matters most here.
//
// Getting this wrong does not merely annoy somebody: it makes the product
// unrecoverable for the person trying to give us money. Every path below is
// either how they pay, how they get their data out, or how they sign in to do
// one of those.
func TestALockedAccountCanStillReachEverythingItNeedsToPay(t *testing.T) {
	s := newStack(t)
	s.lock(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"the billing screen", http.MethodGet, "/billing", ""},
		{"the plans", http.MethodGet, "/pricing", ""},
		{"the upgrade link on the locked page", http.MethodGet, "/billing/upgrade", ""},
		{"starting a checkout", http.MethodPost, "/billing/checkout", ""},
		{"the payment provider's portal", http.MethodPost, "/billing/portal", ""},
		{"coming back from checkout", http.MethodGet, "/billing/done", ""},
		{"the data export", http.MethodGet, "/billing/export", ""},
		{"the payment provider's callback", http.MethodPost, "/webhooks/stripe", "{}"},
		{"signing in", http.MethodGet, "/login", ""},
		{"a forgotten password", http.MethodGet, "/forgot-password", ""},
		{"the account settings", http.MethodGet, "/settings", ""},
		{"the sites list", http.MethodGet, "/sites", ""},
		{"the tracker script", http.MethodGet, tracker.PathLegacy, ""},
		{"the pixel fallback", http.MethodGet, tracker.PixelPath + "?n=pageview&u=https://example.com/&d=example.com", ""},
		{"the docs", http.MethodGet, "/docs", ""},

		// The site configuration screens hold the other way out: the export
		// that gets somebody's data off this install. They answer a redirect to
		// sign-in here rather than a page, which is enough for both things this
		// test asks — that the route exists, and that the payment gate is not
		// what is standing in front of it.
		{"the shield rules", http.MethodGet, "/settings/sites/example.com/shields", ""},
		{"the path cleaning rules", http.MethodGet, "/settings/sites/example.com/paths", ""},
		{"the import and export screen", http.MethodGet, "/settings/sites/example.com/imports", ""},
		{"preparing a site export", http.MethodPost, "/settings/sites/example.com/exports/create", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := s.send(t, tc.method, tc.path, tc.body, nil)

			if response.Code == http.StatusPaymentRequired {
				t.Fatalf("a locked account was refused the path it needs to stop being locked: %s", response.Body.String())
			}

			// A route that had ceased to exist would pass the check above while
			// being just as unreachable, so the absence of the gate is not
			// enough on its own.
			if response.Code == http.StatusNotFound {
				t.Fatalf("%s %s is not a route this process answers", tc.method, tc.path)
			}
		})
	}
}

// TestPayingRestoresEverything is the recovery path end to end: the account is
// locked, the payment lands, and the next rebuild of the locked set gives the
// dashboard and the API back with nothing reissued and nobody redeployed.
func TestPayingRestoresEverything(t *testing.T) {
	s := newStack(t)
	s.lock(t)

	if code := s.send(t, http.MethodGet, "/dashboard/example.com", "", nil).Code; code != http.StatusPaymentRequired {
		t.Fatalf("the account was not locked to begin with: %d", code)
	}

	s.pay(t)

	if code := s.send(t, http.MethodGet, "/dashboard/example.com", "", nil).Code; code == http.StatusPaymentRequired {
		t.Error("paying did not restore the dashboard")
	}

	response := s.send(t, http.MethodGet, "/api/v1/sites", "", s.bearer())
	if response.Code != http.StatusOK {
		t.Errorf("paying did not restore the API: %d (%s)", response.Code, response.Body.String())
	}
}

// TestADormantAccountIsRefusedAndToldCollectionStopped covers the phase after
// collection stops. It is refused on the same paths and told something
// different, because a dormant account has a gap in its history and hearing
// that after paying rather than before is how trust is lost.
func TestADormantAccountIsRefusedAndToldCollectionStopped(t *testing.T) {
	s := newStack(t)

	s.clock(t, lifecycle.State{
		Trigger:   lifecycle.TriggerLapse,
		StartedAt: time.Now().UTC().Add(-(lifecycle.LockedDays + 1) * lifecycle.Day),
	})

	s.lock(t)

	if reason, _ := s.gate.Locked(lockedTeam); reason != access.ReasonDormant {
		t.Fatalf("the reason is %q, want dormant", reason)
	}

	response := s.send(t, http.MethodGet, "/api/v1/sites", "", s.bearer())

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", response.Code)
	}
	if !strings.Contains(response.Body.String(), "stopped collecting") {
		t.Errorf("a dormant account is not told collection stopped: %s", response.Body.String())
	}

	// Everything it needs to come back is still open, exactly as in Locked.
	if code := s.send(t, http.MethodGet, "/billing", "", nil).Code; code == http.StatusPaymentRequired {
		t.Error("a dormant account cannot reach billing")
	}
}

// TestAnActiveAccountIsUnaffectedEverywhere is the case a bug in any of this
// would break for every paying customer at once.
func TestAnActiveAccountIsUnaffectedEverywhere(t *testing.T) {
	s := newStack(t)
	s.pay(t)

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
	}{
		{"the dashboard", http.MethodGet, "/dashboard/example.com", "", nil},
		{"the stats endpoint", http.MethodPost, "/api/stats/example.com/query", `{"metrics":["visitors"],"date_range":"7d"}`, nil},
		{"the v2 query endpoint", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}`, s.bearer()},
		{"the sites API", http.MethodGet, "/api/v1/sites", "", s.bearer()},
		{"the webhooks API", http.MethodGet, "/api/v1/webhooks", "", s.bearer()},
		{"the MCP endpoint", http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`, s.bearer()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := s.send(t, tc.method, tc.path, tc.body, tc.headers)

			if response.Code == http.StatusPaymentRequired {
				t.Fatalf("a paying account was told to pay: %s", response.Body.String())
			}
			if strings.Contains(response.Body.String(), "not currently paying") {
				t.Fatalf("a paying account was told it is not paying: %s", response.Body.String())
			}
		})
	}
}
