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
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/appui"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/avatar"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
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

		// The settings chrome comes from the package both settings surfaces
		// share, so there is one copy of that markup rather than two.
		if tpl, err = tpl.ParseFS(appui.Templates, "templates/*.html"); err != nil {
			return nil, fmt.Errorf("auth: parse settings chrome for %s: %w", entry, err)
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

	// Settings is the shared settings chrome, present only on the screens that
	// belong to that surface. Its presence is what selects the shell, because
	// the settings screens in this package and the ones in internal/settings
	// are one screen to the reader and must not arrive wearing two shells.
	Settings *appui.Shell

	// Header is the bar every signed-in screen wears. It is a pointer so the
	// signed-out and focused shells, which have no account to draw, can leave
	// it out rather than carry an empty one.
	Header *appui.Header

	// Focused gives a signed-in setup step a quiet shell without losing the
	// wider content area it needs. It is separate from Nav because signed-out
	// forms also have no navigation and intentionally stay card-width.
	Focused bool

	// Lang is the language this response is written in. It lives on the page
	// rather than being resolved inside the template function because a
	// template function has no request: the functions are bound once when the
	// templates are parsed at start-up, long before anybody has asked for a
	// language, so the only place the answer can come from is the render data.
	Lang string

	User             *User
	Team             *Team
	CanManageBilling bool
	CanManageMembers bool
	CanManageTeam    bool

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

	// RegistrationEnabled controls public sign-up links and hosted-only trial
	// copy. An invitation remains usable when this is false, but strangers are
	// directed to the installation operator instead of an account form.
	RegistrationEnabled bool
	CommerceEnabled     bool

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
		Title:               title,
		Nav:                 nav,
		Lang:                i18n.Negotiate(r),
		User:                userFrom(r),
		CSRF:                h.csrfToken(r),
		GoogleEnabled:       h.Google.Configured(),
		RegistrationEnabled: !h.DisableRegistration,
		CommerceEnabled:     !h.DisableCommerce,
		BaseURL:             h.BaseURL,
		Now:                 h.Store.Now(),
		Data:                map[string]any{},
	}

	var role teams.Role

	if p.User != nil {
		if _, teamID, err := h.Identify(r); err == nil {
			if team, err := h.Store.TeamByID(r.Context(), teamID); err == nil {
				p.Team = team
				if found, roleErr := h.Teams.RoleOf(r.Context(), teamID, p.User.ID); roleErr == nil {
					role = found
					p.CanManageBilling = teams.Can(role, teams.PermManageBilling)
					p.CanManageMembers = teams.Can(role, teams.PermManageMembers) || teams.Can(role, teams.PermCreateAPIKey)
					p.CanManageTeam = teams.Can(role, teams.PermManageTeam) || teams.Can(role, teams.PermManageSecurity)
				}
			}
		}

		// Every signed-in screen wears the bar, so it is built once here rather
		// than by each handler that happens to remember.
		p.Header = h.header(r, p, role, nav)

		// The account screens wear the settings chrome, resolved here for the
		// same reason: there are eleven render calls between them and each one
		// remembering would be eleven chances to forget.
		if nav == "settings" {
			shell := appui.NewShell(p.Lang, "", header(p).TeamID, role, p.CSRF,
				accountTab(r), "", !h.DisableCommerce, *p.Header)
			p.Settings = &shell
		}
	}

	return p
}

// HeaderFor builds the bar for a screen rendered by another package.
//
// It is exported rather than duplicated because the settings screens live in
// their own package and must not arrive wearing a second, slightly different
// bar. An unauthenticated request answers an empty header, which those screens
// never render because their own guard has already turned the request away.
func (h *Handler) HeaderFor(r *http.Request) appui.Header {
	user := userFrom(r)
	if user == nil {
		return appui.Header{}
	}

	header := appui.Header{
		Name:    user.DisplayName(),
		Email:   user.Email,
		Help:    h.HelpURL,
		Support: h.SupportURL,
	}

	if _, teamID, err := h.Identify(r); err == nil {
		if team, teamErr := h.Store.TeamByID(r.Context(), teamID); teamErr == nil {
			header.TeamName = team.Name
		}
	}

	if h.Avatars != nil {
		header.AvatarURL = avatar.URL(user.ID, h.Avatars.State(r.Context(), user.ID).ETag)
	}

	return header
}

// header builds the bar for one signed-in screen.
//
// The picture is read separately from the person because it lives in its own
// table: the row a session lookup reads on every request has no business
// carrying fifty kilobytes nobody asked for.
func (h *Handler) header(r *http.Request, p *page, role teams.Role, nav string) *appui.Header {
	header := appui.Header{
		Lang:     p.Lang,
		Current:  nav,
		Name:     p.User.DisplayName(),
		Email:    p.User.Email,
		Role:     role,
		Commerce: !h.DisableCommerce,
		CSRF:     p.CSRF,
		Help:     h.HelpURL,
		Support:  h.SupportURL,
	}

	if p.Team != nil {
		header.TeamID = p.Team.ID
		header.TeamName = p.Team.Name
	}

	if h.Avatars != nil {
		header.AvatarURL = avatar.URL(p.User.ID, h.Avatars.State(r.Context(), p.User.ID).ETag)
	}

	return &header
}

// accountTab reads which account screen a request is for.
//
// It comes from the path rather than from each handler, because the navigation
// only has to say where the reader is and the path is exactly that. A path
// nobody mapped falls back to the first section rather than to no selection,
// which would be a navigation that says nothing.
func accountTab(r *http.Request) string {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/settings/security":
		return appui.TabSecurity
	case "/settings/sessions":
		return appui.TabDevices
	case "/settings/team":
		return appui.TabTeamPolicy
	default:
		return appui.TabAccount
	}
}

// header reads the bar off a page, so a nil one does not panic while the shell
// beside it is being built.
func header(p *page) appui.Header {
	if p.Header == nil {
		return appui.Header{}
	}

	return *p.Header
}

// tr renders one catalogue string in the language a request asked for.
//
// It exists because a page title is an argument to newPage rather than a field
// set afterwards, so the handler needs the locale before there is a page to
// read it from. Everything set after the page is built uses p.Lang instead.
func tr(r *http.Request, id string, args ...any) string {
	return i18n.T(i18n.Negotiate(r), id, args...)
}

// render writes a page. It renders into a buffer first so that a template error
// halfway down produces an error page rather than half a page followed by a
// stack trace — which is what happens when a template writes straight to the
// response and then fails.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, p *page, status int) {
	tpl, ok := h.views.pages[name]
	if !ok {
		h.Log.Error("no such template", "template", name, "path", requestLogPath(r))
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	var buf bytes.Buffer

	if err := tpl.ExecuteTemplate(&buf, "layout.html", p); err != nil {
		h.Log.Error("template failed", "template", name, "path", requestLogPath(r), "error", err)
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
	w.WriteHeader(status)

	_, _ = buf.WriteTo(w)
}

// templateFuncs are the helpers the templates may call. The list is short on
// purpose: logic in a template is logic no test covers.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// url carries the current language through an internal link or form.
		"url": func(locale, target string) string {
			return i18n.LocalURL(target, locale)
		},

		// canBilling gates the billing link. It reads the permission matrix
		// rather than listing roles, so who may pay is answered in one place
		// instead of once per template that asks.
		"canBilling": func(role teams.Role) bool {
			return teams.Can(role, teams.PermManageBilling)
		},

		// t renders one catalogue string.
		//
		// The locale is the first argument rather than something the function
		// closes over because these functions are bound once, when the
		// templates are parsed at start-up. There is no request at that moment
		// and there never will be one, so a closure would have to capture a
		// language before anybody had asked for it — and every page would then
		// render in whichever language the process happened to be built with.
		"t": func(locale, id string, args ...any) string {
			return i18n.T(locale, id, args...)
		},

		// n renders a string that changes with a count, and takes the locale
		// first for the same reason t does: the plural rule belongs to the
		// reader's language, which is only known once a request arrives.
		"n": func(locale, id string, count int, args ...any) string {
			return i18n.N(locale, id, count, args...)
		},

		// num narrows any stored integer to the int the plural helper takes.
		//
		// It exists because the template engine checks an argument's type
		// without converting between integer widths, so a count held as an
		// int64 — which is what a row count read out of SQLite is — cannot
		// reach n at all without a step like this one.
		"num": func(value any) int {
			switch number := value.(type) {
			case int:
				return number
			case int64:
				return int(number)
			case int32:
				return int(number)
			case float64:
				return int(number)
			default:
				return 0
			}
		},

		// rtl reports whether a language is written right to left, which is
		// what the page shell turns into dir="rtl". It reads the flag off the
		// catalogue's own locale list so a new right-to-left language is a
		// catalogue change rather than an edit to the layout.
		"rtl": func(locale string) bool {
			for _, candidate := range i18n.Locales() {
				if candidate.Tag == locale {
					return candidate.RTL
				}
			}

			return false
		},

		// ago turns a unix timestamp into "3 minutes ago". The sessions screen
		// is a list of times, and absolute timestamps make "is one of these not
		// me" a subtraction the reader has to do in their head.
		//
		// The current time is an argument because these functions are bound
		// once at start-up: the page's clock is the only one a test can move.
		//
		// Every counted branch goes through the catalogue's plural forms, so a
		// language whose singular differs gets its own wording rather than the
		// English one with a number in front of it.
		"ago": func(now time.Time, locale string, unix int64) string {
			if unix <= 0 {
				return i18n.T(locale, "common.state.never")
			}

			d := now.Sub(time.Unix(unix, 0))

			switch {
			case d < time.Minute:
				return i18n.T(locale, "common.time.just_now")
			case d < time.Hour:
				return i18n.N(locale, "common.time.minutes_ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return i18n.N(locale, "common.time.hours_ago", int(d.Hours()))
			case d < 48*time.Hour:
				return i18n.T(locale, "common.time.yesterday")
			default:
				return i18n.N(locale, "common.time.days_ago", int(d.Hours()/24))
			}
		},

		// until is the countdown the dual-write banner shows, so somebody can
		// see how long the old domain keeps working.
		"until": func(now time.Time, locale string, unix int64) string {
			d := time.Unix(unix, 0).Sub(now)
			if d <= 0 {
				return i18n.T(locale, "common.state.expired")
			}

			if d < time.Hour {
				return i18n.N(locale, "common.time.minutes_left", int(d.Minutes()))
			}

			return i18n.N(locale, "common.time.hours_left", int(d.Hours()))
		},

		// date formats a timestamp for the places a real date reads better than
		// a relative one.
		//
		// The pattern stays English-shaped. Go's time package carries no month
		// names or field orders for any other language, so translating this
		// means a month table and a date pattern per locale — catalogue data
		// that belongs in the i18n package beside the plural rules, not in a
		// template helper. The empty case still goes through the catalogue,
		// because an em dash is not punctuation every script uses.
		"date": func(locale string, unix int64) string {
			if unix <= 0 {
				return i18n.T(locale, "common.state.dash")
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
// The URLs carry no fingerprint, so a cache lifetime is a promise the content
// will not change — and a deploy breaks it. A browser holding yesterday's
// stylesheet against today's markup does not render an old page; it renders a
// broken one, and a reload does not always clear it. So the answer carries an
// ETag over the bytes and asks to be revalidated every time: one conditional
// request, almost always answered 304, and a new build picked up at once.
func assetHandler() http.Handler {
	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic(fmt.Sprintf("auth: embedded assets are missing: %v", err))
	}

	tags := assetETags(sub)
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The tag is set before delegating because net/http checks
		// If-None-Match against whatever is already on the response.
		if tag, ok := tags[strings.TrimPrefix(path.Clean(r.URL.Path), "/")]; ok {
			w.Header().Set("ETag", tag)
		}

		w.Header().Set("Cache-Control", "public, no-cache")
		files.ServeHTTP(w, r)
	})
}

// assetETags hashes every embedded asset once, at start-up. The files cannot
// change while the process runs, so hashing them per request would be work
// repeated for an answer that is already known.
func assetETags(assets fs.FS) map[string]string {
	tags := map[string]string{}

	err := fs.WalkDir(assets, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		body, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}

		sum := sha256.Sum256(body)
		tags[name] = `"` + hex.EncodeToString(sum[:16]) + `"`

		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("auth: embedded assets could not be read: %v", err))
	}

	return tags
}
