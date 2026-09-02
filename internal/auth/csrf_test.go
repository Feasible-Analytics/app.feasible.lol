//
// csrf_test.go
// The double-submit token, and both envelopes it can arrive in.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// newCSRFHandler builds the smallest handler that can issue and check a token.
func newCSRFHandler(t *testing.T) *Handler {
	t.Helper()

	return &Handler{
		Sealer:  newTestSealer(t),
		BaseURL: "http://localhost:19312",
		Log:     logger.New(logger.Options{Level: "error"}),
	}
}

// TestCSRFCookieHasNoDomain checks the same rule the session cookie follows. A
// CSRF cookie scoped to the configured base URL is silently dropped on every
// other hostname the dashboard can be reached on, and a token that does not
// arrive turns every form into a 403.
func TestCSRFCookieHasNoDomain(t *testing.T) {
	h := newCSRFHandler(t)

	recorder := httptest.NewRecorder()
	h.issueCSRF(recorder, "a-token")

	cookie := recorder.Result().Cookies()[0]

	if cookie.Domain != "" {
		t.Errorf("the CSRF cookie must not carry a Domain, got %q", cookie.Domain)
	}

	if !cookie.HttpOnly {
		t.Error("the CSRF cookie should be HttpOnly — the token comes from the rendered form, not from script")
	}
}

// TestCSRFAcceptsAFormFieldAndAHeader checks both envelopes: an ordinary form
// post, and the JSON reorder request that has no form fields to put a token in.
func TestCSRFAcceptsAFormFieldAndAHeader(t *testing.T) {
	h := newCSRFHandler(t)

	issued := httptest.NewRecorder()
	h.issueCSRF(issued, "a-token")

	cookie := issued.Result().Cookies()[0]

	form := httptest.NewRequest(http.MethodPost, "/anything",
		strings.NewReader(url.Values{csrfField: {"a-token"}}.Encode()))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.AddCookie(cookie)

	if !h.CheckFormToken(httptest.NewRecorder(), form) {
		t.Error("a matching form field should be accepted")
	}

	json := httptest.NewRequest(http.MethodPost, "/folders/reorder", strings.NewReader("{}"))
	json.Header.Set("Content-Type", "application/json")
	json.Header.Set(csrfHeader, "a-token")
	json.AddCookie(cookie)

	if !h.CheckFormToken(httptest.NewRecorder(), json) {
		t.Error("a matching header should be accepted")
	}
}

// TestCSRFRefusesWhatItShould checks the three ways a submission fails, since a
// check that never refuses is not a check.
func TestCSRFRefusesWhatItShould(t *testing.T) {
	h := newCSRFHandler(t)

	issued := httptest.NewRecorder()
	h.issueCSRF(issued, "a-token")

	cookie := issued.Result().Cookies()[0]

	// No cookie at all.
	none := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(""))

	if h.CheckFormToken(httptest.NewRecorder(), none) {
		t.Error("a request with no CSRF cookie should be refused")
	}

	// A cookie the browser wrote itself, with no valid signature. This is the
	// whole reason the value is signed rather than just compared.
	forged := httptest.NewRequest(http.MethodPost, "/anything",
		strings.NewReader(url.Values{csrfField: {"chosen"}}.Encode()))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forged.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "chosen"})

	if h.CheckFormToken(httptest.NewRecorder(), forged) {
		t.Error("an unsigned CSRF cookie should be refused")
	}

	// The right cookie with the wrong body.
	mismatched := httptest.NewRequest(http.MethodPost, "/anything",
		strings.NewReader(url.Values{csrfField: {"a-different-token"}}.Encode()))
	mismatched.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mismatched.AddCookie(cookie)

	recorder := httptest.NewRecorder()

	if h.CheckFormToken(recorder, mismatched) {
		t.Error("a mismatched token should be refused")
	}

	if recorder.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", recorder.Code)
	}
}

// TestCSRFTokenIsStableAcrossRenders checks that reloading a page does not
// invalidate a form the user already has open in another tab.
func TestCSRFTokenIsStableAcrossRenders(t *testing.T) {
	h := newCSRFHandler(t)

	issued := httptest.NewRecorder()
	h.issueCSRF(issued, "a-token")

	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	request.AddCookie(issued.Result().Cookies()[0])

	if got := h.csrfToken(request); got != "a-token" {
		t.Errorf("want the existing token back, got %q", got)
	}

	// With no cookie a fresh one is minted rather than an empty string, or
	// every form on a first page load would be unsubmittable.
	if h.csrfToken(httptest.NewRequest(http.MethodGet, "/login", nil)) == "" {
		t.Error("a request with no cookie should get a new token")
	}
}
