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
	"os"
	"path/filepath"
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

// TestBundlesAreUnderTheirBudgets is the size check that does not need Node.
//
// The primary contract applies only to bytes every site downloads. Keeping the
// optional module under its own ceiling prevents either artifact from borrowing
// budget from the other and hiding a base-size regression.
func TestBundlesAreUnderTheirBudgets(t *testing.T) {
	for _, bundle := range []struct {
		name   string
		script []byte
		budget int
	}{
		{name: "base", script: Script, budget: BaseSizeBudget},
		{name: "vitals", script: VitalsScript, budget: VitalsSizeBudget},
	} {
		t.Run(bundle.name, func(t *testing.T) {
			compressed := gzipSize(t, bundle.script)
			t.Logf("%s bundle: %d bytes raw, %d bytes gzipped, budget %d",
				bundle.name, len(bundle.script), compressed, bundle.budget)
			if compressed >= bundle.budget {
				t.Fatalf("the %s bundle is %d bytes gzipped, not under the %d byte budget",
					bundle.name, compressed, bundle.budget)
			}
		})
	}
}

// gzipSize returns one artifact's best-compression wire size.
func gzipSize(t *testing.T, script []byte) int {
	t.Helper()

	var buf bytes.Buffer

	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Write(script); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Len()
}

// TestBundlesAreBuilt guards against empty or placeholder assets, which would
// otherwise pass the budget test with flying colours.
func TestBundlesAreBuilt(t *testing.T) {
	if len(Script) < 1000 || len(VitalsScript) < 1000 {
		t.Fatalf("embedded bundle sizes are base=%d vitals=%d; run `node tracker/build.js`",
			len(Script), len(VitalsScript))
	}

	if !bytes.Contains(Script, []byte("feasible")) {
		t.Fatal("the embedded bundle does not look like the tracker")
	}
	if bytes.Contains(Script, []byte("largest-contentful-paint")) {
		t.Fatal("the always-loaded base contains the Web Vitals implementation")
	}
	if !bytes.Contains(VitalsScript, []byte("largest-contentful-paint")) {
		t.Fatal("the optional module does not contain the maintained Web Vitals implementation")
	}
}

// TestGeneratedArtifactsMatchEmbedded prevents a manual edit to either copy.
// The browser fixture serves tracker/dist while production serves go:embed, so
// byte identity is what makes browser proof apply to the shipped binary.
func TestGeneratedArtifactsMatchEmbedded(t *testing.T) {
	for _, artifact := range []struct {
		name     string
		path     string
		embedded []byte
	}{
		{name: "base", path: "feasible.js", embedded: Script},
		{name: "vitals", path: "vitals.js", embedded: VitalsScript},
	} {
		t.Run(artifact.name, func(t *testing.T) {
			generated, err := os.ReadFile(filepath.Join("..", "..", "tracker", "dist", artifact.path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(generated, artifact.embedded) {
				t.Fatalf("tracker/dist/%s differs from the embedded production asset", artifact.path)
			}
		})
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

// TestVitalsModuleIsServedAsACacheableModule covers the generated route the
// base imports. It uses the same cache and CORS contract as the base while
// carrying no baked site configuration of its own.
func TestVitalsModuleIsServedAsACacheableModule(t *testing.T) {
	handler := newHandler("example.com")
	recorder := get(t, handler, PathVitals)

	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), VitalsScript) {
		t.Fatalf("vitals response status=%d bytes=%d", recorder.Code, recorder.Body.Len())
	}
	if recorder.Header().Get("Cache-Control") != CacheControl || recorder.Header().Get("ETag") == "" {
		t.Fatalf("vitals cache headers are incomplete: %v", recorder.Header())
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("vitals CORS header = %q", recorder.Header().Get("Access-Control-Allow-Origin"))
	}

	request := httptest.NewRequest(http.MethodHead, PathVitals, nil)
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, request)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("vitals HEAD status=%d body=%d headers=%v", head.Code, head.Body.Len(), head.Header())
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

	recorder := get(t, handler, handler.Keyer.Path("example.com")+"?hash=1&exclude=/admin/**&manual=0&vitals=0.25")

	body := recorder.Body.String()

	for _, want := range []string{`"h":1`, `"x":"/admin/**"`, `"d":"example.com"`, `"v":"0.25"`} {
		if !strings.Contains(body, want) {
			t.Errorf("baked configuration is missing %s: %q", want, body[:min(120, len(body))])
		}
	}

	if strings.Contains(body, `"m"`) {
		t.Error("manual=0 must not be baked at all, or it would override a data-manual attribute")
	}
}

// TestBareVitalsOptionCapturesEveryDocument verifies the shorthand emitted by
// a per-site script URL with no explicit sample value.
func TestBareVitalsOptionCapturesEveryDocument(t *testing.T) {
	handler := newHandler("example.com")
	recorder := get(t, handler, handler.Keyer.Path("example.com")+"?vitals")

	if !strings.Contains(recorder.Body.String(), `"v":"1"`) {
		t.Fatalf("bare vitals option was not enabled: %q", recorder.Body.String()[:min(120, recorder.Body.Len())])
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
