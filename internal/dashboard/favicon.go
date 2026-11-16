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
	"net/netip"
	"os"
	"path/filepath"
	"sort"
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
// that a site's rebrand shows up without anybody clearing a cache. It is
// private because whether a name gets an icon depends on who is asking.
const browserCacheControl = "private, max-age=86400"

// refusedCacheControl goes on a tile that stands in for an icon we declined to
// look up. The same URL may deserve a real icon a moment later, once a report
// has listed the name or the rate limit has passed.
const refusedCacheControl = "no-store"

// Bounds on what the proxy holds. Every one is a ceiling on what a stranger
// can make this process keep.
const (
	// DefaultMaxDiskBytes caps the cache directory. A typical icon is a few
	// kilobytes, so this is tens of thousands of sources.
	DefaultMaxDiskBytes = 64 << 20

	// DefaultMaxMemoryBytes caps the icon bytes held in memory.
	DefaultMaxMemoryBytes = 16 << 20

	// maxMemoryEntries caps the entries held in memory, misses included, since
	// a miss weighs nothing and would otherwise never count against the bytes.
	maxMemoryEntries = 20000

	// maxScopes and maxNamesPerScope cap the record of which names each
	// dashboard view has been shown.
	maxScopes        = 5000
	maxNamesPerScope = 2000
)

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

	// Scope names the dashboard view a request belongs to, after proving the
	// request may read it. It is the same key the stats endpoint passes to
	// Allow. Nil, or false, means the request has no view: only the built-in
	// sources get an icon then.
	Scope func(*http.Request) (string, bool)

	// Limit reports whether this request may cost an upstream fetch. Nil
	// allows every one.
	Limit func(*http.Request) bool

	// MaxDiskBytes and MaxMemoryBytes cap the two halves of the cache. Zero
	// takes the defaults.
	MaxDiskBytes   int64
	MaxMemoryBytes int64

	mu          sync.Mutex
	memory      map[string]cached
	memoryBytes int64
	scopes      map[string]*scope

	disk      sync.Mutex
	diskBytes int64
	diskKnown bool
}

// scope is the set of source names one dashboard view has been shown.
type scope struct {
	names map[string]struct{}
	used  time.Time
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
		scopes: map[string]*scope{},
	}
}

// Allow records that a dashboard view was shown these source names, which is
// what entitles a request from that view to an icon for one of them. The stats
// endpoint calls it with every source report it answers.
func (f *Favicons) Allow(key string, names []string) {
	if key == "" || len(names) == 0 {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	entry, ok := f.scopes[key]
	if !ok || len(entry.names)+len(names) > maxNamesPerScope {
		entry = &scope{names: make(map[string]struct{}, len(names))}
		f.scopes[key] = entry
	}

	entry.used = time.Now()

	for _, name := range names {
		if len(entry.names) >= maxNamesPerScope {
			break
		}

		entry.names[name] = struct{}{}
	}

	if len(f.scopes) <= maxScopes {
		return
	}

	// Over the ceiling, the views nobody has looked at longest go first. An
	// open dashboard re-registers its names on its next refresh.
	keys := make([]string, 0, len(f.scopes))
	for candidate := range f.scopes {
		keys = append(keys, candidate)
	}

	sort.Slice(keys, func(i, j int) bool { return f.scopes[keys[i]].used.Before(f.scopes[keys[j]].used) })

	for _, candidate := range keys[:len(keys)-maxScopes*3/4] {
		delete(f.scopes, candidate)
	}
}

// entitled reports whether this request may have a real icon for this name: a
// built-in source, or a name a report already showed to the view it comes from.
func (f *Favicons) entitled(r *http.Request, name string) bool {
	if referrer.Known(name) {
		return true
	}

	if f.Scope == nil {
		return false
	}

	key, ok := f.Scope(r)
	if !ok || key == "" {
		return false
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	entry, ok := f.scopes[key]
	if !ok {
		return false
	}

	_, shown := entry.names[name]

	return shown
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

	body, contentType, cacheControl := f.icon(r, name)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// tileType is the content type of the fallback tile.
const tileType = "image/svg+xml; charset=utf-8"

// icon resolves one source name to image bytes, going to the network at most
// once per name per cache lifetime, and only for a name the request is
// entitled to.
func (f *Favicons) icon(r *http.Request, name string) (body []byte, contentType, cacheControl string) {
	host := hostFor(name)
	if host == "" {
		// A channel, a campaign tag or direct traffic. There is no site to
		// fetch an icon from, and pretending otherwise would spend a request to
		// arrive at the same tile.
		return tile(name), tileType, browserCacheControl
	}

	// A name no report has shown this viewer gets the tile without touching
	// the cache or the network. Anything else would let a stranger fill the
	// disk with names of their choosing, use this server to fetch for them,
	// and learn from the answer which sites send other accounts traffic.
	if !f.entitled(r, name) {
		return tile(name), tileType, refusedCacheControl
	}

	if hit, ok := f.remembered(host); ok {
		if hit.body == nil {
			return tile(name), tileType, browserCacheControl
		}

		return hit.body, hit.contentType, browserCacheControl
	}

	if f.Limit != nil && !f.Limit(r) {
		if f.Log != nil {
			f.Log.Warn("favicon fetch refused by the rate limit", "host", host)
		}

		return tile(name), tileType, refusedCacheControl
	}

	fetched, fetchedType, err := f.fetch(r.Context(), host)
	if err != nil {
		if f.Log != nil {
			f.Log.Debug("favicon fetch failed", "host", host, "error", err)
		}

		f.remember(host, cached{until: time.Now().Add(failureMaxAge)})

		return tile(name), tileType, browserCacheControl
	}

	f.remember(host, cached{body: fetched, contentType: fetchedType, until: time.Now().Add(successMaxAge)})

	return fetched, fetchedType, browserCacheControl
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

	f.hold(host, entry)

	return entry, true
}

// hold puts one answer in memory and keeps the memory half under its caps.
func (f *Favicons) hold(host string, entry cached) {
	limit := f.MaxMemoryBytes
	if limit <= 0 {
		limit = DefaultMaxMemoryBytes
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if previous, ok := f.memory[host]; ok {
		f.memoryBytes -= int64(len(previous.body))
	}

	f.memory[host] = entry
	f.memoryBytes += int64(len(entry.body))

	if f.memoryBytes <= limit && len(f.memory) <= maxMemoryEntries {
		return
	}

	// Expired answers go first, then whatever the map yields until there is
	// headroom again. What is dropped here is still on disk when it was a hit.
	now := time.Now()

	for candidate, held := range f.memory {
		if candidate != host && now.After(held.until) {
			f.memoryBytes -= int64(len(held.body))
			delete(f.memory, candidate)
		}
	}

	for candidate, held := range f.memory {
		if f.memoryBytes <= limit*3/4 && len(f.memory) <= maxMemoryEntries*3/4 {
			break
		}

		if candidate == host {
			continue
		}

		f.memoryBytes -= int64(len(held.body))
		delete(f.memory, candidate)
	}
}

// remember stores an answer in memory, and a successful one on disk as well.
func (f *Favicons) remember(host string, entry cached) {
	f.hold(host, entry)

	if f.Dir == "" || entry.body == nil {
		return
	}

	f.disk.Lock()
	defer f.disk.Unlock()

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

		return
	}

	limit := f.MaxDiskBytes
	if limit <= 0 {
		limit = DefaultMaxDiskBytes
	}

	f.diskBytes += int64(len(entry.body))

	if !f.diskKnown || f.diskBytes > limit {
		f.prune(limit, f.pathFor(host))
	}
}

// prune measures the cache directory and, when it is over the limit, deletes
// the oldest icons until it is a quarter under it, sparing the one just
// written. The caller holds f.disk.
//
// The directory is read rather than trusted from a running total because the
// total starts unknown after a restart and drifts when an icon is rewritten.
func (f *Favicons) prune(limit int64, keep string) {
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return
	}

	type file struct {
		path     string
		size     int64
		modified time.Time
	}

	files := make([]file, 0, len(entries))

	var total int64

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || entry.IsDir() {
			continue
		}

		files = append(files, file{path: filepath.Join(f.Dir, entry.Name()), size: info.Size(), modified: info.ModTime()})
		total += info.Size()
	}

	if total > limit {
		sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })

		removed := 0

		for _, candidate := range files {
			if total <= limit*3/4 {
				break
			}

			if candidate.path == keep {
				continue
			}

			if err := os.Remove(candidate.path); err == nil {
				total -= candidate.size
				removed++
			}
		}

		if f.Log != nil {
			f.Log.Info("favicon cache pruned", "removed", removed, "bytes", total)
		}
	}

	f.diskBytes = total
	f.diskKnown = true
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
func (f *Favicons) fetch(ctx context.Context, host string) (body []byte, contentType string, err error) {
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
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close upstream response: %w", closeErr))
		}
	}()

	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream answered %d", response.StatusCode)
	}

	// One byte past the cap is read so an oversized icon is refused instead of
	// stored cut short.
	body, err = io.ReadAll(io.LimitReader(response.Body, maxIconBytes+1))
	if err != nil {
		return nil, "", err
	}

	if len(body) > maxIconBytes {
		return nil, "", fmt.Errorf("upstream answered with more than %d bytes", maxIconBytes)
	}

	if len(body) == 0 {
		return nil, "", errors.New("upstream answered with an empty body")
	}

	contentType = response.Header.Get("Content-Type")

	// The response is written into an <img> on a page that also renders one
	// account's traffic. Anything that is not an image — an HTML error page a
	// proxy substituted, say — must not be echoed back with its own content
	// type attached.
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("upstream answered %q, not an image", contentType)
	}

	// SVG is the one image type that is also a document: opened directly rather
	// than through an <img>, a script inside one runs on this origin. No icon
	// service answers with it, so refusing it costs nothing.
	if strings.HasPrefix(contentType, "image/svg") {
		return nil, "", errors.New("upstream answered with SVG, which is not served back as an icon")
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

	if !publicHostname(host) {
		return ""
	}

	return host
}

// privateSuffixes are the endings of names that only resolve inside somebody's
// network, or are reserved and resolve nowhere. None of them is a site with an
// icon, and none of them is a name we should be asking a third party about.
var privateSuffixes = []string{
	"local", "localhost", "localdomain", "internal", "intranet", "lan", "home", "corp",
	"private", "test", "example", "invalid", "onion", "arpa",
}

// publicHostname reports whether a lower-cased name is shaped like a hostname
// on the public internet: dotted labels, an alphabetic top-level domain, not an
// address and not an internal name.
func publicHostname(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}

	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}

	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}

	// A numeric last label is an address in some spelling, such as a decimal
	// or shortened IPv4, and never a registered domain.
	last := labels[len(labels)-1]
	if len(last) < 2 || strings.ContainsAny(last, "0123456789") && !strings.HasPrefix(last, "xn--") {
		return false
	}

	for _, suffix := range privateSuffixes {
		if last == suffix {
			return false
		}
	}

	return true
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
