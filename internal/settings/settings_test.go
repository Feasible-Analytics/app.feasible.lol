//
// settings_test.go
// The three screens render, and the Google section hides itself when it must.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/google"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pathclean"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/shields"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/timefmt"
)

// newHandler builds a handler over a temporary install holding one site.
func newHandler(t *testing.T) (*Handler, *accounts.Manager) {
	handler, manager, _ := newHandlerWithControl(t)

	return handler, manager
}

// newHandlerWithControl also hands back system.db, for the tests that assert on
// what the write path recorded there.
func newHandlerWithControl(t *testing.T) (*Handler, *accounts.Manager, *sql.DB) {
	t.Helper()

	ctx := context.Background()
	dataDir := t.TempDir()

	control, err := store.Open(filepath.Join(dataDir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(ctx, control, migrate.System()); err != nil {
		t.Fatal(err)
	}

	exec(t, control, "INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', 0, 0)")
	exec(t, control, "INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', 0, 0)")

	siteCache := sites.New(control)
	if err := siteCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	manager := accounts.NewManager(dataDir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	if _, err := manager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}

	trusted, err := clientip.ParseTrustedProxies(nil)
	if err != nil {
		t.Fatal(err)
	}
	shieldCache := shields.New(siteCache, manager)
	shieldCache.Rejections = shields.NewRejections(manager)

	return &Handler{
		Sites:     siteCache,
		Accounts:  manager,
		Jobs:      jobs.NewClient(control),
		DataDir:   dataDir,
		Trusted:   trusted,
		Shields:   shieldCache,
		Paths:     pathclean.New(siteCache, manager),
		Now:       func() time.Time { return time.Unix(1_800_000_000, 0) },
		CSRF:      func(http.ResponseWriter, *http.Request) string { return "test-csrf" },
		CheckCSRF: func(http.ResponseWriter, *http.Request) bool { return true },

		// The fixture is a hosted install, which is the deployment with a
		// billing link to assert on.
		Commerce: true,
	}, manager, control
}

// TestRejectedHostnameCanBeAllowedFromTheShieldsPage covers the one-click path
// from committed rejection evidence to the live additive hostname policy.
func TestRejectedHostnameCanBeAllowedFromTheShieldsPage(t *testing.T) {
	handler, manager := newHandler(t)
	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	day := handler.Now().UTC().Unix() / 86400
	if _, err := account.Writer().ExecContext(context.Background(), `
		INSERT INTO hostname_rejections (site_id, hostname, day, events)
		VALUES (1, 'preview.example.net', ?, 3)`, day); err != nil {
		t.Fatal(err)
	}

	response := get(t, handler, "/settings/sites/example.com/shields")
	if response.Code != http.StatusOK {
		t.Fatalf("shields page answered %d", response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, "preview.example.net") || !strings.Contains(body, ">Allow<") {
		t.Fatal("the rejected hostname does not have a one-click Allow action")
	}

	request := httptest.NewRequest(http.MethodPost, "/settings/sites/example.com/shields/allow-hostname",
		strings.NewReader("hostname=preview.example.net"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("allow hostname answered %d", recorder.Code)
	}

	rules, err := shields.List(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Kind != shields.KindHostname || rules[0].Value != "preview.example.net" {
		t.Fatalf("allowed rules = %+v", rules)
	}
	if !handler.Shields.AllowsHostname(1, "preview.example.net") {
		t.Fatal("the running shield cache was not refreshed")
	}
}

// exec runs one statement or fails the test.
func exec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

// get fetches one page.
func get(t *testing.T, handler *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "203.0.113.14:41234"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

// TestUnsafeRoutesRequireCSRF proves the shared guard runs before every legacy
// settings mutation while accepting the exact token supplied by its verifier.
func TestUnsafeRoutesRequireCSRF(t *testing.T) {
	handler, _ := newHandler(t)
	handler.CheckCSRF = func(w http.ResponseWriter, r *http.Request) bool {
		if r.PostFormValue("csrf_token") == "valid" {
			return true
		}

		http.Error(w, "bad token", http.StatusForbidden)
		return false
	}

	for _, token := range []string{"", "bad", "valid"} {
		body := strings.NewReader("csrf_token=" + token + "&kind=ip&value=203.0.113.9")
		request := httptest.NewRequest(http.MethodPost, "/settings/sites/example.com/shields/add", body)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if token == "valid" && response.Code != http.StatusSeeOther {
			t.Fatalf("valid token status = %d, want redirect", response.Code)
		}
		if token != "valid" && response.Code != http.StatusForbidden {
			t.Fatalf("token %q status = %d, want forbidden", token, response.Code)
		}
	}
}

// TestScreensRender checks each page comes back whole. The templates are parsed
// at start-up, so a broken one is a panic in this test rather than a blank page
// somebody discovers in production.
func TestScreensRender(t *testing.T) {
	handler, _ := newHandler(t)

	for _, tc := range []struct{ path, want string }{
		{"/settings/sites/example.com/shields", "Blocked addresses"},
		{"/settings/sites/example.com/paths", "Trailing slashes"},
		// The screen's own content rather than a navigation label: a nav that
		// gets reworded should not read as a broken screen.
		{"/settings/sites/example.com/imports", "Upload it here"},
	} {
		response := get(t, handler, tc.path)

		if response.Code != http.StatusOK {
			t.Fatalf("%s answered %d", tc.path, response.Code)
		}

		if !strings.Contains(response.Body.String(), tc.want) {
			t.Errorf("%s does not contain %q", tc.path, tc.want)
		}
	}
}

// TestLanguageSurvivesSettingsLinksFormsAndRedirects proves a settings POST
// keeps validated locale state in both its action and post-redirect-get URL.
func TestLanguageSurvivesSettingsLinksFormsAndRedirects(t *testing.T) {
	handler, _ := newHandler(t)
	body := get(t, handler, "/settings/sites/example.com/paths?lang=de").Body.String()

	for _, want := range []string{
		`action="/settings/sites/example.com/paths/save?lang=de"`,
		`href="/settings/sites/example.com/shields?lang=de"`,
		"Those lost originals cannot be reconstructed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("localized path settings are missing %q", want)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/settings/sites/example.com/paths/trailing-slash?lang=de", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("settings POST answered %d", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); !strings.Contains(location, "lang=de") || !strings.HasPrefix(location, "/settings/sites/example.com/paths?") {
		t.Fatalf("settings redirect lost locale: %q", location)
	}
}

// TestShieldsPageShowsTheResolvedAddress covers the one-click promise: the page
// has to show the customer the address their own traffic arrives on, or
// "block my own traffic" is a hunt through a third-party site.
func TestShieldsPageShowsTheResolvedAddress(t *testing.T) {
	handler, _ := newHandler(t)

	body := get(t, handler, "/settings/sites/example.com/shields").Body.String()

	if !strings.Contains(body, "203.0.113.14") {
		t.Fatal("the resolved address is not on the page")
	}

	if !strings.Contains(body, "Block my own traffic") {
		t.Fatal("there is no one-click button for the customer's own address")
	}
}

// TestShieldsPageWarnsAboutALANAddress covers the proxy trap: when the address
// reaching us is a shared proxy rather than the visitor's own, manually
// blocking it blocks every visitor. The warning names the effect and stops
// there — fixing the proxy is not a customer's job and not their vocabulary.
func TestShieldsPageWarnsAboutALANAddress(t *testing.T) {
	handler, _ := newHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/settings/sites/example.com/shields", nil)
	request.RemoteAddr = "192.168.178.1:41234"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	body := recorder.Body.String()

	if !strings.Contains(body, "private or shared proxy address") {
		t.Fatal("no warning was shown for a private address")
	}
	if !strings.Contains(body, "would block everyone") {
		t.Fatal("the warning does not explain the effect of manually blocking the shared address")
	}

	if strings.Contains(body, "X-Forwarded-For") || strings.Contains(body, "FEASIBLE_") {
		t.Fatal("the warning hands a customer a configuration change they cannot make")
	}

	if strings.Contains(body, "Block my own traffic") {
		t.Fatal("a one-click rule was offered for an address shared by every visitor")
	}
}

// TestGoogleSectionHidesItself is what an install with no OAuth client sees. A
// button that sends somebody to Google and comes back with invalid_client is
// worse than no button.
func TestGoogleSectionHidesItself(t *testing.T) {
	handler, _ := newHandler(t)

	body := get(t, handler, "/settings/sites/example.com/imports").Body.String()

	if strings.Contains(body, "Connect Analytics") {
		t.Fatal("the Google section is offered on an install with no OAuth client")
	}

	app, ok := google.NewApp("id", "secret", "https://example.com")
	if !ok {
		t.Fatal("a complete client was reported as unconfigured")
	}
	handler.Google = app

	body = get(t, handler, "/settings/sites/example.com/imports").Body.String()

	if !strings.Contains(body, "Connect Analytics") {
		t.Fatal("the Google section is hidden on an install that has credentials")
	}

	// The delay has to be stated, or every new customer files the same bug
	// about an empty Search Console report.
	if !strings.Contains(body, "24 to 36 hours") {
		t.Fatal("the Search Console delay is not mentioned anywhere on the page")
	}
}

// TestCompletedExportLinkUsesAndReachesTheRegisteredRoute checks the rendered
// URL and the handler behind it as one workflow. A link can look plausible in
// HTML while missing the /sites segment that scopes every actual export route.
func TestCompletedExportLinkUsesAndReachesTheRegisteredRoute(t *testing.T) {
	handler, manager := newHandler(t)
	ctx := context.Background()

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}

	now := handler.now()
	record, token, err := dataio.CreateExport(ctx, account.Writer(), 1, now)
	if err != nil {
		t.Fatalf("create export: %v", err)
	}
	handler.rememberToken(record.ID, token)

	archive := filepath.Join(t.TempDir(), "completed-export.zip")
	contents := []byte("prepared archive")
	if err := os.WriteFile(archive, contents, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if err := dataio.CompleteExport(ctx, account.Writer(), record.ID, archive, int64(len(contents)), now); err != nil {
		t.Fatalf("complete export: %v", err)
	}

	want := "/settings/sites/example.com/exports/download/" + token
	page := get(t, handler, "/settings/sites/example.com/imports")
	if page.Code != http.StatusOK {
		t.Fatalf("imports page answered %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), `href="`+want+`"`) {
		t.Fatalf("completed export did not link to %s", want)
	}
	if strings.Contains(page.Body.String(), `href="/settings/example.com/exports/download/`) {
		t.Fatal("completed export still uses the unroutable path without /sites")
	}

	download := get(t, handler, want)
	if download.Code != http.StatusOK {
		t.Fatalf("download route answered %d: %s", download.Code, download.Body.String())
	}
	if download.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("download content type = %q", download.Header().Get("Content-Type"))
	}
	if download.Body.String() != string(contents) {
		t.Fatalf("download body = %q, want prepared archive", download.Body.String())
	}
}

// TestPathPreviewDoesNotSave checks the preview button. A regular expression
// that eats half a site's URLs has to be visible before it is stored, not
// after.
func TestPathPreviewDoesNotSave(t *testing.T) {
	handler, manager := newHandler(t)

	form := "action=preview&pattern=%5E%2Fusers%2F%5B%5E%2F%5D%2B%24&replacement=%2Fusers%2F%3Aid&label=Users&enabled-0=on"

	request := httptest.NewRequest(http.MethodPost, "/settings/sites/example.com/paths/save", strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the preview answered %d", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "Preview") {
		t.Fatal("the preview section was not rendered")
	}

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rules, err := pathclean.List(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(rules) != 0 {
		t.Fatalf("the preview stored %d rules — nothing should be saved until Save is pressed", len(rules))
	}
}

// TestPatternsDoNotShadowTheAccountScreens is why these routes are enumerated
// rather than mounted as one "/settings/" prefix.
//
// The account screens own /settings/sessions, /settings/security and
// /settings/team, and they are reached through the root handler. A prefix
// registration would win against "/" for all three and Go's mux would report no
// conflict whatsoever — the account screens would simply stop answering, with
// nothing anywhere to say why. This is the check that would notice.
func TestPatternsDoNotShadowTheAccountScreens(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("account")) //nolint:errcheck // a recorder cannot fail
	})

	for _, pattern := range Patterns() {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("site")) //nolint:errcheck // a recorder cannot fail
		})
	}

	for _, tc := range []struct{ path, want string }{
		{"/settings", "account"},
		{"/settings/profile", "account"},
		{"/settings/sessions", "account"},
		{"/settings/sessions/revoke", "account"},
		{"/settings/security", "account"},
		{"/settings/security/2fa/start", "account"},
		{"/settings/security/2fa/qr.png", "account"},
		{"/settings/team", "account"},
		{"/sites/1/settings", "account"},

		{"/settings/sites/example.com/shields", "site"},
		{"/settings/sites/example.com/shields/add", "site"},
		{"/settings/sites/example.com/shields/delete", "site"},
		{"/settings/sites/example.com/paths", "site"},
		{"/settings/sites/example.com/paths/save", "site"},
		{"/settings/sites/example.com/paths/trailing-slash", "site"},
		{"/settings/sites/example.com/imports", "site"},
		{"/settings/sites/example.com/imports/upload", "site"},
		{"/settings/sites/example.com/imports/delete", "site"},
		{"/settings/sites/example.com/exports/create", "site"},
		{"/settings/sites/example.com/exports/download/a-token", "site"},
		{"/settings/sites/example.com/google/connect", "site"},
		{"/settings/sites/example.com/google/disconnect", "site"},
		{"/settings/google/callback", "site"},
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if got := recorder.Body.String(); got != tc.want {
			t.Errorf("%s reached the %s screens, want %s", tc.path, got, tc.want)
		}
	}
}

// TestDomainOfNamesTheSiteEveryRouteExcept covers the one route that carries no
// site in its path. The authorisation wrapper lets an empty domain through on
// sign-in alone, so a route that should name a site and does not would silently
// skip the ownership check.
func TestDomainOfNamesTheSiteEveryRouteExcept(t *testing.T) {
	mux := http.NewServeMux()

	seen := map[string]string{}

	for _, pattern := range Patterns() {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			seen[r.URL.Path] = DomainOf(r)
		})
	}

	for path, want := range map[string]string{
		"/settings/sites/example.com/shields":             "example.com",
		"/settings/sites/example.com/imports/upload":      "example.com",
		"/settings/sites/example.com/exports/download/xy": "example.com",
		"/settings/google/callback":                       "",
	} {
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))

		if got := seen[path]; got != want {
			t.Errorf("%s resolved to domain %q, want %q", path, got, want)
		}
	}
}

// TestTheGoogleCallbackRefusesAForgedState is the cross-account write. The
// callback names no site in its path, so the gate in front of it proves only a
// session — and the state used to be nothing but a domain and a provider,
// which anybody could type. Replaying an authorisation code with somebody
// else's domain in the state would have written a Google grant into their
// account's database.
func TestTheGoogleCallbackRefusesAForgedState(t *testing.T) {
	handler, _ := newHandler(t)

	app, ok := google.NewApp("id", "secret", "https://example.com")
	if !ok {
		t.Fatal("the Google application would not build")
	}
	handler.Google = app
	handler.Role = func(*http.Request, sites.Site) teams.Role { return teams.RoleOwner }

	// The old shape: a domain and a provider, and nothing tying the request to
	// the browser that started it.
	response := get(t, handler, "/settings/google/callback?code=x&state=example.com%7Cga4")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("a two-field state answered %d, want it refused", response.Code)
	}

	// The right shape with somebody else's form token.
	response = get(t, handler, "/settings/google/callback?code=x&state=example.com%7Cga4%7Cguessed")
	if response.Code != http.StatusForbidden {
		t.Fatalf("a state carrying the wrong form token answered %d, want 403", response.Code)
	}

	// A provider we never issue must not become a stored grant nothing reads.
	response = get(t, handler, "/settings/google/callback?code=x&state=example.com%7Cmade-up%7Ctest-csrf")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("an unknown provider answered %d, want it refused", response.Code)
	}
}

// TestTheGoogleCallbackRefusesSomebodyWhoCannotConfigureTheSite is the second
// half: the form token proves the browser, and this proves the person behind it
// is allowed to configure the site the state names.
func TestTheGoogleCallbackRefusesSomebodyWhoCannotConfigureTheSite(t *testing.T) {
	handler, _ := newHandler(t)

	app, ok := google.NewApp("id", "secret", "https://example.com")
	if !ok {
		t.Fatal("the Google application would not build")
	}
	handler.Google = app

	// No role resolver at all is a handler that cannot answer the question, and
	// a handler that cannot answer it must not write the grant.
	response := get(t, handler, "/settings/google/callback?code=x&state=example.com%7Cga4%7Ctest-csrf")
	if response.Code != http.StatusForbidden {
		t.Fatalf("a callback with no role resolver answered %d, want 403", response.Code)
	}

	// A Viewer may read the dashboard and may not configure the site.
	handler.Role = func(*http.Request, sites.Site) teams.Role { return teams.RoleViewer }

	response = get(t, handler, "/settings/google/callback?code=x&state=example.com%7Cga4%7Ctest-csrf")
	if response.Code != http.StatusForbidden {
		t.Fatalf("a Viewer's callback answered %d, want 403", response.Code)
	}
}

// TestGoogleConnectBindsTheStateToTheBrowser pins the other end of the same
// contract: the state the customer is sent to Google with carries the form
// token, or the callback above can never accept anything.
func TestGoogleConnectBindsTheStateToTheBrowser(t *testing.T) {
	handler, _ := newHandler(t)

	app, ok := google.NewApp("id", "secret", "https://example.com")
	if !ok {
		t.Fatal("the Google application would not build")
	}
	handler.Google = app

	response := get(t, handler, "/settings/sites/example.com/google/connect?provider=search_console")
	if response.Code != http.StatusFound {
		t.Fatalf("connect answered %d, want a redirect to Google", response.Code)
	}

	target, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	if state := target.Query().Get("state"); state != "example.com|search_console|test-csrf" {
		t.Fatalf("state = %q, want the site, the provider and this browser's form token", state)
	}
}

// TestExportViewsHonourTheReadersClock covers the exports table on both dials.
// The "prepared" stamp carries a date and the "expires" stamp does not, so the
// two together check that the meridiem lands with the time and never after the
// timezone abbreviation.
func TestExportViewsHonourTheReadersClock(t *testing.T) {
	handler, _ := newHandler(t)

	prepared := time.Date(2026, 9, 4, 14, 30, 0, 0, time.UTC)
	exports := []dataio.Export{{
		ID:        1,
		Status:    dataio.StatusCompleted,
		CreatedAt: prepared.Unix(),
		ExpiresAt: prepared.Add(24 * time.Hour).Unix(),
	}}

	for _, test := range []struct {
		cycle        string
		wantPrepared string
		wantExpires  string
	}{
		{timefmt.Cycle24, "2026-09-04 14:30 UTC", "14:30 UTC"},
		{timefmt.Cycle12, "2026-09-04 2:30 PM UTC", "2:30 PM UTC"},
	} {
		t.Run(test.cycle, func(t *testing.T) {
			views := handler.exportViews("example.com", "en", test.cycle, exports)

			if len(views) != 1 {
				t.Fatalf("got %d export rows, want 1", len(views))
			}

			if views[0].Prepared != test.wantPrepared {
				t.Fatalf("prepared = %q, want %q", views[0].Prepared, test.wantPrepared)
			}

			if views[0].Expires != test.wantExpires {
				t.Fatalf("expires = %q, want %q", views[0].Expires, test.wantExpires)
			}
		})
	}
}

// TestNextRunHonoursTheReadersClock covers the reports badge. The weekly case
// is the one worth pinning: it returns exactly midnight, which is the boundary
// a 12-hour clock gets wrong by printing "0:00 AM".
func TestNextRunHonoursTheReadersClock(t *testing.T) {
	// A Wednesday, so the next weekly send is the following Monday and the next
	// monthly send is the first of October.
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name  string
		kind  string
		cycle string
		want  string
	}{
		{"weekly midnight on twenty-four", reports.KindWeekly, timefmt.Cycle24, "Mon 7 Sep 00:00 UTC"},
		{"weekly midnight on twelve", reports.KindWeekly, timefmt.Cycle12, "Mon 7 Sep 12:00 AM UTC"},
		{"monthly midnight on twenty-four", reports.KindMonthly, timefmt.Cycle24, "Thu 1 Oct 00:00 UTC"},
		{"monthly midnight on twelve", reports.KindMonthly, timefmt.Cycle12, "Thu 1 Oct 12:00 AM UTC"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nextRun(test.kind, "UTC", test.cycle, now); got != test.want {
				t.Fatalf("nextRun(%s, %s) = %q, want %q", test.kind, test.cycle, got, test.want)
			}
		})
	}
}

// TestStampHonoursTheReadersClock covers the delivery and admin table cells,
// including the empty case that must stay a dash rather than becoming 1970.
func TestStampHonoursTheReadersClock(t *testing.T) {
	at := time.Date(2026, 9, 4, 14, 30, 0, 0, time.UTC).Unix()

	if got := stamp(at, timefmt.Cycle24); got != "4 Sep 14:30 UTC" {
		t.Fatalf("stamp on twenty-four = %q", got)
	}

	if got := stamp(at, timefmt.Cycle12); got != "4 Sep 2:30 PM UTC" {
		t.Fatalf("stamp on twelve = %q", got)
	}

	if got := stamp(0, timefmt.Cycle12); got != "—" {
		t.Fatalf("an absent time stamped as %q, want a dash", got)
	}
}

// TestSavingARuleStampsTheAccount is what keeps every other process on the box
// applying the rule.
//
// This process pushes the new rules into its own snapshot immediately. The
// marker is how a second app shard and the ingest tier find out, and a missed
// stamp is the one failure that leaves them on the old rules for an hour with
// nothing to show for it.
func TestSavingARuleStampsTheAccount(t *testing.T) {
	handler, _, control := newHandlerWithControl(t)

	stamp := func() int64 {
		t.Helper()

		var at sql.NullInt64
		if err := control.QueryRow(
			"SELECT changed_at FROM account_rule_versions WHERE account_id = 1").Scan(&at); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0
			}

			t.Fatal(err)
		}

		return at.Int64
	}

	if stamp() != 0 {
		t.Fatal("the account was stamped before anything was saved")
	}

	// A moving clock, so the second save's marker is genuinely later rather
	// than equal, and a slice so the two run in a fixed order.
	tick := time.Unix(1_800_000_000, 0)
	handler.Now = func() time.Time {
		tick = tick.Add(time.Second)

		return tick
	}

	for _, save := range []struct {
		name    string
		request *http.Request
	}{
		{"a shield rule", postForm(t, "/settings/sites/example.com/shields/add",
			url.Values{"kind": {"page"}, "value": {"/admin*"}, "note": {""}})},
		{"a path rule", postForm(t, "/settings/sites/example.com/paths/trailing-slash", nil)},
	} {
		name, request := save.name, save.request

		before := stamp()

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusSeeOther {
			t.Fatalf("%s: the save answered %d", name, recorder.Code)
		}

		after := stamp()

		if after == 0 {
			t.Errorf("%s was saved without stamping the account", name)
		}

		if after <= before {
			t.Errorf("%s: the marker did not move (%d then %d)", name, before, after)
		}
	}
}

// postForm builds a form POST the settings handler will accept.
func postForm(t *testing.T, path string, values url.Values) *http.Request {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return request
}

// stubSearchProperties points the Google package at a local server answering
// with one owned property.
func stubSearchProperties(t *testing.T, body string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write sites response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	original := google.SearchAPI
	google.SearchAPI = server.URL
	t.Cleanup(func() { google.SearchAPI = original })
}

// connectSearchConsole stores a grant whose access token is still valid, so the
// tests below never reach the token endpoint.
func connectSearchConsole(t *testing.T, handler *Handler, manager *accounts.Manager, property string) {
	t.Helper()

	ctx := context.Background()
	now := handler.now()

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	connection := google.Connection{
		SiteID: 1, AccountID: 1, Provider: google.ProviderSearchConsole, Property: property,
		RefreshToken: "refresh", AccessToken: "valid",
		ExpiresAt: now.Add(time.Hour).Unix(), Status: google.StatusConnected,
	}

	if err := google.SaveConnection(ctx, account.Writer(), connection, now); err != nil {
		t.Fatal(err)
	}
}

// TestThePropertyPickerAppearsOnlyWhenItIsNeeded covers both halves of the
// setup. A grant with nothing chosen has to ask, and a configured one must not
// pay for a call to Google on every render of a screen with nothing left to do.
func TestThePropertyPickerAppearsOnlyWhenItIsNeeded(t *testing.T) {
	handler, manager := newHandler(t)
	handler.Google, _ = google.NewApp("id", "secret", "https://example.com")

	stubSearchProperties(t, `{"siteEntry":[{"siteUrl":"sc-domain:example.com","permissionLevel":"siteOwner"}]}`)
	connectSearchConsole(t, handler, manager, "")

	body := get(t, handler, "/settings/sites/example.com/imports").Body.String()

	if !strings.Contains(body, "sc-domain:example.com") {
		t.Fatal("a connection with no property chosen offers no picker")
	}

	connectSearchConsole(t, handler, manager, "sc-domain:example.com")

	body = get(t, handler, "/settings/sites/example.com/imports").Body.String()

	if strings.Contains(body, `name="property"`) {
		t.Fatal("the picker is still shown after a property was chosen")
	}
}

// TestAPropertyGoogleDidNotOfferIsRefused is the check between a hand-posted
// form and a connection that authorises cleanly and then imports nothing.
func TestAPropertyGoogleDidNotOfferIsRefused(t *testing.T) {
	ctx := context.Background()
	handler, manager := newHandler(t)
	handler.Google, _ = google.NewApp("id", "secret", "https://example.com")

	stubSearchProperties(t, `{"siteEntry":[{"siteUrl":"sc-domain:example.com","permissionLevel":"siteOwner"}]}`)
	connectSearchConsole(t, handler, manager, "")

	request := postForm(t, "/settings/sites/example.com/google/property",
		url.Values{"property": {"sc-domain:someone-else.test"}})

	handler.ServeHTTP(httptest.NewRecorder(), request)

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	stored, err := google.GetConnection(ctx, account.Reader(), 1, google.ProviderSearchConsole)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Property != "" {
		t.Fatalf("property = %q, want a property Google never offered refused", stored.Property)
	}
}

// TestChoosingAPropertyStartsTheBackfill is the whole point of the picker: the
// choice is saved and the history starts arriving without a second button.
func TestChoosingAPropertyStartsTheBackfill(t *testing.T) {
	ctx := context.Background()
	handler, manager := newHandler(t)
	handler.Google, _ = google.NewApp("id", "secret", "https://example.com")

	stubSearchProperties(t, `{"siteEntry":[{"siteUrl":"sc-domain:example.com","permissionLevel":"siteOwner"}]}`)
	connectSearchConsole(t, handler, manager, "")

	request := postForm(t, "/settings/sites/example.com/google/property",
		url.Values{"property": {"sc-domain:example.com"}})

	handler.ServeHTTP(httptest.NewRecorder(), request)

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	stored, err := google.GetConnection(ctx, account.Reader(), 1, google.ProviderSearchConsole)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Property != "sc-domain:example.com" {
		t.Fatalf("property = %q, want the chosen one saved", stored.Property)
	}

	records, err := dataio.ListImports(ctx, account.Reader(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 1 || records[0].Source != dataio.SourceSearchConsole {
		t.Fatalf("imports = %v, want one search backfill queued", records)
	}
}
