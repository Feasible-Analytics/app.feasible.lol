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
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
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

// Handler is the whole server-rendered application. Everything it needs is a
// field rather than a package-level variable, so a test can build one over a
// temporary database and drive real requests through it.
type Handler struct {
	Store   *Store
	Traffic *Traffic
	Mailer  *mail.Mailer
	Sealer  *Sealer
	Google  *Google
	Deleter *Deleter
	Limiter *Limiter
	Keyer   *tracker.Keyer

	// SiteCache is the routing map the ingest path reads. A newly created site
	// is pushed into it directly rather than waiting for the next rebuild, so
	// that the snippet somebody pastes seconds later already resolves.
	SiteCache *sites.Cache

	// Access is the account lock. Almost nothing this handler serves is gated —
	// signing in, settings and the route to billing all stay open, because
	// locking somebody out of the page where they would pay us is self-
	// defeating — but the sites list draws a sparkline per site, and a
	// sparkline is the account's numbers. It is a function rather than the gate
	// itself so the signed-in application does not depend on billing existing,
	// and nil locks nothing.
	Access func(accountID int64) bool

	BaseURL string
	Log     *logger.Logger

	// Verifier fetches a customer's page during the installation check. It is a
	// field so a test can answer without a network, and so the timeout is set
	// in one place.
	Verifier *http.Client

	views *views
	mux   *http.ServeMux
}

// Options are the inputs to NewHandler.
type Options struct {
	Store     *Store
	Traffic   *Traffic
	Mailer    *mail.Mailer
	Sealer    *Sealer
	Google    *Google
	Deleter   *Deleter
	Keyer     *tracker.Keyer
	SiteCache *sites.Cache
	Access    func(accountID int64) bool
	BaseURL   string
	Log       *logger.Logger
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

	h := &Handler{
		Store:     opts.Store,
		Traffic:   opts.Traffic,
		Mailer:    opts.Mailer,
		Sealer:    opts.Sealer,
		Google:    opts.Google,
		Deleter:   opts.Deleter,
		Limiter:   NewLimiter(),
		Keyer:     opts.Keyer,
		SiteCache: opts.SiteCache,
		Access:    opts.Access,
		BaseURL:   strings.TrimRight(opts.BaseURL, "/"),
		Log:       opts.Log,
		Verifier:  &http.Client{Timeout: verifyTimeout},
		views:     views,
	}

	h.mux = h.routes()

	return h, nil
}

// ServeHTTP dispatches to the route table.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
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
	return http.HandlerFunc(h.require(func(w http.ResponseWriter, r *http.Request) {
		domain := domainOf(r)
		if domain == "" {
			next.ServeHTTP(w, r)
			return
		}

		team, err := h.Store.TeamForUser(r.Context(), userFrom(r).ID)
		if err != nil {
			h.notFound(w, r)
			return
		}

		// A site somebody else owns answers exactly as a site that does not
		// exist. Telling the difference would turn this into a way to ask which
		// domains are tracked here.
		site, err := h.Store.SiteByDomain(r.Context(), domain)
		if err != nil || site.AccountID != team.ID {
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

// FormToken returns the existing auth CSRF token and renews its signed cookie.
// Server-rendered packages use this callback without depending on auth types.
func (h *Handler) FormToken(w http.ResponseWriter, r *http.Request) string {
	token := h.csrfToken(r)
	h.issueCSRF(w, token)

	return token
}

// ValidateForm applies the same signed double-submit check used by every auth
// form. It writes the 403 response itself when validation fails.
func (h *Handler) ValidateForm(w http.ResponseWriter, r *http.Request) bool {
	return h.checkCSRF(w, r)
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
	mux.HandleFunc("POST /sites/{id}/settings", h.require(h.doSiteGeneral))
	mux.HandleFunc("POST /sites/{id}/domain", h.require(h.doSiteDomain))
	mux.HandleFunc("POST /sites/{id}/reset", h.require(h.doSiteReset))
	mux.HandleFunc("POST /sites/{id}/delete", h.require(h.doSiteDelete))

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

		team, err := h.Store.TeamForUser(r.Context(), user.ID)
		if err == nil {
			ctx = context.WithValue(r.Context(), contextTeam, team)
			r = r.WithContext(ctx)
		}
		if err == nil && team.Require2FA && !user.TwoFactorEnabled() {
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
