//
// tracker_test.go
// Tests for serving the tracker script.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package tracker

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSites is a routing map with a fixed domain list.
type fakeSites struct{ domains []string }

// Domains satisfies DomainSource.
func (f fakeSites) Domains() []string { return f.domains }

// newHandler builds a handler over a fixed set of domains and a fixed secret,
// so that a token in one test is the same token in the next.
func newHandler(domains ...string) *Handler {
	return New(bytes.Repeat([]byte{7}, SecretSize), fakeSites{domains: domains})
}

// get issues one request through a handler.
func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	return recorder
}

// TestBundleIsUnderBudget is the size check that does not need Node.
//
// The budget is the whole reason this tracker is written the way it is, and a
// limit only enforced by a JavaScript build is a limit that stops being
// enforced the moment somebody edits the bundle without running that build.
func TestBundleIsUnderBudget(t *testing.T) {
	var buf bytes.Buffer

	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Write(Script); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	t.Logf("tracker bundle: %d bytes raw, %d bytes gzipped, budget %d", len(Script), buf.Len(), SizeBudget)

	if buf.Len() > SizeBudget {
		t.Fatalf("the tracker is %d bytes gzipped, over the %d byte budget", buf.Len(), SizeBudget)
	}
}

// TestVitalsBundleIsUnderBudget is the second script's own ceiling.
//
// It is a separate budget rather than a shared one because the two scripts are
// paid for by different people: every site loads the tracker, and only a site
// that asked for vitals loads this. Adding them together would let one grow
// into the other's room.
func TestVitalsBundleIsUnderBudget(t *testing.T) {
	var buf bytes.Buffer

	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Write(VitalsScript); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	t.Logf("vitals bundle: %d bytes raw, %d bytes gzipped, budget %d",
		len(VitalsScript), buf.Len(), VitalsSizeBudget)

	if buf.Len() > VitalsSizeBudget {
		t.Fatalf("the vitals collector is %d bytes gzipped, over the %d byte budget", buf.Len(), VitalsSizeBudget)
	}
}

// TestVitalsScriptIsServed covers the route. It carries no baked configuration
// at all: it reports through the function the core script installed, so there
// is no domain of its own for it to know.
func TestVitalsScriptIsServed(t *testing.T) {
	recorder := get(t, newHandler("example.com"), PathVitals)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()

	if strings.HasPrefix(body, "window.__fsc=") {
		t.Fatal("the vitals collector must not carry a baked configuration")
	}

	if !strings.Contains(body, "largest-contentful-paint") {
		t.Fatal("the served bundle does not look like the vitals collector")
	}

	// The core script must not be served from this path, or a site would load
	// the tracker twice and count every pageview twice with it.
	if bytes.Equal([]byte(body), Script) {
		t.Fatal("the vitals path served the tracker bundle")
	}
}

// TestVitalsCollectorReportsThroughThePublicFunction is the contract that keeps
// this bundle small and keeps exclusions working: it has no transport of its
// own, so an excluded page silently sends nothing because the core script has
// already replaced the function with a no-op.
func TestVitalsCollectorReportsThroughThePublicFunction(t *testing.T) {
	body := string(VitalsScript)

	if !strings.Contains(body, "feasible") {
		t.Fatal("the vitals collector does not call the tracker's public function")
	}

	if strings.Contains(body, "fetch(") || strings.Contains(body, "XMLHttpRequest") {
		t.Fatal("the vitals collector has a transport of its own — it must send through the tracker")
	}
}

// TestBundleIsBuilt guards against an empty or placeholder asset, which would
// otherwise pass the budget test with flying colours.
func TestBundleIsBuilt(t *testing.T) {
	if len(Script) < 1000 {
		t.Fatalf("the embedded bundle is %d bytes — run `node tracker/build.js`", len(Script))
	}

	if !bytes.Contains(Script, []byte("feasible")) {
		t.Fatal("the embedded bundle does not look like the tracker")
	}
}

// TestLegacyScriptIsServedUnconfigured is the drop-in contract: the legacy path
// carries no baked configuration at all, because the whole point of it is that
// an existing snippet's own data-domain keeps working.
func TestLegacyScriptIsServedUnconfigured(t *testing.T) {
	recorder := get(t, newHandler("example.com"), PathLegacy)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}

	if strings.HasPrefix(recorder.Body.String(), "window.__fsc=") {
		t.Fatal("the legacy script must not carry a baked configuration")
	}

	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatalf("content type %q is not JavaScript", got)
	}
}

// TestPerSiteScriptBakesTheDomain covers the delivery mode that exists so a
// customer has no attribute to get wrong.
func TestPerSiteScriptBakesTheDomain(t *testing.T) {
	handler := newHandler("example.com")

	recorder := get(t, handler, handler.Keyer.Path("example.com"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}

	body := recorder.Body.String()

	if !strings.HasPrefix(body, `window.__fsc={"d":"example.com"};`) {
		t.Fatalf("the baked configuration is wrong: %q", body[:min(80, len(body))])
	}

	if !strings.Contains(body, "feasible") {
		t.Fatal("the bundle did not follow the configuration")
	}
}

// TestPerSiteOptionsAreInterpolated checks the settings that have nowhere else
// to live yet. Only the keys asked for are emitted, because an emitted zero
// would override the data attribute a customer added by hand.
func TestPerSiteOptionsAreInterpolated(t *testing.T) {
	handler := newHandler("example.com")

	recorder := get(t, handler, handler.Keyer.Path("example.com")+"?hash=1&exclude=/admin/**&manual=0")

	body := recorder.Body.String()

	for _, want := range []string{`"h":1`, `"x":"/admin/**"`, `"d":"example.com"`} {
		if !strings.Contains(body, want) {
			t.Errorf("baked configuration is missing %s: %q", want, body[:min(120, len(body))])
		}
	}

	if strings.Contains(body, `"m"`) {
		t.Error("manual=0 must not be baked at all, or it would override a data-manual attribute")
	}
}

// TestDomainIsJSONEscaped is the injection guard. A domain is customer input
// and it is concatenated in front of a script; the encoder is what keeps a
// quote in one from ending the string and running whatever follows.
func TestDomainIsJSONEscaped(t *testing.T) {
	nasty := `evil".com`
	handler := newHandler(nasty)

	recorder := get(t, handler, handler.Keyer.Path(nasty))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), `evil\".com`) {
		t.Fatalf("the domain was not escaped: %q", recorder.Body.String()[:60])
	}
}

// TestUnknownTokenIs404 covers a deleted site, a snippet copied between
// accounts and a rotated secret. Serving a working script for a token we cannot
// resolve would report events to nowhere and look like success.
func TestUnknownTokenIs404(t *testing.T) {
	handler := newHandler("example.com")

	for _, path := range []string{
		"/js/fs-aaaaaaaaaaaaaaaa.js",
		"/js/fs-.js",
		"/js/script.min.js",
		"/js/",
	} {
		if code := get(t, handler, path).Code; code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
}

// TestNotModified is the caching contract. The revalidation is answered here so
// that a self-hosted install with nothing in front of it behaves the same as
// one behind a CDN.
func TestNotModified(t *testing.T) {
	handler := newHandler("example.com")

	first := get(t, handler, PathLegacy)

	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag was set")
	}

	request := httptest.NewRequest(http.MethodGet, PathLegacy, nil)
	request.Header.Set("If-None-Match", etag)

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request)

	if second.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", second.Code)
	}
}

// TestEtagVariesByConfiguration stops two sites from sharing a cache entry,
// which would serve one customer's domain to another customer's visitors.
func TestEtagVariesByConfiguration(t *testing.T) {
	handler := newHandler("one.example", "two.example")

	first := get(t, handler, handler.Keyer.Path("one.example")).Header().Get("ETag")
	second := get(t, handler, handler.Keyer.Path("two.example")).Header().Get("ETag")

	if first == second {
		t.Fatal("two sites share an ETag")
	}
}

// TestMethodNotAllowed keeps the script a read-only asset.
func TestMethodNotAllowed(t *testing.T) {
	recorder := httptest.NewRecorder()
	newHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, PathLegacy, nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", recorder.Code)
	}
}
