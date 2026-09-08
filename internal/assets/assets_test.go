//
// assets_test.go
// The digest, the URL it goes in, and the cache policy it makes safe.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package assets

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// tree is a small embedded-looking asset tree.
func tree(css string) fstest.MapFS {
	return fstest.MapFS{
		"app.css":             {Data: []byte(css)},
		"alpine.js":           {Data: []byte("window.Alpine = {}")},
		"favicon.svg":         {Data: []byte("<svg/>")},
		"fonts/archivo.woff2": {Data: []byte("not really a font")},
	}
}

// TestTheDigestFollowsTheBytes is the whole basis for the year-long lifetime:
// the same bytes have to give the same URL, and different bytes a different
// one, or a deploy is either invisible or invalidates a cache for nothing.
func TestTheDigestFollowsTheBytes(t *testing.T) {
	first, again := Digest([]byte("body")), Digest([]byte("body"))
	if first != again {
		t.Error("the same bytes produced two digests, so an unchanged file would churn every cache")
	}

	if Digest([]byte("body")) == Digest([]byte("body ")) {
		t.Error("different bytes produced one digest, so a deploy could be served from cache")
	}

	if got := len(Digest([]byte("body"))); got != 12 {
		t.Errorf("digest is %d characters, want 12", got)
	}
}

// TestAnAssetURLCarriesItsDigest covers what a template renders.
func TestAnAssetURLCarriesItsDigest(t *testing.T) {
	set := Load(tree("body{}"), "test")

	css := set.URL("/app/assets/", "app.css")
	if !strings.HasPrefix(css, "/app/assets/app.css?v=") {
		t.Fatalf("app.css renders as %q", css)
	}

	if !strings.HasSuffix(css, set["app.css"].Digest) {
		t.Errorf("%q does not end in the file's digest", css)
	}

	// A font is addressed by the compiled stylesheet's own url() value, which
	// this code does not rewrite. A digest here would point the preload at a
	// different URL and the browser would fetch the face twice.
	if got := set.URL("/app/assets/", "fonts/archivo.woff2"); got != "/app/assets/fonts/archivo.woff2" {
		t.Errorf("a font renders as %q, want no digest", got)
	}

	// A typo in a layout is a page with a 404 for its stylesheet and nothing
	// saying so, which is exactly what must not happen quietly.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a name the tree does not hold rendered a URL")
			}
		}()

		set.URL("/app/assets/", "nothing.css")
	}()
}

// TestADigestChangesWhenTheFileDoes is the deploy case in one assertion.
func TestADigestChangesWhenTheFileDoes(t *testing.T) {
	before := Load(tree("body{color:red}"), "test").URL("/app/assets/", "app.css")
	after := Load(tree("body{color:blue}"), "test").URL("/app/assets/", "app.css")

	if before == after {
		t.Error("a changed stylesheet kept its URL, so a browser holding the old one would keep it for a year")
	}
}

// TestTheCachePolicyFollowsTheURL is the reason the digest exists.
func TestTheCachePolicyFollowsTheURL(t *testing.T) {
	set := Load(tree("body{}"), "test")
	handler := set.Handler()
	digest := set["app.css"].Digest

	for _, tc := range []struct {
		name  string
		path  string
		cache string
	}{
		{"the digest it was built with", "/app.css?v=" + digest, Immutable},
		{"no digest at all", "/app.css", Revalidate},
		{"a digest from another build", "/app.css?v=Xk3pQ1a9vB2c", Revalidate},
		// A face is identified by its own filename — the weight, the subset and
		// the format are all in it — so it needs no digest to be safe to keep.
		{"a font, which carries no digest", "/fonts/archivo.woff2", Immutable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if got := recorder.Header().Get("Cache-Control"); got != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", got, tc.cache)
			}

			if recorder.Code != http.StatusOK {
				t.Errorf("answered %d, want 200", recorder.Code)
			}

			if got := recorder.Header().Get("Content-Length"); got == "" {
				t.Error("no Content-Length, so the body goes out chunked and a HEAD reports no size")
			}
		})
	}
}

// TestAKnownTagIsAnswered304 keeps the unversioned path cheap: a bookmark
// revalidates every minute and the answer is almost always "unchanged".
func TestAKnownTagIsAnswered304(t *testing.T) {
	set := Load(tree("body{}"), "test")
	handler := set.Handler()

	request := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	request.Header.Set("If-None-Match", `"`+set["app.css"].Digest+`"`)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotModified {
		t.Errorf("answered %d, want 304", recorder.Code)
	}

	if recorder.Body.Len() != 0 {
		t.Error("a 304 carried a body")
	}

	stale := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	stale.Header.Set("If-None-Match", `"Xk3pQ1a9vB2c"`)

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, stale)

	if recorder.Code != http.StatusOK {
		t.Errorf("a stale tag answered %d, want the new bytes", recorder.Code)
	}
}

// TestAnUnknownAssetIs404 checks the handler does not answer for a path it has
// nothing behind. The set is a map over the embedded tree, so there is nothing
// on disk a name could reach.
func TestAnUnknownAssetIs404(t *testing.T) {
	handler := Load(tree("body{}"), "test").Handler()

	for _, path := range []string{"/nothing.css", "/secret", "/fonts/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, recorder.Code)
		}
	}
}

// TestLoadRefusesAnEmptyTree is what turns a binary built without the asset
// step into a process that will not start, rather than one that starts and
// serves every page unstyled.
func TestLoadRefusesAnEmptyTree(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an empty asset tree was accepted")
		}
	}()

	Load(fstest.MapFS{}, "test")
}

// TestTheContentTypeIsTheSameOnEveryHost pins the table. mime.TypeByExtension
// reads the machine's own mime.types, so the same binary would label a
// stylesheet two ways on two servers.
func TestTheContentTypeIsTheSameOnEveryHost(t *testing.T) {
	for name, want := range map[string]string{
		"app.css":             "text/css; charset=utf-8",
		"alpine.js":           "text/javascript; charset=utf-8",
		"favicon.svg":         "image/svg+xml",
		"fonts/archivo.woff2": "font/woff2",
		"fonts/OFL.txt":       "text/plain; charset=utf-8",
		"mystery.bin":         "application/octet-stream",
	} {
		if got := ContentType(name); got != want {
			t.Errorf("%s is labelled %q, want %q", name, got, want)
		}
	}
}

// TestOnlyGetAndHeadAreAnswered keeps the asset tree from looking like
// somewhere to send anything else.
func TestOnlyGetAndHeadAreAnswered(t *testing.T) {
	handler := Load(tree("body{}"), "test").Handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, "/app.css", nil))

		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d, want 405", method, recorder.Code)
		}

		if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s was told %q is allowed", method, got)
		}
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/app.css", nil))

	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD answered %d with %d bytes, want 200 and no body", head.Code, head.Body.Len())
	}

	if head.Header().Get("Content-Length") == "" {
		t.Error("a HEAD reported no size, which is the only thing it is for")
	}
}

// TestAPathIsCleanedBeforeItIsLookedUp covers the shapes a proxy or a hand-typed
// URL produces. A traversal cannot escape either way — the lookup is a map over
// the embedded set — but a doubled slash must still find the file.
func TestAPathIsCleanedBeforeItIsLookedUp(t *testing.T) {
	handler := Load(tree("body{}"), "test").Handler()

	for path, want := range map[string]int{
		"//app.css":         http.StatusOK,
		"/./app.css":        http.StatusOK,
		"/fonts/../app.css": http.StatusOK,
		"/../app.css":       http.StatusOK,
		"/../../secret":     http.StatusNotFound,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code != want {
			t.Errorf("%s answered %d, want %d", path, recorder.Code, want)
		}
	}
}

// TestEveryAssetALayoutAsksForExists is what stops a typo in a layout becoming
// a page that renders with a 404 for its stylesheet and nothing anywhere saying
// so. Set.URL panics on an unknown name; this is the check that the names the
// three layouts actually pass are known.
func TestEveryAssetALayoutAsksForExists(t *testing.T) {
	asked := map[string]bool{}

	for _, root := range []string{
		filepath.Join("..", "auth", "templates"),
		filepath.Join("..", "settings", "templates"),
		filepath.Join("..", "billingui", "templates"),
		filepath.Join("..", "appui", "templates"),
	} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".html") {
				return err
			}

			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			for _, match := range assetCall.FindAllStringSubmatch(string(body), -1) {
				asked[match[1]] = true
			}

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(asked) == 0 {
		t.Fatal("no layout asks for an asset, so this check is watching nothing")
	}

	for name := range asked {
		if _, ok := UI[name]; !ok {
			t.Errorf("a layout asks for %q, which is not in the embedded tree: %v", name, UI.Names())
		}
	}
}

// assetCall matches the template helper's argument.
var assetCall = regexp.MustCompile(`asset\s+"([^"]+)"`)
