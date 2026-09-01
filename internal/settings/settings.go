//
// settings.go
// The server-rendered settings pages: shields, path cleaning, import and export.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package settings owns the whole /settings/ surface: the shield rules, the
// path cleaning rules and their preview, the import and export screens, the
// team screen, sharing, scheduled reports and the ingestion health panel.
//
// It is one package because it is one surface. The screens are served by two
// handlers, because two of them ask genuinely different permission questions —
// "does this person own this site" and "what may this person do in this team" —
// but they share one layout, one navigation, one flash convention and one route
// table. Two packages under one URL segment is how a tab bar ends up leading
// somewhere its own shell cannot render, and how a pattern in one of them
// silently takes a screen in the other off the air. See routes.go.
//
// It is server-rendered Go templates rather than React. The dashboard is the
// only part of this product that needs a client-side application; a settings
// form that posts and redirects is smaller, works without JavaScript, and does
// not need an API endpoint per field.
package settings

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/google"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pathclean"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/shields"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// PathPrefix is the segment the whole settings surface hangs off.
const PathPrefix = "/settings/"

// SitePrefix is where every per-site screen lives, on both handlers.
//
// The literal `sites` segment is the whole reason a site screen can never
// collide with an account screen. A pattern of /settings/{domain}/shields
// claims the entire second segment, so `members`, `team` and `security` are all
// legal domains as far as the mux is concerned — and /settings/members/shields
// then matches two patterns with neither more specific than the other, which is
// a start-up panic rather than a route. Reserving one literal segment for sites
// removes the overlap instead of arguing about precedence within it.
const SitePrefix = PathPrefix + "sites/"

// actions are the routes one site's screens answer, relative to its domain.
var actions = []string{
	"conversions",
	"conversions/goals/create",
	"conversions/goals/update",
	"conversions/goals/delete",
	"conversions/properties/allow",
	"conversions/properties/allow-all",
	"conversions/properties/delete",
	"conversions/funnels/save",
	"conversions/funnels/delete",
	"shields",
	"shields/add",
	"shields/delete",
	"shields/allow-hostname",
	"paths",
	"paths/save",
	"paths/trailing-slash",
	"imports",
	"imports/upload",
	"imports/delete",
	"exports/create",
	"exports/download/{token}",
	"google/connect",
	"google/disconnect",
}

// Patterns lists the routes the site configuration screens own, as ServeMux
// patterns. Routes() in routes.go joins them to the rest of the surface.
//
// They are enumerated rather than mounted as one prefix because the account
// screens already own /settings/sessions, /settings/security and
// /settings/team. A prefix registration would swallow all three and Go's mux
// would report no conflict at all — the account screens would simply stop
// answering, with nothing anywhere to say why.
func Patterns() []string {
	patterns := make([]string, 0, len(actions)+1)

	// The callback names no site, so it is registered literally rather than
	// under the {domain} wildcard.
	patterns = append(patterns, google.CallbackPath)

	for _, action := range actions {
		patterns = append(patterns, SitePrefix+"{domain}/"+action)
	}

	return patterns
}

// DomainOf reads the site a request names, for the authorisation check that
// wraps these pages.
//
// The OAuth callback answers empty: it names its site in the signed state
// parameter rather than in the path, and is authorised when that state is
// verified.
func DomainOf(r *http.Request) string {
	return r.PathValue("domain")
}

// previewLimit bounds how many merges the path cleaning preview renders. A rule
// that merges ten thousand paths is a rule the customer needs to see the top of
// and then reconsider, not scroll through.
const previewLimit = 50

//go:embed templates
var templateFS embed.FS

// pages holds the parsed templates, one set per screen. They are parsed once at
// start-up rather than per request: a template error is then a start-up failure
// with a filename in it rather than a blank page at three in the morning.
var pages = map[string]*template.Template{
	"conversions": mustParse("conversions.html"),
	"shields":     mustParse("shields.html"),
	"paths":       mustParse("paths.html"),
	"imports":     mustParse("imports.html"),
}

// mustParse builds one screen's template set. Panicking is honest: an embedded
// template that will not parse is a broken binary, not something an operator
// can fix by changing a setting.
func mustParse(name string) *template.Template {
	parsed, err := template.New("layout.html").Funcs(funcs()).ParseFS(templateFS,
		"templates/layout.html", "templates/"+name)
	if err != nil {
		panic("settings: " + err.Error())
	}

	return parsed
}

// funcs are the catalogue lookups the templates render every string through.
//
// The locale is the first argument rather than something the function resolves
// for itself, because these are bound once at start-up when there is no request
// to read a language from. Every call site therefore passes $.Lang.
func funcs() template.FuncMap {
	return template.FuncMap{
		"url": func(locale, target string) string {
			return i18n.LocalURL(target, locale)
		},
		"t": func(locale, id string, args ...any) string {
			return i18n.T(locale, id, args...)
		},

		"n": func(locale, id string, count int, args ...any) string {
			return i18n.N(locale, id, count, args...)
		},

		// The shared layout sets dir on the html element, so every screen on
		// this surface needs this whether or not its own body does.
		"rtl": func(locale string) bool {
			for _, l := range i18n.Locales() {
				if l.Tag == locale {
					return l.RTL
				}
			}

			return false
		},

		// The permission model is read from the templates rather than
		// precomputed into the page, which is what keeps it one reviewable
		// table in the teams package instead of half a table and a handful of
		// booleans assembled per screen.
		"label": func(role teams.Role) string { return teams.Label(role) },
		"can": func(role teams.Role, permission string) bool {
			return teams.Can(role, teams.Permission(permission))
		},
		"canBilling": func(role teams.Role) bool {
			return role == teams.RoleOwner || role == teams.RoleAdmin || role == teams.RoleBilling
		},
		"stepGoalID": func(funnel goals.Funnel, position int) int64 {
			for _, step := range funnel.Steps {
				if step.Position == position {
					return step.GoalID
				}
			}
			return 0
		},
		"propertyName": func(goal goals.Goal, index int) string {
			if index < 0 || index >= len(goal.Properties) {
				return ""
			}
			return goal.Properties[index].Name
		},
		"propertyValue": func(goal goals.Goal, index int) string {
			if index < 0 || index >= len(goal.Properties) {
				return ""
			}
			return goal.Properties[index].Value
		},
		"sub": func(left, right int) int { return left - right },

		// A drop reason is translated at render rather than when the panel is
		// built, because the same panel is also the JSON the API returns, and a
		// payload whose wording moved with the reader's language is a payload
		// nobody could match on.
		"explain": func(locale, reason string) string { return health.ExplainIn(locale, reason) },
	}
}

// tr renders one catalogue string in the language a request asked for. It is
// for the places that need a string before a page exists to carry the locale,
// which is every redirect: the message travels in the query string already
// rendered, exactly as the flash on the signed-in screens does.
func tr(r *http.Request, id string, args ...any) string {
	return i18n.T(i18n.Negotiate(r), id, args...)
}

// Handler serves the settings pages backed by per-account databases.
//
// Nothing here authorises a request. The mount wraps every route in the signed
// in application's own check, which is what keeps one definition of "may this
// person configure this site" rather than a second one that drifts.
type Handler struct {
	Sites    *sites.Cache
	Accounts *accounts.Manager
	Jobs     *jobs.Client
	Log      *logger.Logger

	// CSRF mints the signed-in application's form token and CheckCSRF verifies
	// it. They are callbacks because this package owns settings, not sessions or
	// the application's sealing key.
	CSRF      func(http.ResponseWriter, *http.Request) string
	CheckCSRF func(http.ResponseWriter, *http.Request) bool
	Role      func(*http.Request, sites.Site) teams.Role

	// DataDir is where uploads and prepared exports are written.
	DataDir string

	// Trusted is the proxy allow-list the ingest path uses. The settings page
	// resolves the viewer's address through exactly the same rules, because an
	// address resolved a different way here would produce a rule that does not
	// match the traffic it was created from.
	Trusted *ingest.TrustedProxies

	// Shields and Paths are the running snapshots. They are updated in place on
	// save so a customer sees a rule take effect immediately rather than after
	// a refresh interval they have no way to know about.
	Shields *shields.Cache
	Paths   *pathclean.Cache

	// Google is the OAuth application, or nil when none is configured. Nil is
	// what hides every Google feature: a button that sends somebody to Google
	// and comes back with invalid_client is worse than no button.
	Google *google.App

	// Now is injectable so a test can assert on what a page renders without
	// depending on the clock.
	Now func() time.Time

	// mu guards the token map below.
	mu sync.Mutex

	// tokens holds the download tokens this process minted, keyed by export id.
	// Only the hash reaches the database, so a link can be rendered on the page
	// that created it and nowhere else — which is what keeps a leaked copy of the
	// database useless for downloading somebody's traffic. They live for the life
	// of the process: after a restart the customer prepares a new export, which
	// costs seconds and is the honest trade for not keeping a replayable secret.
	tokens map[int64]string
}

// rememberToken records a download token for the life of this process.
func (h *Handler) rememberToken(exportID int64, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.tokens == nil {
		h.tokens = map[int64]string{}
	}

	h.tokens[exportID] = token
}

// downloadToken reads back a token this process minted.
func (h *Handler) downloadToken(exportID int64) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	token, ok := h.tokens[exportID]

	return token, ok
}

// now reads the handler's clock.
func (h *Handler) now() time.Time {
	if h.Now == nil {
		return time.Now().UTC()
	}

	return h.Now().UTC()
}

// page is what every template renders against.
type page struct {
	TitleID string
	Tab     string
	Domain  string
	Message string
	Error   string
	CSRF    string
	Role    teams.Role
	TeamID  int64

	// Lang is the language this response is written in. It lives on the page
	// rather than being resolved inside a template function, because a template
	// function is bound at start-up and has no request to read.
	Lang string

	// Screen-specific fields. They are on one struct rather than three so the
	// shared layout can be a single template with one data type behind it.
	Viewer            shields.Viewer
	Groups            []ruleGroup
	MaxRules          int
	RejectedHostnames []shields.RejectedHostname

	Rules           []pathclean.Rule
	Merges          []pathclean.Merge
	Previewed       bool
	TrailingSlashOn bool

	Imports    []dataio.Import
	Exports    []exportView
	SheetNames []string

	GoogleEnabled         bool
	SearchConsoleNoticeID string
	GA4                   *google.Connection
	SearchConsole         *google.Connection

	Goals             []goals.Goal
	Properties        []goals.Property
	SeenProperties    []string
	Funnels           []goals.Funnel
	NoBackfillNotice  string
	PropertyPIINotice string
	FunnelStepSlots   []int
}

// ruleGroup is one shield kind as the page renders it. The three labels are
// catalogue ids rather than prose, so a kind's copy lives beside every other
// translated string instead of in a switch in this file.
type ruleGroup struct {
	Kind          string
	TitleID       string
	HintID        string
	PlaceholderID string
	Rules         []shields.Rule
}

// exportView is one prepared export, rendered.
type exportView struct {
	Prepared string
	Status   string
	Size     string
	URL      string
	Expires  string
	Expired  bool
	Failure  string

	// Ready marks an archive that is built but has no link on this page,
	// because the download token was minted by a process that has since
	// restarted. Saying so is better than leaving a finished export looking as
	// though it is still building.
	Ready bool
}

// ServeHTTP routes the settings pages. It is one handler with a small switch
// rather than a mux per screen, because every route needs the same three
// things first — the domain, the site and the account database — and doing that
// resolution once is what keeps a new screen to one case.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i18n.Apply(w, r)
	path := strings.TrimPrefix(r.URL.Path, SitePrefix)

	// The OAuth callback is the one route that does not name a site in its
	// path: Google will only redirect to one registered URI, so the site
	// travels in the state parameter instead.
	if r.URL.Path == google.CallbackPath {
		h.googleCallback(w, r)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if h.CheckCSRF == nil || !h.CheckCSRF(w, r) {
			if h.CheckCSRF == nil {
				http.Error(w, "form token verification is unavailable", http.StatusForbidden)
			}
			return
		}
	}

	domain, action, _ := strings.Cut(path, "/")
	if domain == "" {
		http.Error(w, "the URL must name a site, as /settings/sites/<domain>/shields", http.StatusBadRequest)
		return
	}

	site, ok := h.Sites.Lookup(domain)
	if !ok {
		http.Error(w, "no site is registered for "+domain, http.StatusNotFound)
		return
	}

	switch action {
	case "conversions":
		h.conversions(w, r, site)
	case "conversions/goals/create":
		h.createGoal(w, r, site)
	case "conversions/goals/update":
		h.updateGoal(w, r, site)
	case "conversions/goals/delete":
		h.deleteGoal(w, r, site)
	case "conversions/properties/allow":
		h.allowProperty(w, r, site)
	case "conversions/properties/allow-all":
		h.allowAllProperties(w, r, site)
	case "conversions/properties/delete":
		h.deleteProperty(w, r, site)
	case "conversions/funnels/save":
		h.saveFunnel(w, r, site)
	case "conversions/funnels/delete":
		h.deleteFunnel(w, r, site)
	case "", "shields":
		h.shields(w, r, site)
	case "shields/add":
		h.addShield(w, r, site)
	case "shields/delete":
		h.deleteShield(w, r, site)
	case "shields/allow-hostname":
		h.allowRejectedHostname(w, r, site)
	case "paths":
		h.paths(w, r, site, nil, false)
	case "paths/save":
		h.savePaths(w, r, site)
	case "paths/trailing-slash":
		h.toggleTrailingSlash(w, r, site)
	case "imports":
		h.imports(w, r, site)
	case "imports/upload":
		h.uploadImport(w, r, site)
	case "imports/delete":
		h.deleteImport(w, r, site)
	case "exports/create":
		h.createExport(w, r, site)
	case "google/connect":
		h.googleConnect(w, r, site)
	case "google/disconnect":
		h.googleDisconnect(w, r, site)
	default:
		if token, found := strings.CutPrefix(action, "exports/download/"); found {
			h.downloadExport(w, r, site, token)
			return
		}

		http.NotFound(w, r)
	}
}

// render writes one screen.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data page) {
	template, ok := pages[name]
	if !ok {
		http.Error(w, "unknown settings page", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.CSRF != nil {
		data.CSRF = h.CSRF(w, r)
	}
	if site, ok := h.Sites.Lookup(data.Domain); ok {
		data.TeamID = site.TeamID
		if h.Role != nil {
			data.Role = h.Role(r, site)
		}
	}

	if err := template.ExecuteTemplate(w, "layout", data); err != nil && h.Log != nil {
		h.Log.Error("settings page could not be rendered", "page", name, "error", err)
	}
}

// redirect sends the browser back to a screen with a message. Post-redirect-get
// is what stops a reload re-submitting a rule, which is otherwise the fastest
// way to hit the thirty-rule cap by accident.
func (h *Handler) redirect(w http.ResponseWriter, r *http.Request, domain, tab, message, failure string) {
	values := make(url.Values)
	if message != "" {
		values.Set("ok", message)
	}
	if failure != "" {
		values.Set("err", failure)
	}

	target := SitePrefix + domain + "/" + tab
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, i18n.LocalURL(target, i18n.Negotiate(r)), http.StatusSeeOther)
}

// flash reads the message a redirect carried.
func flash(r *http.Request) (string, string) {
	return r.URL.Query().Get("ok"), r.URL.Query().Get("err")
}

// shields renders the shield rules and the viewer's own address.
func (h *Handler) shields(w http.ResponseWriter, r *http.Request, site sites.Site) {
	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // the request result is more useful than an unlock error
	account := lease.Account

	rules, err := shields.List(r.Context(), account.Reader(), site.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var rejected []shields.RejectedHostname
	if h.Shields != nil && h.Shields.Rejections != nil {
		rejected, err = h.Shields.Rejections.ListRejected(r.Context(), site.AccountID, site.ID, 1)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rejected = allowableRejections(rejected, rules)
	}

	message, failure := flash(r)

	data := page{
		TitleID: "auth.title.shields", Tab: "shields", Domain: site.Domain,
		Lang:    i18n.Negotiate(r),
		Message: message, Error: failure,
		Viewer:            shields.ResolveViewer(r, h.Trusted),
		MaxRules:          shields.MaxRulesPerKind,
		Groups:            groupRules(rules),
		RejectedHostnames: rejected,
	}

	h.render(w, r, "shields", data)
}

// allowableRejections removes aggregate and already-allowed hostnames from the
// one-click list.
func allowableRejections(rejected []shields.RejectedHostname, rules []shields.Rule) []shields.RejectedHostname {
	allowed := map[string]struct{}{}
	for _, rule := range rules {
		if rule.Kind == shields.KindHostname {
			allowed[rule.Value] = struct{}{}
		}
	}

	out := make([]shields.RejectedHostname, 0, len(rejected))
	for _, rejection := range rejected {
		if rejection.Hostname == shields.OtherHostname {
			continue
		}
		if _, exists := allowed[rejection.Hostname]; exists {
			continue
		}
		out = append(out, rejection)
	}

	return out
}

// groupRules splits a rule list into the four sections the page shows, naming
// the copy that explains what each one does.
func groupRules(rules []shields.Rule) []ruleGroup {
	groups := []ruleGroup{
		{
			Kind: shields.KindIP, TitleID: "auth.shields.ip_title",
			HintID:        "auth.shields.ip_hint",
			PlaceholderID: "auth.shields.ip_placeholder",
		},
		{
			Kind: shields.KindCountry, TitleID: "auth.shields.country_title",
			HintID:        "auth.shields.country_hint",
			PlaceholderID: "auth.shields.country_placeholder",
		},
		{
			Kind: shields.KindPage, TitleID: "auth.shields.page_title",
			HintID:        "auth.shields.page_hint",
			PlaceholderID: "auth.shields.page_placeholder",
		},
		{
			Kind: shields.KindHostname, TitleID: "auth.shields.hostname_title",
			HintID:        "auth.shields.hostname_hint",
			PlaceholderID: "auth.shields.hostname_placeholder",
		},
	}

	for i := range groups {
		for _, rule := range rules {
			if rule.Kind == groups[i].Kind {
				groups[i].Rules = append(groups[i].Rules, rule)
			}
		}
	}

	return groups
}

// addShield stores one rule and refreshes the running snapshot.
func (h *Handler) addShield(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to add a rule", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // mutation and snapshot refresh share one fence
	account := lease.Account

	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, site.Domain, "shields", "", tr(r, "auth.shields.error_unreadable_form"))
		return
	}

	rule, err := shields.Add(r.Context(), account.Writer(), site.ID,
		r.PostFormValue("kind"), r.PostFormValue("value"), r.PostFormValue("note"), h.now())
	if err != nil {
		h.redirect(w, r, site.Domain, "shields", "", err.Error())
		return
	}

	h.refreshShields(r.Context(), account.Reader(), site.ID)

	h.redirect(w, r, site.Domain, "shields", tr(r, "auth.shields.flash_added", "rule", rule.Value), "")
}

// deleteShield removes one rule.
func (h *Handler) deleteShield(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to remove a rule", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // mutation and snapshot refresh share one fence
	account := lease.Account

	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)

	if err := shields.Delete(r.Context(), account.Writer(), site.ID, id); err != nil {
		h.redirect(w, r, site.Domain, "shields", "", err.Error())
		return
	}

	h.refreshShields(r.Context(), account.Reader(), site.ID)

	h.redirect(w, r, site.Domain, "shields", tr(r, "auth.shields.flash_removed"), "")
}

// allowRejectedHostname turns one named rejection into an additive hostname
// rule and refreshes the running policy immediately.
func (h *Handler) allowRejectedHostname(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to allow a hostname", http.StatusMethodNotAllowed)
		return
	}

	account, err := h.Accounts.Open(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	hostname := r.PostFormValue("hostname")
	if hostname == shields.OtherHostname {
		h.redirect(w, r, site.Domain, "shields", "", tr(r, "auth.shields.error_aggregate_hostname"))
		return
	}

	rule, err := shields.Add(r.Context(), account.Writer(), site.ID, shields.KindHostname,
		hostname, tr(r, "auth.shields.rejected_hostname_note"), h.now())
	if err != nil {
		h.redirect(w, r, site.Domain, "shields", "", err.Error())
		return
	}

	h.refreshShields(r.Context(), account.Reader(), site.ID)
	h.redirect(w, r, site.Domain, "shields", tr(r, "auth.shields.flash_hostname_allowed", "hostname", rule.Value), "")
}

// refreshShields pushes a site's rules into the running snapshot. Waiting for
// the timer would mean a customer clicks save, generates a test event, sees it
// counted, and reports the feature as broken.
func (h *Handler) refreshShields(ctx context.Context, db *sql.DB, siteID int64) {
	if h.Shields == nil {
		return
	}

	rules, err := shields.List(ctx, db, siteID)
	if err != nil {
		if h.Log != nil {
			h.Log.Error("shield snapshot could not be refreshed", "site", siteID, "error", err)
		}

		return
	}

	h.Shields.Set(siteID, rules)
}

// paths renders the path cleaning rules, and a preview when one was asked for.
func (h *Handler) paths(w http.ResponseWriter, r *http.Request, site sites.Site, merges []pathclean.Merge, previewed bool) {
	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // rendering finishes before deletion can unlink the shard
	account := lease.Account

	rules, err := pathclean.List(r.Context(), account.Reader(), site.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	message, failure := flash(r)

	h.render(w, r, "paths", page{
		TitleID: "auth.title.paths", Tab: "paths", Domain: site.Domain,
		Lang:    i18n.Negotiate(r),
		Message: message, Error: failure,
		Rules:           rules,
		Merges:          merges,
		Previewed:       previewed,
		TrailingSlashOn: hasTrailingSlashRule(rules),
	})
}

// hasTrailingSlashRule reports whether the one-click trailing slash rule is on.
func hasTrailingSlashRule(rules []pathclean.Rule) bool {
	for _, rule := range rules {
		if rule.Pattern == pathclean.TrailingSlashPattern && rule.Enabled {
			return true
		}
	}

	return false
}

// savePaths previews or saves the whole rule list.
func (h *Handler) savePaths(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to save rules", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // all path rewrites share one deletion fence
	account := lease.Account

	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, site.Domain, "paths", "", tr(r, "auth.paths.error_unreadable_form"))
		return
	}

	rules := rulesFromForm(r)

	if r.PostFormValue("action") == "preview" {
		merges, err := pathclean.Preview(r.Context(), account.Reader(), site.ID, rules, previewLimit)
		if err != nil {
			h.redirect(w, r, site.Domain, "paths", "", err.Error())
			return
		}

		h.paths(w, r, site, merges, true)

		return
	}

	if err := pathclean.Replace(r.Context(), account.Writer(), site.ID, rules, h.now()); err != nil {
		h.redirect(w, r, site.Domain, "paths", "", err.Error())
		return
	}

	moved, err := h.applyPaths(r.Context(), account, site.ID)
	if err != nil {
		h.redirect(w, r, site.Domain, "paths", "", err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "paths",
		i18n.N(i18n.Negotiate(r), "auth.paths.flash_saved", moved), "")
}

// applyPaths rebuilds the query-time map and updates the running snapshot. Both
// halves are needed: the map is what makes the rules retroactive, and the
// snapshot is what stops the dimension table growing from the next event on.
func (h *Handler) applyPaths(ctx context.Context, account *accounts.Account, siteID int64) (int, error) {
	moved, err := pathclean.Materialise(ctx, account.Writer(), account.Intern, siteID)
	if err != nil {
		return 0, err
	}

	if h.Paths != nil {
		set, err := pathclean.RulesetFor(ctx, account.Reader(), siteID)
		if err != nil {
			return 0, err
		}

		h.Paths.Set(siteID, set)
	}

	return moved, nil
}

// rulesFromForm reads the rule table back off the page. Blank rows are dropped
// so that the always-present empty row at the bottom does not become a rule
// that matches everything.
func rulesFromForm(r *http.Request) []pathclean.Rule {
	patterns := r.PostForm["pattern"]
	replacements := r.PostForm["replacement"]
	labels := r.PostForm["label"]

	var rules []pathclean.Rule

	for i, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			continue
		}

		rule := pathclean.Rule{
			Position: len(rules),
			Pattern:  strings.TrimSpace(pattern),
			Enabled:  r.PostFormValue("enabled-"+strconv.Itoa(i)) != "",
		}

		if i < len(replacements) {
			rule.Replacement = replacements[i]
		}
		if i < len(labels) {
			rule.Label = labels[i]
		}

		rules = append(rules, rule)
	}

	return rules
}

// toggleTrailingSlash switches the one-click trailing slash rule on or off.
func (h *Handler) toggleTrailingSlash(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to change this", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // all path rewrites share one deletion fence
	account := lease.Account

	rules, err := pathclean.List(r.Context(), account.Reader(), site.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	kept := make([]pathclean.Rule, 0, len(rules)+1)
	found := false

	for _, rule := range rules {
		if rule.Pattern == pathclean.TrailingSlashPattern {
			found = true
			continue
		}

		rule.Position = len(kept)
		kept = append(kept, rule)
	}

	message := tr(r, "auth.paths.flash_slash_off")

	if !found {
		// It goes last, so a more specific rule written by hand still wins.
		kept = append(kept, pathclean.Rule{
			Position:    len(kept),
			Pattern:     pathclean.TrailingSlashPattern,
			Replacement: pathclean.TrailingSlashReplacement,
			Label:       pathclean.TrailingSlashLabel,
			Enabled:     true,
		})

		message = tr(r, "auth.paths.flash_slash_on")
	}

	if err := pathclean.Replace(r.Context(), account.Writer(), site.ID, kept, h.now()); err != nil {
		h.redirect(w, r, site.Domain, "paths", "", err.Error())
		return
	}

	if _, err := h.applyPaths(r.Context(), account, site.ID); err != nil {
		h.redirect(w, r, site.Domain, "paths", "", err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "paths", message, "")
}

// imports renders the import and export screen.
func (h *Handler) imports(w http.ResponseWriter, r *http.Request, site sites.Site) {
	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // imported rows remain linked throughout rendering
	account := lease.Account

	records, err := dataio.ListImports(r.Context(), account.Reader(), site.ID, 25)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	exports, err := dataio.ListExports(r.Context(), account.Reader(), site.ID, 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	message, failure := flash(r)

	data := page{
		TitleID: "auth.title.imports", Tab: "imports", Domain: site.Domain,
		Lang:    i18n.Negotiate(r),
		Message: message, Error: failure,
		Imports:               records,
		Exports:               h.exportViews(site.Domain, i18n.Negotiate(r), exports),
		SheetNames:            dataio.SheetNames(),
		GoogleEnabled:         h.Google != nil,
		SearchConsoleNoticeID: google.SearchConsoleDelayNotice,
	}

	if h.Google != nil {
		data.GA4, _ = google.GetConnection(r.Context(), account.Reader(), site.ID, google.ProviderGA4)
		data.SearchConsole, _ = google.GetConnection(r.Context(), account.Reader(), site.ID, google.ProviderSearchConsole)
	}

	h.render(w, r, "imports", data)
}

// exportViews renders the export list. The download URL is only ever built from
// a token the caller already holds, which is why a completed export whose token
// this process did not just mint shows no link.
func (h *Handler) exportViews(domain, locale string, exports []dataio.Export) []exportView {
	now := h.now()

	views := make([]exportView, 0, len(exports))

	for _, export := range exports {
		view := exportView{
			Prepared: time.Unix(export.CreatedAt, 0).UTC().Format("2006-01-02 15:04 MST"),
			Status:   export.Status,
			Size:     humanBytes(export.Bytes),
			Expires:  time.Unix(export.ExpiresAt, 0).UTC().Format("15:04 MST"),
			Expired:  export.Expired(now),
			Failure:  export.Failure,
			Ready:    export.Status == dataio.StatusCompleted,
		}

		if token, ok := h.downloadToken(export.ID); ok && export.Status == dataio.StatusCompleted && !view.Expired {
			view.URL = i18n.LocalURL(SitePrefix+domain+"/exports/download/"+token, locale)
		}

		views = append(views, view)
	}

	return views
}

// humanBytes renders a size the way a person reads one.
func humanBytes(size int64) string {
	switch {
	case size <= 0:
		return "—"
	case size < 1024:
		return fmt.Sprintf("%d B", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

// uploadImport takes the uploaded file and enqueues the job.
func (h *Handler) uploadImport(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a file to import", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // upload and enqueue share one deletion fence
	account := lease.Account

	if err := r.ParseMultipartForm(8 << 20); err != nil {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.imports.error_unreadable_upload"))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.imports.error_choose_file"))
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && h.Log != nil {
			h.Log.Warn("could not close uploaded import", "file", header.Filename, "error", closeErr)
		}
	}()

	record, err := dataio.CreateImport(r.Context(), account.Writer(), site.ID,
		dataio.SourceCSV, header.Filename, h.now())
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	destination := dataio.ImportPath(h.DataDir, site.AccountID, record.ID, header.Filename)

	// Copy, then remove — never rename. The upload's temporary file and the
	// data directory are routinely on different filesystems, and rename fails
	// there with a cross-device error that reads like a kernel problem.
	if _, err := dataio.SaveUpload(file, destination); err != nil {
		h.failAndRedirect(w, r, account, site.Domain, record.ID, err.Error())
		return
	}

	if err := dataio.SetUploadPath(r.Context(), account.Writer(), record.ID, destination); err != nil {
		h.failAndRedirect(w, r, account, site.Domain, record.ID, err.Error())
		return
	}

	_, err = h.Jobs.EnqueueOwned(r.Context(), site.AccountID, jobs.QueueImports, jobs.KindCSVImport,
		dataio.ImportArgs{AccountID: site.AccountID, SiteID: site.ID, ImportID: record.ID},
		fmt.Sprintf("account-%d-import-%d", site.AccountID, record.ID))
	if err != nil {
		h.failAndRedirect(w, r, account, site.Domain, record.ID, err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "imports", tr(r, "auth.imports.flash_queued", "file", header.Filename), "")
}

// failAndRedirect writes a failure onto the import row and shows it. The row
// always carries the reason, so a customer never sees an import that simply
// stopped.
func (h *Handler) failAndRedirect(w http.ResponseWriter, r *http.Request, account *accounts.Account, domain string, id int64, reason string) {
	if err := dataio.FailImport(r.Context(), account.Writer(), id, reason, h.now()); err != nil && h.Log != nil {
		h.Log.Error("import failure could not be recorded", "import", id, "error", err)
	}

	h.redirect(w, r, domain, "imports", "", reason)
}

// deleteImport removes an import and the history it brought in.
func (h *Handler) deleteImport(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to delete an import", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // the request result is more useful than an unlock error
	account := lease.Account

	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)

	if err := dataio.DeleteImport(r.Context(), account.Writer(), site.ID, id); err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "imports", tr(r, "auth.imports.flash_deleted"), "")
}

// createExport prepares a full site export and opens its download window.
func (h *Handler) createExport(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to prepare an export", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // the request result is more useful than an unlock error
	account := lease.Account

	export, token, err := dataio.CreateExport(r.Context(), account.Writer(), site.ID, h.now())
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	h.rememberToken(export.ID, token)

	_, err = h.Jobs.EnqueueOwned(r.Context(), site.AccountID, jobs.QueueExports, jobs.KindSiteExport,
		dataio.ExportArgs{AccountID: site.AccountID, SiteID: site.ID, ExportID: export.ID},
		fmt.Sprintf("account-%d-export-%d", site.AccountID, export.ID))
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "imports", tr(r, "auth.imports.flash_export_started"), "")
}

// downloadExport serves a prepared archive.
func (h *Handler) downloadExport(w http.ResponseWriter, r *http.Request, site sites.Site, token string) {
	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // ServeContent must finish before deletion can unlink the archive
	account := lease.Account

	export, err := dataio.ExportByToken(r.Context(), account.Reader(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// The token is enough to identify an export, but not to reach one that
	// belongs to another site: a link is scoped to the site it was made on.
	if export.SiteID != site.ID {
		http.NotFound(w, r)
		return
	}

	if export.Expired(h.now()) {
		http.Error(w, "this download link has expired — prepare a new export", http.StatusGone)
		return
	}

	if export.Status != dataio.StatusCompleted || export.Path == "" {
		http.Error(w, "this export is still being prepared", http.StatusAccepted)
		return
	}

	file, err := os.Open(export.Path)
	if err != nil {
		http.Error(w, "the prepared archive is no longer on disk — prepare a new export", http.StatusGone)
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && h.Log != nil {
			h.Log.Warn("could not close export download", "file", export.Path, "error", closeErr)
		}
	}()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+dataio.SafeFilename(site.Domain)+"-export-"+
			time.Unix(export.CreatedAt, 0).UTC().Format("20060102")+`.zip"`)

	http.ServeContent(w, r, filepath.Base(export.Path), time.Unix(export.CompletedAt, 0), file)
}

// googleConnect sends the customer to Google.
func (h *Handler) googleConnect(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if h.Google == nil {
		http.Error(w, "no Google application is configured on this install", http.StatusNotFound)
		return
	}

	provider := r.URL.Query().Get("provider")

	scope := google.ScopeAnalytics
	if provider == google.ProviderSearchConsole {
		scope = google.ScopeSearchConsole
	}

	// The state carries the site and the provider because the callback URL is
	// fixed — Google redirects to one registered URI — and guessing the site
	// from a session would connect the wrong one for anybody with two tabs open.
	state := site.Domain + "|" + provider

	http.Redirect(w, r, h.Google.AuthorizeURL(state, scope), http.StatusFound)
}

// googleDisconnect removes one site's grant.
func (h *Handler) googleDisconnect(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST to disconnect", http.StatusMethodNotAllowed)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // provider grant mutation shares one deletion fence
	account := lease.Account

	if err := google.DeleteConnection(r.Context(), account.Writer(), site.ID, r.PostFormValue("provider")); err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "imports", tr(r, "auth.imports.flash_disconnected"), "")
}

// googleCallback finishes the OAuth exchange and stores the grant against the
// one site the customer was configuring.
func (h *Handler) googleCallback(w http.ResponseWriter, r *http.Request) {
	if h.Google == nil {
		http.Error(w, "no Google application is configured on this install", http.StatusNotFound)
		return
	}

	domain, provider, _ := strings.Cut(r.URL.Query().Get("state"), "|")

	site, ok := h.Sites.Lookup(domain)
	if !ok {
		http.Error(w, "that authorisation does not name a site we serve", http.StatusBadRequest)
		return
	}

	if failure := r.URL.Query().Get("error"); failure != "" {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.imports.error_google_returned", "detail", failure))
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // token persistence shares one deletion fence
	account := lease.Account

	token, err := h.Google.Exchange(r.Context(), r.URL.Query().Get("code"), h.now())
	if err != nil {
		if errors.Is(err, google.ErrInvalidGrant) {
			h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.imports.error_google_incomplete"))
			return
		}

		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	connection := google.Connection{
		SiteID:       site.ID,
		AccountID:    site.AccountID,
		Provider:     provider,
		RefreshToken: token.RefreshToken,
		AccessToken:  token.AccessToken,
		ExpiresAt:    token.ExpiresAt.Unix(),
		Scopes:       token.Scope,
		Status:       google.StatusConnected,
	}

	if err := google.SaveConnection(r.Context(), account.Writer(), connection, h.now()); err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}

	h.redirect(w, r, site.Domain, "imports", tr(r, "auth.imports.flash_connected"), "")
}
