//
// handlers_google.go
// The two requests a Google sign-in is made of.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"net/http"
)

// startGoogle redirects to Google's consent screen.
//
// With no credentials configured this is a message rather than a broken
// redirect. The button is hidden on the sign-in page, so reaching here at all
// means somebody typed the URL — and an honest "this is not configured" is more
// use to them than a 404.
func (h *Handler) startGoogle(w http.ResponseWriter, r *http.Request) {
	if !h.Google.Configured() {
		p := h.newPage(r, tr(r, "auth.title.google_disabled"), "")
		p.Error = h.Google.DisabledReason() + "."

		h.render(w, r, "error", p, http.StatusNotFound)

		return
	}

	pkce, err := NewPKCE()
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if err := SetOAuthStateCookie(w, h.Sealer, pkce, safeNext(r.URL.Query().Get("next")), h.BaseURL); err != nil {
		h.fail(w, r, err)
		return
	}

	http.Redirect(w, r, h.Google.AuthURL(pkce), http.StatusFound)
}

// finishGoogle handles the callback.
//
// The state from the cookie and the state in the query have to match. Without
// that check the callback accepts an authorization code from anybody, which is
// how somebody gets logged into an account that is not theirs — or gets their
// own Google identity attached to a victim's session.
func (h *Handler) finishGoogle(w http.ResponseWriter, r *http.Request) {
	verifier, state, next, err := ReadOAuthStateCookie(w, r, h.Sealer, h.BaseURL)
	if err != nil {
		h.googleError(w, r, err.Error())
		return
	}

	if r.URL.Query().Get("state") != state {
		h.Log.Warn("google callback state did not match", "path", requestLogPath(r))
		h.googleError(w, r, tr(r, "auth.error.google_state"))

		return
	}

	// Google reports a declined consent screen as an error parameter rather
	// than an absent code, and "you clicked cancel" is not a failure worth
	// showing an error page for.
	if reason := r.URL.Query().Get("error"); reason != "" {
		if reason == "access_denied" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		h.googleError(w, r, tr(r, "auth.error.google_refused", "reason", reason))

		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		h.googleError(w, r, tr(r, "auth.error.google_no_code"))
		return
	}

	profile, err := h.Google.Exchange(r.Context(), code, verifier)
	if err != nil {
		h.Log.Warn("google token exchange failed", "error", err)
		h.googleError(w, r, tr(r, "auth.error.google_exchange"))

		return
	}

	_, invited := h.pendingInvitation(r)
	user, created, err := h.Store.resolveProfile(r.Context(), profile, !h.DisableRegistration || invited)
	if err != nil {
		h.Log.Warn("google profile could not be resolved", "error", err)
		h.googleError(w, r, err.Error())

		return
	}

	if created {
		h.Log.Info("account created through google", "user", user.ID)
	}

	// Every sign-in, not only the first: somebody who changes their Google
	// photo expects to see it here, and this is the cheapest trigger that
	// notices.
	h.Avatars.FromGoogle(r.Context(), user.ID, profile.Picture)

	// Two-factor still applies. A second factor that a federated login skips is
	// not a second factor; it is a setting.
	if user.TwoFactorEnabled() {
		h.setPendingTwoFactor(w, user.ID, next)
		http.Redirect(w, r, "/login/2fa", http.StatusFound)

		return
	}

	h.startSession(w, r, user)
	if created || invited {
		request := r.WithContext(context.WithValue(r.Context(), contextUser, user))
		http.Redirect(w, r, h.afterVerification(request, next), http.StatusFound)
		return
	}

	http.Redirect(w, r, next, http.StatusFound)
}

// googleError renders a failed federated sign-in with a way back.
func (h *Handler) googleError(w http.ResponseWriter, r *http.Request, message string) {
	p := h.newPage(r, tr(r, "auth.title.google"), "")
	p.Error = message

	h.render(w, r, "error", p, http.StatusBadRequest)
}
