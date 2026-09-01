//
// dashboard_test.go
// Tests for serving the embedded dashboard.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
)

// fakeSites is a routing map with a fixed answer.
type fakeSites struct {
	domains []string
}

// Domains satisfies DomainSource.
func (f fakeSites) Domains() []string { return f.domains }

// get runs one request through a handler and returns the recorder.
func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	return w
}

// TestAssetsAreEmbedded is the check that the front-end build actually ran. A
// binary that starts and then serves a blank dashboard is the one failure mode
// of go:embed that produces no error anywhere, so it gets its own test.
func TestAssetsAreEmbedded(t *testing.T) {
	names := AssetNames()

	for _, want := range []string{"app.css", "app.js", "index.html"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}

		if !found {
			t.Fatalf("%s is not embedded; assets are %v — run `make assets`", want, names)
		}
	}
}

// TestShellCarriesTheSiteList covers the bootstrap. The site picker cannot ask
// a question before it knows which sites exist, so the list is written into the
// page rather than fetched, and an empty blob would render the "no sites" screen
// on an install that has some.
func TestShellCarriesTheSiteList(t *testing.T) {
	h := New(fakeSites{domains: []string{"two.example", "one.example"}})

	body := get(t, h, "/dashboard/one.example").Body.String()

	// Sorted, so that two loads of the same install produce the same document.
	if !strings.Contains(body, `{"sites":["one.example","two.example"]`) {
		t.Fatalf("the shell does not carry a sorted site list: %s", body)
	}

	if strings.Contains(body, bootstrapPlaceholder) {
		t.Fatal("the bootstrap placeholder survived into the response")
	}
}

// TestAuthenticatedBootstrapCarriesNavigationAndLock proves the shell can
// render recovery context before the client mounts any report queries.
func TestAuthenticatedBootstrapCarriesNavigationAndLock(t *testing.T) {
	h := New(nil)
	h.Resolve = func(_ http.ResponseWriter, _ *http.Request) Bootstrap {
		return Bootstrap{
			Sites: []string{"locked.example"},
			Navigation: &Navigation{
				Email: "owner@example.com", SitesURL: "/sites?team_id=7",
				BillingURL: "/billing?team=7", LogoutURL: "/logout", CSRF: "token",
			},
			Lock: &Lock{Reason: "lifecycle", Error: "Your dashboard is locked."},
		}
	}

	body := get(t, h, "/dashboard/locked.example").Body.String()
	for _, want := range []string{`"navigation":{`, `"billing_url":"/billing?team=7"`, `"lock":{"reason":"lifecycle"`} {
		if !strings.Contains(body, want) {
			t.Errorf("authenticated bootstrap is missing %s: %s", want, body)
		}
	}
}

// TestShellHandlesNoSites checks the empty install. A nil source is what a
// process with no database has, and it must render rather than panic.
func TestShellHandlesNoSites(t *testing.T) {
	body := get(t, New(nil), "/dashboard/").Body.String()

	if !strings.Contains(body, `{"sites":[]`) {
		t.Fatalf("an install with no sites did not render an empty list: %s", body)
	}
}

// TestShellCarriesTheCatalogue checks that the strings travel with the page.
//
// The dashboard has no catalogue of its own and no fallback logic: every id it
// can ask for has to be in the map the server wrote, already merged over
// English. A shell that shipped without it would render an interface of raw
// message ids, with a 200 and nothing in any log.
func TestShellCarriesTheCatalogue(t *testing.T) {
	body := get(t, New(nil), "/dashboard/").Body.String()

	if !strings.Contains(body, `"locale":"`+i18n.DefaultLocale+`"`) {
		t.Fatalf("the shell does not name the locale it was rendered in: %s", body)
	}

	// One id that exists in the shared catalogue is enough: the question is
	// whether the map was written at all, not whether it is complete, which the
	// i18n package's own tests answer.
	if !strings.Contains(body, `"common.action.save":`) {
		t.Fatalf("the shell does not carry the message catalogue: %s", body)
	}
}

// TestShellRemembersAnExplicitLanguage covers the ?lang= override.
//
// A language switcher that works once and then reverts on the next page is the
// commonest way this feature ships broken: the parameter is gone from the URL
// and nothing wrote down what it said.
func TestShellRemembersAnExplicitLanguage(t *testing.T) {
	response := get(t, New(nil), "/dashboard/?lang=en")

	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == i18n.CookieName && cookie.Value == "en" {
			return
		}
	}

	t.Fatalf("an explicit ?lang= did not set the %s cookie: %v", i18n.CookieName, response.Result().Cookies())
}

// TestShellReferencesHashedAssets is what makes the immutable cache lifetime
// safe: a new build has to be a new URL, or a browser holding last week's bundle
// against this week's shell would render a dashboard nobody can reproduce.
func TestShellReferencesHashedAssets(t *testing.T) {
	h := New(fakeSites{})
	body := get(t, h, "/dashboard/").Body.String()

	for _, name := range []string{"app.js", "app.css"} {
		want := AssetPrefix + name + "?v=" + h.files[name].digest

		if !strings.Contains(body, want) {
			t.Fatalf("the shell does not reference %s: %s", want, body)
		}
	}

	if strings.Contains(body, "__JS__") || strings.Contains(body, "__CSS__") {
		t.Fatal("an asset placeholder survived into the response")
	}
}

// TestClientRoutesRenderTheShell covers the SPA's own routing. Every path under
// the prefix is a page the client knows how to draw, so a 404 here would break
// every shared link the moment somebody typed a site name into it.
func TestClientRoutesRenderTheShell(t *testing.T) {
	h := New(fakeSites{domains: []string{"one.example"}})

	for _, target := range []string{
		"/dashboard/",
		"/dashboard/one.example",
		"/dashboard/one.example?period=7d&details=sources:channels",
		"/dashboard/anything/at/all",
	} {
		w := get(t, h, target)

		if w.Code != http.StatusOK {
			t.Errorf("%s answered %d, want 200", target, w.Code)
		}

		if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s answered %q, want HTML", target, w.Header().Get("Content-Type"))
		}
	}
}

// TestVersionedAssetIsImmutable covers the two cache lifetimes. The digest in
// the query string is the whole basis for holding an asset for a year, so an
// unversioned request must not get the same promise.
func TestVersionedAssetIsImmutable(t *testing.T) {
	h := New(fakeSites{})
	digest := h.files["app.js"].digest

	versioned := get(t, h, AssetPrefix+"app.js?v="+digest)
	if got := versioned.Header().Get("Cache-Control"); got != assetCacheControl {
		t.Errorf("versioned asset answered Cache-Control %q, want %q", got, assetCacheControl)
	}

	bare := get(t, h, AssetPrefix+"app.js")
	if got := bare.Header().Get("Cache-Control"); got != unversionedCacheControl {
		t.Errorf("unversioned asset answered Cache-Control %q, want %q", got, unversionedCacheControl)
	}

	stale := get(t, h, AssetPrefix+"app.js?v=notthedigest")
	if got := stale.Header().Get("Cache-Control"); got != unversionedCacheControl {
		t.Errorf("a wrong digest answered Cache-Control %q, want %q", got, unversionedCacheControl)
	}
}

// TestAssetRevalidates covers the conditional request the unversioned path makes
// every minute. Without it, a bookmarked asset URL costs a full download a
// minute for as long as the tab is open.
func TestAssetRevalidates(t *testing.T) {
	h := New(fakeSites{})

	request := httptest.NewRequest(http.MethodGet, AssetPrefix+"app.css", nil)
	request.Header.Set("If-None-Match", `"`+h.files["app.css"].digest+`"`)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, request)

	if w.Code != http.StatusNotModified {
		t.Fatalf("a matching ETag answered %d, want 304", w.Code)
	}

	if w.Body.Len() != 0 {
		t.Fatalf("a 304 carried %d bytes of body", w.Body.Len())
	}
}

// TestUnknownAssetIs404 keeps the asset directory from becoming a file server.
// The shell fallback is for client routes, not for anything somebody appends to
// the asset prefix.
func TestUnknownAssetIs404(t *testing.T) {
	if code := get(t, New(fakeSites{}), AssetPrefix+"secrets.env").Code; code != http.StatusNotFound {
		t.Fatalf("an unknown asset answered %d, want 404", code)
	}
}

// TestNonGetIsRefused covers the method check. The dashboard is a document, and
// a POST to it is either a mistake or a probe.
func TestNonGetIsRefused(t *testing.T) {
	w := httptest.NewRecorder()
	New(fakeSites{}).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/dashboard/", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST answered %d, want 405", w.Code)
	}

	if w.Header().Get("Allow") == "" {
		t.Error("a 405 without an Allow header tells the caller nothing")
	}
}

// TestShellIsNotFramable covers the clickjacking header. The dashboard renders
// one account's traffic and carries destructive controls, so being embedded in
// somebody else's page is never a legitimate use.
func TestShellIsNotFramable(t *testing.T) {
	w := get(t, New(fakeSites{}), "/dashboard/")

	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options is %q, want DENY", got)
	}

	if got := w.Header().Get("Cache-Control"); got != shellCacheControl {
		t.Fatalf("the shell may be cached: %q — it carries the site list", got)
	}
}
