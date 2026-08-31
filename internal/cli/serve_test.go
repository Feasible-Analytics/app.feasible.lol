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
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
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
	routes  http.Handler
	gate    *access.Gate
	key     string
	dataDir string
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

	t.Cleanup(func() { manager.CloseAll() })
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

	app, err := buildApp(e, control, manager, service, secret, mailer, com.Gate)
	if err != nil {
		t.Fatal(err)
	}

	public := buildPublic(e, control, service.Sites, manager, com.Gate)

	_, key, err := public.Keys.Create(ctx, lockedTeam, 1, "test", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	return &stack{
		routes:  serveRoutes(e, service, manager, secret, dir, app, public, com),
		gate:    com.Gate,
		key:     key,
		dataDir: dir,
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

	if _, err := migrate.Run(context.Background(), control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Unix()

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, name, created_at, updated_at) VALUES (1, 'owner@example.com', 'Owner', ?, ?)`, []any{now, now}},
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Example Co', ?, ?)`, []any{lockedTeam, now, now}},
		{`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, 1, 'owner', ?)`, []any{lockedTeam, now}},
		{`INSERT INTO sites (id, account_id, domain, timezone, created_at, updated_at) VALUES (1, ?, 'example.com', 'UTC', ?, ?)`, []any{lockedTeam, now, now}},
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
		{"the payment provider's portal", http.MethodGet, "/billing/portal", ""},
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
