//
// assets.go
// Content-addressed static files, and the cache policy that makes them safe.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// A fingerprinted URL is what lets an answer be cached for a year: a new build
// is a new URL, so a browser cannot serve yesterday's stylesheet against
// today's markup. Without one, any lifetime is a promise a deploy breaks.
//

package assets

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
)

// The two cache lifetimes. A request that named the digest is asking for bytes
// that cannot change, so it is answered for a year. One that did not is a
// bookmark or a hand-typed URL and has to see a deploy quickly.
const (
	Immutable  = "public, max-age=31536000, immutable"
	Revalidate = "public, max-age=60"
)

// Digest is the short content hash an asset is addressed by.
//
// Twelve base64 characters is 72 bits, which is far more than enough to tell
// two builds apart and short enough to keep the URL readable in a network
// panel.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)

	return base64.RawURLEncoding.EncodeToString(sum[:])[:12]
}

// File is one embedded asset, ready to write.
type File struct {
	Body        []byte
	ContentType string

	// Digest is the content hash the URL carries. It is what makes the
	// immutable lifetime safe.
	Digest string
}

// Set is every asset one surface serves, keyed by its path below the prefix.
type Set map[string]File

// Load reads and hashes a whole embedded tree once.
//
// It panics on an unreadable tree. The files are embedded at compile time, so a
// failure here means the binary was built without running the asset build, and
// a process that starts and then serves a blank page is far harder to diagnose
// than one that refuses to start with the reason.
func Load(fsys fs.FS, surface string) Set {
	set := Set{}

	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}

		set[name] = File{Body: body, ContentType: contentTypeOf(name), Digest: Digest(body)}

		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("%s: embedded assets could not be read — run `make assets` before building: %v", surface, err))
	}

	if len(set) == 0 {
		panic(surface + ": the embedded asset tree is empty — run `make assets` before building")
	}

	return set
}

// Names lists what the set holds, sorted, for a test or a diagnostic.
func (s Set) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// URL is how a template addresses one asset.
//
// A name the set does not hold is returned unchanged rather than blank: a
// missing digest is a slow asset, and a blank href is a page with no
// stylesheet.
//
// A font is returned without one. The compiled stylesheet addresses its faces
// by plain path — Tailwind writes those url() values and this code does not
// rewrite them — so a fingerprinted preload would point at a different URL from
// the one the stylesheet then asks for, and the browser would fetch the face
// twice. The filename carries the weight and the subset, so a different face is
// a different name either way.
func (s Set) URL(prefix, name string) string {
	file, ok := s[name]
	if !ok || strings.HasPrefix(name, "fonts/") {
		return prefix + name
	}

	return prefix + name + "?v=" + file.Digest
}

// Serve writes one asset, or a 404 for a name the set does not hold.
func (s Set) Serve(w http.ResponseWriter, r *http.Request, name string) {
	file, ok := s[name]
	if !ok {
		http.NotFound(w, r)

		return
	}

	cache := Revalidate
	if r.URL.Query().Get("v") == file.Digest {
		cache = Immutable
	}

	etag := `"` + file.Digest + `"`

	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", etag)

	// A revalidation costs a round trip and nothing else, which is what the
	// short lifetime is for.
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write(file.Body)
	}
}

// Handler serves a whole set under one prefix.
func (s Set) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "GET this asset", http.StatusMethodNotAllowed)

			return
		}

		s.Serve(w, r, strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/"))
	})
}

// contentTypeOf names a file by its extension, because that is the only thing
// an embedded tree carries. An unknown extension is served as bytes rather than
// guessed at from the content: sniffing a stylesheet wrongly is how a page
// silently renders unstyled.
func contentTypeOf(name string) string {
	if found := mime.TypeByExtension(path.Ext(name)); found != "" {
		return found
	}

	return "application/octet-stream"
}
