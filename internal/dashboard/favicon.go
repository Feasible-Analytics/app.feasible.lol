//
// favicon.go
// The source-icon proxy: fetched once by us, never by the reader's browser.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/referrer"
)

// FaviconPattern is the route the proxy is mounted on. The name in the path is
// the canonical source name a report groups by — "Hacker News", not a hostname
// — because that is the only thing a row on the Sources card actually has.
const FaviconPattern = "GET /favicon/sources/{name}"

// FaviconPrefix is what the front end builds its URLs from.
const FaviconPrefix = "/favicon/sources/"

// iconEndpoint is where an icon is fetched from, with the hostname substituted
// in. It is a public icon service rather than the source's own origin because
// most sites answer /favicon.ico with an HTML error page, a redirect chain or
// nothing at all, and a proxy that succeeds a third of the time is worse than
// no proxy — the rows look randomly broken rather than consistently plain.
const iconEndpoint = "https://icons.duckduckgo.com/ip3/%s.ico"

// Limits on one upstream fetch. An icon is a few kilobytes; anything near the
// ceiling is a misconfigured server or somebody feeding us a file, and either
// way it is not going in a 16-pixel box.
const (
	fetchTimeout  = 5 * time.Second
	maxIconBytes  = 128 << 10
	successMaxAge = 7 * 24 * time.Hour

	// A miss is remembered too, and for a shorter time. Without a negative
	// cache, every load of a dashboard with a long tail of unknown referrers
	// re-fetches every one of them, and a slow upstream turns into a slow
	// dashboard.
	failureMaxAge = 6 * time.Hour
)

// browserCacheControl is how long the reader's browser may keep an icon. A day
// is long enough that scrolling a dashboard costs no requests and short enough
// that a site's rebrand shows up without anybody clearing a cache.
const browserCacheControl = "public, max-age=86400"

// hostAllowed is what a hostname may contain before we will put it in a URL.
// The value reaching here comes from our own source table or from a stored
// referrer hostname, but it is still attacker-influenced — anybody can send a
// Referer header — and this is the line between "a hostname" and "a path
// fragment with a query string glued on".
const hostAllowed = "abcdefghijklmnopqrstuvwxyz0123456789.-"

// Favicons proxies and caches the icons the source rows are drawn with.
//
// The fetch happens on our server rather than in the reader's browser, and that
// is the entire reason this exists as a proxy rather than as an <img> pointing
// at a third party. A dashboard that loaded icons directly would announce, to
// every site that ever linked to yours, that somebody is looking at their
// referral traffic — one Referer header at a time, on a product whose pitch is
// that it does not do that.
type Favicons struct {
	// Dir is where fetched icons are written. An empty Dir keeps the cache in
	// memory only, which is what a test wants and what a read-only filesystem
	// forces.
	Dir string

	// Client is the HTTP client used upstream. It is a field so a test can
	// answer without a network and so the timeout is stated in one place.
	Client *http.Client

	Log *logger.Logger

	mu     sync.Mutex
	memory map[string]cached
}

// cached is one remembered answer, hit or miss.
type cached struct {
	body        []byte
	contentType string
	until       time.Time
}

// NewFavicons builds the proxy over a cache directory.
func NewFavicons(dir string, log *logger.Logger) *Favicons {
	return &Favicons{
		Dir:    dir,
		Client: &http.Client{Timeout: fetchTimeout},
		Log:    log,
		memory: map[string]cached{},
	}
}

// ServeHTTP answers one icon request.
//
// It never returns an error status. A row on a report card either has an icon
// or has a letter tile, and a 404 in that slot renders as a broken-image glyph
// that looks like a bug in the dashboard rather than an absence of an icon.
func (f *Favicons) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		name = strings.TrimPrefix(r.URL.Path, FaviconPrefix)
	}

	body, contentType := f.icon(r.Context(), name)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", browserCacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// icon resolves one source name to image bytes, going to the network at most
// once per name per cache lifetime.
func (f *Favicons) icon(ctx context.Context, name string) ([]byte, string) {
	host := hostFor(name)
	if host == "" {
		// A channel, a campaign tag or direct traffic. There is no site to
		// fetch an icon from, and pretending otherwise would spend a request to
		// arrive at the same tile.
		return tile(name), "image/svg+xml; charset=utf-8"
	}

	if hit, ok := f.remembered(host); ok {
		if hit.body == nil {
			return tile(name), "image/svg+xml; charset=utf-8"
		}

		return hit.body, hit.contentType
	}

	body, contentType, err := f.fetch(ctx, host)
	if err != nil {
		if f.Log != nil {
			f.Log.Debug("favicon fetch failed", "host", host, "error", err)
		}

		f.remember(host, cached{until: time.Now().Add(failureMaxAge)})

		return tile(name), "image/svg+xml; charset=utf-8"
	}

	f.remember(host, cached{body: body, contentType: contentType, until: time.Now().Add(successMaxAge)})

	return body, contentType
}

// remembered reads the memory cache, falling back to disk. Disk is what makes a
// restart cheap: without it, every deploy costs one upstream fetch per distinct
// source across every dashboard, which on a busy shard is a burst nobody asked
// for.
func (f *Favicons) remembered(host string) (cached, bool) {
	f.mu.Lock()
	hit, ok := f.memory[host]
	f.mu.Unlock()

	if ok && time.Now().Before(hit.until) {
		return hit, true
	}

	if f.Dir == "" {
		return cached{}, false
	}

	file := f.pathFor(host)

	info, err := os.Stat(file)
	if err != nil || time.Since(info.ModTime()) > successMaxAge {
		return cached{}, false
	}

	body, err := os.ReadFile(file)
	if err != nil || len(body) == 0 {
		return cached{}, false
	}

	entry := cached{body: body, contentType: "image/x-icon", until: info.ModTime().Add(successMaxAge)}

	f.mu.Lock()
	f.memory[host] = entry
	f.mu.Unlock()

	return entry, true
}

// remember stores an answer in memory, and a successful one on disk as well.
func (f *Favicons) remember(host string, entry cached) {
	f.mu.Lock()
	f.memory[host] = entry
	f.mu.Unlock()

	if f.Dir == "" || entry.body == nil {
		return
	}

	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return
	}

	// A partial file left by a crash would be served forever as a corrupt
	// image, so the write lands under a temporary name and is renamed into
	// place, which is atomic on every filesystem we run on.
	temporary := f.pathFor(host) + ".tmp"

	if err := os.WriteFile(temporary, entry.body, 0o644); err != nil {
		return
	}

	if err := os.Rename(temporary, f.pathFor(host)); err != nil {
		_ = os.Remove(temporary)
	}
}

// pathFor is where one host's icon is cached. The filename is a hash rather
// than the hostname because a hostname reaching the filesystem is a path
// traversal waiting to be found, and nothing ever needs to read this directory
// by eye.
func (f *Favicons) pathFor(host string) string {
	sum := sha256.Sum256([]byte(host))

	return filepath.Join(f.Dir, hex.EncodeToString(sum[:])[:32]+".ico")
}

// fetch pulls one icon from upstream.
func (f *Favicons) fetch(ctx context.Context, host string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(iconEndpoint, host), nil)
	if err != nil {
		return nil, "", err
	}

	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream answered %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxIconBytes))
	if err != nil {
		return nil, "", err
	}

	if len(body) == 0 {
		return nil, "", errors.New("upstream answered with an empty body")
	}

	contentType := response.Header.Get("Content-Type")

	// The response is written into an <img> on a page that also renders one
	// account's traffic. Anything that is not an image — an HTML error page a
	// proxy substituted, say — must not be echoed back with its own content
	// type attached.
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("upstream answered %q, not an image", contentType)
	}

	return body, contentType, nil
}

// hostFor turns a source name into a hostname safe to put in a URL path.
func hostFor(name string) string {
	host := strings.ToLower(referrer.HostFor(name))
	if host == "" || len(host) > 253 {
		return ""
	}

	for _, character := range host {
		if !strings.ContainsRune(hostAllowed, character) {
			return ""
		}
	}

	// A hostname with no dot is not a site, and a leading or trailing dot is
	// not a hostname at all.
	if !strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return ""
	}

	return host
}

// tilePalette is the set of backgrounds a letter tile picks from. They are the
// mid-weight steps of the neutral and accent families, so a page full of tiles
// reads as one system rather than as a bag of stickers.
var tilePalette = []string{"#0d9488", "#0284c7", "#7c3aed", "#c2410c", "#4f46e5", "#0f766e", "#b45309", "#be123c"}

// tile draws the fallback icon: the source's initial on a colour derived from
// its name.
//
// It is a real image rather than a blank space because a row with nothing in
// the icon slot reads as a failed load. Deriving the colour from the name means
// the same source is the same colour on every dashboard and after every
// restart, which is what lets somebody recognise a row without reading it.
func tile(name string) []byte {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		trimmed = "?"
	}

	initial := strings.ToUpper(string([]rune(trimmed)[0:1]))
	if initial == "<" || initial == "&" || initial == ">" || initial == `"` {
		initial = "?"
	}

	sum := sha256.Sum256([]byte(strings.ToLower(trimmed)))
	colour := tilePalette[int(sum[0])%len(tilePalette)]

	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" width="16" height="16">` +
		`<rect width="16" height="16" rx="3" fill="` + colour + `"/>` +
		`<text x="8" y="11.5" text-anchor="middle" fill="#ffffff" ` +
		`font-family="system-ui,-apple-system,sans-serif" font-size="10" font-weight="600">` +
		initial + `</text></svg>`)
}
