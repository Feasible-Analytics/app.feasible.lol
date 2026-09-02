//
// web.go
// The server-rendered surface: what it is made of, and every route it answers.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/destructive"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/outbound"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
)

// contextKey is this package's private key type for request-scoped values. A
// named type rather than a string is what stops another package's context value
// from colliding with ours under the same name.
type contextKey string

// The values are prefixed with the product rather than the package so they
// cannot be mistaken for message ids: everything named auth.* in this tree is a
// string in the catalogue, and the completeness check reads that literally.
const (
	contextUser    contextKey = "feasible.auth.user"
	contextSession contextKey = "feasible.auth.session"
	contextTeam    contextKey = "feasible.auth.team"
)

// Request authorization failures are sentinels so an API can return the right
// status without matching a sentence intended for a browser.
var (
	ErrUnauthenticated = errors.New("auth: no authenticated session")
	ErrSiteForbidden   = errors.New("auth: this session cannot access the site")
	ErrTeamRequired    = errors.New("auth: an explicit team_id is required")
)

// Handler is the whole server-rendered application. Everything it needs is a
// field rather than a package-level variable, so a test can build one over a
// temporary database and drive real requests through it.
type Handler struct {
	Store       *Store
	Teams       *teams.Store
	Traffic     *Traffic
	Mailer      *mail.Mailer
	Sealer      *Sealer
	Google      *Google
	Deleter     *Deleter
	Destructive *destructive.Service
	Limiter     *Limiter
	Keyer       *tracker.Keyer

	// Trusted is the proxy allow-list every rate limit resolves a client
	// address through. Behind our own reverse proxy every connection arrives
	// from the proxy, so without it ten bad passwords from anybody lock sign-in
	// for everybody. Nil trusts no forwarded header, which is the safe default.
	Trusted *clientip.TrustedProxies

	// SiteCache is the routing map the ingest path reads. A newly created site
	// is pushed into it directly rather than waiting for the next rebuild, so
	// that the snippet somebody pastes seconds later already resolves.
	SiteCache *sites.Cache

	// ProvisionSite creates account-backed defaults immediately after the
	// control row is committed. The hook keeps auth independent of analytics
	// packages while making site creation one product-level operation.
	ProvisionSite func(context.Context, int64, int64, time.Time) error

	// Access is the account lock. Almost nothing this handler serves is gated —
	// signing in, settings and the route to billing all stay open, because
	// locking somebody out of the page where they would pay us is self-
	// defeating — but the sites list draws a sparkline per site, and a
	// sparkline is the account's numbers. It is a function rather than the gate
	// itself so the signed-in application does not depend on billing existing,
	// and nil locks nothing.
	Access func(accountID int64) bool

	// DisableRegistration closes only public account creation. Invitations can
	// still add members to an operator-created account, and every existing
	// identity can continue to sign in through password or Google.
	DisableRegistration bool

	// DisableCommerce removes billing navigation in the unrestricted
	// self-hosted product. Routes may remain mounted for stable URLs, but the
	// signed-in interface must not present an upgrade path where none applies.
	DisableCommerce bool

	BaseURL string
	Log     *logger.Logger

	// Verifier fetches a customer's page during the installation check. It is a
	// field so a test can answer without a network, and so the timeout is set
	// in one place. It dials through the outbound policy: the domain is a value
	// the customer typed, so without that the check is a way to ask the server
	// what it can see on its own network and read the answer back.
	Verifier *http.Client

	views *views
	mux   *http.ServeMux
}

// DashboardNavigation is the permission-aware product map the React shell may
// expose. It deliberately carries resolved URLs rather than raw ids so locale,
// team selection, and authorization stay server decisions.
type DashboardNavigation struct {
	Name            string
	Email           string
	SitesURL        string
	SiteSettingsURL string
	ConversionsURL  string
	AccountURL      string
	BillingURL      string
	ExportURL       string
	LogoutURL       string
	CSRF            string
	TeamID          int64
}

// Options are the inputs to NewHandler.
type Options struct {
	Store               *Store
	Teams               *teams.Store
	Traffic             *Traffic
	Mailer              *mail.Mailer
	Sealer              *Sealer
	Google              *Google
	Deleter             *Deleter
	Destructive         *destructive.Service
	Keyer               *tracker.Keyer
	Trusted             *clientip.TrustedProxies
	SiteCache           *sites.Cache
	ProvisionSite       func(context.Context, int64, int64, time.Time) error
	Access              func(accountID int64) bool
	DisableRegistration bool
	DisableCommerce     bool
	BaseURL             string
	Log                 *logger.Logger

	// OutboundPolicy bounds where the installation check may connect. Its zero
	// value refuses loopback and every private range, which is the safe default
	// for a build that forgets to set it.
	OutboundPolicy outbound.Policy
}

// NewHandler builds the application and parses its templates.
//
// Templates are parsed once at construction rather than per request, so a
// broken template is a start-up failure rather than a 500 on whichever page
// nobody visits until Friday.
func NewHandler(opts Options) (*Handler, error) {
	views, err := newViews()
	if err != nil {
		return nil, err
	}

	teamStore := opts.Teams
	if teamStore == nil && opts.Store != nil {
		teamStore = teams.NewStore(opts.Store.DB())
	}

	h := &Handler{
		Store:               opts.Store,
		Teams:               teamStore,
		Traffic:             opts.Traffic,
		Mailer:              opts.Mailer,
		Sealer:              opts.Sealer,
		Google:              opts.Google,
		Deleter:             opts.Deleter,
		Destructive:         opts.Destructive,
		Limiter:             NewLimiter(),
		Keyer:               opts.Keyer,
		Trusted:             opts.Trusted,
		SiteCache:           opts.SiteCache,
		ProvisionSite:       opts.ProvisionSite,
		Access:              opts.Access,
		DisableRegistration: opts.DisableRegistration,
		DisableCommerce:     opts.DisableCommerce,
		BaseURL:             strings.TrimRight(opts.BaseURL, "/"),
		Log:                 opts.Log,
		Verifier:            opts.OutboundPolicy.NewClient(verifyTimeout),
		views:               views,
	}

	h.mux = h.routes()

	return h, nil
}

// ServeHTTP dispatches to the route table.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i18n.Apply(w, r)
	h.mux.ServeHTTP(&languageResponseWriter{ResponseWriter: w, request: r}, r)
}

// languageResponseWriter adds the negotiated locale to same-origin redirects.
// External OAuth destinations pass through LocalURL unchanged.
type languageResponseWriter struct {
	http.ResponseWriter
	request *http.Request
}

// WriteHeader preserves locale state immediately before a redirect is written.
func (w *languageResponseWriter) WriteHeader(status int) {
	if status >= 300 && status < 400 {
		location := w.Header().Get("Location")
		if location != "" {
			w.Header().Set("Location", i18n.LocalURL(location, i18n.Negotiate(w.request)))
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

// GuardSite wraps a handler owned by another package in this package's sign-in
// check, and refuses a site the signed-in team does not own.
//
// It exists so that a screen living outside this package is not thereby outside
// its authorisation. The site configuration screens edit which traffic a site
// counts and prepare a full archive of it, and mounting them beside the signed
// in application without this would make both reachable by anybody who can
// reach the port.
//
// A request naming no site is let through on sign-in alone. That is what the
// OAuth callback needs: Google redirects to one registered URI, so that request
// carries its site in the state parameter and is authorised when the state is
// verified rather than from the path.
func (h *Handler) GuardSite(domainOf func(*http.Request) string, next http.Handler) http.Handler {
	return h.GuardSitePermission(domainOf, teams.PermManageSiteSettings, next)
}

// GuardSitePermission protects an HTML handler with a session and a live
// site-role check. The requested site, not a user's first team, supplies the
// authorization context, so guests and multi-team users resolve correctly.
func (h *Handler) GuardSitePermission(domainOf func(*http.Request) string, permission teams.Permission, next http.Handler) http.Handler {
	return http.HandlerFunc(h.require(func(w http.ResponseWriter, r *http.Request) {
		domain := domainOf(r)
		if domain == "" {
			next.ServeHTTP(w, r)
			return
		}

		site, known := h.SiteCache.Lookup(domain)
		if !known {
			h.notFound(w, r)
			return
		}

		if err := h.authoriseCurrentSite(r, site.ID, permission); err != nil {
			h.notFound(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}))
}

// OptionalAccount attaches a valid session and its account when both exist,
// then always serves the public page. Pricing uses it to turn its call to
// action into an authenticated checkout form without making prices private.
func (h *Handler) OptionalAccount(next http.Handler) http.Handler {
	return h.optional(func(w http.ResponseWriter, r *http.Request) {
		user := userFrom(r)
		explicitTeam := strings.TrimSpace(r.FormValue("team")) != ""
		if user == nil {
			if explicitTeam {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !user.Verified() {
			if explicitTeam {
				http.Redirect(w, r, verificationPath(r.URL.RequestURI()), http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		requested, err := billingTeamID(r)
		if err != nil {
			h.notFound(w, r)
			return
		}

		team, _, err := h.Store.BillingTeamForUser(r.Context(), user.ID, requested)
		if err != nil {
			if requested > 0 {
				h.notFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if team.Require2FA && !user.TwoFactorEnabled() {
			if requested > 0 {
				http.Redirect(w, r, "/settings/security?required=1", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), contextTeam, team))

		next.ServeHTTP(w, r)
	})
}

// RequireAccount admits only a fully verified session that belongs to an
// account. It deliberately reuses require so email verification and team-wide
// two-factor policy cannot drift between billing and the rest of the app.
func (h *Handler) RequireAccount(next http.Handler) http.Handler {
	return h.require(func(w http.ResponseWriter, r *http.Request) {
		requested, err := billingTeamID(r)
		if err != nil {
			h.notFound(w, r)
			return
		}

		team, _, err := h.Store.BillingTeamForUser(r.Context(), userFrom(r).ID, requested)
		if err != nil {
			h.notFound(w, r)
			return
		}
		if team.Require2FA && !userFrom(r).TwoFactorEnabled() {
			http.Redirect(w, r, "/settings/security?required=1", http.StatusFound)
			return
		}

		r = r.WithContext(context.WithValue(r.Context(), contextTeam, team))
		next.ServeHTTP(w, r)
	})
}

// billingTeamID parses the optional account selector used by billing GETs and
// POSTs. Authorization always happens after parsing through the membership
// table, so naming another account never grants access to it.
func billingTeamID(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.FormValue("team"))
	if raw == "" {
		return 0, nil
	}

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("auth: %q is not a billing account id", raw)
	}

	return id, nil
}

// CurrentAccount returns the account and checkout address established by the
// authentication middleware. Any requested account has already been resolved
// through membership and billing role, so raw request data never reaches the
// billing service as authorization.
func (h *Handler) CurrentAccount(r *http.Request) (int64, string, error) {
	user := userFrom(r)
	team := teamFrom(r)
	if user == nil || team == nil {
		return 0, "", fmt.Errorf("auth: request has no authenticated account")
	}

	return team.ID, user.Email, nil
}

// ValidateForm applies the same signed double-submit check used by every auth
// form. It writes the 403 response itself when validation fails.
func (h *Handler) ValidateForm(w http.ResponseWriter, r *http.Request) bool {
	return h.CheckFormToken(w, r)
}

// GuardSiteAPI protects a JSON endpoint with the same session, verification,
// two-factor and live site-role checks as the HTML surface. permission may
// choose by method, which is how annotation reads remain available to viewers
// while writes require an editor.
func (h *Handler) GuardSiteAPI(domainOf func(*http.Request) string, permission func(*http.Request) teams.Permission, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, session, ok := h.currentUser(r)
		if !ok || !user.Verified() {
			h.apiAccessError(w, http.StatusUnauthorized, "an authenticated, verified session is required")
			return
		}

		site, known := h.SiteCache.Lookup(domainOf(r))
		if !known {
			h.apiAccessError(w, http.StatusNotFound, "no such site")
			return
		}

		ctx := context.WithValue(r.Context(), contextUser, user)
		ctx = context.WithValue(ctx, contextSession, session)
		r = r.WithContext(ctx)

		if err := h.authoriseCurrentSite(r, site.ID, permission(r)); err != nil {
			status := http.StatusForbidden
			if errors.Is(err, teams.ErrNotFound) {
				status = http.StatusNotFound
			}

			h.apiAccessError(w, status, "this session cannot access the site")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !h.CheckFormToken(w, r) {
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// AuthoriseSiteRequest validates a session for one site without writing a
// response. The stats capability layer uses it as one of its authorization
// choices alongside public and shared-link access.
func (h *Handler) AuthoriseSiteRequest(r *http.Request, siteID int64, permission teams.Permission) (*User, error) {
	user, _, ok := h.currentUser(r)
	if !ok || !user.Verified() {
		return nil, ErrUnauthenticated
	}

	request := r.WithContext(context.WithValue(r.Context(), contextUser, user))
	if err := h.authoriseCurrentSite(request, siteID, permission); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSiteForbidden, err)
	}

	return user, nil
}

// authoriseCurrentSite checks the user already attached to a request against
// the site's current team and its two-factor policy.
func (h *Handler) authoriseCurrentSite(r *http.Request, siteID int64, permission teams.Permission) error {
	user := userFrom(r)
	if user == nil || h.Teams == nil {
		return ErrUnauthenticated
	}

	var requireTwoFactor bool

	err := h.Store.DB().QueryRowContext(r.Context(), `
		SELECT teams.require_2fa
		FROM sites
		JOIN teams ON teams.id = COALESCE(sites.owner_team_id, sites.account_id)
		WHERE sites.id = ?
	`, siteID).Scan(&requireTwoFactor)
	if errors.Is(err, sql.ErrNoRows) {
		return teams.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("auth: read site security policy: %w", err)
	}

	if requireTwoFactor && !user.TwoFactorEnabled() {
		return ErrTwoFactorNeeded
	}

	_, err = h.Teams.AuthoriseSite(r.Context(), siteID, user.ID, permission)

	return err
}

// apiAccessError writes the fixed JSON shape every data endpoint uses.
func (h *Handler) apiAccessError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// routes builds the route table.
//
// Method-qualified patterns mean a GET to a form-submission path is a 405
// rather than a page that renders and then does nothing, which is the failure
// mode that wastes an afternoon when a redirect goes to the wrong verb.
func (h *Handler) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// The static assets. They are served from the binary, so a release has
	// nothing to copy alongside it and no CDN to be blocked by.
	mux.Handle("GET /app/assets/", http.StripPrefix("/app/assets/", assetHandler()))

	// Signed out.
	mux.HandleFunc("GET /{$}", h.handleRoot)
	mux.HandleFunc("GET /register", h.optional(h.showRegister))
	mux.HandleFunc("POST /register", h.optional(h.doRegister))
	mux.HandleFunc("GET /login", h.optional(h.showLogin))
	mux.HandleFunc("POST /login", h.optional(h.doLogin))
	mux.HandleFunc("POST /logout", h.optional(h.doLogout))
	mux.HandleFunc("GET /forgot-password", h.optional(h.showForgot))
	mux.HandleFunc("POST /forgot-password", h.optional(h.doForgot))
	mux.HandleFunc("GET /reset-password", h.optional(h.showReset))
	mux.HandleFunc("POST /reset-password", h.optional(h.doReset))
	mux.HandleFunc("GET /invitations/{token}", h.optional(h.beginInvitation))

	// Half-signed-in: the address is not proven, or the second factor is not
	// answered yet. These deliberately do not go through requireUser.
	mux.HandleFunc("GET /verify-email", h.optional(h.showVerify))
	mux.HandleFunc("POST /verify-email", h.optional(h.doVerify))
	mux.HandleFunc("POST /verify-email/resend", h.optional(h.doResendVerify))
	mux.HandleFunc("GET /verify-email/confirm", h.optional(h.doVerifyLink))
	mux.HandleFunc("GET /login/2fa", h.optional(h.showTwoFactorChallenge))
	mux.HandleFunc("POST /login/2fa", h.optional(h.doTwoFactorChallenge))

	// Google, which answers with a hidden button and a clear log line rather
	// than a 404 when no credentials are configured.
	mux.HandleFunc("GET /auth/google", h.optional(h.startGoogle))
	mux.HandleFunc("GET /auth/google/callback", h.optional(h.finishGoogle))

	// Signed in.
	mux.HandleFunc("GET /sites", h.require(h.showSites))
	mux.HandleFunc("GET /sites/new", h.require(h.showNewSite))
	mux.HandleFunc("POST /sites/new", h.require(h.doNewSite))
	mux.HandleFunc("POST /sites/{id}/pin", h.require(h.doPinSite))
	mux.HandleFunc("GET /sites/{id}/settings", h.require(h.showSiteSettings))
	mux.HandleFunc("GET /sites/domain/{domain}/settings", h.require(h.showSiteSettingsByDomain))
	mux.HandleFunc("POST /sites/{id}/settings", h.require(h.doSiteGeneral))
	mux.HandleFunc("POST /sites/{id}/domain", h.require(h.doSiteDomain))
	mux.HandleFunc("POST /sites/{id}/reset", h.require(h.doSiteReset))
	mux.HandleFunc("POST /sites/{id}/delete", h.require(h.doSiteDelete))
	mux.HandleFunc("POST /sites/{id}/transfer", h.require(h.doSiteTransfer))
	mux.HandleFunc("POST /api/sites/{id}/transfer", h.require(h.doSiteTransferAPI))
	mux.HandleFunc("GET /invitations/accept", h.require(h.acceptInvitation))

	mux.HandleFunc("POST /folders", h.require(h.doCreateFolder))
	mux.HandleFunc("POST /folders/{id}/rename", h.require(h.doRenameFolder))
	mux.HandleFunc("POST /folders/{id}/delete", h.require(h.doDeleteFolder))
	mux.HandleFunc("POST /folders/reorder", h.require(h.doReorder))

	mux.HandleFunc("GET /onboarding/{id}", h.require(h.showOnboarding))
	mux.HandleFunc("GET /onboarding/{id}/status", h.require(h.onboardingStatus))
	mux.HandleFunc("POST /onboarding/{id}/verify", h.require(h.doVerifyInstall))
	mux.HandleFunc("POST /onboarding/{id}/skip", h.require(h.doSkipOnboarding))

	mux.HandleFunc("GET /settings", h.require(h.showAccountSettings))
	mux.HandleFunc("POST /settings/profile", h.require(h.doUpdateProfile))
	mux.HandleFunc("POST /settings/password", h.require(h.doChangePassword))
	mux.HandleFunc("POST /settings/delete", h.require(h.doDeleteAccount))
	mux.HandleFunc("POST /settings/confirm-code", h.require(h.doSendConfirmation))
	mux.HandleFunc("GET /settings/sessions", h.require(h.showSessions))
	mux.HandleFunc("POST /settings/sessions/revoke", h.require(h.doRevokeSession))
	mux.HandleFunc("GET /settings/security", h.require(h.showSecurity))
	mux.HandleFunc("POST /settings/security/2fa/start", h.require(h.doStartTwoFactor))
	mux.HandleFunc("GET /settings/security/2fa/qr.png", h.require(h.twoFactorQR))
	mux.HandleFunc("POST /settings/security/2fa/enable", h.require(h.doEnableTwoFactor))
	mux.HandleFunc("POST /settings/security/2fa/disable", h.require(h.doDisableTwoFactor))
	mux.HandleFunc("POST /settings/security/2fa/recovery", h.require(h.doRegenerateRecovery))
	mux.HandleFunc("GET /settings/team", h.require(h.showTeamSettings))
	mux.HandleFunc("POST /settings/team", h.require(h.doTeamSettings))

	return mux
}

// handleRoot sends people where they belong. The root is not a page: signed in
// it is the sites list, signed out it is the sign-in form, and rendering
// something in between would be a screen nobody ever wants to be on.
func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.currentUser(r); ok {
		http.Redirect(w, r, "/sites", http.StatusFound)
		return
	}

	http.Redirect(w, r, "/login", http.StatusFound)
}

// currentUser resolves the session cookie. It returns three values rather than
// an error because "not signed in" is the ordinary case on half these routes
// and is not a failure.
func (h *Handler) currentUser(r *http.Request) (*User, *Session, bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, nil, false
	}

	session, err := h.Store.SessionByToken(r.Context(), cookie.Value)
	if err != nil {
		return nil, nil, false
	}

	user, err := h.Store.UserByID(r.Context(), session.UserID)
	if err != nil {
		return nil, nil, false
	}

	return user, session, true
}

// optional attaches the signed-in user when there is one and carries on either
// way. It is what the sign-in and sign-up pages use, so that somebody who is
// already signed in can be redirected rather than shown a login form.
func (h *Handler) optional(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if user, session, ok := h.currentUser(r); ok {
			ctx := context.WithValue(r.Context(), contextUser, user)
			ctx = context.WithValue(ctx, contextSession, session)
			r = r.WithContext(ctx)
		}

		next(w, r)
	}
}

// require refuses anybody who is not fully signed in, and it is the only place
// that decides what "fully" means.
//
// The three gates are in a deliberate order. An unverified address goes to the
// verification screen, because everything downstream assumes we can reach the
// person by email. A team that requires two-factor sends a member who has not
// enrolled to the enrolment screen — not to an error, which would lock the team
// out of its own account the moment an admin turned the policy on.
func (h *Handler) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, session, ok := h.currentUser(r)
		if !ok {
			// The path is carried through the redirect so that a bookmarked
			// deep link survives the detour through sign-in.
			http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}

		if !user.Verified() {
			http.Redirect(w, r, "/verify-email", http.StatusFound)
			return
		}

		ctx := context.WithValue(r.Context(), contextUser, user)
		ctx = context.WithValue(ctx, contextSession, session)
		r = r.WithContext(ctx)

		team, teamErr := h.Store.TeamForUser(r.Context(), user.ID)
		if teamErr == nil {
			ctx = context.WithValue(r.Context(), contextTeam, team)
			r = r.WithContext(ctx)
		}
		requireTwoFactor, err := h.userRequiresTwoFactor(r.Context(), user.ID)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if requireTwoFactor && !user.TwoFactorEnabled() {
			// The enrolment screen itself has to stay reachable, or the policy
			// is a door locked from the inside.
			if !strings.HasPrefix(r.URL.Path, "/settings/security") {
				http.Redirect(w, r, "/settings/security?required=1", http.StatusFound)
				return
			}
		}

		next(w, r)
	}
}

// userRequiresTwoFactor checks every team membership rather than whichever row
// SQLite returns first. A multi-team user cannot bypass one team's policy by
// also belonging to a team without it.
func (h *Handler) userRequiresTwoFactor(ctx context.Context, userID int64) (bool, error) {
	var required bool

	err := h.Store.DB().QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM teams
			JOIN team_memberships ON team_memberships.team_id = teams.id
			WHERE team_memberships.user_id = ? AND teams.require_2fa = 1
		)
	`, userID).Scan(&required)
	if err != nil {
		return false, fmt.Errorf("auth: read two-factor policies: %w", err)
	}

	return required, nil
}

// Protect is require, for a handler this package does not own.
//
// The team, sharing, report and health screens live in their own package but
// are the same signed-in application to the person using them, and the three
// gates that decide what "signed in" means — a session, a verified address, a
// team's two-factor policy — must not be re-implemented over there where they
// could drift. One of them being wrong is somebody administering an account
// they are not in.
func (h *Handler) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(h.require(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !h.CheckFormToken(w, r) {
				return
			}
		}

		next.ServeHTTP(w, r)
	}))
}

// GuardDashboard requires a signed-in viewer and, when the URL names a site,
// checks that exact site. Assets and the bare picker route need only the
// session; every site dashboard is resolved through its live membership.
func (h *Handler) GuardDashboard(next http.Handler) http.Handler {
	return http.HandlerFunc(h.require(func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/dashboard/")
		domain, _, _ := strings.Cut(trimmed, "/")

		if domain != "" && domain != "assets" {
			site, ok := h.SiteCache.Lookup(domain)
			if !ok || h.authoriseCurrentSite(r, site.ID, teams.PermViewDashboard) != nil {
				h.notFound(w, r)
				return
			}
		}

		next.ServeHTTP(w, r)
	}))
}

// NavigationForDashboard resolves the current domain and role after
// GuardDashboard has authenticated the request. A missing domain still gets
// account-level links while the site picker chooses the first accessible site.
func (h *Handler) NavigationForDashboard(w http.ResponseWriter, r *http.Request) DashboardNavigation {
	user := userFrom(r)
	if user == nil {
		return DashboardNavigation{}
	}

	navigation := DashboardNavigation{
		Name:       user.DisplayName(),
		Email:      user.Email,
		SitesURL:   "/sites",
		AccountURL: "/settings",
		LogoutURL:  "/logout",
		CSRF:       h.FormToken(w, r),
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/dashboard/")
	domain, _, _ := strings.Cut(trimmed, "/")
	site, ok := h.SiteCache.Lookup(domain)
	if !ok {
		return navigation
	}

	role, err := h.Teams.SiteRole(r.Context(), site.ID, user.ID)
	if err != nil {
		return navigation
	}

	navigation.TeamID = site.TeamID
	navigation.SitesURL = "/sites?team_id=" + strconv.FormatInt(site.TeamID, 10)
	if teams.Can(role, teams.PermManageSiteSettings) {
		navigation.SiteSettingsURL = "/sites/domain/" + site.Domain + "/settings"
		navigation.ConversionsURL = "/settings/sites/" + site.Domain + "/conversions"
	}
	if role == teams.RoleOwner || role == teams.RoleAdmin || role == teams.RoleBilling {
		navigation.ExportURL = "/billing/export?team=" + strconv.FormatInt(site.TeamID, 10)
		if !h.DisableCommerce {
			navigation.BillingURL = "/billing?team=" + strconv.FormatInt(site.TeamID, 10)
		}
	}

	return navigation
}

// RoleForSite returns the current authenticated role for a separately rendered
// site screen. The surrounding guard remains the authorization boundary; this
// value only decides which already-authorized navigation links to draw.
func (h *Handler) RoleForSite(r *http.Request, siteID int64) teams.Role {
	user := userFrom(r)
	if user == nil {
		return ""
	}

	role, err := h.Teams.SiteRole(r.Context(), siteID, user.ID)
	if err != nil {
		return ""
	}

	return role
}

// GuardTeam protects a billing or administration handler with a live team-role
// check and the application's CSRF verifier for unsafe methods.
func (h *Handler) GuardTeam(permission teams.Permission, next http.Handler) http.Handler {
	return http.HandlerFunc(h.require(func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.teamForRequest(r, permission); err != nil {
			h.teamSelectionError(w, r, err)

			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !h.CheckFormToken(w, r) {
				return
			}
		}

		next.ServeHTTP(w, r)
	}))
}

// AuthoriseTeamRequest resolves the authenticated explicit team context for a
// handler that needs the team id after GuardTeam has admitted it.
func (h *Handler) AuthoriseTeamRequest(r *http.Request, permission teams.Permission) (int64, error) {
	team, err := h.teamForRequest(r, permission)
	if err != nil {
		return 0, err
	}

	return team.ID, nil
}

// FormToken issues and returns the application's signed double-submit token so
// server-rendered forms outside this package use the same CSRF boundary.
func (h *Handler) FormToken(w http.ResponseWriter, r *http.Request) string {
	token := h.csrfToken(r)
	h.issueCSRF(w, token)

	return token
}

// AccessibleDomains lists only the sites the signed-in user may view. Team
// memberships and per-site guest memberships are combined here so the
// dashboard picker cannot disclose another team's domains from the global
// routing cache.
func (h *Handler) AccessibleDomains(r *http.Request) ([]string, error) {
	user := userFrom(r)
	if user == nil {
		return nil, ErrUnauthenticated
	}

	rows, err := h.Store.DB().QueryContext(r.Context(), `
		SELECT sites.domain, team_memberships.role
		FROM sites
		JOIN team_memberships ON team_memberships.team_id = COALESCE(sites.owner_team_id, sites.account_id)
		WHERE team_memberships.user_id = ?
		UNION ALL
		SELECT sites.domain, guest_memberships.role
		FROM sites
		JOIN guest_memberships ON guest_memberships.site_id = sites.id
		WHERE guest_memberships.user_id = ?
		ORDER BY 1
	`, user.ID, user.ID)
	if err != nil {
		return nil, fmt.Errorf("auth: list accessible sites: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}
	var domains []string

	for rows.Next() {
		var (
			domain string
			role   teams.Role
		)

		if err := rows.Scan(&domain, &role); err != nil {
			return nil, fmt.Errorf("auth: list accessible sites: %w", err)
		}

		if teams.Can(role, teams.PermViewDashboard) && !seen[domain] {
			seen[domain] = true
			domains = append(domains, domain)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: list accessible sites: %w", err)
	}

	return domains, nil
}

// Identify reports who is signed in and which team they are acting in.
//
// It reads the values Protect put on the request rather than the database, so a
// handler behind it cannot be told about a different person from the one the
// gates admitted. Outside Protect there is nobody, and it says so rather than
// falling back to an account it guessed.
func (h *Handler) Identify(r *http.Request) (userID, teamID int64, err error) {
	user := userFrom(r)
	if user == nil {
		return 0, 0, fmt.Errorf("auth: nobody is signed in on this request")
	}

	if domain := r.PathValue("domain"); domain != "" {
		if site, ok := h.SiteCache.Lookup(domain); ok {
			if _, err := h.Teams.AuthoriseSite(r.Context(), site.ID, user.ID, teams.PermViewDashboard); err == nil {
				teamID, err := h.Teams.TeamIDForSite(r.Context(), site.ID)
				if err == nil {
					return user.ID, teamID, nil
				}
			}
		}
	}
	if strings.HasPrefix(r.URL.Path, "/sites/") || strings.HasPrefix(r.URL.Path, "/onboarding/") {
		siteID, parseErr := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if parseErr == nil && siteID > 0 {
			site, siteErr := h.Store.SiteByIDAny(r.Context(), siteID)
			if siteErr == nil {
				if _, siteErr = h.Teams.AuthoriseSite(r.Context(), site.ID, user.ID, teams.PermViewDashboard); siteErr == nil {
					return user.ID, site.TeamID, nil
				}
			}
		}
	}

	teamID, err = h.selectedTeamID(r, user.ID, teams.PermViewDashboard)
	if err != nil {
		return 0, 0, err
	}

	if _, err := h.Teams.Authorise(r.Context(), teamID, user.ID, teams.PermViewDashboard); err != nil {
		return 0, 0, err
	}

	return user.ID, teamID, nil
}

// teamForRequest resolves an explicit team context and enforces one permission
// against the live membership. A single eligible team is an unambiguous
// default; a multi-team user must name team_id.
func (h *Handler) teamForRequest(r *http.Request, permission teams.Permission) (*Team, error) {
	user := userFrom(r)
	if user == nil {
		return nil, ErrUnauthenticated
	}

	teamID, err := h.selectedTeamID(r, user.ID, permission)
	if err != nil {
		return nil, err
	}

	team, err := h.Store.TeamByID(r.Context(), teamID)
	if err != nil {
		return nil, err
	}
	if team.Require2FA && !user.TwoFactorEnabled() {
		return nil, ErrTwoFactorNeeded
	}

	if _, err := h.Teams.Authorise(r.Context(), teamID, user.ID, permission); err != nil {
		return nil, err
	}

	return team, nil
}

// selectedTeamID reads team_id or infers the sole team on which the user has
// the requested permission.
func (h *Handler) selectedTeamID(r *http.Request, userID int64, permission teams.Permission) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("team_id"))
	if raw == "" {
		raw = strings.TrimSpace(r.FormValue("team_id"))
	}

	if raw != "" {
		teamID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || teamID < 1 {
			return 0, ErrTeamRequired
		}

		return teamID, nil
	}

	if domain := strings.TrimSpace(r.URL.Query().Get("site_context")); domain != "" {
		site, ok := h.SiteCache.Lookup(domain)
		if !ok {
			return 0, ErrTeamRequired
		}

		return h.Teams.TeamIDForSite(r.Context(), site.ID)
	}

	ids, err := h.Teams.TeamIDs(r.Context(), userID, permission)
	if err != nil {
		return 0, err
	}
	if len(ids) != 1 {
		return 0, ErrTeamRequired
	}

	return ids[0], nil
}

// RequestUser returns the identity attached by one of this handler's guards.
// It never falls back to a cookie lookup, so a downstream handler cannot use
// it unless the request has already crossed the authorization boundary.
func RequestUser(r *http.Request) *User {
	return userFrom(r)
}

// requestLogPath removes bearer invitation material before a request path is
// written to an application log. The acceptance route contains no secret and
// remains distinguishable from the initial token-bearing route.
func requestLogPath(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/invitations/") && r.URL.Path != "/invitations/accept" {
		return "/invitations/[redacted]"
	}

	return r.URL.Path
}

// userFrom pulls the signed-in user back out of the request context.
func userFrom(r *http.Request) *User {
	user, _ := r.Context().Value(contextUser).(*User)

	return user
}

// sessionFrom pulls the current session back out of the request context.
func sessionFrom(r *http.Request) *Session {
	session, _ := r.Context().Value(contextSession).(*Session)

	return session
}

// teamFrom pulls the authenticated account back out of the request context.
func teamFrom(r *http.Request) *Team {
	team, _ := r.Context().Value(contextTeam).(*Team)

	return team
}

// pathID reads a numeric path wildcard. A malformed id is zero, which every
// caller then fails to find — a 404 rather than a 500, which is the right
// answer for a URL somebody typed wrong.
func pathID(r *http.Request, name string) int64 {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0
	}

	return id
}

// urlQueryEscape escapes a value for a query string. It is a tiny wrapper so
// the redirect lines above stay readable.
func urlQueryEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "&", "%26"), "?", "%3F")
}

// PruneExpired deletes the dead sessions and spent reset tokens. It is exported
// so the serving process can run it on a timer: neither table is load-bearing,
// but both accumulate rows that are only interesting to somebody who has stolen
// the file.
func (h *Handler) PruneExpired(ctx context.Context) {
	if sessions, err := h.Store.PruneSessions(ctx); err != nil {
		h.Log.Warn("could not prune expired sessions", "error", err)
	} else if sessions > 0 {
		h.Log.Debug("pruned expired sessions", "rows", sessions)
	}

	if resets, err := h.Store.PruneResets(ctx); err != nil {
		h.Log.Warn("could not prune spent password resets", "error", err)
	} else if resets > 0 {
		h.Log.Debug("pruned spent password resets", "rows", resets)
	}
}

// PruneInterval is how often the expired-row sweep runs. Hourly is far more
// often than it needs to be and costs one indexed delete.
const PruneInterval = time.Hour

// RunPrune sweeps until the context is cancelled.
func (h *Handler) RunPrune(ctx context.Context) {
	ticker := time.NewTicker(PruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.PruneExpired(ctx)
		}
	}
}
