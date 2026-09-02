//
// web_test.go
// The real flows, driven through the route table with real HTTP requests.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/destructive"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
	"github.com/pquerna/otp/totp"
)

// testApp is a handler plus the pieces a test wants to look inside — the store
// to make assertions against, and the captured mail to read a code out of.
type testApp struct {
	*Handler

	store *Store
	sent  *captureSender
}

// captureSender keeps every message in memory instead of sending it, so a test
// can read the verification code the same way a person reads their inbox.
type captureSender struct {
	messages []mail.Message
}

// Send records a message and reports the acceptance a real transport would.
// Anything else would be a transport that declined every message, which the
// mailer correctly refuses to call a send.
func (c *captureSender) Send(_ context.Context, msg mail.Message) (mail.Result, error) {
	c.messages = append(c.messages, msg)

	return mail.Result{Transport: "capture", Accepted: true, Detail: "captured"}, nil
}

// last returns the most recent message, failing the test when none was sent.
func (c *captureSender) last(t *testing.T) mail.Message {
	t.Helper()

	if len(c.messages) == 0 {
		t.Fatal("no email was sent")
	}

	return c.messages[len(c.messages)-1]
}

// newTestApp builds the whole application over temporary databases.
func newTestApp(t *testing.T) *testApp {
	t.Helper()

	store, db := newTestStore(t)

	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)

	t.Cleanup(func() { checkClose(t, "account manager", manager.CloseAll) })

	sender := &captureSender{}

	mailer := mail.NewWithTransport(sender, "feasible <no-reply@example.com>", "http://localhost:19312")

	log := logger.New(logger.Options{Level: "error", Output: os.Stderr})
	purger := &lifecycle.Purger{
		Store:    lifecycle.NewStore(db),
		Accounts: manager,
		DataDir:  dataDir,
		Log:      log,
	}

	handler, err := NewHandler(Options{
		Store:       store,
		Teams:       teams.NewStore(db),
		Traffic:     NewTraffic(manager),
		Mailer:      mailer,
		Sealer:      newTestSealer(t),
		Google:      NewGoogle("", "", "http://localhost:19312"),
		Deleter:     NewDeleter(purger, log),
		Destructive: &destructive.Service{DB: db, Accounts: manager},
		SiteCache:   sites.New(db),
		BaseURL:     "http://localhost:19312",
		Log:         log,
	})
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}

	return &testApp{Handler: handler, store: store, sent: sender}
}

// client is a cookie-keeping HTTP client over a test server, which is what
// makes a multi-request flow — register, verify, create a site — testable as
// one sequence.
type client struct {
	t      *testing.T
	app    *testApp
	server *httptest.Server
	http   *http.Client
}

// newClient starts a test server for the application and returns a client that
// keeps cookies and does not follow redirects, so a test can assert on where a
// handler tried to send the browser.
func newClient(t *testing.T, app *testApp) *client {
	t.Helper()

	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	return &client{
		t:      t,
		app:    app,
		server: server,
		http: &http.Client{
			Jar: &cookieJar{},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// get fetches a page.
func (c *client) get(path string) *http.Response {
	c.t.Helper()

	resp, err := c.http.Get(c.server.URL + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}

	return resp
}

// body fetches a page and returns its HTML, which is how the tests assert on
// what somebody would actually see.
func (c *client) body(path string) string {
	c.t.Helper()

	resp := c.get(path)
	defer closeResponseBody(c.t, resp)

	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)

	return string(buf[:n])
}

// post submits a form, filling in the CSRF token from the page it came from so
// that every test exercises the real check rather than bypassing it.
func (c *client) post(path string, form url.Values) *http.Response {
	c.t.Helper()

	if form.Get(csrfField) == "" {
		form.Set(csrfField, c.csrfToken())
	}

	resp, err := c.http.PostForm(c.server.URL+path, form)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}

	return resp
}

// csrfToken reads the token out of the current cookie jar, minting one by
// loading a page if there is not one yet. Every form post in these tests goes
// through it, so the real check is exercised rather than bypassed.
func (c *client) csrfToken() string {
	c.t.Helper()

	if token, ok := c.tokenFromJar(); ok {
		return token
	}

	// Loading any page issues one.
	closeResponseBody(c.t, c.get("/login"))

	if token, ok := c.tokenFromJar(); ok {
		return token
	}

	c.t.Fatal("no csrf cookie was issued")

	return ""
}

// tokenFromJar unwraps the signed CSRF cookie, if the jar is holding one.
func (c *client) tokenFromJar() (string, bool) {
	for _, cookie := range c.http.Jar.Cookies(&url.URL{Scheme: "http", Host: "test"}) {
		if cookie.Name != csrfCookieName {
			continue
		}

		if value, ok := c.app.Sealer.VerifySignedValue(cookie.Value); ok {
			return value, true
		}
	}

	return "", false
}

// cookieJar is a minimal jar. net/http/cookiejar would work, but it applies
// public-suffix rules that a 127.0.0.1 test server does not satisfy, and the
// behaviour under test is our own cookie handling rather than the jar's.
type cookieJar struct {
	cookies map[string]*http.Cookie
}

// SetCookies stores what the server sent, honouring a deletion.
func (j *cookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	if j.cookies == nil {
		j.cookies = map[string]*http.Cookie{}
	}

	for _, cookie := range cookies {
		if cookie.MaxAge < 0 || cookie.Value == "" {
			delete(j.cookies, cookie.Name)
			continue
		}

		j.cookies[cookie.Name] = cookie
	}
}

// Cookies returns everything stored, since a test server is one origin.
func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(j.cookies))

	for _, cookie := range j.cookies {
		out = append(out, cookie)
	}

	return out
}

// registerAndVerify walks a browser through the whole sign-up: the form, the
// emailed code, and the verification screen. It returns the client, already
// signed in.
func registerAndVerify(t *testing.T, app *testApp) *client {
	t.Helper()

	c := newClient(t, app)

	resp := c.post("/register", url.Values{
		"email":    {"person@example.com"},
		"password": {"a long enough password"},
		"name":     {"Person"},
	})
	closeResponseBody(t, resp)

	if location := resp.Header.Get("Location"); location != "/verify-email" {
		t.Fatalf("registration should go to the verification screen, got %q (status %d)", location, resp.StatusCode)
	}

	// The code is read out of the captured email, exactly as a person reads it
	// out of their inbox.
	message := app.sent.last(t)

	code := extractCode(t, message.Text)

	resp = c.post("/verify-email", url.Values{"code": {code}})
	closeResponseBody(t, resp)

	if !strings.HasPrefix(resp.Header.Get("Location"), "/sites") {
		t.Fatalf("verification should go to the sites list, got %q (status %d)",
			resp.Header.Get("Location"), resp.StatusCode)
	}

	return c
}

// signedClientFor creates a browser session for a seeded verified user. Tests
// that compare roles use it so every request crosses the real session guard.
func signedClientFor(t *testing.T, app *testApp, userID int64) *client {
	t.Helper()

	c := newClient(t, app)
	token, _, err := app.store.CreateSession(context.Background(), userID, "Test browser")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	c.http.Jar.SetCookies(&url.URL{Scheme: "http", Host: "test"}, []*http.Cookie{{
		Name: SessionCookieName, Value: token, Path: "/",
	}})

	return c
}

// TestInvitationLinkRedeemsThroughSignupAndVerification drives the bearer link
// through the public route, recipient-bound signup, verification and final
// membership grant rather than calling the team store directly.
func TestInvitationLinkRedeemsThroughSignupAndVerification(t *testing.T) {
	app := newTestApp(t)
	registerAndVerify(t, app)

	owner, err := app.store.UserByEmail(context.Background(), "person@example.com")
	if err != nil {
		t.Fatalf("read owner: %v", err)
	}
	team, err := app.store.TeamForUser(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("read team: %v", err)
	}

	teamStore := teams.NewStore(app.store.DB())
	token, _, err := teamStore.Invite(context.Background(), owner.ID, teams.Invitation{
		TeamID: team.ID, Email: "invited@example.com", Role: teams.RoleEditor,
	})
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	app.DisableRegistration = true

	invited := newClient(t, app)
	resp := invited.get("/invitations/" + token)
	closeResponseBody(t, resp)
	if location := resp.Header.Get("Location"); location != "/login?next=/invitations/accept" {
		t.Fatalf("new recipient should reach the neutral auth path, got %q", location)
	}

	resp = invited.post("/register", url.Values{
		"email":    {"attacker@example.com"},
		"password": {"a long enough password"},
		"name":     {"Invited"},
	})
	closeResponseBody(t, resp)

	user, err := app.store.UserByEmail(context.Background(), "invited@example.com")
	if err != nil {
		t.Fatalf("signup was not bound to recipient: %v", err)
	}
	if _, err := app.store.UserByEmail(context.Background(), "attacker@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("signup accepted a substituted invitation email: %v", err)
	}

	code := extractCode(t, app.sent.last(t).Text)
	resp = invited.post("/verify-email", url.Values{"code": {code}})
	closeResponseBody(t, resp)
	if location := resp.Header.Get("Location"); location != "/invitations/accept" {
		t.Fatalf("verification did not resume invitation, got %q", location)
	}

	resp = invited.get("/invitations/accept")
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("accept status = %d", resp.StatusCode)
	}

	role, err := teamStore.RoleOf(context.Background(), team.ID, user.ID)
	if err != nil || role != teams.RoleEditor {
		t.Fatalf("accepted role = %q, %v; want editor", role, err)
	}
}

// TestSelfHostedRegistrationIsClosed removes public signup forms and refuses
// both registration methods while leaving the ordinary login page available.
func TestSelfHostedRegistrationIsClosed(t *testing.T) {
	app := newTestApp(t)
	app.DisableRegistration = true
	c := newClient(t, app)

	login := c.body("/login")
	if strings.Contains(login, "/register") {
		t.Fatalf("self-hosted login still advertises registration: %s", login)
	}

	resp := c.get("/register")
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET registration status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	resp = c.post("/register", url.Values{
		"email": {"stranger@example.com"}, "password": {"a long enough password"},
	})
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST registration status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	if _, err := app.store.UserByEmail(context.Background(), "stranger@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled registration created a user: %v", err)
	}
}

// TestSelfHostedNavigationHidesBillingButKeepsExports ensures removing the
// paywall surface does not accidentally remove the unrestricted data export.
func TestSelfHostedNavigationHidesBillingButKeepsExports(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)
	app.DisableCommerce = true

	body := c.body("/sites")
	if strings.Contains(body, `href="/billing`) {
		t.Fatalf("self-hosted sidebar still links to billing: %s", body)
	}

	user, err := app.store.UserByEmail(context.Background(), "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	team, err := app.store.TeamForUser(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.store.CreateSite(context.Background(), team.ID, "self-hosted.example", "", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SiteCache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard/"+site.Domain, nil)
	req = req.WithContext(context.WithValue(req.Context(), contextUser, user))
	nav := app.NavigationForDashboard(httptest.NewRecorder(), req)
	if nav.BillingURL != "" || nav.ExportURL == "" {
		t.Fatalf("self-hosted navigation billing=%q export=%q", nav.BillingURL, nav.ExportURL)
	}
}

// TestInvitationEntryIsNonEnumeratingAndRejectsTerminalTokens checks that valid
// existing/new recipients receive identical responses while recipient binding,
// expiry and revocation remain enforced.
func TestInvitationEntryIsNonEnumeratingAndRejectsTerminalTokens(t *testing.T) {
	app := newTestApp(t)
	ownerClient := registerAndVerify(t, app)
	owner, err := app.store.UserByEmail(context.Background(), "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	team, err := app.store.TeamForUser(context.Background(), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	teamStore := teams.NewStore(app.store.DB())

	locations := []string{}
	for _, email := range []string{"person@example.com", "new-recipient@example.com"} {
		token, _, err := teamStore.Invite(context.Background(), owner.ID, teams.Invitation{
			TeamID: team.ID, Email: email, Role: teams.RoleEditor,
		})
		if err != nil {
			t.Fatal(err)
		}
		anonymous := newClient(t, app)
		resp := anonymous.get("/invitations/" + token)
		locations = append(locations, resp.Header.Get("Location"))
		closeResponseBody(t, resp)
	}
	if locations[0] != locations[1] || locations[0] != "/login?next=/invitations/accept" {
		t.Fatalf("existing/new invitation redirects = %q/%q", locations[0], locations[1])
	}

	wrongToken, _, err := teamStore.Invite(context.Background(), owner.ID, teams.Invitation{
		TeamID: team.ID, Email: "somebody-else@example.com", Role: teams.RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := ownerClient.get("/invitations/" + wrongToken)
	closeResponseBody(t, resp)
	resp = ownerClient.get("/invitations/accept")
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong recipient acceptance = %d, want 403", resp.StatusCode)
	}

	expiredToken, expired, err := teamStore.Invite(context.Background(), owner.ID, teams.Invitation{
		TeamID: team.ID, Email: "expired@example.com", Role: teams.RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.DB().Exec(`UPDATE team_invitations SET expires_at = 0 WHERE id = ?`, expired.ID); err != nil {
		t.Fatal(err)
	}
	resp = newClient(t, app).get("/invitations/" + expiredToken)
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired invitation entry = %d, want 404", resp.StatusCode)
	}

	revokedToken, revoked, err := teamStore.Invite(context.Background(), owner.ID, teams.Invitation{
		TeamID: team.ID, Email: "revoked@example.com", Role: teams.RoleViewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := teamStore.RevokeInvitation(context.Background(), owner.ID, team.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	resp = newClient(t, app).get("/invitations/" + revokedToken)
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked invitation entry = %d, want 404", resp.StatusCode)
	}
}

// TestDashboardAndSiteRoutesEnforceEveryRole drives the HTTP authorization
// boundaries for all five team roles and both per-site guest roles. Store-level
// matrix tests alone cannot catch a route mounted with the wrong permission.
func TestDashboardAndSiteRoutesEnforceEveryRole(t *testing.T) {
	app := newTestApp(t)
	registerAndVerify(t, app)
	ctx := context.Background()

	owner, err := app.store.UserByEmail(ctx, "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	team, err := app.store.TeamForUser(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.store.CreateSite(ctx, team.ID, "roles.example", "Roles", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	teamStore := teams.NewStore(app.store.DB())
	stamp := app.store.Now().Unix()
	clients := map[teams.Role]*client{}

	for index, role := range []teams.Role{
		teams.RoleAdmin, teams.RoleEditor, teams.RoleBilling, teams.RoleViewer,
		teams.RoleGuestEditor, teams.RoleGuestViewer,
	} {
		result, err := app.store.DB().ExecContext(ctx, `
			INSERT INTO users (email, name, email_verified_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, fmt.Sprintf("role-%d@example.com", index), teams.Label(role), stamp, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		userID, _ := result.LastInsertId()

		if teams.IsGuestRole(role) {
			if _, err := app.store.DB().ExecContext(ctx, `
				INSERT INTO guest_memberships (site_id, user_id, role, created_at) VALUES (?, ?, ?, ?)
			`, site.ID, userID, role, stamp); err != nil {
				t.Fatal(err)
			}
		} else if _, err := app.store.DB().ExecContext(ctx, `
			INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, ?, ?)
		`, team.ID, userID, role, stamp); err != nil {
			t.Fatal(err)
		}

		clients[role] = signedClientFor(t, app, userID)
	}

	if err := app.SiteCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	dashboard := httptest.NewServer(app.GuardDashboard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	t.Cleanup(dashboard.Close)

	for role, c := range clients {
		response, err := c.http.Get(dashboard.URL + "/dashboard/roles.example")
		if err != nil {
			t.Fatalf("%s dashboard: %v", role, err)
		}
		closeResponseBody(t, response)
		if response.StatusCode != http.StatusNoContent {
			t.Errorf("%s dashboard answered %d, want 204", role, response.StatusCode)
		}
	}

	settingsPath := "/sites/" + strconv.FormatInt(site.ID, 10) + "/settings"
	for role, want := range map[teams.Role]int{
		teams.RoleAdmin:       http.StatusOK,
		teams.RoleEditor:      http.StatusOK,
		teams.RoleBilling:     http.StatusNotFound,
		teams.RoleViewer:      http.StatusNotFound,
		teams.RoleGuestEditor: http.StatusOK,
		teams.RoleGuestViewer: http.StatusNotFound,
	} {
		response := clients[role].get(settingsPath)
		closeResponseBody(t, response)
		if response.StatusCode != want {
			t.Errorf("%s site settings answered %d, want %d", role, response.StatusCode, want)
		}
	}

	for _, role := range []teams.Role{teams.RoleEditor, teams.RoleGuestEditor} {
		response := clients[role].post("/sites/"+strconv.FormatInt(site.ID, 10)+"/delete",
			url.Values{"confirm": {site.Domain}})
		closeResponseBody(t, response)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s deleted a site: status %d", role, response.StatusCode)
		}
	}

	if _, err := teamStore.AuthoriseSite(ctx, site.ID, owner.ID, teams.PermViewDashboard); err != nil {
		t.Fatalf("role checks changed the site unexpectedly: %v", err)
	}
}

// TestBillingGuardRejectsTeamSpoofingAndMissingCSRF covers the commerce mount's
// exact security contract: session, explicit team, Billing permission and the
// application's form token are all required before account-specific handlers.
func TestBillingGuardRejectsTeamSpoofingAndMissingCSRF(t *testing.T) {
	app := newTestApp(t)
	ownerClient := registerAndVerify(t, app)
	ctx := context.Background()
	owner, _ := app.store.UserByEmail(ctx, "person@example.com")
	team, _ := app.store.TeamForUser(ctx, owner.ID)

	protected := app.GuardTeam(teams.PermManageBilling, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		teamID, err := app.AuthoriseTeamRequest(r, teams.PermManageBilling)
		if err != nil || teamID != team.ID {
			http.Error(w, "wrong team", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server := httptest.NewServer(protected)
	t.Cleanup(server.Close)

	public := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := public.Get(server.URL + "/billing?team=" + strconv.FormatInt(team.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusFound || !strings.HasPrefix(response.Header.Get("Location"), "/login") {
		t.Fatalf("unsigned caller-selected billing team answered %d at %q", response.StatusCode, response.Header.Get("Location"))
	}

	response, err = ownerClient.http.Get(server.URL + "/billing?team_id=" + strconv.FormatInt(team.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("owner billing GET answered %d", response.StatusCode)
	}

	if _, err := app.store.DB().ExecContext(ctx, `UPDATE team_memberships SET role = 'viewer' WHERE team_id = ? AND user_id = ?`, team.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	response, err = ownerClient.http.Get(server.URL + "/billing?team_id=" + strconv.FormatInt(team.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer billing GET answered %d, want 403", response.StatusCode)
	}

	if _, err := app.store.DB().ExecContext(ctx, `UPDATE team_memberships SET role = 'billing' WHERE team_id = ? AND user_id = ?`, team.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	response, err = ownerClient.http.PostForm(server.URL+"/billing/checkout", url.Values{
		"team_id": {strconv.FormatInt(team.ID, 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("billing POST without CSRF answered %d, want 403", response.StatusCode)
	}

	response, err = ownerClient.http.PostForm(server.URL+"/billing/checkout", url.Values{
		"team_id":    {strconv.FormatInt(team.ID, 10)},
		"csrf_token": {ownerClient.csrfToken()},
	})
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("billing POST with CSRF answered %d", response.StatusCode)
	}
}

// TestSiteAPIGuardRejectsMissingCSRF proves session-authenticated JSON writes
// cannot be driven cross-site even when the browser carries a valid session.
func TestSiteAPIGuardRejectsMissingCSRF(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)
	ctx := context.Background()
	user, _ := app.store.UserByEmail(ctx, "person@example.com")
	team, _ := app.store.TeamForUser(ctx, user.ID)
	site, err := app.store.CreateSite(ctx, team.ID, "api-guard.example", "", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SiteCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	guarded := app.GuardSiteAPI(func(*http.Request) string { return site.Domain },
		func(*http.Request) teams.Permission { return teams.PermManageSiteSettings },
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server := httptest.NewServer(guarded)
	t.Cleanup(server.Close)

	response, err := c.http.Post(server.URL+"/api/sites/api-guard.example/annotations", "application/json", strings.NewReader(`{"shown_on":"2026-08-31","body":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("site API write without CSRF answered %d, want 403", response.StatusCode)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/sites/api-guard.example/annotations",
		strings.NewReader(`{"shown_on":"2026-08-31","body":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(csrfHeader, c.csrfToken())
	response, err = c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("site API write with CSRF answered %d", response.StatusCode)
	}
}

// TestMultiTeamRequestsRequireExplicitContext proves a user with two eligible
// teams is never silently assigned whichever membership SQLite returns first.
func TestMultiTeamRequestsRequireExplicitContext(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)
	ctx := context.Background()
	owner, _ := app.store.UserByEmail(ctx, "person@example.com")
	first, _ := app.store.TeamForUser(ctx, owner.ID)
	stamp := app.store.Now().Unix()

	result, err := app.store.DB().ExecContext(ctx, `INSERT INTO teams (name, created_at, updated_at) VALUES ('Second', ?, ?)`, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := result.LastInsertId()
	if _, err := app.store.DB().ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
	`, second, owner.ID, stamp); err != nil {
		t.Fatal(err)
	}

	response := c.get("/sites")
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("ambiguous sites request answered %d, want 400", response.StatusCode)
	}

	response = c.get("/sites?team_id=" + strconv.FormatInt(first.ID, 10))
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("explicit first team answered %d", response.StatusCode)
	}
	response = c.get("/sites?team_id=" + strconv.FormatInt(second, 10))
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("explicit second team answered %d", response.StatusCode)
	}
}

// TestMultiTeamSiteNavigationKeepsTheSelectedSiteAndTeam proves every route
// away from a site settings screen stays in the team that owns that site.
func TestMultiTeamSiteNavigationKeepsTheSelectedSiteAndTeam(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)
	ctx := context.Background()
	owner, _ := app.store.UserByEmail(ctx, "person@example.com")
	stamp := app.store.Now().Unix()

	result, err := app.store.DB().ExecContext(ctx,
		`INSERT INTO teams (name, created_at, updated_at) VALUES ('Second', ?, ?)`, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := result.LastInsertId()
	if _, err := app.store.DB().ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
	`, second, owner.ID, stamp); err != nil {
		t.Fatal(err)
	}

	site, err := app.store.CreateSite(ctx, second, "second-team.example", "", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SiteCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	body := c.body("/sites/" + strconv.FormatInt(site.ID, 10) + "/settings")
	for _, want := range []string{
		`href="/sites?team_id=` + strconv.FormatInt(second, 10) + `"`,
		`href="/dashboard/second-team.example"`,
		`href="/settings/sites/second-team.example/conversions"`,
		`href="/billing?team=` + strconv.FormatInt(second, 10) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("second-team site settings are missing %s", want)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("GET /dashboard/{domain}", app.GuardDashboard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		navigation := app.NavigationForDashboard(w, r)
		_, _ = fmt.Fprintf(w, "%s\n%s\n%s\n%s",
			navigation.SitesURL, navigation.SiteSettingsURL, navigation.ConversionsURL, navigation.BillingURL)
	})))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	response, err := c.http.Get(server.URL + "/dashboard/second-team.example")
	if err != nil {
		t.Fatal(err)
	}
	navigationBody, _ := io.ReadAll(response.Body)
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard navigation answered %d", response.StatusCode)
	}
	for _, want := range []string{
		"/sites?team_id=" + strconv.FormatInt(second, 10),
		"/sites/domain/second-team.example/settings",
		"/settings/sites/second-team.example/conversions",
		"/billing?team=" + strconv.FormatInt(second, 10),
	} {
		if !strings.Contains(string(navigationBody), want) {
			t.Errorf("dashboard navigation is missing %q: %s", want, navigationBody)
		}
	}
}

// TestSiteContextUsesTheLiveOwnerAfterTransfer proves that authorization and
// team selection change in the same request as a transfer, without waiting for
// the routing cache's next refresh interval.
func TestSiteContextUsesTheLiveOwnerAfterTransfer(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)
	ctx := context.Background()
	owner, _ := app.store.UserByEmail(ctx, "person@example.com")
	first, _ := app.store.TeamForUser(ctx, owner.ID)
	stamp := app.store.Now().Unix()

	result, err := app.store.DB().ExecContext(ctx,
		`INSERT INTO teams (name, created_at, updated_at) VALUES ('Destination', ?, ?)`, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := result.LastInsertId()
	if _, err := app.store.DB().ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
	`, second, owner.ID, stamp); err != nil {
		t.Fatal(err)
	}

	site, err := app.store.CreateSite(ctx, first.ID, "transferred.example", "", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SiteCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Teams.TransferSite(ctx, owner.ID, site.ID, second); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /settings/sites/{domain}/sharing", app.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, teamID, err := app.Identify(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, teamID)
	})))
	mux.Handle("GET /settings/members", app.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, teamID, err := app.Identify(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, teamID)
	})))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	for _, path := range []string{
		"/settings/sites/transferred.example/sharing",
		"/settings/members?site_context=transferred.example",
	} {
		response, err := c.http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		closeResponseBody(t, response)
		if response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != strconv.FormatInt(second, 10) {
			t.Fatalf("%s answered %d %q, want live team %d", path, response.StatusCode, body, second)
		}
	}
}

// TestSiteTransferRoutesExerciseTheProductionWorkflow checks the responsive
// form, CSRF boundary, direct browser route, stale API compare-and-swap, and an
// unauthorized destination through the mounted application handler.
func TestSiteTransferRoutesExerciseTheProductionWorkflow(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)
	ctx := context.Background()
	owner, _ := app.store.UserByEmail(ctx, "person@example.com")
	source, _ := app.store.TeamForUser(ctx, owner.ID)
	stamp := app.store.Now().Unix()

	result, err := app.store.DB().ExecContext(ctx,
		`INSERT INTO teams (name, created_at, updated_at) VALUES ('Destination', ?, ?)`, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	destination, _ := result.LastInsertId()
	if _, err := app.store.DB().ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
	`, destination, owner.ID, stamp); err != nil {
		t.Fatal(err)
	}
	result, err = app.store.DB().ExecContext(ctx, `
		INSERT INTO users (email, name, email_verified_at, created_at, updated_at)
		VALUES ('outside@example.test', 'Outside', ?, ?, ?)
	`, stamp, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	outsider, _ := result.LastInsertId()
	result, err = app.store.DB().ExecContext(ctx,
		`INSERT INTO teams (name, created_at, updated_at) VALUES ('Unavailable', ?, ?)`, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, _ := result.LastInsertId()
	if _, err := app.store.DB().ExecContext(ctx, `
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
	`, unavailable, outsider, stamp); err != nil {
		t.Fatal(err)
	}

	site, err := app.store.CreateSite(ctx, source.ID, "move.example", "", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := "/sites/" + itoa(site.ID) + "/settings"
	body := c.body(settingsPath)
	for _, want := range []string{
		"Transfer site", `action="/sites/` + itoa(site.ID) + `/transfer"`,
		`name="csrf_token"`, "grid-cols-1", "sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("transfer form is missing %q", want)
		}
	}

	apiPath := "/api/sites/" + itoa(site.ID) + "/transfer"
	request, err := http.NewRequest(http.MethodPost, c.server.URL+apiPath,
		strings.NewReader(fmt.Sprintf(`{"from_team_id":%d,"to_team_id":%d,"confirm":"move.example"}`, source.ID, destination)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("transfer API without CSRF answered %d, want 403", response.StatusCode)
	}

	response = c.post("/sites/"+itoa(site.ID)+"/transfer", url.Values{
		"from_team_id": {itoa(source.ID)}, "to_team_id": {itoa(destination)}, "confirm": {site.Domain},
	})
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusFound ||
		response.Header.Get("Location") != "/sites?team_id="+itoa(destination)+"&transferred=1" {
		t.Fatalf("browser transfer answered %d location %q", response.StatusCode, response.Header.Get("Location"))
	}

	// The user owns the destination and is therefore still an eligible source
	// actor, but the old from_team_id is stale.
	request, _ = http.NewRequest(http.MethodPost, c.server.URL+apiPath,
		strings.NewReader(fmt.Sprintf(`{"from_team_id":%d,"to_team_id":%d,"confirm":"move.example"}`, source.ID, destination)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(csrfHeader, c.csrfToken())
	response, err = c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale transfer API answered %d, want 409", response.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodPost, c.server.URL+apiPath,
		strings.NewReader(fmt.Sprintf(`{"from_team_id":%d,"to_team_id":%d,"confirm":"move.example"}`, destination, unavailable)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(csrfHeader, c.csrfToken())
	response, err = c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown destination membership answered %d, want 404", response.StatusCode)
	}
}

// TestRequestLogPathRedactsInvitationBearer checks that even an invalid token
// cannot reach application logs through an error or template-rendering path.
func TestRequestLogPathRedactsInvitationBearer(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/invitations/super-secret-token", nil)
	if got := requestLogPath(request); got != "/invitations/[redacted]" {
		t.Fatalf("logged invitation path = %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "/invitations/accept", nil)
	if got := requestLogPath(request); got != "/invitations/accept" {
		t.Fatalf("tokenless acceptance path = %q", got)
	}
}

// extractCode pulls the digit run out of a rendered verification email.
func extractCode(t *testing.T, body string) string {
	t.Helper()

	var run strings.Builder

	for _, r := range body {
		if r >= '0' && r <= '9' {
			run.WriteRune(r)

			if run.Len() == VerificationCodeDigits {
				return run.String()
			}

			continue
		}

		run.Reset()
	}

	t.Fatalf("no %d-digit code in the email:\n%s", VerificationCodeDigits, body)

	return ""
}

// extractVerificationLink returns the one-tap URL from a captured plain-text
// message so a test can open it in a different browser session.
func extractVerificationLink(t *testing.T, body string) string {
	t.Helper()

	for _, field := range strings.Fields(body) {
		candidate := strings.TrimSpace(field)
		if strings.HasPrefix(candidate, "http") && strings.Contains(candidate, "/verify-email/confirm?") {
			return candidate
		}
	}

	t.Fatalf("no verification link in the email:\n%s", body)
	return ""
}

// TestAccountMiddlewareRequiresVerificationRolesAndMembership exercises the
// billing boundary with explicit team selection and real membership roles.
func TestAccountMiddlewareRequiresVerificationRolesAndMembership(t *testing.T) {
	app := newTestApp(t)
	app.mux.Handle("GET /commerce-probe", app.RequireAccount(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		teamID, email, err := app.CurrentAccount(r)
		if err != nil {
			t.Errorf("resolve authenticated account: %v", err)
			http.Error(w, "missing account", http.StatusInternalServerError)
			return
		}

		_, _ = w.Write([]byte(strconv.FormatInt(teamID, 10) + "|" + email))
	})))
	app.mux.Handle("GET /commerce-optional", app.OptionalAccount(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		teamID, _, err := app.CurrentAccount(r)
		if err != nil {
			_, _ = w.Write([]byte("public"))
			return
		}
		_, _ = w.Write([]byte(strconv.FormatInt(teamID, 10)))
	})))

	c := newClient(t, app)
	selectedSignedOut := c.get("/commerce-optional?team=2")
	closeResponseBody(t, selectedSignedOut)
	if selectedSignedOut.StatusCode != http.StatusFound || selectedSignedOut.Header.Get("Location") != "/login?next=%2Fcommerce-optional%3Fteam%3D2" {
		t.Fatalf("selected signed-out route answered %d at %q", selectedSignedOut.StatusCode, selectedSignedOut.Header.Get("Location"))
	}

	signedOut := c.get("/commerce-probe?team=999")
	closeResponseBody(t, signedOut)
	if signedOut.StatusCode != http.StatusFound || !strings.HasPrefix(signedOut.Header.Get("Location"), "/login?next=") {
		t.Fatalf("signed-out account route answered %d and redirected to %q", signedOut.StatusCode, signedOut.Header.Get("Location"))
	}

	registered := c.post("/register", url.Values{
		"email":    {"billing-owner@example.com"},
		"password": {"a long enough password"},
		"name":     {"Billing Owner"},
	})
	closeResponseBody(t, registered)

	unverified := c.get("/commerce-probe?team=999")
	closeResponseBody(t, unverified)
	if unverified.StatusCode != http.StatusFound || unverified.Header.Get("Location") != "/verify-email" {
		t.Fatalf("unverified account route answered %d and redirected to %q", unverified.StatusCode, unverified.Header.Get("Location"))
	}

	code := extractCode(t, app.sent.last(t).Text)
	verified := c.post("/verify-email", url.Values{"code": {code}})
	closeResponseBody(t, verified)

	forged := c.get("/commerce-probe?team=999")
	closeResponseBody(t, forged)
	if forged.StatusCode != http.StatusNotFound {
		t.Fatalf("forged account selector answered %d, want 404", forged.StatusCode)
	}

	body := c.body("/commerce-probe")
	if body != "1|billing-owner@example.com" {
		t.Fatalf("commerce resolved %q, want the authenticated account and email", body)
	}

	now := app.store.Now().Unix()
	if _, err := app.store.DB().Exec(`
		INSERT INTO teams (id, name, created_at, updated_at) VALUES
			(2, 'Billing team', ?, ?),
			(3, 'Viewer team', ?, ?),
			(4, 'Editor team', ?, ?),
			(5, 'Admin team', ?, ?);
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES
			(2, 1, 'billing', ?),
			(3, 1, 'viewer', ?),
			(4, 1, 'editor', ?),
			(5, 1, 'admin', ?);
	`, now, now, now, now, now, now, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	for _, allowed := range []string{"2", "5"} {
		if got := c.body("/commerce-probe?team=" + allowed); got != allowed+"|billing-owner@example.com" {
			t.Errorf("authorized team %s resolved %q", allowed, got)
		}
	}
	if got := c.body("/commerce-optional?team=2"); got != "2" {
		t.Fatalf("optional selected team resolved %q", got)
	}
	for _, denied := range []string{"3", "4"} {
		response := c.get("/commerce-probe?team=" + denied)
		closeResponseBody(t, response)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("non-billing team %s answered %d, want 404", denied, response.StatusCode)
		}
	}
	if _, err := app.store.DB().Exec(`DELETE FROM team_memberships WHERE team_id = 2 AND user_id = 1`); err != nil {
		t.Fatal(err)
	}
	removed := c.get("/commerce-probe?team=2")
	closeResponseBody(t, removed)
	if removed.StatusCode != http.StatusNotFound {
		t.Fatalf("removed billing membership answered %d, want 404", removed.StatusCode)
	}
	removedOptional := c.get("/commerce-optional?team=2")
	closeResponseBody(t, removedOptional)
	if removedOptional.StatusCode != http.StatusNotFound {
		t.Fatalf("removed optional membership answered %d, want 404", removedOptional.StatusCode)
	}
	if got := c.body("/commerce-probe"); got != "1|billing-owner@example.com" {
		t.Fatalf("removed selected membership fell back as %q", got)
	}
}

// TestRegisterVerifyAndCreateASite walks the whole first-run path end to end,
// which is the sequence every new customer takes and the one worth having a
// single test for.
func TestRegisterVerifyAndCreateASite(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)

	body := c.body("/sites")

	if !strings.Contains(body, "No sites yet") {
		t.Errorf("a new account should see the empty state:\n%s", body)
	}

	resp := c.post("/sites/new", url.Values{
		"domain":       {"https://Example.com/pricing"},
		"display_name": {"Marketing site"},
		"timezone":     {"Europe/London"},
	})
	closeResponseBody(t, resp)

	if !strings.HasPrefix(resp.Header.Get("Location"), "/onboarding/") {
		t.Fatalf("creating a site should go to onboarding, got %q", resp.Header.Get("Location"))
	}

	site, err := app.store.SiteByDomain(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("the site should exist: %v", err)
	}

	if site.Timezone != "Europe/London" {
		t.Errorf("the browser timezone should have been used, got %q", site.Timezone)
	}

	// The onboarding screen has to carry the snippet, the waiting state and the
	// skip option.
	onboarding := c.body(resp.Header.Get("Location"))

	for _, fragment := range []string{"Waiting for your first pageview", "Copy snippet", "Skip for now", "WordPress"} {
		if !strings.Contains(onboarding, fragment) {
			t.Errorf("the onboarding screen is missing %q", fragment)
		}
	}

	// The list shows the friendly name rather than the domain.
	list := c.body("/sites")

	if !strings.Contains(list, "Marketing site") {
		t.Error("the sites list should show the display name")
	}
}

// TestPurchaseIntentSurvivesAuthentication covers both email-proof routes and
// ordinary login. Monthly/yearly choice remains a same-origin path throughout,
// while an external next target is reduced to the normal sites destination.
func TestPurchaseIntentSurvivesAuthentication(t *testing.T) {
	t.Run("typed verification", func(t *testing.T) {
		app := newTestApp(t)
		c := newClient(t, app)
		next := "/pricing?plan=monthly&team=1"

		registered := c.post("/register", url.Values{
			"email":    {"monthly@example.com"},
			"password": {"a long enough password"},
			"next":     {next},
		})
		closeResponseBody(t, registered)
		if got := registered.Header.Get("Location"); got != "/verify-email?next=%2Fpricing%3Fplan%3Dmonthly%26team%3D1" {
			t.Fatalf("registration intent redirected to %q", got)
		}

		verified := c.post("/verify-email", url.Values{
			"code": {extractCode(t, app.sent.last(t).Text)},
			"next": {next},
		})
		closeResponseBody(t, verified)
		if got := verified.Header.Get("Location"); got != next {
			t.Fatalf("typed verification redirected to %q, want %q", got, next)
		}
	})

	t.Run("one tap link in another browser", func(t *testing.T) {
		app := newTestApp(t)
		registering := newClient(t, app)
		next := "/pricing?plan=yearly&team=1"

		registered := registering.post("/register", url.Values{
			"email":    {"yearly@example.com"},
			"password": {"a long enough password"},
			"next":     {next},
		})
		closeResponseBody(t, registered)

		link, err := url.Parse(extractVerificationLink(t, app.sent.last(t).Text))
		if err != nil {
			t.Fatal(err)
		}
		verifying := newClient(t, app)
		verified := verifying.get(link.RequestURI())
		closeResponseBody(t, verified)
		if got := verified.Header.Get("Location"); got != next {
			t.Fatalf("one-tap verification redirected to %q, want %q", got, next)
		}
		if body := verifying.body("/sites"); strings.Contains(body, "Sign in") {
			t.Fatal("one-tap verification did not sign in the second browser")
		}
	})

	t.Run("login and open redirect defense", func(t *testing.T) {
		app := newTestApp(t)
		_ = registerAndVerify(t, app)
		fresh := newClient(t, app)

		loggedIn := fresh.post("/login", url.Values{
			"email":    {"person@example.com"},
			"password": {"a long enough password"},
			"next":     {"/pricing?plan=monthly&team=1"},
		})
		closeResponseBody(t, loggedIn)
		if got := loggedIn.Header.Get("Location"); got != "/pricing?plan=monthly&team=1" {
			t.Fatalf("login redirected to %q", got)
		}

		for _, hostile := range []string{
			"https://attacker.example/steal",
			"//attacker.example/steal",
			"/\\attacker.example/steal",
			"/%5c%5cattacker.example/steal",
		} {
			another := newClient(t, app)
			rejected := another.post("/login", url.Values{
				"email":    {"person@example.com"},
				"password": {"a long enough password"},
				"next":     {hostile},
			})
			closeResponseBody(t, rejected)
			if got := rejected.Header.Get("Location"); got != "/sites" {
				t.Errorf("hostile next target %q redirected to %q", hostile, got)
			}
		}
	})
}

// TestLanguageSurvivesAuthFormsRedirectsAndValidation drives explicit German
// state through signed-out links, a validation response, an auth detour, and a
// successful login while proving an unsafe next target is discarded.
func TestLanguageSurvivesAuthFormsRedirectsAndValidation(t *testing.T) {
	app := newTestApp(t)
	c := newClient(t, app)

	body := c.body("/login?lang=de&next=%2Fsettings")
	for _, want := range []string{`action="/login?lang=de"`, `href="/forgot-password?lang=de"`, "Willkommen zurück"} {
		if !strings.Contains(body, want) {
			t.Errorf("German login page is missing %q", want)
		}
	}

	response := c.post("/login?lang=de", url.Values{
		"email": {"nobody@example.com"}, "password": {"wrong password"},
	})
	invalidBody, _ := io.ReadAll(response.Body)
	closeResponseBody(t, response)
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(invalidBody), `action="/login?lang=de"`) || !strings.Contains(string(invalidBody), "passen zu keinem Konto") {
		t.Fatalf("validation response lost German state: status=%d body=%s", response.StatusCode, invalidBody)
	}

	response = c.get("/settings?lang=de")
	closeResponseBody(t, response)
	if location := response.Header.Get("Location"); location != "/login?lang=de&next=%2Fsettings%3Flang%3Dde" {
		t.Fatalf("auth detour location = %q", location)
	}

	verified := registerAndVerify(t, app)
	response = verified.post("/logout?lang=de", url.Values{})
	closeResponseBody(t, response)
	response = verified.post("/login?lang=de", url.Values{
		"email": {"person@example.com"}, "password": {"a long enough password"}, "next": {"//evil.example/path"},
	})
	closeResponseBody(t, response)
	if location := response.Header.Get("Location"); location != "/sites?lang=de" {
		t.Fatalf("unsafe next produced location %q, want local German sites route", location)
	}
}

// TestSafeNextRejectsHostLikePaths covers raw and percent-encoded browser
// normalisations while preserving an ordinary same-origin path and query.
func TestSafeNextRejectsHostLikePaths(t *testing.T) {
	tests := map[string]string{
		"https://evil.example/path": "/sites",
		"//evil.example/path":       "/sites",
		`/\evil.example/path`:       "/sites",
		"/%5c%5cevil.example/path":  "/sites",
		"/%2f%2fevil.example/path":  "/sites",
		"/settings?tab=security":    "/settings?tab=security",
	}

	for input, want := range tests {
		if got := safeNext(input); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestALockedAccountGetsTheSitesListWithoutItsNumbers is the last place a
// locked account could still read its own traffic.
//
// The page has to stay open — it is the route to site settings and, two clicks
// on, to the billing screen that unlocks everything — but the sparkline beside
// each site is thirty days of that account's visitors, which is the report the
// dashboard has just refused.
func TestALockedAccountGetsTheSitesListWithoutItsNumbers(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)

	resp := c.post("/sites/new", url.Values{
		"domain":       {"example.com"},
		"display_name": {"Marketing site"},
		"timezone":     {"UTC"},
	})
	closeResponseBody(t, resp)

	open := c.body("/sites")
	if !strings.Contains(open, "<polyline") {
		t.Fatalf("a paying account should see the sparkline:\n%s", open)
	}

	app.Access = func(int64) bool { return true }

	locked := c.body("/sites")

	if strings.Contains(locked, "<polyline") {
		t.Error("a locked account was still shown its traffic")
	}

	// Everything that is not a number stays. Locking somebody out of the page
	// that leads to billing is how an account becomes unrecoverable for the
	// person trying to pay us.
	if !strings.Contains(locked, "Marketing site") {
		t.Errorf("the sites list itself was taken away:\n%s", locked)
	}
	if !strings.Contains(locked, "/settings") {
		t.Error("the locked list has no route to settings")
	}

	app.Access = nil

	if restored := c.body("/sites"); !strings.Contains(restored, "<polyline") {
		t.Error("paying did not bring the sparklines back")
	}
}

// TestSignedOutRequestsAreRedirected checks the gate on every signed-in route,
// including that the path somebody asked for survives the detour.
func TestSignedOutRequestsAreRedirected(t *testing.T) {
	app := newTestApp(t)

	c := newClient(t, app)

	resp := c.get("/sites")
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("want a redirect, got %d", resp.StatusCode)
	}

	if !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("want a redirect to /login, got %q", resp.Header.Get("Location"))
	}

	if !strings.Contains(resp.Header.Get("Location"), "next=") {
		t.Error("the path asked for should survive the detour through sign-in")
	}
}

// TestUnverifiedUsersAreHeldAtVerification checks the second gate: everything
// downstream assumes we can reach the person by email.
func TestUnverifiedUsersAreHeldAtVerification(t *testing.T) {
	app := newTestApp(t)

	c := newClient(t, app)

	resp := c.post("/register", url.Values{
		"email":    {"person@example.com"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, resp)

	resp = c.get("/sites")
	closeResponseBody(t, resp)

	if resp.Header.Get("Location") != "/verify-email" {
		t.Errorf("an unverified account should be held at verification, got %q", resp.Header.Get("Location"))
	}
}

// TestFormsRejectAMissingToken checks the CSRF guard actually refuses, since a
// check that is only present is not a check.
func TestFormsRejectAMissingToken(t *testing.T) {
	app := newTestApp(t)

	c := newClient(t, app)

	// Load a page so the cookie exists, then submit a wrong token.
	closeResponseBody(t, c.get("/login"))

	resp := c.post("/login", url.Values{
		csrfField:  {"not-the-token"},
		"email":    {"a@example.com"},
		"password": {"whatever"},
	})
	defer closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 for a mismatched form token, got %d", resp.StatusCode)
	}
}

// TestLoginFailureDoesNotRevealWhetherAnAccountExists checks that the wrong
// password and the unknown address produce the same message. Distinguishing
// them turns the sign-in form into a way to find out who uses us.
func TestLoginFailureDoesNotRevealWhetherAnAccountExists(t *testing.T) {
	app := newTestApp(t)
	registerAndVerify(t, app)

	c := newClient(t, app)

	wrongPassword := c.post("/login", url.Values{
		"email":    {"person@example.com"},
		"password": {"the wrong password"},
	})
	defer closeResponseBody(t, wrongPassword)

	unknown := newClient(t, app).post("/login", url.Values{
		"email":    {"nobody@example.com"},
		"password": {"the wrong password"},
	})
	defer closeResponseBody(t, unknown)

	if wrongPassword.StatusCode != unknown.StatusCode {
		t.Errorf("both failures should answer the same way, got %d and %d",
			wrongPassword.StatusCode, unknown.StatusCode)
	}
}

// TestLoginIsRateLimited checks the guard in front of password guessing.
func TestLoginIsRateLimited(t *testing.T) {
	app := newTestApp(t)
	registerAndVerify(t, app)

	c := newClient(t, app)

	var last *http.Response

	for i := 0; i < LoginAttempts+2; i++ {
		if last != nil {
			closeResponseBody(t, last)
		}

		last = c.post("/login", url.Values{
			"email":    {"person@example.com"},
			"password": {"the wrong password"},
		})
	}

	defer closeResponseBody(t, last)

	if last.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429 once the limit is reached, got %d", last.StatusCode)
	}
}

// TestPasswordResetFlow walks the whole recovery path: ask, read the emailed
// link, set a new password, and sign in with it.
func TestPasswordResetFlow(t *testing.T) {
	app := newTestApp(t)
	registerAndVerify(t, app)

	c := newClient(t, app)

	resp := c.post("/forgot-password", url.Values{"email": {"person@example.com"}})
	closeResponseBody(t, resp)

	// The response is identical whether or not the address exists, so a test
	// has to read the mailbox to know a link was sent.
	message := app.sent.last(t)

	token := extractResetToken(t, message.Text)

	resp = c.post("/reset-password", url.Values{
		"token":    {token},
		"password": {"a brand new password"},
	})
	closeResponseBody(t, resp)

	if !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Fatalf("a completed reset should go to sign-in, got %q", resp.Header.Get("Location"))
	}

	signIn := newClient(t, app)

	resp = signIn.post("/login", url.Values{
		"email":    {"person@example.com"},
		"password": {"a brand new password"},
	})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("the new password should work, got %d", resp.StatusCode)
	}

	// The link is single-use, so submitting it again must fail.
	again := newClient(t, app).post("/reset-password", url.Values{
		"token":    {token},
		"password": {"yet another password"},
	})
	defer closeResponseBody(t, again)

	if again.StatusCode != http.StatusBadRequest {
		t.Errorf("a spent reset link should be refused, got %d", again.StatusCode)
	}
}

// extractResetToken pulls the token out of a rendered reset email.
func extractResetToken(t *testing.T, body string) string {
	t.Helper()

	const marker = "reset-password?token="

	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("no reset link in the email:\n%s", body)
	}

	token := body[idx+len(marker):]
	if end := strings.IndexAny(token, " \n\r\t<"); end >= 0 {
		token = token[:end]
	}

	decoded, err := url.QueryUnescape(token)
	if err != nil {
		t.Fatalf("decode reset token: %v", err)
	}

	return decoded
}

// TestGoogleButtonIsHiddenWithoutCredentials checks the behaviour we need while
// the credentials are still being issued: no button, and a URL that explains
// itself rather than 500ing.
func TestGoogleButtonIsHiddenWithoutCredentials(t *testing.T) {
	app := newTestApp(t)

	c := newClient(t, app)

	if strings.Contains(c.body("/login"), "Continue with Google") {
		t.Error("the Google button should be hidden when no client is configured")
	}

	resp := c.get("/auth/google")
	defer closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for an unconfigured provider, got %d", resp.StatusCode)
	}
}

// TestSessionsScreenListsAndRevokes checks the login-management screen through
// the browser, including that revoking the current session signs it out.
func TestSessionsScreenListsAndRevokes(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)

	body := c.body("/settings/sessions")

	if !strings.Contains(body, "This browser") {
		t.Errorf("the current session should be marked:\n%s", body)
	}

	resp := c.post("/settings/sessions/revoke", url.Values{"all": {"1"}})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("want a redirect after revoking, got %d", resp.StatusCode)
	}

	// The current session survives "everywhere else".
	if strings.Contains(c.body("/settings/sessions"), "Sign in") {
		t.Error("the browser doing the revoking should stay signed in")
	}
}

// TestTwoFactorSetupAndChallenge walks enrolment and then a fresh sign-in
// through the code screen, which is the only way to know the two paths agree.
func TestTwoFactorSetupAndChallenge(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)

	resp := c.post("/settings/security/2fa/start", url.Values{})
	body := readAll(t, resp)

	if !strings.Contains(body, "Scan this with your authenticator app") {
		t.Fatalf("the setup screen should show the QR code:\n%s", body)
	}

	user, err := app.store.UserByEmail(context.Background(), "person@example.com")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	key, err := app.store.TOTPKey(context.Background(), app.Sealer, user)
	if err != nil {
		t.Fatalf("read totp key: %v", err)
	}

	code := currentCode(t, key.Secret(), app.store)

	resp = c.post("/settings/security/2fa/enable", url.Values{"code": {code}})
	body = readAll(t, resp)

	if !strings.Contains(body, "Save your recovery codes") {
		t.Fatalf("finishing enrolment should show the recovery codes once:\n%s", body)
	}

	// A fresh sign-in now has to pass through the code screen.
	fresh := newClient(t, app)

	resp = fresh.post("/login", url.Values{
		"email":    {"person@example.com"},
		"password": {"a long enough password"},
		"next":     {"/pricing?plan=yearly&team=1"},
	})
	closeResponseBody(t, resp)

	if resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("a two-factor account should go to the code screen, got %q", resp.Header.Get("Location"))
	}

	resp = fresh.post("/login/2fa", url.Values{"code": {"000000"}})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong code should be refused, got %d", resp.StatusCode)
	}

	resp = fresh.post("/login/2fa", url.Values{"code": {currentCode(t, key.Secret(), app.store)}})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("the right code should complete the sign-in, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/pricing?plan=yearly&team=1" {
		t.Errorf("two-factor sign-in lost purchase intent and redirected to %q", got)
	}
}

// currentCode produces a valid TOTP code for a secret at the store's clock.
func currentCode(t *testing.T, secret string, s *Store) string {
	t.Helper()

	code, err := totp.GenerateCode(secret, s.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	return code
}

// TestSiteSettingsChangeDomainAndDelete drives the settings screen, including
// the confirmation the destructive actions demand and the warning the delete
// form has to carry.
func TestSiteSettingsChangeDomainAndDelete(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)

	resp := c.post("/sites/new", url.Values{
		"domain":   {"old.example.com"},
		"timezone": {"Etc/UTC"},
	})
	closeResponseBody(t, resp)

	site, err := app.store.SiteByDomain(context.Background(), "old.example.com")
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	path := "/sites/" + itoa(site.ID) + "/settings"

	body := c.body(path)

	if !strings.Contains(body, "traffic is still arriving") && !strings.Contains(body, "Traffic is still arriving") {
		t.Error("the delete section must warn that traffic is still arriving")
	}

	resp = c.post("/sites/"+itoa(site.ID)+"/domain", url.Values{"domain": {"new.example.com"}})
	closeResponseBody(t, resp)

	changed, err := app.store.SiteByID(context.Background(), site.AccountID, site.ID)
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	if changed.Domain != "new.example.com" || changed.PreviousDomain != "old.example.com" {
		t.Errorf("the domain change did not open the dual-write window: %+v", changed)
	}

	// A wrong confirmation must not delete anything.
	resp = c.post("/sites/"+itoa(site.ID)+"/delete", url.Values{"confirm": {"wrong"}})
	closeResponseBody(t, resp)

	if _, err := app.store.SiteByID(context.Background(), site.AccountID, site.ID); err != nil {
		t.Fatalf("the site should still exist: %v", err)
	}

	resp = c.post("/sites/"+itoa(site.ID)+"/delete", url.Values{"confirm": {"new.example.com"}})
	closeResponseBody(t, resp)

	if _, err := app.store.SiteByID(context.Background(), site.AccountID, site.ID); err != ErrNotFound {
		t.Errorf("the site should be gone, got %v", err)
	}
}

// TestOnboardingStatusFlips checks the poll the waiting screen runs: it says
// waiting until traffic exists, and reports it once it does.
func TestOnboardingStatusFlips(t *testing.T) {
	app := newTestApp(t)
	c := registerAndVerify(t, app)

	resp := c.post("/sites/new", url.Values{"domain": {"example.com"}, "timezone": {"Etc/UTC"}})
	closeResponseBody(t, resp)

	site, err := app.store.SiteByDomain(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	body := c.body("/onboarding/" + itoa(site.ID) + "/status")

	if !strings.Contains(body, `"received":false`) {
		t.Errorf("a brand-new site should report no traffic yet: %s", body)
	}
}

// TestAnotherTeamsSiteIs404 checks the scoping through the router, since a site
// id in a URL is guessable.
func TestAnotherTeamsSiteIs404(t *testing.T) {
	app := newTestApp(t)
	owner := registerAndVerify(t, app)

	resp := owner.post("/sites/new", url.Values{"domain": {"example.com"}, "timezone": {"Etc/UTC"}})
	closeResponseBody(t, resp)

	site, err := app.store.SiteByDomain(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	// A second account, signed up the same way.
	other := newClient(t, app)

	resp = other.post("/register", url.Values{
		"email":    {"other@example.com"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, resp)

	user, err := app.store.UserByEmail(context.Background(), "other@example.com")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if err := app.store.MarkVerified(context.Background(), user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	page := other.get("/sites/" + itoa(site.ID) + "/settings")
	defer closeResponseBody(t, page)

	if page.StatusCode != http.StatusNotFound {
		t.Errorf("another team's site should be a 404, got %d", page.StatusCode)
	}
}

// TestDeletingAnOwnedTeamDoesNotPromoteAnotherMembership proves a surviving
// admin membership cannot become implicit ownership after the user's own team
// is gone. Team name, team-wide 2FA, and permanent deletion stay owner-only.
func TestDeletingAnOwnedTeamDoesNotPromoteAnotherMembership(t *testing.T) {
	app := newTestApp(t)
	owner := registerAndVerify(t, app)
	ctx := context.Background()

	member, err := app.store.UserByEmail(ctx, "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, otherTeam, err := app.store.CreateUser(ctx, "other-owner@example.com", "Other Owner", "unused hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.DB().Exec(`
		INSERT INTO team_memberships (team_id, user_id, role, created_at)
		VALUES (?, ?, 'admin', ?)
	`, otherTeam.ID, member.ID, app.store.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	deleted := owner.post("/settings/delete", url.Values{
		"confirm":  {"DELETE"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, deleted)
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("owned team deletion answered %d", deleted.StatusCode)
	}

	remaining := newClient(t, app)
	loggedIn := remaining.post("/login", url.Values{
		"email":    {"person@example.com"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, loggedIn)
	if loggedIn.StatusCode != http.StatusFound {
		t.Fatalf("surviving member could not sign back in: %d", loggedIn.StatusCode)
	}

	renamed := remaining.post("/settings/team", url.Values{
		"name":        {"Hijacked name"},
		"require_2fa": {"1"},
	})
	closeResponseBody(t, renamed)
	if renamed.StatusCode != http.StatusBadRequest {
		t.Fatalf("team mutation without explicit context answered %d, want 400", renamed.StatusCode)
	}

	removed := remaining.post("/settings/delete", url.Values{
		"confirm":  {"DELETE"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, removed)
	if removed.StatusCode != http.StatusBadRequest {
		t.Fatalf("account deletion without explicit context answered %d, want 400", removed.StatusCode)
	}

	var name string
	var require2FA bool
	if err := app.store.DB().QueryRow(`SELECT name, require_2fa FROM teams WHERE id = ?`, otherTeam.ID).Scan(&name, &require2FA); err != nil {
		t.Fatal(err)
	}
	if name != "Other Owner" || require2FA {
		t.Fatalf("another team was changed to name=%q require_2fa=%t", name, require2FA)
	}
}

// TestAssetsAreServedFromTheBinary checks the embedded stylesheet and script
// reach the browser, since every page depends on both.
func TestAssetsAreServedFromTheBinary(t *testing.T) {
	app := newTestApp(t)

	c := newClient(t, app)

	for _, path := range []string{"/app/assets/app.css", "/app/assets/alpine.js"} {
		resp := c.get(path)
		closeResponseBody(t, resp)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s should be served, got %d", path, resp.StatusCode)
		}
	}
}

// readAll drains a response body and closes it.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()

	defer closeResponseBody(t, resp)

	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)

	return string(buf[:n])
}

// itoa formats an id for a URL path.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
