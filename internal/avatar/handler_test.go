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

// TestAProviderWithNoPictureLeavesAStoredOneAlone covers the clean "no
// picture" answer. A person who removed their Google photo keeps the last one
// stored rather than having it replaced by an empty row.
func TestAProviderWithNoPictureLeavesAStoredOneAlone(t *testing.T) {
	avatars, _, userID := newStore(t)
	ctx := context.Background()

	picture, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatal(err)
	}

	if err := avatars.Save(ctx, userID, SourceGoogle, picture); err != nil {
		t.Fatal(err)
	}

	provider := serving(t, http.StatusNotFound, "text/plain", []byte("gone"))

	refresher := &Refresher{Store: avatars, Client: provider.Client(), Run: func(work func()) { work() }}
	refresher.FromGoogle(ctx, userID, provider.URL)

	got, err := avatars.Read(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got.Bytes, picture.Bytes) {
		t.Error("a provider with no picture erased the one already stored")
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

// TestAPersonWithNoPictureGetsTheirInitials is the default for everybody who
// did not sign in with Google: a picture drawn here, from nothing but the name
// and address already on the account.
func TestAPersonWithNoPictureGetsTheirInitials(t *testing.T) {
	avatars, _, userID := newStore(t)

	handler := route(avatars, userID)
	handler.Person = func(*http.Request, int64) (string, string) { return "Ada Lovelace", "ada@example.com" }

	request := httptest.NewRequest(http.MethodGet, "/app/avatar/1", nil)
	request.SetPathValue("user", "1")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("a person with no stored picture was answered %d", response.Code)
	}

	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/svg+xml") {
		t.Errorf("content type = %q, want an SVG", got)
	}

	if !strings.Contains(response.Body.String(), ">AL</text>") {
		t.Errorf("the picture does not carry the initials: %s", response.Body.String())
	}

	// The same validator contract as a stored picture, so a page load costs a
	// 304 rather than the image again.
	again := httptest.NewRequest(http.MethodGet, "/app/avatar/1", nil)
	again.SetPathValue("user", "1")
	again.Header.Set("If-None-Match", response.Header().Get("ETag"))

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, again)

	if second.Code != http.StatusNotModified {
		t.Errorf("a browser holding the initials was answered %d, want 304", second.Code)
	}
}

// TestAStoredPictureOutranksTheInitials keeps a Google picture on the button.
func TestAStoredPictureOutranksTheInitials(t *testing.T) {
	avatars, _, userID := newStore(t)

	picture, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatal(err)
	}

	if err := avatars.Save(context.Background(), userID, SourceGoogle, picture); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/app/avatar/1", nil)
	request.SetPathValue("user", "1")

	response := httptest.NewRecorder()
	route(avatars, userID).ServeHTTP(response, request)

	if response.Header().Get("Content-Type") != "image/png" {
		t.Errorf("content type = %q, want the stored picture", response.Header().Get("Content-Type"))
	}
}
