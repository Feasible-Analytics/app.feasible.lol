//
// csrf.go
// The token that proves a form submission came from one of our own pages.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

// csrfCookieName carries the token the forms echo back.
const csrfCookieName = "feasible_csrf"

// csrfField is the form field name every template posts it in.
const csrfField = "csrf_token"

// csrfHeader is where a JSON request carries the same token, since a JSON body
// has no form fields to read it out of.
const csrfHeader = "X-CSRF-Token"

// csrfToken returns the token for this request, minting one if the browser does
// not have it yet.
//
// This is the signed double-submit pattern: the token lives in a cookie and is
// echoed in the form body, and a request is accepted only when the two agree.
// Another origin can cause the browser to send our cookie, but it cannot read
// it, so it cannot put the matching value in the body. The signature is what
// stops a subdomain — or anything else that can write a cookie for this host —
// from choosing both halves itself.
//
// SameSite=Lax on the session cookie already blocks the cross-site form post
// this defends against, in every browser that honours it. This is the second
// lock, because "every browser" is not a claim worth betting an account on.
func (h *Handler) csrfToken(r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookieName); err == nil {
		if value, ok := h.Sealer.VerifySignedValue(cookie.Value); ok {
			return value
		}
	}

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		// Failing closed here would mean no form on the page can be submitted.
		// Failing to an empty token means checkCSRF rejects everything, which
		// is the same outcome with a message the user can act on.
		return ""
	}

	return base64.RawURLEncoding.EncodeToString(raw)
}

// issueCSRF writes the cookie for a token. It is called on every render rather
// than only when the token is new, so the cookie's lifetime keeps pace with the
// session's and a long-lived tab does not fail on submit.
func (h *Handler) issueCSRF(w http.ResponseWriter, token string) {
	if token == "" {
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    h.Sealer.SignedValue(token),
		Path:     "/",
		HttpOnly: true,
		// No Domain, for the same reason the session cookie has none: a cookie
		// scoped to the configured base URL is silently dropped on every other
		// hostname the dashboard can be reached on, and a CSRF cookie that does
		// not arrive turns every form into a 403.
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionIdleWindow.Seconds()),
	})
}

// checkCSRF verifies a submission. It returns false and writes the response
// itself, so a handler's first line can be a plain guard clause.
func (h *Handler) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		h.Log.Warn("form submitted with no csrf cookie", "path", requestLogPath(r))
		http.Error(w, "Your browser did not send the form token. Reload the page and try again.", http.StatusForbidden)

		return false
	}

	expected, ok := h.Sealer.VerifySignedValue(cookie.Value)
	if !ok {
		h.Log.Warn("form submitted with an unsigned csrf cookie", "path", requestLogPath(r))
		http.Error(w, "The form token could not be verified. Reload the page and try again.", http.StatusForbidden)

		return false
	}

	// The drag-and-drop save posts JSON rather than a form, so the token comes
	// in a header there. It is the same token and the same check — only the
	// envelope differs.
	submitted := r.Header.Get(csrfHeader)
	if submitted == "" {
		submitted = r.PostFormValue(csrfField)
	}

	if !constantTimeEqual(expected, submitted) {
		h.Log.Warn("form token did not match", "path", requestLogPath(r))
		http.Error(w, "The form token did not match. Reload the page and try again.", http.StatusForbidden)

		return false
	}

	return true
}

// IssueCSRF returns the current form token and refreshes its signed cookie.
// Settings screens live in a separate package and use this narrow callback so
// they share the application's CSRF implementation without duplicating keys or
// cookie policy.
func (h *Handler) IssueCSRF(w http.ResponseWriter, r *http.Request) string {
	token := h.csrfToken(r)
	h.issueCSRF(w, token)

	return token
}

// CheckCSRF exposes the application's submission check to separately owned
// settings routes mounted behind the same signed-in handler.
func (h *Handler) CheckCSRF(w http.ResponseWriter, r *http.Request) bool {
	return h.checkCSRF(w, r)
}
