//
// handler_test.go
// Fetching a picture, and who may read one back.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package avatar

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serving starts a provider that answers every request with one response.
func serving(t *testing.T, status int, contentType string, body []byte) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	return server
}

// TestAFetchedPictureIsStoredAgainstThePerson is the Google sign-in path.
func TestAFetchedPictureIsStoredAgainstThePerson(t *testing.T) {
	avatars, _, userID := newStore(t)
	provider := serving(t, http.StatusOK, "image/png", square(t, 128, "png"))

	refresher := &Refresher{
		Store:  avatars,
		Client: provider.Client(),
		Run:    func(work func()) { work() },
	}

	refresher.FromGoogle(context.Background(), userID, provider.URL+"/photo.jpg")

	got, err := avatars.Read(context.Background(), userID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Bytes) == 0 {
		t.Fatal("the Google picture was not stored")
	}
	if got.Type != "image/png" {
		t.Errorf("stored type = %q, want image/png", got.Type)
	}
}

// TestAMissingGravatarIsRememberedRatherThanRetried is why d=404 is asked for.
// Gravatar answers 404 for an address nobody registered, and that answer has to
// be kept or the lookup repeats on every sign-in for ever.
func TestAMissingGravatarIsRememberedRatherThanRetried(t *testing.T) {
	avatars, db, userID := newStore(t)
	provider := serving(t, http.StatusNotFound, "text/plain", []byte("not found"))

	refresher := &Refresher{
		Store:    avatars,
		Client:   provider.Client(),
		Gravatar: true,
		Run:      func(work func()) { work() },
	}

	refresher.store(context.Background(), userID, SourceGravatar, provider.URL)

	var fetchedAt, hasBytes int64
	if err := db.QueryRow(`
		SELECT COALESCE(avatar_fetched_at, 0), LENGTH(COALESCE(avatar_bytes, x'')) FROM users WHERE id = ?
	`, userID).Scan(&fetchedAt, &hasBytes); err != nil {
		t.Fatalf("read row: %v", err)
	}

	if fetchedAt == 0 {
		t.Error("a 404 from the provider left no record that it had been asked")
	}
	if hasBytes != 0 {
		t.Error("a 404 from the provider stored bytes")
	}
}

// TestGravatarIsSkippedWhenSwitchedOffOrAlreadyAsked covers the two guards a
// self-hoster and a repeat sign-in each depend on. Neither may produce an
// outbound request.
func TestGravatarIsSkippedWhenSwitchedOffOrAlreadyAsked(t *testing.T) {
	avatars, _, userID := newStore(t)

	asked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	off := &Refresher{Store: avatars, Client: server.Client(), Gravatar: false, Run: func(work func()) { work() }}
	off.EnsureGravatar(context.Background(), userID, "a@example.com", false)

	on := &Refresher{Store: avatars, Client: server.Client(), Gravatar: true, Run: func(work func()) { work() }}
	on.EnsureGravatar(context.Background(), userID, "a@example.com", true)

	if asked {
		t.Error("a switched-off or already-asked lookup still reached a provider")
	}
}

// TestAnOversizedDownloadIsRefusedBeforeItIsDecoded is the memory bound. The
// response is a stranger's, and without the cap the provider decides how much
// memory this process spends.
func TestAnOversizedDownloadIsRefusedBeforeItIsDecoded(t *testing.T) {
	provider := serving(t, http.StatusOK, "image/png", bytes.Repeat([]byte("x"), MaxDownloadBytes+64))

	_, err := Fetch(context.Background(), provider.Client(), provider.URL)
	if err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want the size refusal", err)
	}
}

// TestAFailedFetchLeavesTheAccountUsable is the sign-in guarantee: a provider
// that is down or slow costs the person their picture, never their session.
func TestAFailedFetchLeavesTheAccountUsable(t *testing.T) {
	avatars, _, userID := newStore(t)
	provider := serving(t, http.StatusInternalServerError, "text/plain", []byte("down"))

	refresher := &Refresher{Store: avatars, Client: provider.Client(), Run: func(work func()) { work() }}
	refresher.FromGoogle(context.Background(), userID, provider.URL)

	got, err := avatars.Read(context.Background(), userID)
	if err != nil {
		t.Fatalf("read after a failed fetch: %v", err)
	}
	if len(got.Bytes) != 0 {
		t.Error("a failed fetch stored something")
	}
}

// route builds the handler with a fixed answer to "is this you".
func route(avatars *Store, viewer int64) *Handler {
	return &Handler{
		Store:     avatars,
		Authorise: func(_ *http.Request, userID int64) bool { return userID == viewer },
	}
}

// TestOnlyTheOwnerMayReadAPicture is the reason the route checks the id rather
// than only the session. A numeric URL anybody signed in could walk is a way to
// learn who has an account here.
func TestOnlyTheOwnerMayReadAPicture(t *testing.T) {
	avatars, _, userID := newStore(t)

	picture, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if err := avatars.Save(context.Background(), userID, SourceGoogle, picture); err != nil {
		t.Fatalf("save: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/app/avatar/1", nil)
	request.SetPathValue("user", "1")

	mine := httptest.NewRecorder()
	route(avatars, userID).ServeHTTP(mine, request)
	if mine.Code != http.StatusOK {
		t.Fatalf("the owner was answered %d", mine.Code)
	}
	if mine.Header().Get("Content-Type") != "image/png" {
		t.Errorf("content type = %q", mine.Header().Get("Content-Type"))
	}
	if !strings.Contains(mine.Header().Get("Cache-Control"), "private") {
		t.Errorf("cache control = %q, want it private to one account", mine.Header().Get("Cache-Control"))
	}

	theirs := httptest.NewRecorder()
	route(avatars, userID+1).ServeHTTP(theirs, request)
	if theirs.Code != http.StatusNotFound {
		t.Errorf("somebody else was answered %d, want 404", theirs.Code)
	}
}

// TestABrowserHoldingThePictureIsToldSoRatherThanSentItAgain checks the
// validator does its job, which is what makes the long cache header safe.
func TestABrowserHoldingThePictureIsToldSoRatherThanSentItAgain(t *testing.T) {
	avatars, _, userID := newStore(t)

	picture, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if err := avatars.Save(context.Background(), userID, SourceGoogle, picture); err != nil {
		t.Fatalf("save: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/app/avatar/1", nil)
	request.SetPathValue("user", "1")
	request.Header.Set("If-None-Match", `"something-else", `+picture.ETag)

	recorder := httptest.NewRecorder()
	route(avatars, userID).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotModified {
		t.Fatalf("a browser holding the picture was answered %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes of body", recorder.Body.Len())
	}
}

// TestAPersonWithNoPictureIsANotFound is what leaves the letter circle in
// place: the front end asks for the image, gets a 404, and falls back.
func TestAPersonWithNoPictureIsANotFound(t *testing.T) {
	avatars, _, userID := newStore(t)

	request := httptest.NewRequest(http.MethodGet, "/app/avatar/1", nil)
	request.SetPathValue("user", "1")

	recorder := httptest.NewRecorder()
	route(avatars, userID).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Errorf("a person with no picture was answered %d, want 404", recorder.Code)
	}
}

// TestTheUrlIsEmptyWithoutAStoredPicture is what keeps avatar_url off the wire
// for the majority of accounts, so the front end never asks for a 404.
func TestTheUrlIsEmptyWithoutAStoredPicture(t *testing.T) {
	if got := URL(7, ""); got != "" {
		t.Errorf("URL without a picture = %q, want empty", got)
	}
	if got := URL(7, `"abc"`); got != "/app/avatar/7" {
		t.Errorf("URL = %q, want /app/avatar/7", got)
	}
}
