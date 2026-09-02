//
// admin_test.go
// Every screen parses, renders, and refuses what the role does not allow.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// fixture is the whole settings surface over a temporary install.
type fixture struct {
	handler  *TeamHandler
	control  *sql.DB
	recorder *health.Recorder
	mail     *invitationCapture
	now      time.Time

	teamID int64
	owner  int64
	viewer int64
	siteID int64
	domain string
}

// invitationCapture records the one mail call the settings screen makes.
type invitationCapture struct {
	to, team, inviter, role, link string
	expires                       time.Time
}

// SendInvitation captures a delivery without exposing it to a logger.
func (c *invitationCapture) SendInvitation(_ context.Context, to, team, inviter, role, link string, expires time.Time) error {
	c.to = to
	c.team = team
	c.inviter = inviter
	c.role = role
	c.link = link
	c.expires = expires

	return nil
}

// newFixture builds and seeds everything the screens read.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	dir := t.TempDir()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}

	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.System()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &fixture{control: control, now: now, domain: "acme.example", mail: &invitationCapture{}}

	team, err := control.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', ?, ?)`,
		now.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}
	f.teamID, _ = team.LastInsertId()

	f.owner = f.user(t, "owner@example.com", "Anna Owner")
	f.viewer = f.user(t, "viewer@example.com", "Vic Viewer")

	f.member(t, f.owner, teams.RoleOwner)
	f.member(t, f.viewer, teams.RoleViewer)

	site, err := control.Exec(`
		INSERT INTO sites (account_id, domain, timezone, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
	`, f.teamID, f.domain, "America/New_York", now.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}
	f.siteID, _ = site.LastInsertId()

	cache := sites.New(control)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	f.recorder = health.NewRecorder(manager, cache, nil)
	f.recorder.Now = func() time.Time { return now }

	f.handler = NewTeamHandler(&TeamHandler{
		System:  control,
		Teams:   teams.NewStore(control),
		Sharing: sharing.NewStore(control),
		Reports: reports.NewStore(control),
		Health:  health.NewStore(manager, cache, control),
		Sites:   cache,
		Mail:    f.mail,
		BaseURL: "http://localhost:19300",
	})
	f.handler.Teams.Now = func() time.Time { return f.now }
	f.handler.Health.Now = func() time.Time { return f.now }

	return f
}

// user inserts a person.
func (f *fixture) user(t *testing.T, email, name string) int64 {
	t.Helper()

	result, err := f.control.Exec(`INSERT INTO users (email, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		email, name, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	id, _ := result.LastInsertId()

	return id
}

// member joins a person to the team.
func (f *fixture) member(t *testing.T, userID int64, role teams.Role) {
	t.Helper()

	if _, err := f.control.Exec(`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		f.teamID, userID, string(role), f.now.Unix()); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
}

// as makes the handler act as one user.
func (f *fixture) as(userID int64) {
	f.handler.Identify = func(*http.Request) (Identity, error) {
		return Identity{UserID: userID, TeamID: f.teamID}, nil
	}
}

// get fetches one screen.
func (f *fixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	return recorder
}

// post submits one form.
func (f *fixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)

	return recorder
}

// TestEveryScreenRenders is the smoke test that a template which will not
// execute is caught here rather than by a customer.
func TestEveryScreenRenders(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	for _, path := range []string{
		"/settings/members",
		"/settings/sites/acme.example/sharing",
		"/settings/sites/acme.example/reports",
		"/settings/sites/acme.example/health",
	} {
		recorder := f.get(t, path)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", path, recorder.Code, recorder.Body.String())
		}

		body := recorder.Body.String()

		if !strings.Contains(body, "</html>") {
			t.Fatalf("%s did not render a complete page", path)
		}

		// html/template stops at the first undefined field, so a truncated page
		// is how a template failure shows up. The layout's closing tag being
		// present is what proves execution reached the end.
		if strings.Contains(body, "<no value>") {
			t.Fatalf("%s rendered <no value>", path)
		}
	}
}

// TestTheTeamScreenSaysWhoCannotCreateKeys checks that the matrix is on the
// screen rather than only in the code.
func TestTheTeamScreenSaysWhoCannotCreateKeys(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	body := f.get(t, "/settings/members").Body.String()

	if !strings.Contains(body, "Cannot create") {
		t.Fatal("the team screen does not show that a Viewer cannot create API keys")
	}

	if !strings.Contains(body, "unlimited team members") {
		t.Fatal("the team screen does not state the unlimited-members promise")
	}

	if !strings.Contains(body, "48 hours") {
		t.Fatal("the team screen does not state the invitation expiry")
	}
}

// TestTheSharingScreenStatesTheThreeRules checks that the traps are documented
// where somebody building an embed will read them.
func TestTheSharingScreenStatesTheThreeRules(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	body := f.get(t, "/settings/sites/acme.example/sharing").Body.String()

	for _, want := range []string{
		// Embed parameters only work on a share URL.
		"only work on a share or public URL",
		// A password-protected link cannot be embedded, and this is not a setting.
		"cannot be embedded, and this is not a setting",
		// The ad-block limitation, stated honestly.
		"filter lists block embedded dashboards",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the sharing screen does not say %q", want)
		}
	}
}

// TestAViewerCannotReachSiteSettings checks that dashboard-only roles cannot
// read configuration or mutate it by posting directly.
func TestAViewerCannotChangeSiteSettings(t *testing.T) {
	f := newFixture(t)
	f.as(f.viewer)

	if code := f.get(t, "/settings/sites/acme.example/sharing").Code; code != http.StatusForbidden {
		t.Fatalf("a viewer read the sharing screen: %d", code)
	}

	// But not act on them.
	for _, action := range []struct{ path, body string }{
		{"/settings/sites/acme.example/sharing/public", "public=1"},
		{"/settings/sites/acme.example/sharing/links", "name=x"},
		{"/settings/sites/acme.example/reports/alert", "kind=spike&threshold=10"},
		{"/settings/sites/acme.example/health/allow", "hostname=other.example"},
	} {
		if code := f.post(t, action.path, action.body).Code; code != http.StatusForbidden {
			t.Errorf("a viewer's POST to %s answered %d, want 403", action.path, code)
		}
	}
}

// TestInvitationFormDeliversMail checks the end of the settings flow that used
// to stop after writing a bearer URL to the application log.
func TestInvitationFormDeliversMail(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	recorder := f.post(t, "/settings/members/invite",
		"team_id="+strconv.FormatInt(f.teamID, 10)+"&email=new%40example.com&role=editor")
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("invite answered %d: %s", recorder.Code, recorder.Body.String())
	}

	if f.mail.to != "new@example.com" || f.mail.team != "Acme" || f.mail.role != "Editor" {
		t.Fatalf("wrong invitation delivery: %+v", f.mail)
	}
	if !strings.HasPrefix(f.mail.link, "http://localhost:19300/invitations/") {
		t.Fatalf("invitation did not carry its redemption link: %q", f.mail.link)
	}
	if got := f.mail.expires.Sub(f.now); got != teams.InvitationTTL {
		t.Fatalf("mail expiry = %s, want %s", got, teams.InvitationTTL)
	}
}

// TestMakingASitePublicShowsItsStableURL checks the round trip through the form.
func TestMakingASitePublicShowsItsStableURL(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	recorder := f.post(t, "/settings/sites/acme.example/sharing/public", "public=1")

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("the form answered %d: %s", recorder.Code, recorder.Body.String())
	}

	// The notice travels in the query string, so the URL inside it is escaped.
	if !strings.Contains(recorder.Header().Get("Location"), "public%2Facme.example") {
		t.Fatalf("the notice does not carry the public URL: %s", recorder.Header().Get("Location"))
	}

	body := f.get(t, "/settings/sites/acme.example/sharing").Body.String()

	if !strings.Contains(body, "http://localhost:19300/public/acme.example") {
		t.Fatal("the sharing screen does not show the public URL")
	}
}

// TestAProtectedLinkIsNotOfferedAsAnEmbedSnippet checks that the screen never
// hands somebody a snippet that would render a refusal page.
func TestAProtectedLinkIsNotOfferedAsAnEmbedSnippet(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	if code := f.post(t, "/settings/sites/acme.example/sharing/links", "name=client&password=hunter2").Code; code != http.StatusSeeOther {
		t.Fatalf("creating a protected link answered %d", code)
	}

	var passwordHash, passwordSalt string
	if err := f.control.QueryRow(`
		SELECT password_hash, password_salt FROM shared_links WHERE site_id = ? AND name = 'client'
	`, f.siteID).Scan(&passwordHash, &passwordSalt); err != nil {
		t.Fatal(err)
	}
	if passwordHash == "" || passwordSalt == "" {
		t.Fatalf("settings stored an unsalted protected link: hash/salt %q/%q", passwordHash, passwordSalt)
	}

	body := f.get(t, "/settings/sites/acme.example/sharing").Body.String()

	if strings.Contains(body, "&lt;iframe") || strings.Contains(body, "<iframe") {
		t.Fatal("an embed snippet was offered when the only link is password protected")
	}

	if !strings.Contains(body, "No — see below") {
		t.Fatal("the link is not marked as unembeddable")
	}

	// An open link does get a snippet.
	if code := f.post(t, "/settings/sites/acme.example/sharing/links", "name=open").Code; code != http.StatusSeeOther {
		t.Fatalf("creating an open link answered %d", code)
	}

	body = f.get(t, "/settings/sites/acme.example/sharing").Body.String()

	if !strings.Contains(body, "iframe") {
		t.Fatal("an open link did not produce an embed snippet")
	}
}

// TestTheReportsScreenSaysWhenTheNextOneGoesOut checks that "Monday 00:00" is
// resolved into the site's own calendar rather than left as a phrase.
func TestTheReportsScreenSaysWhenTheNextOneGoesOut(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	body := f.get(t, "/settings/sites/acme.example/reports").Body.String()

	if !strings.Contains(body, "America/New_York") {
		t.Fatal("the reports screen does not name the site's timezone")
	}

	if !strings.Contains(body, "Mon ") {
		t.Fatal("the reports screen does not resolve the next weekly run to a date")
	}

	if !strings.Contains(body, "two alerts are sent per site per day") {
		t.Fatal("the reports screen does not state the rate limit")
	}
}

// TestSavingASubscriptionRoundTrips checks the form.
func TestSavingASubscriptionRoundTrips(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	recorder := f.post(t, "/settings/sites/acme.example/reports/subscription",
		"kind=weekly&enabled=1&recipients=anna%40example.com%2C+sam%40example.com")

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("the form answered %d", recorder.Code)
	}

	body := f.get(t, "/settings/sites/acme.example/reports").Body.String()

	if !strings.Contains(body, "anna@example.com, sam@example.com") {
		t.Fatal("the saved recipients are not on the screen")
	}
}

// TestABadAddressComesBackAsAProblemNotACrash checks the error path.
func TestABadAddressComesBackAsAProblemNotACrash(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	recorder := f.post(t, "/settings/sites/acme.example/reports/subscription",
		"kind=weekly&enabled=1&recipients=not-an-address")

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("a bad address answered %d", recorder.Code)
	}

	if !strings.Contains(recorder.Header().Get("Location"), "err=") {
		t.Fatalf("a bad address produced no problem message: %s", recorder.Header().Get("Location"))
	}
}

// TestTheHealthScreenShowsTheResolvedAddressSection checks that the debug
// output a customer would otherwise produce with curl is on the page.
func TestTheHealthScreenShowsTheResolvedAddressSection(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	// One real request, so the screen has a derived event to show rather than
	// the empty state.
	f.recorder.Observe(ingest.Observation{
		SiteID:    f.siteID,
		AccountID: f.teamID,
		Accepted:  true,
		UserAgent: "Mozilla/5.0 (a browser)",
		Debug: ingest.Debug{
			Domain:         f.domain,
			ClientIP:       "203.0.113.9",
			ClientIPSource: ingest.SourceForwardedFor,
			TrustedProxy:   true,
			Hostname:       f.domain,
			Pathname:       "/pricing",
		},
	})

	if _, err := f.recorder.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	body := f.get(t, "/settings/sites/acme.example/health").Body.String()

	for _, want := range []string{
		"Resolved client IP",
		"203.0.113.9",
		"Read from",
		ingest.SourceForwardedFor,
		"Send a test event",
		"Hostname allow-list",
		"Dropped events, by reason",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the health screen is missing %q", want)
		}
	}
}

// TestASettingsPageIsNeverFramable checks the header on the screens that hold
// an account's administration.
func TestASettingsPageIsNeverFramable(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	recorder := f.get(t, "/settings/members")

	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("the settings screens are framable, which is how somebody is tricked into pressing Remove")
	}

	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a settings page is cacheable, so a shared machine keeps the last person's team on screen")
	}
}

// TestANewAPIKeyIsShownOnceAndNotPutInTheURL checks how the secret travels.
func TestANewAPIKeyIsShownOnceAndNotPutInTheURL(t *testing.T) {
	f := newFixture(t)
	f.as(f.owner)

	created := f.post(t, "/settings/members/api-keys", "name=reporting")

	if created.Code != http.StatusSeeOther {
		t.Fatalf("creating a key answered %d", created.Code)
	}

	if strings.Contains(created.Header().Get("Location"), teams.KeyPrefix) {
		t.Fatal("the key was put in the redirect's query string, where it lands in browser history and access logs")
	}

	cookies := created.Result().Cookies()
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0].Value, teams.KeyPrefix) {
		t.Fatalf("the key did not travel in a cookie: %+v", cookies)
	}

	// The key is shown exactly once: the page that renders it clears the cookie.
	request := httptest.NewRequest(http.MethodGet, "/settings/members", nil)
	request.AddCookie(cookies[0])

	first := httptest.NewRecorder()
	f.handler.ServeHTTP(first, request)

	if !strings.Contains(first.Body.String(), cookies[0].Value) {
		t.Fatal("the new key was not shown at all")
	}

	cleared := first.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("the key cookie was not cleared: %+v", cleared)
	}

	second := f.get(t, "/settings/members")
	if strings.Contains(second.Body.String(), cookies[0].Value) {
		t.Fatal("the key is still shown on a second load")
	}
}

// TestAgoReadsAsAPhrase checks the two shapes on these screens.
func TestAgoReadsAsAPhrase(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	cases := map[time.Duration]string{
		41 * time.Hour:   "in 41 hours",
		-3 * time.Hour:   "3 hours ago",
		30 * time.Second: "in less than a minute",
		-90 * time.Hour:  "3 days ago",
		time.Hour:        "in 1 hour",
	}

	for delta, want := range cases {
		if got := ago(now.Add(delta), now); got != want {
			t.Errorf("ago(%v) = %q, want %q", delta, got, want)
		}
	}
}

// TestAnInstallWithNoTeamAnswers404 checks the identity seam's failure mode.
func TestAnInstallWithNoTeamAnswers404(t *testing.T) {
	f := newFixture(t)

	if _, err := f.control.Exec(`DELETE FROM team_memberships`); err != nil {
		t.Fatalf("clear memberships: %v", err)
	}

	f.handler.Identify = FirstTeamOwner(f.control)

	if code := f.get(t, "/settings/members").Code; code != http.StatusNotFound {
		t.Fatalf("an install with no owner answered %d", code)
	}
}
