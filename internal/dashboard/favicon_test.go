//
// favicon_test.go
// Tests for the source-icon proxy and its cache.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dashboard

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stub answers every upstream request from a script, and records what was
// asked. The proxy must never reach the real network from a test: an icon
// service that is down would otherwise fail this package's suite.
type stub struct {
	status      int
	contentType string
	body        []byte
	calls       []string
	err         error
}

// RoundTrip satisfies http.RoundTripper.
func (s *stub) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls = append(s.calls, r.URL.String())

	if s.err != nil {
		return nil, s.err
	}

	header := http.Header{}
	if s.contentType != "" {
		header.Set("Content-Type", s.contentType)
	}

	return &http.Response{
		StatusCode: s.status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Request:    r,
	}, nil
}

// newProxy builds a proxy over a stub and a temporary cache directory.
func newProxy(t *testing.T, upstream *stub) *Favicons {
	t.Helper()

	f := NewFavicons(t.TempDir(), nil)
	f.Client = &http.Client{Transport: upstream}
	f.Scope = func(*http.Request) (string, bool) { return view, true }

	return f
}

// fetch runs one icon request through the proxy.
func fetch(t *testing.T, f *Favicons, name string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(FaviconPattern, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, FaviconPrefix+url.PathEscape(name), nil))

	return w
}

// TestHostForResolvesSourceNames covers the reverse lookup. A report groups by
// the canonical name, so a proxy that could not turn "Hacker News" back into a
// hostname would never fetch anything.
func TestHostForResolvesSourceNames(t *testing.T) {
	cases := map[string]string{
		"Hacker News":         "news.ycombinator.com",
		"DuckDuckGo":          "duckduckgo.com",
		"producthunt.com":     "producthunt.com",
		"www.producthunt.com": "producthunt.com",

		// No site to fetch from. These are the common rows on the Channels tab
		// and the empty-source row, and each must cost zero requests.
		"Direct":         "",
		"Organic Search": "",
		"":               "",
	}

	for name, want := range cases {
		if got := hostFor(name); got != want {
			t.Errorf("hostFor(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestHostForRefusesAnythingButAHostname is the guard on what reaches an
// outbound URL. The value here is derived from a stored referrer, which is
// ultimately whatever a browser put in a Referer header, so it is
// attacker-influenced by definition.
func TestHostForRefusesAnythingButAHostname(t *testing.T) {
	for _, name := range []string{
		"evil.example/../../etc/passwd",
		"evil.example?x=1",
		"evil.example#fragment",
		"evil.example:8080",
		"has space.example",
		"localhost",
		"nodot",
		".leading.dot",
		"trailing.dot.",
		"evil.example\n.ico",
	} {
		if got := hostFor(name); got != "" {
			t.Errorf("hostFor(%q) = %q, want it refused", name, got)
		}
	}
}

// TestServesUpstreamIcon is the happy path.
func TestServesUpstreamIcon(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	f := newProxy(t, upstream)

	w := fetch(t, f, "Hacker News")

	if w.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", w.Code)
	}

	if w.Body.String() != "icon-bytes" {
		t.Fatalf("body is %q, want the upstream bytes", w.Body.String())
	}

	if len(upstream.calls) != 1 || !strings.Contains(upstream.calls[0], "news.ycombinator.com") {
		t.Fatalf("upstream was asked %v, want one request for news.ycombinator.com", upstream.calls)
	}
}

// TestSecondRequestIsCached covers the point of the cache. Without it, one
// dashboard load with a long tail of referrers is one upstream request per
// distinct source, every time anybody opens the page.
func TestSecondRequestIsCached(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	f := newProxy(t, upstream)

	fetch(t, f, "Hacker News")
	fetch(t, f, "Hacker News")

	if len(upstream.calls) != 1 {
		t.Fatalf("upstream was asked %d times, want 1", len(upstream.calls))
	}
}

// TestCacheSurvivesRestart covers the on-disk half. A deploy must not cost one
// upstream fetch per distinct source across every dashboard on the shard.
func TestCacheSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	first := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	warm := NewFavicons(dir, nil)
	warm.Client = &http.Client{Transport: first}
	fetch(t, warm, "Hacker News")

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the icon was not written to disk: %v, %v", entries, err)
	}

	// The filename is a hash rather than the hostname, because a hostname
	// reaching the filesystem is a path traversal waiting to be found.
	if strings.Contains(entries[0].Name(), "ycombinator") {
		t.Fatalf("the cache filename carries the hostname: %s", entries[0].Name())
	}

	second := &stub{err: io.ErrUnexpectedEOF}
	cold := NewFavicons(dir, nil)
	cold.Client = &http.Client{Transport: second}

	if body := fetch(t, cold, "Hacker News").Body.String(); body != "icon-bytes" {
		t.Fatalf("a restarted proxy served %q, want the cached icon", body)
	}

	if len(second.calls) != 0 {
		t.Fatalf("a restarted proxy went upstream %d times, want 0", len(second.calls))
	}
}

// TestFailureFallsBackToATile is the rule that matters most in a report card. A
// broken-image glyph in the icon slot reads as a bug in the dashboard, so the
// proxy never answers anything but 200.
func TestFailureFallsBackToATile(t *testing.T) {
	for _, upstream := range []*stub{
		{status: http.StatusNotFound, contentType: "image/png", body: []byte("nope")},
		{err: io.ErrUnexpectedEOF},
		{status: http.StatusOK, contentType: "text/html", body: []byte("<html>an error page</html>")},
		{status: http.StatusOK, contentType: "image/x-icon"},

		// SVG is the one image type that is also a document. Echoed back on
		// this origin it would run its own script for anybody who opened the
		// icon URL directly, so it falls back to a tile like any other refusal.
		{status: http.StatusOK, contentType: "image/svg+xml",
			body: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
	} {
		f := newProxy(t, upstream)
		shown(f, "producthunt.com")

		w := fetch(t, f, "producthunt.com")

		if w.Code != http.StatusOK {
			t.Errorf("a failed fetch answered %d, want 200", w.Code)
		}

		if !strings.HasPrefix(w.Header().Get("Content-Type"), "image/svg+xml") {
			t.Errorf("a failed fetch answered %q, want an SVG tile", w.Header().Get("Content-Type"))
		}

		if !strings.Contains(w.Body.String(), "<svg") {
			t.Errorf("a failed fetch did not answer with a tile: %s", w.Body.String())
		}
	}
}

// TestFailureIsRemembered covers the negative cache. A slow or broken upstream
// must not be re-asked once per row per page load.
func TestFailureIsRemembered(t *testing.T) {
	upstream := &stub{status: http.StatusNotFound}
	f := newProxy(t, upstream)
	shown(f, "producthunt.com")

	fetch(t, f, "producthunt.com")
	fetch(t, f, "producthunt.com")

	if len(upstream.calls) != 1 {
		t.Fatalf("a known-bad host was asked %d times, want 1", len(upstream.calls))
	}
}

// TestChannelNamesNeverGoUpstream covers the most common rows on the page.
// "Direct" and the channel names have no site behind them, and asking anyway
// would be a wasted request on every single dashboard load.
func TestChannelNamesNeverGoUpstream(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon")}
	f := newProxy(t, upstream)

	for _, name := range []string{"Direct", "Organic Search", "Paid Social"} {
		w := fetch(t, f, name)

		if !strings.Contains(w.Body.String(), "<svg") {
			t.Errorf("%q did not get a tile: %s", name, w.Body.String())
		}
	}

	if len(upstream.calls) != 0 {
		t.Fatalf("upstream was asked %v for names with no site behind them", upstream.calls)
	}
}

// TestTileIsStableAndEscaped covers the fallback image itself. The colour has to
// be a function of the name so the same source looks the same on every
// dashboard, and the initial has to be safe: the name comes from a referrer
// header and is written straight into an SVG document.
func TestTileIsStableAndEscaped(t *testing.T) {
	first := string(tile("Reddit"))
	if first != string(tile("Reddit")) {
		t.Fatal("the same name produced two different tiles")
	}

	if string(tile("Reddit")) == string(tile("Bluesky")) {
		t.Fatal("two different names produced the same tile")
	}

	for _, name := range []string{"<script>", "\"onload=alert(1)", "&amp;"} {
		if body := string(tile(name)); strings.Contains(body, "<script") || strings.Count(body, "<") != 5 {
			t.Errorf("tile(%q) is not safe SVG: %s", name, body)
		}
	}
}

// view is the scope every test request belongs to unless it says otherwise.
const view = "1\x00session"

// shown records that the test view was shown these names, as the stats
// endpoint does when it answers a source report.
func shown(f *Favicons, names ...string) {
	f.Allow(view, names)
}

// TestAnUnknownNameCostsNothing is the abuse case. A name no report has shown
// the viewer must not reach the network, the disk or the memory cache, however
// much it looks like a hostname.
func TestAnUnknownNameCostsNothing(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	f := newProxy(t, upstream)

	for _, name := range []string{"random-a1b2c3.com", "another.random.io", "producthunt.com"} {
		w := fetch(t, f, name)

		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<svg") {
			t.Errorf("%q answered %d %q, want the tile", name, w.Code, w.Body.String())
		}

		if got := w.Header().Get("Cache-Control"); got != refusedCacheControl {
			t.Errorf("%q answered Cache-Control %q, want %q", name, got, refusedCacheControl)
		}
	}

	if len(upstream.calls) != 0 {
		t.Errorf("upstream was asked %v for names nobody was shown", upstream.calls)
	}

	if entries, _ := os.ReadDir(f.Dir); len(entries) != 0 {
		t.Errorf("%d file(s) were written for names nobody was shown", len(entries))
	}

	if len(f.memory) != 0 {
		t.Errorf("%d answer(s) were remembered for names nobody was shown", len(f.memory))
	}
}

// TestAnIconAlreadyCachedIsNotServedToAStranger keeps the cache from answering
// "does anybody here get traffic from this site" to somebody who was never
// shown the name.
func TestAnIconAlreadyCachedIsNotServedToAStranger(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	f := newProxy(t, upstream)

	shown(f, "producthunt.com")

	if body := fetch(t, f, "producthunt.com").Body.String(); body != "icon-bytes" {
		t.Fatalf("the entitled view got %q, want the icon", body)
	}

	f.Scope = func(*http.Request) (string, bool) { return "2\x00public:2", true }

	if body := fetch(t, f, "producthunt.com").Body.String(); !strings.Contains(body, "<svg") {
		t.Errorf("another view got %q, want the tile", body)
	}

	f.Scope = func(*http.Request) (string, bool) { return "", false }

	if body := fetch(t, f, "producthunt.com").Body.String(); !strings.Contains(body, "<svg") {
		t.Errorf("a request with no view got %q, want the tile", body)
	}
}

// TestTheRateLimitStopsFetchesNotCachedIcons covers the limit on misses. Once
// it is spent a new name gets the tile, and a name already cached is still
// served, because that costs nothing.
func TestTheRateLimitStopsFetchesNotCachedIcons(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	f := newProxy(t, upstream)

	shown(f, "one.com", "two.com")
	fetch(t, f, "one.com")

	f.Limit = func(*http.Request) bool { return false }

	if body := fetch(t, f, "two.com").Body.String(); !strings.Contains(body, "<svg") {
		t.Errorf("a fetch over the limit answered %q, want the tile", body)
	}

	if body := fetch(t, f, "one.com").Body.String(); body != "icon-bytes" {
		t.Errorf("a cached icon answered %q over the limit, want the icon", body)
	}

	if len(upstream.calls) != 1 {
		t.Errorf("upstream was asked %d times, want 1", len(upstream.calls))
	}
}

// TestTheDiskCacheStaysUnderItsCap fills the cache well past its limit and
// checks the directory never ends a write above it and keeps the newest icon.
func TestTheDiskCacheStaysUnderItsCap(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: bytes.Repeat([]byte("x"), 1000)}
	f := newProxy(t, upstream)
	f.MaxDiskBytes = 10_000

	for index := range 50 {
		name := "site" + strconv.Itoa(index) + ".com"

		shown(f, name)
		fetch(t, f, name)

		if size := directorySize(t, f.Dir); size > f.MaxDiskBytes {
			t.Fatalf("after %d icons the cache holds %d bytes, over the %d cap", index+1, size, f.MaxDiskBytes)
		}
	}

	if _, err := os.Stat(f.pathFor("site49.com")); err != nil {
		t.Errorf("the newest icon was evicted: %v", err)
	}
}

// TestACacheLeftOverTheCapIsPrunedOnTheFirstWrite covers a directory that was
// already too large when the process started.
func TestACacheLeftOverTheCapIsPrunedOnTheFirstWrite(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: []byte("icon-bytes")}
	f := newProxy(t, upstream)
	f.MaxDiskBytes = 10_000

	for index := range 30 {
		file := filepath.Join(f.Dir, "old"+strconv.Itoa(index)+".ico")

		if err := os.WriteFile(file, bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
			t.Fatal(err)
		}

		// The lower the number, the older the file.
		age := time.Now().Add(-time.Duration(30-index) * time.Hour)
		if err := os.Chtimes(file, age, age); err != nil {
			t.Fatal(err)
		}
	}

	shown(f, "new.com")
	fetch(t, f, "new.com")

	if size := directorySize(t, f.Dir); size > f.MaxDiskBytes {
		t.Errorf("the cache holds %d bytes after a write, over the %d cap", size, f.MaxDiskBytes)
	}

	if _, err := os.Stat(filepath.Join(f.Dir, "old0.ico")); err == nil {
		t.Error("the oldest icon survived the prune")
	}

	if _, err := os.Stat(filepath.Join(f.Dir, "old29.ico")); err != nil {
		t.Errorf("the newest of the old icons was evicted before older ones: %v", err)
	}
}

// TestTheMemoryCacheStaysUnderItsCap covers the half of the cache that would
// otherwise grow with every distinct source until the process was restarted.
func TestTheMemoryCacheStaysUnderItsCap(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/x-icon", body: bytes.Repeat([]byte("x"), 1000)}
	f := newProxy(t, upstream)
	f.MaxMemoryBytes = 10_000

	for index := range 50 {
		name := "site" + strconv.Itoa(index) + ".com"

		shown(f, name)
		fetch(t, f, name)

		if f.memoryBytes > f.MaxMemoryBytes {
			t.Fatalf("after %d icons memory holds %d bytes, over the %d cap", index+1, f.memoryBytes, f.MaxMemoryBytes)
		}
	}
}

// TestAnOversizedIconIsRefused keeps a file near the ceiling from being stored
// cut short and served as a corrupt image.
func TestAnOversizedIconIsRefused(t *testing.T) {
	upstream := &stub{status: http.StatusOK, contentType: "image/png", body: bytes.Repeat([]byte("x"), maxIconBytes+1)}
	f := newProxy(t, upstream)

	if body := fetch(t, f, "Hacker News").Body.String(); !strings.Contains(body, "<svg") {
		t.Error("an icon over the size limit was served")
	}

	if entries, _ := os.ReadDir(f.Dir); len(entries) != 0 {
		t.Errorf("%d file(s) were written for an oversized icon", len(entries))
	}
}

// TestHostForRefusesAddressesAndInternalNames covers the names that are shaped
// like a hostname and are not a public site.
func TestHostForRefusesAddressesAndInternalNames(t *testing.T) {
	for _, name := range []string{
		"127.0.0.1",
		"10.0.0.8",
		"192.168.1.1",
		"2130706433.1",
		"0x7f.0.0.1",
		"printer.local",
		"db.internal",
		"router.lan",
		"wiki.corp",
		"app.localhost",
		"1.0.0.127.in-addr.arpa",
		"hidden.onion",
		"-bad.com",
		"bad-.com",
		"double..dot.com",
		"single.c",
	} {
		if got := hostFor(name); got != "" {
			t.Errorf("hostFor(%q) = %q, want it refused", name, got)
		}
	}
}

// TestTheRecordOfShownNamesIsBounded keeps Allow itself from being the thing
// that grows without limit.
func TestTheRecordOfShownNamesIsBounded(t *testing.T) {
	f := NewFavicons("", nil)

	for index := range maxScopes + 500 {
		f.Allow("scope"+strconv.Itoa(index), []string{"a.com"})
	}

	if len(f.scopes) > maxScopes {
		t.Errorf("%d views are remembered, over the %d cap", len(f.scopes), maxScopes)
	}

	names := make([]string, 0, maxNamesPerScope*3)
	for index := range maxNamesPerScope * 3 {
		names = append(names, "site"+strconv.Itoa(index)+".com")
	}

	f.Allow("big", names)
	f.Allow("big", names)

	if got := len(f.scopes["big"].names); got > maxNamesPerScope {
		t.Errorf("one view remembers %d names, over the %d cap", got, maxNamesPerScope)
	}
}

// directorySize adds up the files in the cache directory.
func directorySize(t *testing.T, dir string) int64 {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var total int64

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}

		total += info.Size()
	}

	return total
}
