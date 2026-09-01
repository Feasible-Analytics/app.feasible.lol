//
// dashboard.go
// Serving the compiled React dashboard out of the binary.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package dashboard serves the stats dashboard: one HTML shell, two compiled
// assets, and a favicon proxy for the source rows.
//
// The assets are embedded rather than read from disk because the whole promise
// of this product is a single binary with nothing to copy alongside it. The
// JavaScript sources live in web/ and are compiled by web/build.mjs, which
// writes straight into this package's assets directory — `go:embed` cannot
// reach outside its own tree, and a Makefile step that copies the files is a
// step that gets skipped, leaving the binary serving a bundle whose source
// nobody can find.
//
// Everything the dashboard reads comes from POST /api/stats/{domain}/query.
// There is no per-card endpoint and there will not be one: every report in the
// product is the same request with different metrics and dimensions, and a
// handler per card is a handler per way for the same number to be wrong.
package dashboard

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
)

// assets holds the compiled bundle. The build writes app.js, app.css and
// index.html here; nothing else in the directory is served.
//
//go:embed assets/index.html assets/app.js assets/app.css
var assets embed.FS

// The paths this package answers on.
const (
	// PathPrefix is the SPA's mount point. Everything under it renders the same
	// shell, because the client owns its own routing — /dashboard/example.com
	// and /dashboard/example.com?period=7d are one document.
	PathPrefix = "/dashboard/"

	// AssetPrefix is where the two compiled files live.
	AssetPrefix = PathPrefix + "assets/"
)

// bootstrapPlaceholder is what the shell carries until a request fills it in.
// The site list is written into the page rather than fetched because it is the
// one thing every screen needs before it can ask a single question, and a round
// trip for it would put a blank frame in front of every load.
const bootstrapPlaceholder = "__BOOTSTRAP__"

// Cache lifetimes. The shell is re-rendered per request and must never be held,
// because it carries the site list; the assets are addressed by a digest in the
// query string and so can be held forever.
const (
	shellCacheControl = "no-cache, must-revalidate"
	assetCacheControl = "public, max-age=31536000, immutable"

	// A request for an asset without the digest is somebody's bookmark or a
	// hand-typed URL. It gets a short life rather than an immutable one, so a
	// deploy is not invisible to them for a year.
	unversionedCacheControl = "public, max-age=60"
)

// DomainSource is the routing map the site picker is built from. It is an
// interface rather than the concrete site cache so this package can be served
// by a process with no database, and so its tests do not need one.
type DomainSource interface {
	// Domains lists every domain currently accepting traffic.
	Domains() []string
}

// Handler serves the shell, the assets and nothing else.
type Handler struct {
	// Sites is where the site picker's list comes from. A nil Sites serves an
	// empty list, which renders the "no sites yet" screen rather than a broken
	// one.
	Sites DomainSource

	// Domains replaces the global site list for an authenticated request. The
	// serving process supplies a membership-aware resolver; shared/public shells
	// pass their own single-site bootstrap directly to WriteShell.
	Domains func(*http.Request) []string

	// shellHead and shellTail bracket the bootstrap placeholder, so filling it
	// in per request is two writes rather than a scan and a copy of the whole
	// document.
	shellHead []byte
	shellTail []byte

	// files is each asset's body, content type and digest, resolved once at
	// construction. Hashing on every request would put a SHA-256 of a quarter
	// of a megabyte in front of every page load.
	files map[string]asset
}

// asset is one compiled file, ready to write.
type asset struct {
	body        []byte
	contentType string

	// digest is the short content hash the shell puts in the asset's query
	// string. It is what makes the immutable cache lifetime safe: a new build
	// is a new URL, so nothing can serve a stale bundle against a new shell.
	digest string
}

// New builds the handler, resolving the shell and the asset digests once.
//
// It panics on a missing or unreadable asset. That is deliberate: the files are
// embedded at compile time, so a failure here means the binary was built
// without running the front-end build, and a process that starts and then
// serves a blank dashboard is far harder to diagnose than one that refuses to
// start with the reason.
func New(sites DomainSource) *Handler {
	h := &Handler{Sites: sites, files: map[string]asset{}}

	for name, contentType := range map[string]string{
		"app.js":  "text/javascript; charset=utf-8",
		"app.css": "text/css; charset=utf-8",
	} {
		body, err := assets.ReadFile("assets/" + name)
		if err != nil {
			panic(fmt.Sprintf("dashboard: %s is missing — run `make assets` before building: %v", name, err))
		}

		h.files[name] = asset{body: body, contentType: contentType, digest: digestOf(body)}
	}

	shell, err := assets.ReadFile("assets/index.html")
	if err != nil {
		panic(fmt.Sprintf("dashboard: index.html is missing — run `make assets` before building: %v", err))
	}

	rendered := string(shell)
	for name, file := range h.files {
		rendered = strings.ReplaceAll(rendered, placeholderFor(name), AssetPrefix+name+"?v="+file.digest)
	}

	head, tail, found := strings.Cut(rendered, bootstrapPlaceholder)
	if !found {
		panic("dashboard: the shell has no " + bootstrapPlaceholder + " placeholder to write the site list into")
	}

	h.shellHead, h.shellTail = []byte(head), []byte(tail)

	return h
}

// placeholderFor is the token the built shell carries for one asset.
func placeholderFor(name string) string {
	if strings.HasSuffix(name, ".css") {
		return "__CSS__"
	}

	return "__JS__"
}

// digestOf is the short content hash an asset is addressed by. Twelve base64
// characters is 72 bits, which is far more than enough to tell two builds apart
// and short enough to keep the URL readable in a network panel.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)

	return base64.RawURLEncoding.EncodeToString(sum[:])[:12]
}

// ServeHTTP routes one request to the asset it named, or to the shell.
//
// Anything under the prefix that is not a known asset renders the shell rather
// than 404ing, because the client owns its own routing and every path under
// /dashboard/ is a page it knows how to draw — including one it will redirect
// away from.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET the dashboard from this endpoint", http.StatusMethodNotAllowed)

		return
	}

	if strings.HasPrefix(r.URL.Path, AssetPrefix) {
		h.serveAsset(w, r, path.Base(r.URL.Path))

		return
	}

	h.serveShell(w, r)
}

// serveAsset writes one compiled file.
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	file, ok := h.files[name]
	if !ok {
		http.NotFound(w, r)

		return
	}

	cache := unversionedCacheControl
	if r.URL.Query().Get("v") == file.digest {
		cache = assetCacheControl
	}

	w.Header().Set("Content-Type", file.contentType)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", `"`+file.digest+`"`)

	// A revalidation costs a round trip and nothing else, which matters on the
	// unversioned path where the browser asks every minute.
	if match := r.Header.Get("If-None-Match"); match == `"`+file.digest+`"` {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		_, _ = w.Write(file.body)
	}
}

// Bootstrap is what the shell boots from. It is a type rather than an inline
// struct because the shared-link and public-dashboard handlers render the same
// shell with a different one, and two independent literals of the same shape
// would drift the moment either grew a field.
type Bootstrap struct {
	Sites []string `json:"sites"`

	// Locale is the tag the server negotiated, for Intl and the plural rules.
	Locale string `json:"locale"`

	// Messages is every string the dashboard can ask for, already merged over
	// English. They travel with the page for the same reason the site list
	// does: they are needed before the first paint, and fetching them would put
	// a frame of untranslated interface in front of every load. Arriving merged
	// is what stops the dashboard and the server-rendered screens growing two
	// different answers to "what does this locale say".
	Messages map[string]string `json:"messages"`

	// Shared is present only when the page is being served through a shared
	// link or a public dashboard. Its absence is what tells the front end it is
	// the ordinary authenticated dashboard.
	Shared *Shared `json:"shared,omitempty"`
}

// Shared is the read-only mode the dashboard runs in behind a share URL.
type Shared struct {
	// Mode is "share" or "public".
	Mode string `json:"mode"`

	// Base is the path prefix every URL the front end builds must keep.
	//
	// This field is the whole fix for a real bug: the incumbent's shared
	// dashboard dropped its /share/<token> segment as soon as a filter was
	// applied, so copying the URL after filtering produced a link that
	// redirected to a login and back forever. Handing the front end its base
	// path, rather than letting it assume /dashboard, makes that impossible.
	Base string `json:"base"`

	Domain string `json:"domain"`

	// Capability is sent on each stats request. For a shared link it is the
	// unguessable slug; for a public dashboard it is the literal word public.
	// The stats endpoint revalidates the backing row on every request.
	Capability string `json:"capability"`

	// Embed is set when the page is being rendered for an iframe. The front
	// end reads it to strip the chrome — and, more importantly, to stop
	// touching browser storage at all.
	Embed bool `json:"embed"`

	// Theme and Background are the embed parameters. They are applied by the
	// server rather than read from the URL by the client so that they only ever
	// take effect on a share URL, which is where they are documented to work.
	Theme      string `json:"theme,omitempty"`
	Background string `json:"background,omitempty"`

	// Storage says whether the front end may use localStorage.
	//
	// It is false in an embed, and that is not a preference. A third-party
	// iframe in Brave, or in any browser with third-party cookies blocked, does
	// not return null from a storage accessor — it *throws*. The incumbent's
	// unguarded read of localStorage killed their entire embedded dashboard for
	// those users, showing a blank frame with an exception nobody but the
	// visitor could see.
	Storage bool `json:"storage"`

	// SegmentID pins the view to one saved segment, or zero for the whole site.
	SegmentID int64 `json:"segment_id,omitempty"`
}

// serveShell writes the HTML with this instance's site list in it.
func (h *Handler) serveShell(w http.ResponseWriter, r *http.Request) {
	// The dashboard reads one account's traffic and must not be framed by
	// another site: a clickjacked dashboard is a way to make somebody delete a
	// site they meant to keep.
	//
	// It is also why the embed parameters do nothing here. They are documented
	// as working on a share URL only, and an authenticated dashboard that
	// honoured them would be an authenticated dashboard somebody had put in an
	// iframe — which is the thing this header exists to prevent.
	w.Header().Set("X-Frame-Options", "DENY")

	h.WriteShell(w, r, h.bootstrap(r))
}

// WriteShell renders the SPA shell with a caller-supplied bootstrap. It sets
// every header except the framing policy, which is the one decision that
// differs between the authenticated dashboard and an embeddable share link and
// so belongs to whichever handler knows the answer.
//
// The language is resolved here rather than by the caller, and before anything
// is written, because resolving it can set the cookie that remembers an
// explicit ?lang= choice, and a Set-Cookie after the status line is a header
// nobody receives. A caller that already filled the field keeps it, so a shared
// link can pin a locale the way it pins a theme.
func (h *Handler) WriteShell(w http.ResponseWriter, r *http.Request, boot Bootstrap) {
	if boot.Locale == "" {
		boot.Locale = i18n.Apply(w, r)
	}

	if boot.Messages == nil {
		boot.Messages = i18n.Messages(boot.Locale)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", shellCacheControl)
	w.Header().Set("Referrer-Policy", "same-origin")

	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}

	_, _ = w.Write(h.shellHead)
	_, _ = w.Write(encodeBootstrap(boot))
	_, _ = w.Write(h.shellTail)
}

// bootstrap builds the site list the authenticated shell boots from.
//
// The list is sorted so that two loads of the same install produce the same
// document, which is what makes the shell diffable and the site picker's order
// stable rather than following whatever order a map iterated in.
func (h *Handler) bootstrap(r *http.Request) Bootstrap {
	domains := []string{}
	if h.Domains != nil {
		domains = h.Domains(r)
	} else if h.Sites != nil {
		domains = h.Sites.Domains()
	}

	sort.Strings(domains)

	return Bootstrap{Sites: domains}
}

// encodeBootstrap renders the boot blob written into the shell.
func encodeBootstrap(boot Bootstrap) []byte {
	if boot.Sites == nil {
		boot.Sites = []string{}
	}

	if boot.Messages == nil {
		boot.Messages = map[string]string{}
	}

	body, err := json.Marshal(boot)
	if err != nil {
		// Everything being encoded is strings, bools and ints, so this cannot
		// fail; answering with an empty payload rather than a 500 means a
		// hypothetical failure costs the site picker and the translations, not
		// the page.
		return []byte(`{"sites":[],"locale":"` + i18n.DefaultLocale + `","messages":{}}`)
	}

	return body
}

// AssetNames lists what is embedded, for a start-up log line and for the test
// that checks the build ran. A binary serving a dashboard nobody compiled is
// the one failure mode of embedding that is silent.
func AssetNames() []string {
	var names []string

	_ = fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		names = append(names, path.Base(p))

		return nil
	})

	sort.Strings(names)

	return names
}
