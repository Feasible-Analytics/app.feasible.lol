//
// main.go
// A throwaway static site for exercising the real tracker in a real browser.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Command testsite serves the pages in testsite/ with the tracker snippet
// pointing at a running instance. It is a separate binary, never embedded in
// the product, and exists because the only honest test of a tracking script is
// a browser loading a real page from a real origin.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// baseURLPlaceholder is substituted in every served page. Using a placeholder
// rather than a hard-coded URL is what lets the same page work on localhost and
// over Tailscale, where the snippet must point at the MagicDNS hostname.
const baseURLPlaceholder = "{{BASE_URL}}"

// main parses the flags and serves the directory. Flags rather than environment
// variables on purpose: this tool is not part of the product, and every
// FEASIBLE_* variable is a documented part of the deployment contract.
func main() {
	listen := flag.String("listen", "127.0.0.1:19303", "listen address (host:port)")
	baseURL := flag.String("base-url", "http://localhost:19300", "base URL of the running instance, baked into the snippet")
	dir := flag.String("dir", "testsite", "directory of pages to serve")
	flag.Parse()

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler(*dir, strings.TrimRight(*baseURL, "/")),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("test site listening on http://%s (snippet points at %s)", *listen, *baseURL)

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// handler serves one directory, rewriting the base URL placeholder in HTML on
// the way out. Rewriting at serve time rather than generating a file keeps the
// page a plain committed artefact that anyone can read and edit.
func handler(dir, baseURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}

		// filepath.Clean has already removed any ".." segments, but a path that
		// still escapes the directory is a bug worth refusing rather than
		// serving.
		path := filepath.Join(dir, name)
		if !strings.HasPrefix(path, filepath.Clean(dir)+string(os.PathSeparator)) {
			http.NotFound(w, r)
			return
		}

		body, err := os.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if !strings.HasSuffix(name, ".html") {
			http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(body)))
			return
		}

		page := strings.ReplaceAll(string(body), baseURLPlaceholder, baseURL)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// Nothing here is cacheable: the whole point is to reload the page and
		// see whether the tracker fired.
		w.Header().Set("Cache-Control", "no-store")

		fmt.Fprint(w, page)
	})
}
