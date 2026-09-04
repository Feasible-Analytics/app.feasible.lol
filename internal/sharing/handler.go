//
// handler.go
// Serving a dashboard to somebody who has no account.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/dashboard"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// The two mount points. They are constants because the front end has to keep
// the prefix on every URL it builds, and a prefix that lives in two places is a
// prefix that will eventually be spelled two ways.
const (
	SharePrefix  = "/share/"
	PublicPrefix = "/public/"
)

// The route patterns this package answers on.
const (
	SharePattern  = SharePrefix
	PublicPattern = PublicPrefix
)

// Shell renders the dashboard SPA. It is an interface so this package can be
// tested without the compiled front-end bundle, and so the two share modes go
// through exactly the same shell as the authenticated dashboard rather than a
// second copy that drifts.
type Shell interface {
	WriteShell(w http.ResponseWriter, r *http.Request, boot dashboard.Bootstrap)
}

// Handler serves public dashboards and shared links.
type Handler struct {
	Store    *Store
	Shell    Shell
	Security Security
	Log      *logger.Logger

	// Secret signs the cookie that records a solved password.
	Secret []byte
}

// New builds the handler.
func New(store *Store, shell Shell, security Security, secret []byte, log *logger.Logger) *Handler {
	return &Handler{Store: store, Shell: shell, Security: security, Secret: secret, Log: log}
}

// ServeHTTP routes one request. Everything under a share or public prefix that
// is not the password endpoint renders the shell, because the front end owns
// its own routing and every path beneath the prefix is a view it can draw.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Security.Apply(w, r) {
		return
	}

	// Nothing under here may be framed unless a specific decision below says
	// otherwise. Setting the safe value first means a path that forgets to
	// decide inherits the safe answer.
	DenyFraming(w)

	switch {
	case strings.HasPrefix(r.URL.Path, PublicPrefix):
		h.servePublic(w, r)

	case strings.HasPrefix(r.URL.Path, SharePrefix):
		h.serveShare(w, r)

	default:
		http.NotFound(w, r)
	}
}

// servePublic renders a site that has been made fully public.
//
// Public means public: no token, no cookie, a stable URL somebody can put in a
// blog post that still resolves next year. It is also why embed parameters work
// here — a public dashboard is already readable by anyone, so framing it adds
// no exposure.
func (h *Handler) servePublic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET a public dashboard from this endpoint", http.StatusMethodNotAllowed)

		return
	}

	domain := firstSegment(strings.TrimPrefix(r.URL.Path, PublicPrefix))
	if domain == "" {
		http.NotFound(w, r)

		return
	}

	link, err := h.Store.PublicSite(r.Context(), domain)
	if err != nil {
		h.notFound(w, r, err)

		return
	}

	embed := h.embedFrom(r)
	if embed.Embed {
		AllowFraming(w)
	}

	h.Shell.WriteShell(w, r, dashboard.Bootstrap{
		Sites: []string{link.Domain},
		Shared: &dashboard.Shared{
			Mode:       "public",
			Base:       PublicPrefix + link.Domain,
			Domain:     link.Domain,
			Capability: "public",
			Embed:      embed.Embed,
			Theme:      embed.Theme,
			Background: embed.Background,
			Storage:    !embed.Embed,
		},
	})
}

// serveShare renders a tokenised link, asking for the password first when there
// is one.
func (h *Handler) serveShare(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, SharePrefix)
	slug := firstSegment(rest)

	if slug == "" {
		http.NotFound(w, r)

		return
	}

	link, err := h.Store.Resolve(r.Context(), slug)
	if err != nil {
		h.notFound(w, r, err)

		return
	}

	if strings.HasSuffix(strings.TrimSuffix(rest, "/"), "/password") {
		h.servePassword(w, r, link)

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET a shared dashboard from this endpoint", http.StatusMethodNotAllowed)

		return
	}

	embed := h.embedFrom(r)

	// The refusal this package exists to make. A password-protected link can
	// never be embedded, because the form that takes the password would have to
	// be framable to work — and a framable login form is one an attacker can
	// put invisibly under a button on their own page.
	if embed.Embed && !link.Embeddable() {
		h.refuseEmbed(w, link)

		return
	}

	if link.HasPassword && !h.solved(r, link.Slug) {
		h.passwordForm(w, link, "")

		return
	}

	if embed.Embed {
		AllowFraming(w)
	}

	h.Shell.WriteShell(w, r, dashboard.Bootstrap{
		Sites: []string{link.Domain},
		Shared: &dashboard.Shared{
			Mode:       "share",
			Base:       link.Path(),
			Domain:     link.Domain,
			Capability: link.Slug,
			Embed:      embed.Embed,
			Theme:      embed.Theme,
			Background: embed.Background,
			Storage:    !embed.Embed,
			SegmentID:  link.SegmentID,
		},
	})
}

// servePassword takes one attempt at a link's password.
//
// It answers 404 to a GET so the form is only ever reachable from the link
// itself, and it never becomes framable — the endpoint that would have to drop
// X-Frame-Options for an embed to work is this one, which is why embeds of a
// protected link are refused before anything reaches here.
func (h *Handler) servePassword(w http.ResponseWriter, r *http.Request, link Link) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST the password to this endpoint", http.StatusMethodNotAllowed)

		return
	}

	if err := r.ParseForm(); err != nil {
		h.passwordForm(w, link, "That form could not be read. Try again.")

		return
	}

	err := h.Store.CheckPasswordForSource(r.Context(), link.ID, PasswordSourceKey(h.Secret, r),
		r.PostFormValue("password"))

	switch {
	case errors.Is(err, ErrPasswordThrottled):
		w.Header().Set("Retry-After", "900")
		h.passwordFormStatus(w, link, "Too many attempts. Try again later.", http.StatusTooManyRequests)

		return

	case errors.Is(err, ErrWrongPassword):
		h.passwordForm(w, link, "That password is not correct.")

		return

	case err != nil:
		h.notFound(w, r, err)

		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:  CookieName(link.Slug),
		Value: SignSlug(h.Secret, link.Slug),
		Path:  "/",

		// HttpOnly because nothing on the page reads it, and SameSite=Lax
		// because the cookie is only ever presented by somebody following the
		// link. Neither costs anything here and both remove a way to misuse it.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.Security.RequireHTTPS,
	})

	http.Redirect(w, r, link.Path(), http.StatusSeeOther)
}

// solved reports whether this browser has already answered the password.
func (h *Handler) solved(r *http.Request, slug string) bool {
	cookie, err := r.Cookie(CookieName(slug))
	if err != nil {
		return false
	}

	return ValidSignature(h.Secret, slug, cookie.Value)
}

// embedParams is the three parameters an embed URL may carry.
type embedParams struct {
	Embed      bool
	Theme      string
	Background string
}

// embedFrom reads the embed parameters.
//
// They are read here — on a share or public URL — and nowhere else. That is the
// documented rule and it is enforced by where this function is called from
// rather than by a note in the interface: somebody who pastes ?embed=true onto
// their authenticated dashboard URL gets the ordinary dashboard, because the
// authenticated handler never asks.
func (h *Handler) embedFrom(r *http.Request) embedParams {
	query := r.URL.Query()

	return embedParams{
		Embed:      strings.EqualFold(strings.TrimSpace(query.Get("embed")), "true"),
		Theme:      NormaliseTheme(query.Get("theme")),
		Background: NormaliseBackground(query.Get("background")),
	}
}

// notFound answers a missing or private link. Every failure in here answers the
// same way, so the endpoint cannot be used to work out which slugs exist.
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request, cause error) {
	if !errors.Is(cause, ErrNotFound) && h.Log != nil {
		h.Log.Error("a shared link could not be served", "route", "shared dashboard", "error", cause)
	}

	http.Error(w, "This link does not exist, or it has been revoked.", http.StatusNotFound)
}

// refuseEmbed explains why a password-protected link will not render in a
// frame. It is a plain page with the reason on it rather than a 404, because
// the person seeing it is the customer who built the embed and the only useful
// response is telling them what to do instead.
func (h *Handler) refuseEmbed(w http.ResponseWriter, link Link) {
	DenyFraming(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)

	_ = refusalTemplate.Execute(w, map[string]string{"Path": link.Path()})
}

// passwordForm renders the gate.
func (h *Handler) passwordForm(w http.ResponseWriter, link Link, problem string) {
	status := http.StatusOK
	if problem != "" {
		status = http.StatusUnauthorized
	}
	h.passwordFormStatus(w, link, problem, status)
}

// passwordFormStatus renders the gate with the caller-selected authentication
// or throttling status while preserving identical framing and cache policy.
func (h *Handler) passwordFormStatus(w http.ResponseWriter, link Link, problem string, status int) {
	DenyFraming(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	w.WriteHeader(status)

	_ = passwordTemplate.Execute(w, map[string]string{
		"Domain":  link.Domain,
		"Action":  link.Path() + "/password",
		"Problem": problem,
	})
}

// firstSegment reads the first path segment of a trimmed path.
func firstSegment(path string) string {
	path = strings.TrimPrefix(path, "/")

	segment, _, _ := strings.Cut(path, "/")

	return segment
}

// The two server-rendered pages in this package. They are here rather than in a
// template directory because they are the only HTML this package owns, they
// have no shared layout with anything else, and a page whose whole job is to
// take one field does not need a build step.
var passwordTemplate = template.Must(template.New("password").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{.Domain}} · protected dashboard</title>
<style>
  :root { color-scheme: light dark; }
  @font-face { font-family:"Archivo"; font-style:normal; font-weight:100 900; font-display:swap;
               src:url("/app/assets/fonts/archivo-latin-wght-normal.woff2") format("woff2-variations"); }
  body { margin:0; min-height:100vh; display:grid; place-items:center; background:#eae9e9; color:#444141;
         font:15px/1.5 "Archivo",ui-sans-serif,system-ui,-apple-system,"Segoe UI",Helvetica,Arial,sans-serif; }
  .brand { font-size:18px; font-weight:800; letter-spacing:-.025em; color:#201e1d; margin:0 0 14px; }
  .brand b { color:#ec3013; }
  .card { background:#f3f2f2; border:2px solid #9f9d9d; padding:24px; }
  h1 { font-size:20px; font-weight:800; letter-spacing:-.018em; margin:0 0 4px; color:#201e1d; }
  p { margin:0 0 16px; color:#605d5d; font-size:14px; }
  a { color:#ae1800; }
  .card { width:min(360px,92vw); }
  label { display:block; font-size:13px; font-weight:700; margin-bottom:6px; color:#201e1d; }
  input { width:100%; box-sizing:border-box; padding:10px; font-size:15px; font:inherit;
          background:#f3f2f2; border:2px solid #201e1d; color:#201e1d; }
  button { margin-top:14px; width:100%; padding:12px; font-size:15px; font-weight:800; font-family:inherit;
           background:#ec3013; color:#f3f2f2; border:2px solid transparent; cursor:pointer; }
  button:hover { background:#dd2b0f; }
  .problem { background:#f8e4e3; border:2px solid #b91c1c; color:#991b1b;
             padding:8px 10px; font-size:13px; margin-bottom:14px; }
  @media (prefers-color-scheme: dark) {
    body { background:#161514; color:#eae7e7; }
    .brand { color:#f3f2f2; } .brand b { color:#ff5a3c; }
    .card { background:#201e1d; border-color:#5a5654; }
    h1 { color:#f3f2f2; }
    p { color:#bab6b6; }
    a { color:#ff8f77; }
    input { background:#161514; border-color:#8d8987; color:#eae7e7; }
    button { background:#ff5a3c; color:#161514; }
    .problem { background:#2a1614; border-color:#f87171; color:#fca5a5; }
  }
</style>
</head><body>
<div>
<p class="brand">Feasible<b>.lol</b></p>
<form class="card" method="post" action="{{.Action}}">
  <h1>{{.Domain}}</h1>
  <p>This dashboard is protected. Enter the password you were given.</p>
  {{if .Problem}}<div class="problem">{{.Problem}}</div>{{end}}
  <label for="password">Password</label>
  <input id="password" name="password" type="password" autocomplete="current-password" autofocus>
  <button type="submit">View the dashboard</button>
</form>
</div>
</body></html>`))

var refusalTemplate = template.Must(template.New("refusal").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>This link cannot be embedded</title>
<style>
  :root { color-scheme: light dark; }
  @font-face { font-family:"Archivo"; font-style:normal; font-weight:100 900; font-display:swap;
               src:url("/app/assets/fonts/archivo-latin-wght-normal.woff2") format("woff2-variations"); }
  body { margin:0; min-height:100vh; display:grid; place-items:center; background:#eae9e9; color:#444141;
         font:15px/1.5 "Archivo",ui-sans-serif,system-ui,-apple-system,"Segoe UI",Helvetica,Arial,sans-serif; }
  .brand { font-size:18px; font-weight:800; letter-spacing:-.025em; color:#201e1d; margin:0 0 14px; }
  .brand b { color:#ec3013; }
  .card { background:#f3f2f2; border:2px solid #9f9d9d; padding:24px; }
  h1 { font-size:20px; font-weight:800; letter-spacing:-.018em; margin:0 0 4px; color:#201e1d; }
  p { margin:0 0 16px; color:#605d5d; font-size:14px; }
  a { color:#ae1800; }
  .card { width:min(520px,92vw); }
  p { margin:0 0 12px; }
  code { background:#eae7e7; border:1px solid #d1d0d0; padding:1px 5px; font-size:13px; }
  @media (prefers-color-scheme: dark) {
    body { background:#161514; color:#eae7e7; }
    .brand { color:#f3f2f2; } .brand b { color:#ff5a3c; }
    .card { background:#201e1d; border-color:#5a5654; }
    h1 { color:#f3f2f2; }
    p { color:#bab6b6; }
    a { color:#ff8f77; }
    code { background:#161514; border-color:#3a3735; }
  }
</style>
</head><body>
<div>
<p class="brand">Feasible<b>.lol</b></p>
<div class="card">
  <h1>A password-protected link cannot be embedded</h1>
  <p>Embedding this dashboard would mean serving its password form inside your page's frame. A login
     form that any site is allowed to frame is a form an attacker can place invisibly under a button
     on their own page — a clickjacking attack — and there is no setting that makes that safe.</p>
  <p>Two things work instead: create a second shared link with no password and embed that one, or
     make the site fully public and embed <code>/public/&lt;domain&gt;</code>.</p>
  <p><a href="{{.Path}}">Open this dashboard directly</a></p>
</div>
</div>
</body></html>`))
