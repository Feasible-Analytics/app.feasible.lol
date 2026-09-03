//
// admin.go
// The team screen and the per-site screens a team administers a site through.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/appui"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// keyCookie carries a freshly-minted API key across the redirect that follows
// its creation. It exists because the alternative — putting the secret in a
// query string — writes a live credential into browser history, the proxy's
// access log and anything that scrapes a Referer header.
const keyCookie = "feasible_new_key"

// Identity is who is making a request and which team they are acting in.
type Identity struct {
	UserID int64
	TeamID int64
}

// invitationSender is the mail capability this screen needs. Keeping the
// interface here lets tests capture delivery without constructing a transport.
type invitationSender interface {
	SendInvitation(context.Context, string, string, string, string, string, time.Time) error
}

// Handler serves every settings screen.
type TeamHandler struct {
	System   *sql.DB
	Teams    *teams.Store
	Sharing  *sharing.Store
	Reports  *reports.Store
	Health   *health.Store
	Notifier *reports.Notifier
	Sites    *sites.Cache

	// Commerce says whether this deployment sells a subscription. A self-hosted
	// install has none, and a billing link there leads to a page about a
	// product it does not have.
	Commerce bool

	// Header builds the bar these screens wear. It is injected because the
	// person, their team and their picture are the application's to resolve,
	// and a second answer here would be a second, slightly different bar.
	Header func(*http.Request) appui.Header

	Mail invitationSender
	Log  *logger.Logger
	CSRF func(http.ResponseWriter, *http.Request) string

	// BaseURL is what every URL shown on these pages is built from — the share
	// links, the embed snippet and the test event's target.
	BaseURL string

	// Identify resolves the acting user and the team they are acting in.
	//
	// It is a function rather than a session store of this package's own so
	// that every permission check below is made against the person the
	// application's own gates admitted. These screens transfer ownership and
	// mint credentials; a second opinion about who is asking is the one bug
	// here that cannot be recovered from.
	//
	// A caller that supplies none gets FirstTeamOwner, which is for a test with
	// no session in front of it. The serving process always supplies one.
	Identify func(*http.Request) (Identity, error)

	templates map[string]*template.Template
}

// New builds the handler and parses every page once. A template that will not
// parse is a programming error, so it panics rather than failing on the first
// request: a process that starts and then 500s on one screen is far harder to
// diagnose than one that refuses to start with the reason.
func NewTeamHandler(h *TeamHandler) *TeamHandler {
	h.BaseURL = strings.TrimRight(h.BaseURL, "/")
	h.templates = map[string]*template.Template{}

	if h.Identify == nil {
		h.Identify = FirstTeamOwner(h.System)
	}

	for _, name := range []string{"team", "sharing", "reports", "health"} {
		parsed, err := template.New("layout.html").Funcs(funcs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err == nil {
			// The header and the section list come from the shared package, so
			// every settings screen wears the same chrome.
			parsed, err = parsed.ParseFS(appui.Templates, "templates/*.html")
		}
		if err != nil {
			panic(fmt.Sprintf("settings: %s.html will not parse: %v", name, err))
		}

		h.templates[name] = parsed
	}

	return h
}

// shellDomain is the site the navigation should be about.
//
// The people screen belongs to a team rather than to one site, but it is
// reached from a site's settings and is one entry in that site's section list.
// Arriving there to a one-item navigation is the disappearing-navigation
// problem the shared shell exists to fix, so the site travels in the query and
// is checked against this team before it is trusted — an unchecked domain would
// draw links to somebody else's site.
func (h *TeamHandler) shellDomain(r *http.Request, data screen) string {
	if data.Domain != "" {
		return data.Domain
	}

	context := strings.TrimSpace(r.URL.Query().Get("site_context"))
	if context == "" || h.Sites == nil {
		return ""
	}

	site, ok := h.Sites.Lookup(context)
	if !ok || site.TeamID != data.TeamID {
		if h.Log != nil {
			h.Log.Warn("a settings screen was asked for a site context it cannot use",
				"site_context", context, "team", data.TeamID)
		}

		return ""
	}

	return site.Domain
}

// page is what every template is executed against.
//
// It is one flat struct for every screen rather than one per screen, because
// the layout has to reach Title, Nav, Domain and the two message fields
// whichever page it is wrapping. A per-page type would mean either an interface
// with five accessors or a layout that takes its own struct and re-nests the
// page's — both of which are more machinery than four screens deserve.
type screen struct {
	// The first five fields are the layout's contract, spelled exactly as the
	// site configuration screens' own page struct spells them. There is one
	// layout for this surface, so there is one set of names it can reach; two
	// would be two shells and two navigations.
	TitleID string
	Tab     string
	Domain  string
	Message string
	Error   string

	// Shield is the rule kind Shields is filtered to. These screens never set
	// it; it is here because the shared chrome is resolved from one call that
	// both page types make.
	Shield string

	// Shell is the header and section list, shared with every other settings
	// screen so the two packages cannot drift into two navigations.
	Shell appui.Shell

	// Lang is the locale the request negotiated. It is on the page rather than
	// looked up per helper because the layout needs it for the html element's
	// lang and dir attributes as well, and two negotiations of the same request
	// could disagree.
	Lang   string
	CSRF   string
	TeamID int64

	// BaseURL is what the share links and the embed snippet are built from.
	BaseURL string

	// The team screen.
	Team            teamSummary
	Role            teams.Role
	Members         []teamsMember
	Guests          []teamsGuest
	Invitations     []invitationView
	APIKeys         []apiKeyView
	NewKey          string
	AssignableRoles []teams.Role
	InvitableRoles  []teams.Role
	Sites           []siteOption

	// The sharing screen.
	IsPublic bool
	Links    []sharing.Link
	EmbedURL string
	HTTPS    bool

	// The reports screen.
	Timezone      string
	Subscriptions []subscriptionView
	Alerts        []alertView
	Deliveries    []deliveryView

	// The health screen.
	Panel            health.Panel
	TruncatedTotal   int64
	LastRequestAt    string
	TestEvent        *health.TestEventResult
	TestEventDerived string
}

// ServeHTTP routes one settings request.
func (h *TeamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Framing and referrer policy come from the listener's default headers;
	// this surface only needs to stop a shared machine keeping the last
	// person's team on screen.
	w.Header().Set("Cache-Control", "no-store")

	path := strings.TrimPrefix(r.URL.Path, PathPrefix)

	identity, err := h.Identify(r)
	if err != nil {
		http.Error(w, "This install has no team yet.", http.StatusNotFound)

		return
	}

	switch {
	case path == membersPath:
		h.teamPage(w, r, identity, "", "")

	case strings.HasPrefix(path, membersPath+"/"):
		h.teamAction(w, r, identity, strings.TrimPrefix(path, membersPath+"/"))

	case strings.HasPrefix(path, sitesPath):
		h.sitePath(w, r, identity, strings.TrimPrefix(path, sitesPath))

	default:
		http.NotFound(w, r)
	}
}

// sitePath splits a site route into its domain and its action.
func (h *TeamHandler) sitePath(w http.ResponseWriter, r *http.Request, identity Identity, rest string) {
	domain, action, _ := strings.Cut(rest, "/")

	domain, err := url.PathUnescape(domain)
	if err != nil || domain == "" {
		http.NotFound(w, r)

		return
	}

	site, ok := h.Sites.Lookup(domain)
	if !ok {
		http.Error(w, "No site is registered for "+domain, http.StatusNotFound)

		return
	}

	// A guest is a member of nothing, so the check is per site rather than per
	// team — which is the entire point of the guest roles.
	if _, err := h.Teams.AuthoriseSite(r.Context(), site.ID, identity.UserID, teams.PermManageSiteSettings); err != nil {
		h.forbidden(w, err)

		return
	}

	switch {
	case action == "sharing" || strings.HasPrefix(action, "sharing/"):
		h.sharingRoute(w, r, identity, site, strings.TrimPrefix(strings.TrimPrefix(action, "sharing"), "/"))

	case action == "reports" || strings.HasPrefix(action, "reports/"):
		h.reportsRoute(w, r, identity, site, strings.TrimPrefix(strings.TrimPrefix(action, "reports"), "/"))

	case action == "health" || strings.HasPrefix(action, "health/"):
		h.healthRoute(w, r, identity, site, strings.TrimPrefix(strings.TrimPrefix(action, "health"), "/"))

	default:
		http.NotFound(w, r)
	}
}

// render executes one page.
//
// The locale is negotiated here rather than by each screen, so a page cannot be
// built without one and end up rendering every label as its own message id.
func (h *TeamHandler) render(w http.ResponseWriter, r *http.Request, name string, data screen) {
	data.BaseURL = h.BaseURL
	data.Lang = i18n.Negotiate(r)
	if identity, err := h.Identify(r); err == nil {
		data.TeamID = identity.TeamID
		if role, roleErr := h.Teams.RoleOf(r.Context(), identity.TeamID, identity.UserID); roleErr == nil {
			data.Role = role
		}
	}
	if h.CSRF != nil {
		data.CSRF = h.CSRF(w, r)
	}

	// The chrome is resolved here rather than in each screen, so a screen added
	// later cannot arrive with a different set of places to go.
	data.Shell = appui.NewShell(data.Lang, h.shellDomain(r, data),
		data.TeamID, data.Role, data.CSRF, data.Tab, data.Shield, h.Commerce, h.headerFor(r))

	parsed, ok := h.templates[name]
	if !ok {
		http.Error(w, "no such page", http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := parsed.ExecuteTemplate(w, "layout", data); err != nil && h.Log != nil {
		// The response is already partly written by the time a template fails,
		// so there is nothing to send but a log line. An undefined variable is
		// caught by html/template at execution rather than rendering as
		// nothing, which is what makes that log line the whole story.
		h.Log.Error("a settings page could not be rendered", "page", name, "error", err)
	}
}

// forbidden answers a permission failure with the reason. The reason is the
// role, not the operation: "an Editor cannot do this" tells somebody who to ask,
// where "forbidden" tells them to file a ticket.
func (h *TeamHandler) forbidden(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, teams.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)

	case errors.Is(err, teams.ErrNotFound):
		http.Error(w, "You are not a member of this team.", http.StatusForbidden)

	default:
		if h.Log != nil {
			h.Log.Error("a settings authorisation check failed", "error", err)
		}

		http.Error(w, "The request could not be answered.", http.StatusInternalServerError)
	}
}

// redirect sends the browser back to a page with a message.
//
// Every form posts and redirects rather than rendering in place, so a refresh
// re-reads the page instead of re-sending the form — which for "create an API
// key" would otherwise mean a second key every time somebody pressed reload.
func (h *TeamHandler) redirect(w http.ResponseWriter, r *http.Request, to, notice, problem string) {
	// The parameter names are the ones flash() reads, which are the site
	// configuration screens' names too. One surface with two spellings of
	// "here is what just happened" is a message that silently vanishes the
	// first time a redirect crosses from one of these screens to the other.
	query := url.Values{}
	if teamID := r.FormValue("team_id"); teamID != "" {
		query.Set("team_id", teamID)
	}

	if notice != "" {
		query.Set("ok", notice)
	}
	if problem != "" {
		query.Set("err", problem)
	}

	if len(query) > 0 {
		to += "?" + query.Encode()
	}

	http.Redirect(w, r, to, http.StatusSeeOther)
}

// FirstTeamOwner resolves the install's first team and its owner.
//
// It exists for a test that drives these screens with no session in front of
// them, and it is deliberately the dullest possible implementation: the lowest
// team id and the owner of it. It reads nothing off the request, so a process
// that used it would let anybody who can reach the port administer that team —
// which is why the serving process supplies the signed-in user instead and this
// is never the default there.
func FirstTeamOwner(control *sql.DB) func(*http.Request) (Identity, error) {
	return func(r *http.Request) (Identity, error) {
		var identity Identity

		err := control.QueryRowContext(r.Context(), `
			SELECT team_memberships.team_id, team_memberships.user_id
			FROM team_memberships
			WHERE team_memberships.role = 'owner'
			ORDER BY team_memberships.team_id, team_memberships.user_id
			LIMIT 1
		`).Scan(&identity.TeamID, &identity.UserID)
		if err != nil {
			return Identity{}, fmt.Errorf("settings: no team owner exists yet: %w", err)
		}

		return identity, nil
	}
}

// teamName reads a team's name for the page header.
func teamName(ctx context.Context, control *sql.DB, teamID int64) string {
	var name string

	if err := control.QueryRowContext(ctx, `SELECT name FROM teams WHERE id = ?`, teamID).Scan(&name); err != nil {
		return "Your team"
	}

	return name
}

// ago renders a deadline as a phrase. It is written here rather than pulled
// from a library because there are exactly two shapes on these screens — "in 41
// hours" and "3 hours ago" — and a dependency for two sentences is a
// dependency for two sentences.
func ago(target, now time.Time) string {
	delta := target.Sub(now)

	if delta >= 0 {
		return "in " + roughly(delta)
	}

	return roughly(-delta) + " ago"
}

// roughly renders a duration at the coarsest unit that still says something.
func roughly(delta time.Duration) string {
	switch {
	case delta < time.Minute:
		return "less than a minute"
	case delta < time.Hour:
		return plural(int(delta.Minutes()), "minute")
	case delta < 48*time.Hour:
		return plural(int(delta.Hours()), "hour")
	}

	return plural(int(delta.Hours()/24), "day")
}

// plural renders a count with its unit.
func plural(count int, unit string) string {
	if count == 1 {
		return "1 " + unit
	}

	return fmt.Sprintf("%d %ss", count, unit)
}

// stamp renders a unix time for a table cell.
func stamp(unix int64) string {
	if unix == 0 {
		return "—"
	}

	return time.Unix(unix, 0).UTC().Format("2 Jan 15:04 MST")
}

// headerFor asks the application for the bar.
//
// Nothing injected means a test rendering this screen on its own, and it gets
// an empty bar — but a running process without it would draw a header with no
// name, no picture and a sign-out button that fails its own token check, so it
// says so once rather than serving that quietly.
func (h *TeamHandler) headerFor(r *http.Request) appui.Header {
	if h.Header == nil {
		if h.Log != nil {
			h.Log.Warn("a settings screen was rendered with no account bar wired in")
		}

		return appui.Header{}
	}

	return h.Header(r)
}
