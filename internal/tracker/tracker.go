//
// tracker.go
// Serving the browser script: one base, one optional module, two delivery modes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package tracker serves the browser tracking script and the noscript pixel.
//
// There is one base bundle, embedded in the binary, and two ways to ask for it.
// The legacy-compatible path reads `data-domain` and `data-api` off its own
// script tag, so an existing installation migrates by repointing one hostname.
// The per-site path carries an opaque token instead, and the configuration the
// bundle would otherwise have read from attributes is written in front of it at
// serve time. A separately embedded ES module is requested by that same base
// only when its one configuration enables Web Vitals.
//
// The per-site path exists because filter lists name files individually. A
// customer proxying the script under one memorable filename loses their traffic
// the day that filename is added to a list, and the only remedy is renaming it.
// A token that differs per site means one listing costs one site rather than
// every site, and rotating the secret renames every path at once.
//
// None of this is an escape from blocklists and it should never be sold as one.
// The hosted domain gets blocked, custom domains are blocked on some browsers
// via the redirect, and proxied filenames get listed one at a time. Randomised
// paths raise the cost; they do not end the game.
package tracker

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Script is the compiled base bundle, written by tracker/build.js. It is embedded
// rather than read from disk because the whole promise of this product is a
// single binary with nothing to copy alongside it.
//
//go:embed assets/feasible.js
var Script []byte

// VitalsScript is the optional generated module loaded by Script only when the
// resolved embedded configuration enables Web Vitals.
//
//go:embed assets/vitals.js
var VitalsScript []byte

// BaseSizeBudget is the largest the primary tracker may be over the wire.
// Stable client event ids and current-policy checks on live and persisted sends
// add a small fixed cost. The planned 3.25 KiB post-feature ceiling is kept in
// sync with tracker/build.js by the generated-asset test.
const BaseSizeBudget = 13 * 256

// VitalsSizeBudget is the separate ceiling for the maintained optional Web
// Vitals module. Sites that do not enable capture never request these bytes.
const VitalsSizeBudget = 6 * 1024

// Size budgets are enforced here as well as in tracker/build.js.
//
// It is enforced by a test in this package rather than only by the JavaScript
// build, so `go test ./...` catches an over-budget committed artifact even on a
// machine that is not rebuilding JavaScript.

// Paths the handler answers.
const (
	// PathPrefix is what a mux routes to this handler.
	PathPrefix = "/js/"

	// PathLegacy is the drop-in variant. The name is deliberately the dullest
	// one available: it is the path an existing snippet already points at, and
	// the whole point is that a migrating customer changes the hostname and
	// nothing else.
	PathLegacy = PathPrefix + "script.js"

	// PathVitals is the generated ES module dynamically imported by either base
	// delivery mode when its one embedded configuration enables capture.
	PathVitals = PathPrefix + "vitals.js"

	// sitePrefix and siteSuffix bracket the per-site token.
	sitePrefix = "fs-"
	siteSuffix = ".js"
)

// CacheControl is how long a browser may keep the script.
//
// One hour is a deliberate compromise. Caching it for a year would be free
// bandwidth, but a tracker bug then lives in browser caches for a year — and
// every hard-won lesson in this package is a story that ends with "old scripts
// stay in caches for months". One hour means a fix reaches everybody within the
// hour, and an ETag means the hourly revalidation is a 304 rather than a
// download.
const CacheControl = "public, max-age=3600"

// DomainSource is the routing map this handler reads to turn a per-site token
// back into a domain. It is an interface rather than the concrete site cache so
// that the script can be served by a process that has no database at all, and
// so the tests do not need one.
type DomainSource interface {
	// Domains lists every domain currently accepting traffic.
	Domains() []string
}

// Handler serves both script variants.
type Handler struct {
	// Keyer derives and resolves the per-site tokens. A nil Keyer serves only
	// the legacy path, which is what a process with no data directory can
	// honestly offer.
	Keyer *Keyer
}

// New builds a handler over a routing map. The keyer is constructed here rather
// than by the caller so that there is one place that decides how a token is
// derived, and no way for the dashboard to render a snippet the server would
// not answer.
func New(secret []byte, sites DomainSource) *Handler {
	return &Handler{Keyer: NewKeyer(secret, sites)}
}

// ServeHTTP routes one request to the variant it asked for.
//
// Anything under /js/ that is not one of the two known shapes is a 404 rather
// than a redirect or a default. A typo in a snippet has to be visible: serving
// a working script from a wrong URL means the customer's real snippet stays
// wrong and nobody ever finds out.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET the tracker script from this endpoint", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == PathLegacy {
		h.serve(w, r, Script, nil)
		return
	}

	if r.URL.Path == PathVitals {
		h.serve(w, r, VitalsScript, nil)
		return
	}

	token, ok := siteToken(r.URL.Path)
	if !ok || h.Keyer == nil {
		http.NotFound(w, r)
		return
	}

	domain, ok := h.Keyer.Resolve(token)
	if !ok {
		// An unknown token is a site that was deleted, a snippet copied
		// between accounts, or a rotated secret. None of them is a page we can
		// help by serving a script that would report to nowhere.
		http.NotFound(w, r)
		return
	}

	h.serve(w, r, Script, bakedConfig(domain, r))
}

// siteToken pulls the token out of a per-site path, reporting whether the path
// had the right shape at all.
func siteToken(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, PathPrefix+sitePrefix)
	if !ok {
		return "", false
	}

	token, ok := strings.CutSuffix(rest, siteSuffix)
	if !ok || token == "" || strings.Contains(token, "/") {
		return "", false
	}

	return token, true
}

// bakedConfig collects the settings written in front of the bundle.
//
// Only the keys the server actually knows are emitted. The script layers this
// object over whatever it read from its own `data-*` attributes, so an absent
// key falls through to the attribute rather than overwriting it with a zero
// value — which is how a `data-hash` on a per-site snippet would otherwise
// silently do nothing.
//
// The options come from the query string because there is nowhere else for them
// to come from yet: the sites table stores a domain and a timezone, not a
// tracker configuration. A query string is also what makes the cache key
// correct for free, since two snippets with different options are two URLs.
func bakedConfig(domain string, r *http.Request) map[string]any {
	cfg := map[string]any{"d": domain}
	query := r.URL.Query()

	// The endpoint is only baked when the customer names one. Left alone, the
	// script posts to the origin it was loaded from, which is what makes a
	// reverse proxy work with no second setting to keep in sync — and baking an
	// absolute URL back to us is exactly what would defeat that proxy.
	for key, param := range map[string]string{
		"a": "api",
		"x": "exclude",
		"f": "file-types",
		"n": "alias",
	} {
		if value := strings.TrimSpace(query.Get(param)); value != "" {
			cfg[key] = value
		}
	}

	// Vitals is an optional mode of the same bundle. A bare query flag captures
	// every document, while a value is passed through as the document sample
	// rate and interpreted by the browser tracker.
	if query.Has("vitals") {
		value := strings.TrimSpace(query.Get("vitals"))
		if value == "" {
			value = "1"
		}
		cfg["v"] = value
	}

	for key, param := range map[string]string{
		"m": "manual",
		"h": "hash",
		"l": "local",
	} {
		// An absent flag is left out rather than baked as false, so that
		// `data-hash` on the tag still works. Only a flag that is present and
		// not explicitly off becomes a baked 1.
		if query.Has(param) && truthy(query.Get(param)) {
			cfg[key] = 1
		}
	}

	return cfg
}

// truthy reads a query flag. Presence is enough — `?hash` and `?hash=1` mean
// the same thing — because a customer hand-editing a snippet URL should not
// have to guess which spelling we parse.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off":
		return false
	}

	return true
}

// serve writes the script, with the baked configuration in front of it when
// there is one.
//
// The ETag covers the configuration as well as the bundle, so two sites never
// share a cache entry and a changed option invalidates immediately. Answering
// the conditional request here rather than leaving it to a CDN means the same
// behaviour on a self-hosted install with nothing in front of it.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, script []byte, cfg map[string]any) {
	body := script

	if cfg != nil {
		prefix, err := configPrefix(cfg)
		if err != nil {
			http.Error(w, "could not build the site configuration", http.StatusInternalServerError)
			return
		}

		body = append(prefix, script...)
	}

	sum := sha256.Sum256(body)
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", CacheControl)
	w.Header().Set("ETag", etag)

	// The script is fetched cross-origin by every site we serve, so it is
	// readable by anyone; nothing in it is secret. A site with a strict CSP has
	// to allow this origin in `script-src` *and* in `connect-src` — allowing
	// only the first loads a script that is then blocked from sending anything,
	// which looks exactly like a tracker that does not work.
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}

	_, _ = w.Write(body)
}

// configPrefix renders the baked configuration as the one statement the bundle
// looks for.
//
// It is JSON rather than hand-built JavaScript because a domain is customer
// input: a quote or a backslash in one, concatenated into a script, is a script
// that either fails to parse or runs whatever the customer put there. Go's
// encoder also escapes the line separators that are valid in JSON and fatal in
// JavaScript, which hand-built string concatenation would not.
func configPrefix(cfg map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("tracker: encode site configuration: %w", err)
	}

	return append(append([]byte("window.__fsc="), encoded...), ';', '\n'), nil
}
