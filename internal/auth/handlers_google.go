//
// handlers_google.go
// The two requests a Google sign-in is made of.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
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
		p := h.newPage(r, "Google sign-in is not set up", "")
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
		h.Log.Warn("google callback state did not match", "path", r.URL.Path)
		h.googleError(w, r, "The sign-in could not be verified. Start again from the sign-in page.")

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

		h.googleError(w, r, "Google refused the sign-in: "+reason+".")

		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		h.googleError(w, r, "Google did not return a sign-in code. Start again from the sign-in page.")
		return
	}

	profile, err := h.Google.Exchange(r.Context(), code, verifier)
	if err != nil {
		h.Log.Warn("google token exchange failed", "error", err)
		h.googleError(w, r, "We could not complete the Google sign-in. Start again from the sign-in page.")

		return
	}

	user, created, err := h.Store.ResolveProfile(r.Context(), profile)
	if err != nil {
		h.Log.Warn("google profile could not be resolved", "error", err)
		h.googleError(w, r, err.Error())

		return
	}

	if created {
		h.Log.Info("account created through google", "user", user.ID)
	}

	// Two-factor still applies. A second factor that a federated login skips is
	// not a second factor; it is a setting.
	if user.TwoFactorEnabled() {
		h.setPendingTwoFactor(w, user.ID, next)
		http.Redirect(w, r, "/login/2fa", http.StatusFound)

		return
	}

	h.startSession(w, r, user)
	http.Redirect(w, r, next, http.StatusFound)
}

// googleError renders a failed federated sign-in with a way back.
func (h *Handler) googleError(w http.ResponseWriter, r *http.Request, message string) {
	p := h.newPage(r, "Google sign-in", "")
	p.Error = message

	h.render(w, r, "error", p, http.StatusBadRequest)
}
