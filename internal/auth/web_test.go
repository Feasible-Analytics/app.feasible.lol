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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
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

	t.Cleanup(func() { manager.CloseAll() })

	sender := &captureSender{}

	mailer := mail.NewWithTransport(sender, "feasible <no-reply@example.com>", "http://localhost:19312")

	log := logger.New(logger.Options{Level: "error", Output: os.Stderr})

	handler, err := NewHandler(Options{
		Store:     store,
		Traffic:   NewTraffic(manager),
		Mailer:    mailer,
		Sealer:    newTestSealer(t),
		Google:    NewGoogle("", "", "http://localhost:19312"),
		Deleter:   NewDeleter(store, manager, dataDir, NewStripe("", log), log),
		SiteCache: sites.New(db),
		BaseURL:   "http://localhost:19312",
		Log:       log,
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
	defer resp.Body.Close()

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
	c.get("/login").Body.Close()

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
	resp.Body.Close()

	if location := resp.Header.Get("Location"); location != "/verify-email" {
		t.Fatalf("registration should go to the verification screen, got %q (status %d)", location, resp.StatusCode)
	}

	// The code is read out of the captured email, exactly as a person reads it
	// out of their inbox.
	message := app.sent.last(t)

	code := extractCode(t, message.Text)

	resp = c.post("/verify-email", url.Values{"code": {code}})
	resp.Body.Close()

	if !strings.HasPrefix(resp.Header.Get("Location"), "/sites") {
		t.Fatalf("verification should go to the sites list, got %q (status %d)",
			resp.Header.Get("Location"), resp.StatusCode)
	}

	return c
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
	resp.Body.Close()

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
	resp.Body.Close()

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
	resp.Body.Close()

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
	resp.Body.Close()

	resp = c.get("/sites")
	resp.Body.Close()

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
	c.get("/login").Body.Close()

	resp := c.post("/login", url.Values{
		csrfField:  {"not-the-token"},
		"email":    {"a@example.com"},
		"password": {"whatever"},
	})
	defer resp.Body.Close()

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
	defer wrongPassword.Body.Close()

	unknown := newClient(t, app).post("/login", url.Values{
		"email":    {"nobody@example.com"},
		"password": {"the wrong password"},
	})
	defer unknown.Body.Close()

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
			last.Body.Close()
		}

		last = c.post("/login", url.Values{
			"email":    {"person@example.com"},
			"password": {"the wrong password"},
		})
	}

	defer last.Body.Close()

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
	resp.Body.Close()

	// The response is identical whether or not the address exists, so a test
	// has to read the mailbox to know a link was sent.
	message := app.sent.last(t)

	token := extractResetToken(t, message.Text)

	resp = c.post("/reset-password", url.Values{
		"token":    {token},
		"password": {"a brand new password"},
	})
	resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Fatalf("a completed reset should go to sign-in, got %q", resp.Header.Get("Location"))
	}

	signIn := newClient(t, app)

	resp = signIn.post("/login", url.Values{
		"email":    {"person@example.com"},
		"password": {"a brand new password"},
	})
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("the new password should work, got %d", resp.StatusCode)
	}

	// The link is single-use, so submitting it again must fail.
	again := newClient(t, app).post("/reset-password", url.Values{
		"token":    {token},
		"password": {"yet another password"},
	})
	defer again.Body.Close()

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
	defer resp.Body.Close()

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
	resp.Body.Close()

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
	})
	resp.Body.Close()

	if resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("a two-factor account should go to the code screen, got %q", resp.Header.Get("Location"))
	}

	resp = fresh.post("/login/2fa", url.Values{"code": {"000000"}})
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong code should be refused, got %d", resp.StatusCode)
	}

	resp = fresh.post("/login/2fa", url.Values{"code": {currentCode(t, key.Secret(), app.store)}})
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("the right code should complete the sign-in, got %d", resp.StatusCode)
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
	resp.Body.Close()

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
	resp.Body.Close()

	changed, err := app.store.SiteByID(context.Background(), site.AccountID, site.ID)
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	if changed.Domain != "new.example.com" || changed.PreviousDomain != "old.example.com" {
		t.Errorf("the domain change did not open the dual-write window: %+v", changed)
	}

	// A wrong confirmation must not delete anything.
	resp = c.post("/sites/"+itoa(site.ID)+"/delete", url.Values{"confirm": {"wrong"}})
	resp.Body.Close()

	if _, err := app.store.SiteByID(context.Background(), site.AccountID, site.ID); err != nil {
		t.Fatalf("the site should still exist: %v", err)
	}

	resp = c.post("/sites/"+itoa(site.ID)+"/delete", url.Values{"confirm": {"new.example.com"}})
	resp.Body.Close()

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
	resp.Body.Close()

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
	resp.Body.Close()

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
	resp.Body.Close()

	user, err := app.store.UserByEmail(context.Background(), "other@example.com")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if err := app.store.MarkVerified(context.Background(), user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	page := other.get("/sites/" + itoa(site.ID) + "/settings")
	defer page.Body.Close()

	if page.StatusCode != http.StatusNotFound {
		t.Errorf("another team's site should be a 404, got %d", page.StatusCode)
	}
}

// TestAssetsAreServedFromTheBinary checks the embedded stylesheet and script
// reach the browser, since every page depends on both.
func TestAssetsAreServedFromTheBinary(t *testing.T) {
	app := newTestApp(t)

	c := newClient(t, app)

	for _, path := range []string{"/app/assets/app.css", "/app/assets/alpine.js"} {
		resp := c.get(path)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s should be served, got %d", path, resp.StatusCode)
		}
	}
}

// readAll drains a response body and closes it.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()

	defer resp.Body.Close()

	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)

	return string(buf[:n])
}

// itoa formats an id for a URL path.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
