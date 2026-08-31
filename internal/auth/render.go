//
// render.go
// Parsing the embedded templates once and rendering a page from them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// templateFS and assetFS hold the interface. Both are embedded because a
// release is one binary: an assets directory that has to be copied next to it
// is a directory that will be missing on somebody's server.
//
//go:embed templates
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// views is every page, pre-parsed. Each page gets its own template set
// containing the layout, every partial and that one page, because two pages
// both defining "content" cannot live in the same set.
type views struct {
	pages map[string]*template.Template
}

// newViews parses the whole template tree. A parse error here stops the process
// starting, which is where a broken template belongs — the alternative is a 500
// on whichever page nobody opens until a customer does.
func newViews() (*views, error) {
	entries, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil {
		return nil, fmt.Errorf("auth: find templates: %w", err)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("auth: no page templates were embedded")
	}

	v := &views{pages: map[string]*template.Template{}}

	for _, entry := range entries {
		name := strings.TrimSuffix(path.Base(entry), ".html")

		tpl, err := template.New("layout.html").Funcs(templateFuncs()).ParseFS(templateFS,
			"templates/layout.html", "templates/partials/*.html", entry)
		if err != nil {
			return nil, fmt.Errorf("auth: parse %s: %w", entry, err)
		}

		v.pages[name] = tpl
	}

	return v, nil
}

// page is what every template is rendered with. It is one struct rather than a
// bare map so that a typo in a field name is a compile error in the handler
// rather than a silently empty value on the page.
type page struct {
	Title string
	Nav   string

	User *User
	Team *Team

	// CSRF is the token the forms submit back. Every page carries one, whether
	// or not it has a form, so that a form added later cannot be added without
	// it.
	CSRF string

	// Flash and Error are the two messages a page can carry. They are separate
	// because they are styled and announced differently, and a success rendered
	// as a failure is worse than no message.
	Flash string
	Error string

	// GoogleEnabled decides whether the sign-in button is drawn at all. With no
	// credentials configured the button would start an OAuth flow that cannot
	// finish, so it is not shown.
	GoogleEnabled bool

	BaseURL string
	Now     time.Time

	// Data is the page's own values. It is a map because each page needs a
	// different shape and a struct per page would be forty types that exist to
	// be constructed once.
	Data map[string]any
}

// newPage builds the common part of a render. Every handler goes through it so
// that no page can be rendered without the CSRF token, the signed-in user or
// the base URL.
func (h *Handler) newPage(r *http.Request, title, nav string) *page {
	p := &page{
		Title:         title,
		Nav:           nav,
		User:          userFrom(r),
		CSRF:          h.csrfToken(r),
		GoogleEnabled: h.Google.Configured(),
		BaseURL:       h.BaseURL,
		Now:           h.Store.Now(),
		Data:          map[string]any{},
	}

	if p.User != nil {
		if team, err := h.Store.TeamForUser(r.Context(), p.User.ID); err == nil {
			p.Team = team
		}
	}

	return p
}

// render writes a page. It renders into a buffer first so that a template error
// halfway down produces an error page rather than half a page followed by a
// stack trace — which is what happens when a template writes straight to the
// response and then fails.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, p *page, status int) {
	tpl, ok := h.views.pages[name]
	if !ok {
		h.Log.Error("no such template", "template", name, "path", r.URL.Path)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	var buf bytes.Buffer

	if err := tpl.ExecuteTemplate(&buf, "layout.html", p); err != nil {
		h.Log.Error("template failed", "template", name, "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// The cookie is refreshed on every render, so a tab left open all day still
	// holds a token the next submission can be checked against.
	h.issueCSRF(w, p.CSRF)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// These pages carry session cookies and forms. A cached copy in a shared
	// proxy is somebody else's account rendered into your browser.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)

	_, _ = buf.WriteTo(w)
}

// templateFuncs are the helpers the templates may call. The list is short on
// purpose: logic in a template is logic no test covers.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// ago turns a unix timestamp into "3 minutes ago". The sessions screen
		// is a list of times, and absolute timestamps make "is one of these not
		// me" a subtraction the reader has to do in their head.
		"ago": func(unix int64) string {
			if unix <= 0 {
				return "never"
			}

			d := time.Since(time.Unix(unix, 0))

			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d hours ago", int(d.Hours()))
			case d < 48*time.Hour:
				return "yesterday"
			default:
				return fmt.Sprintf("%d days ago", int(d.Hours()/24))
			}
		},

		// until is the countdown the dual-write banner shows, so somebody can
		// see how long the old domain keeps working.
		"until": func(unix int64) string {
			d := time.Until(time.Unix(unix, 0))
			if d <= 0 {
				return "expired"
			}

			if d < time.Hour {
				return fmt.Sprintf("%d minutes", int(d.Minutes()))
			}

			return fmt.Sprintf("%d hours", int(d.Hours()))
		},

		// date formats a timestamp for the places a real date reads better than
		// a relative one.
		"date": func(unix int64) string {
			if unix <= 0 {
				return "—"
			}

			return time.Unix(unix, 0).Format("2 Jan 2006")
		},

		// sparkline turns a series into an SVG polyline.
		//
		// It is drawn here rather than by a charting library because it is
		// twenty points with no axes, no labels and no interaction, and a
		// dependency for that would be more code than the chart.
		"sparkline": sparklinePath,

		// dict builds a map inline, which is how a partial is given more than
		// one value without inventing a type for every one of them.
		"dict": func(values ...any) map[string]any {
			out := map[string]any{}

			for i := 0; i+1 < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					continue
				}

				out[key] = values[i+1]
			}

			return out
		},

		"add": func(a, b int) int { return a + b },
	}
}

// sparklineWidth and sparklineHeight are the drawing box. They are the viewBox
// rather than the rendered size: the SVG scales to whatever the layout gives
// it, so the numbers only set the aspect ratio.
const (
	sparklineWidth  = 120
	sparklineHeight = 28
)

// sparklinePath turns a series of counts into an SVG polyline's points
// attribute.
//
// The series is normalised to its own maximum, which is the right choice for a
// list of unrelated sites: a shared scale would flatten every small site into a
// straight line beside one busy one, and the question this chart answers is
// "which way is this one going", not "how do these compare".
func sparklinePath(series []int64) template.HTMLAttr {
	if len(series) < 2 {
		return template.HTMLAttr("")
	}

	var max int64
	for _, v := range series {
		if v > max {
			max = v
		}
	}

	if max == 0 {
		// A flat line along the bottom, rather than nothing at all: an empty
		// box next to a site reads as "broken", a flat line reads as "quiet".
		return template.HTMLAttr(fmt.Sprintf("0,%d %d,%d", sparklineHeight-1, sparklineWidth, sparklineHeight-1))
	}

	var b strings.Builder

	step := float64(sparklineWidth) / float64(len(series)-1)

	for i, v := range series {
		if i > 0 {
			b.WriteByte(' ')
		}

		x := float64(i) * step
		y := float64(sparklineHeight-2) * (1 - float64(v)/float64(max))

		fmt.Fprintf(&b, "%.1f,%.1f", x, y+1)
	}

	return template.HTMLAttr(b.String())
}

// assetHandler serves the embedded CSS and JavaScript.
//
// They are fingerprint-free and cached for a day rather than a year: the
// dashboard is behind a login, so a stale asset costs one person one reload,
// while a year-long cache means a fix nobody sees until next spring.
func assetHandler() http.Handler {
	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic(fmt.Sprintf("auth: embedded assets are missing: %v", err))
	}

	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}
