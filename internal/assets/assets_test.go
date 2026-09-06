//
// assets_test.go
// The digest, the URL it goes in, and the cache policy it makes safe.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package assets

import (
	"net/http"
	"net/http/httptest"
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
	if Digest([]byte("body")) != Digest([]byte("body")) {
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

	// A name nothing knows about still renders a usable path: a blank href is
	// a page with no stylesheet.
	if got := set.URL("/app/assets/", "nothing.css"); got != "/app/assets/nothing.css" {
		t.Errorf("an unknown asset renders as %q", got)
	}
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

			if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
				t.Errorf("Content-Type = %q, want a stylesheet", got)
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
// nothing behind, and that a traversal cannot climb out of the tree.
func TestAnUnknownAssetIs404(t *testing.T) {
	handler := Load(tree("body{}"), "test").Handler()

	for _, path := range []string{"/nothing.css", "/../secret", "/fonts/"} {
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
