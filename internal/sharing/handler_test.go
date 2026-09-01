//
// handler_test.go
// Serving a dashboard to somebody with no account, and refusing the one combination we will not build.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/dashboard"
)

// recordingShell captures the bootstrap the handler would have rendered, so a
// test can assert on what the front end is told without needing the compiled
// bundle.
type recordingShell struct {
	boot dashboard.Bootstrap
	seen bool
}

// WriteShell records the bootstrap and writes a stub page.
func (r *recordingShell) WriteShell(w http.ResponseWriter, _ *http.Request, boot dashboard.Bootstrap) {
	r.boot = boot
	r.seen = true

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	body, _ := json.Marshal(boot)
	_, _ = w.Write(body)
}

// newHandler wires a handler over the store fixture.
func newHandler(t *testing.T, baseURL string) (*Handler, *fixture, *recordingShell) {
	t.Helper()

	f := newFixture(t)
	shell := &recordingShell{}

	return New(f.store, shell, NewSecurity(baseURL), DeriveSecret([]byte("root")), nil), f, shell
}

// TestAnEmbedOfAPasswordProtectedLinkIsRefused is the trap this package was
// built around.
//
// The response must be a refusal *and* must stay unframable. Serving the
// password form inside a third-party frame is the thing that makes it
// clickjackable, so the refusal cannot itself be framable either.
func TestAnEmbedOfAPasswordProtectedLinkIsRefused(t *testing.T) {
	handler, f, shell := newHandler(t, "http://localhost:19300")

	link, err := f.store.CreateLink(context.Background(), f.siteID, "client", "hunter2", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, link.Path()+"?embed=true", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an embed of a protected link answered %d, want 400", recorder.Code)
	}

	if shell.seen {
		t.Fatal("the dashboard was rendered for an embed of a protected link")
	}

	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("the refusal page is framable, which is the whole problem it exists to avoid")
	}

	body := recorder.Body.String()

	for _, want := range []string{"cannot be embedded", "clickjack", "second shared link"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("the refusal does not explain %q", want)
		}
	}
}

// TestAProtectedLinkAsksForItsPasswordAndThenRenders drives the whole gate.
func TestAProtectedLinkAsksForItsPasswordAndThenRenders(t *testing.T) {
	handler, f, shell := newHandler(t, "http://localhost:19300")

	link, err := f.store.CreateLink(context.Background(), f.siteID, "client", "hunter2", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	gate := httptest.NewRecorder()
	handler.ServeHTTP(gate, httptest.NewRequest(http.MethodGet, link.Path(), nil))

	if shell.seen {
		t.Fatal("a protected link rendered the dashboard without a password")
	}

	if !strings.Contains(gate.Body.String(), "This dashboard is protected") {
		t.Fatalf("the password form was not rendered: %s", gate.Body.String())
	}

	// The wrong password comes back as a form with a reason, not a 500.
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, formRequest(link.Path()+"/password", "password=nope"))

	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password answered %d, want 401", wrong.Code)
	}

	if !strings.Contains(wrong.Body.String(), "not correct") {
		t.Fatal("the wrong-password form does not say what went wrong")
	}

	// The right password sets a cookie and redirects back to the link.
	solved := httptest.NewRecorder()
	handler.ServeHTTP(solved, formRequest(link.Path()+"/password", "password=hunter2"))

	if solved.Code != http.StatusSeeOther {
		t.Fatalf("the right password answered %d, want 303", solved.Code)
	}

	cookies := solved.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies were set", len(cookies))
	}

	if !cookies[0].HttpOnly {
		t.Error("the share cookie is readable from JavaScript for no reason")
	}

	// And now the dashboard renders.
	final := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, link.Path(), nil)
	request.AddCookie(cookies[0])

	handler.ServeHTTP(final, request)

	if !shell.seen {
		t.Fatal("the dashboard did not render for a solved password")
	}
}

// TestPasswordGateReturns429AfterTheSourceLinkBudget checks the HTTP boundary
// exposes a bounded retry response without hiding the link as missing.
func TestPasswordGateReturns429AfterTheSourceLinkBudget(t *testing.T) {
	handler, f, _ := newHandler(t, "http://localhost:19300")
	link, err := f.store.CreateLink(context.Background(), f.siteID, "client", "hunter2", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < PasswordAttemptLimit; attempt++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, formRequest(link.Path()+"/password", "password=wrong"))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", attempt, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, formRequest(link.Path()+"/password", "password=wrong"))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "900" {
		t.Fatalf("throttled status/retry = %d/%q", response.Code, response.Header().Get("Retry-After"))
	}
}

// TestTheBootstrapCarriesTheSharePathSoFiltersKeepIt is the fix for a real bug.
//
// The incumbent's shared dashboard built its URLs against its own dashboard
// path, so applying a filter dropped the /share/<token> segment and produced a
// link that redirected to a login and back forever.
func TestTheBootstrapCarriesTheSharePathSoFiltersKeepIt(t *testing.T) {
	handler, f, shell := newHandler(t, "http://localhost:19300")

	link, err := f.store.CreateLink(context.Background(), f.siteID, "open", "", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, link.Path()+"?period=7d", nil))

	if !shell.seen || shell.boot.Shared == nil {
		t.Fatal("the shell was not rendered in share mode")
	}

	if shell.boot.Shared.Base != link.Path() {
		t.Fatalf("the front end was given base %q, want %q", shell.boot.Shared.Base, link.Path())
	}

	if shell.boot.Shared.Domain != f.domain {
		t.Fatalf("the bootstrap names domain %q", shell.boot.Shared.Domain)
	}

	if len(shell.boot.Sites) != 1 {
		t.Fatalf("a shared link exposed %d sites, want 1", len(shell.boot.Sites))
	}
}

// TestAnEmbedTellsTheFrontEndNotToTouchStorage is the other trap.
//
// In a third-party frame with storage blocked — Brave by default, and any
// browser with third-party cookies off — a storage accessor throws rather than
// returning null. An unguarded read killed an incumbent's entire embed.
func TestAnEmbedTellsTheFrontEndNotToTouchStorage(t *testing.T) {
	handler, f, shell := newHandler(t, "http://localhost:19300")

	link, err := f.store.CreateLink(context.Background(), f.siteID, "open", "", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, link.Path()+"?embed=true&theme=dark&background=%23112233", nil))

	shared := shell.boot.Shared
	if shared == nil {
		t.Fatal("the shell was not rendered in share mode")
	}

	if !shared.Embed {
		t.Fatal("the embed parameter was not honoured on a share URL")
	}

	if shared.Storage {
		t.Fatal("an embed was told it may use localStorage")
	}

	if shared.Theme != "dark" {
		t.Fatalf("the theme parameter came through as %q", shared.Theme)
	}

	if shared.Background != "#112233" {
		t.Fatalf("the background parameter came through as %q", shared.Background)
	}

	if recorder.Header().Get("X-Frame-Options") != "" {
		t.Fatal("an embeddable link kept X-Frame-Options, so the iframe will be blank")
	}

	if !strings.Contains(recorder.Header().Get("Content-Security-Policy"), "frame-ancestors *") {
		t.Fatal("an embeddable link did not open frame-ancestors")
	}
}

// TestANonEmbeddedShareKeepsStorageAndChrome checks that a shared link opened
// as a page in its own right is not stripped down like an embed.
func TestANonEmbeddedShareKeepsStorageAndChrome(t *testing.T) {
	handler, f, shell := newHandler(t, "http://localhost:19300")

	link, err := f.store.CreateLink(context.Background(), f.siteID, "open", "", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, link.Path(), nil))

	if shell.boot.Shared.Embed {
		t.Fatal("a plain share URL was treated as an embed")
	}

	if !shell.boot.Shared.Storage {
		t.Fatal("a plain share URL was told not to use storage")
	}

	if recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("a non-embedded share is framable")
	}
}

// TestAPublicSiteServesWithoutAToken checks the stable public URL.
func TestAPublicSiteServesWithoutAToken(t *testing.T) {
	handler, f, shell := newHandler(t, "http://localhost:19300")
	ctx := context.Background()

	before := httptest.NewRecorder()
	handler.ServeHTTP(before, httptest.NewRequest(http.MethodGet, PublicPrefix+f.domain, nil))

	if before.Code != http.StatusNotFound {
		t.Fatalf("a private site answered %d on the public URL", before.Code)
	}

	if err := f.store.SetPublic(ctx, f.siteID, true); err != nil {
		t.Fatalf("set public: %v", err)
	}

	after := httptest.NewRecorder()
	handler.ServeHTTP(after, httptest.NewRequest(http.MethodGet, PublicPrefix+f.domain, nil))

	if after.Code != http.StatusOK || !shell.seen {
		t.Fatalf("a public site answered %d", after.Code)
	}

	if shell.boot.Shared.Mode != "public" {
		t.Fatalf("the bootstrap mode is %q", shell.boot.Shared.Mode)
	}

	if shell.boot.Shared.Base != PublicPrefix+f.domain {
		t.Fatalf("the public base is %q", shell.boot.Shared.Base)
	}
}

// TestAnUnknownLinkAnswersTheSameAsAPrivateOne checks that the endpoint cannot
// be used to work out which slugs exist.
func TestAnUnknownLinkAnswersTheSameAsAPrivateOne(t *testing.T) {
	handler, _, _ := newHandler(t, "http://localhost:19300")

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/share/does-not-exist", nil))

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, PublicPrefix+"nobody.example", nil))

	if first.Code != http.StatusNotFound || second.Code != http.StatusNotFound {
		t.Fatalf("codes were %d and %d, want two 404s", first.Code, second.Code)
	}

	if first.Body.String() != second.Body.String() {
		t.Fatal("a missing link and a private site answer differently")
	}
}

// TestThePasswordEndpointOnlyTakesPOST checks that the form cannot be triggered
// by a link somebody was sent.
func TestThePasswordEndpointOnlyTakesPOST(t *testing.T) {
	handler, f, _ := newHandler(t, "http://localhost:19300")

	link, err := f.store.CreateLink(context.Background(), f.siteID, "client", "hunter2", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, link.Path()+"/password", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the password endpoint answered %d", recorder.Code)
	}
}

// formRequest builds a form POST.
func formRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return request
}
