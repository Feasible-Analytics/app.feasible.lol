//
// unsubscribe_test.go
// The page a report recipient with no account lands on.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUnsubscriber answers without a database.
type fakeUnsubscriber struct {
	site    string
	list    string
	known   bool
	removed []string
	err     error
}

// Describe names what the token would stop.
func (f *fakeUnsubscriber) Describe(_ context.Context, _ string) (string, string, bool) {
	return f.site, f.list, f.known
}

// Remove records the token it was asked about.
func (f *fakeUnsubscriber) Remove(_ context.Context, token string) error {
	f.removed = append(f.removed, token)

	return f.err
}

// TestTheUnsubscribePageAsksBeforeItActs is the reason a GET does not remove
// anybody: a link-scanning proxy in a corporate mail path follows every link in
// every message, and would otherwise unsubscribe people who never clicked.
func TestTheUnsubscribePageAsksBeforeItActs(t *testing.T) {
	app := newTestApp(t)
	h := app.Handler
	fake := &fakeUnsubscriber{site: "harbor.my", list: "weekly report", known: true}
	h.Unsubscribe = fake

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/unsubscribe/abc123", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d", recorder.Code)
	}

	body := recorder.Body.String()

	for _, want := range []string{"harbor.my", "weekly report", `method="post"`, "/unsubscribe/abc123"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}

	if len(fake.removed) != 0 {
		t.Errorf("the GET removed %q", fake.removed)
	}
}

// TestAnUnknownTokenSaysSoRatherThanFailing is what somebody clicking a link
// from a year ago sees.
func TestAnUnknownTokenSaysSoRatherThanFailing(t *testing.T) {
	app := newTestApp(t)
	h := app.Handler
	h.Unsubscribe = &fakeUnsubscriber{known: false}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/unsubscribe/gone", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d, want a page rather than an error", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "not subscribed") {
		t.Errorf("the page does not say the address is off the list:\n%s", recorder.Body.String())
	}
}

// TestThePostRemovesTheAddress covers the button on that page.
func TestThePostRemovesTheAddress(t *testing.T) {
	app := newTestApp(t)
	h := app.Handler
	fake := &fakeUnsubscriber{site: "harbor.my", list: "weekly report", known: true}
	h.Unsubscribe = fake

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/unsubscribe/abc123", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d", recorder.Code)
	}

	if len(fake.removed) != 1 || fake.removed[0] != "abc123" {
		t.Fatalf("the handler removed %q", fake.removed)
	}

	if !strings.Contains(recorder.Body.String(), "unsubscribed") {
		t.Errorf("the page does not confirm:\n%s", recorder.Body.String())
	}
}

// TestOneClickNeedsNoFormToken is RFC 8058: the POST comes from the reader's
// mail provider, which has no session with us and no form to have put a token
// in. A CSRF check here would make the header advertise a control that fails.
func TestOneClickNeedsNoFormToken(t *testing.T) {
	app := newTestApp(t)
	h := app.Handler
	fake := &fakeUnsubscriber{known: true}
	h.Unsubscribe = fake

	request := httptest.NewRequest(http.MethodPost, "/unsubscribe/abc123",
		strings.NewReader("List-Unsubscribe=One-Click"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d, want the provider to see a success", recorder.Code)
	}

	if len(fake.removed) != 1 {
		t.Fatalf("one-click removed %q", fake.removed)
	}

	// A provider wants a status, not a page.
	if recorder.Body.Len() != 0 {
		t.Errorf("one-click was answered with a page:\n%s", recorder.Body.String())
	}
}

// TestNoUnsubscriberStillRendersAPage keeps a self-hosted install with the
// feature unwired from returning a 500 to somebody who clicked a link.
func TestNoUnsubscriberStillRendersAPage(t *testing.T) {
	app := newTestApp(t)
	h := app.Handler
	h.Unsubscribe = nil

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(method, "/unsubscribe/abc123", nil))

		if recorder.Code != http.StatusOK {
			t.Errorf("%s: code = %d", method, recorder.Code)
		}
	}
}
