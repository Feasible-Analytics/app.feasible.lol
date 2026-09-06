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
		SELECT fetched_at, LENGTH(COALESCE(bytes, x'')) FROM user_avatars WHERE user_id = ?
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
	ctx := context.Background()

	asked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	seam := func(string) string { return server.URL }
	off := &Refresher{Store: avatars, Client: server.Client(), Gravatar: false, Run: func(work func()) { work() }, gravatar: seam}
	off.EnsureGravatar(ctx, userID, "a@example.com")

	if asked {
		t.Fatal("a switched-off lookup still reached a provider")
	}

	// A remembered answer — a picture or a miss — is what the second guard
	// keys off.
	if err := avatars.RememberMiss(ctx, userID, SourceGravatar); err != nil {
		t.Fatalf("remember miss: %v", err)
	}

	on := &Refresher{Store: avatars, Client: server.Client(), Gravatar: true, Run: func(work func()) { work() }, gravatar: seam}
	on.EnsureGravatar(ctx, userID, "a@example.com")

	if asked {
		t.Error("an already-answered lookup asked the provider again")
	}
}

// TestAGoogleAccountWithNoPhotoStillGetsAGravatar is why the guard is "has a
// provider answered" rather than "did they use Google". Signing in with Google
// says nothing about whether Google has a photo.
func TestAGoogleAccountWithNoPhotoStillGetsAGravatar(t *testing.T) {
	avatars, _, userID := newStore(t)
	provider := serving(t, http.StatusOK, "image/png", square(t, 96, "png"))

	refresher := &Refresher{
		Store:    avatars,
		Client:   provider.Client(),
		Gravatar: true,
		Run:      func(work func()) { work() },
		gravatar: func(string) string { return provider.URL },
	}

	// Google supplied no picture URL, so nothing was stored and nothing asked.
	refresher.FromGoogle(context.Background(), userID, "")
	refresher.EnsureGravatar(context.Background(), userID, "a@example.com")

	got, err := avatars.Read(context.Background(), userID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Bytes) == 0 {
		t.Error("a Google account with no photo was left without a Gravatar")
	}
}

// TestAProviderHavingABadDayIsRetriedRatherThanRecorded separates a clear "no
// picture" from a failure. Writing a 500 or a rate limit down as "this person
// has no avatar, never ask again" is the silent failure the house rules forbid.
func TestAProviderHavingABadDayIsRetriedRatherThanRecorded(t *testing.T) {
	avatars, _, userID := newStore(t)
	provider := serving(t, http.StatusTooManyRequests, "text/plain", []byte("slow down"))

	refresher := &Refresher{Store: avatars, Client: provider.Client(), Run: func(work func()) { work() }}
	refresher.store(context.Background(), userID, SourceGravatar, provider.URL)

	state, err := avatars.State(context.Background(), userID)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.Asked {
		t.Error("a rate limit was recorded as a permanent fact about this person")
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

// TestAnExpiredMissIsAskedAboutAgain is the regression that matters: a Gravatar
// created after the first sign-in has to be found.
func TestAnExpiredMissIsAskedAboutAgain(t *testing.T) {
	ctx := context.Background()
	avatars, db, userID := newStore(t)

	asks := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asks++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(square(t, 64, "png"))
	}))
	t.Cleanup(provider.Close)

	refresher := &Refresher{
		Store:    avatars,
		Client:   provider.Client(),
		Gravatar: true,
		Run:      func(work func()) { work() },
		gravatar: func(string) string { return provider.URL + "/avatar" },
	}

	// A miss recorded a fortnight ago.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_avatars (user_id, type, etag, source, fetched_at)
		VALUES (?, '', '', ?, ?)`, userID, SourceGravatar, avatars.now()-2*int64(MissRetry.Seconds())); err != nil {
		t.Fatal(err)
	}

	refresher.EnsureGravatar(ctx, userID, "a@example.com")

	if asks != 1 {
		t.Fatalf("the provider was asked %d times, want 1 — a stale miss is never retried", asks)
	}

	got, err := avatars.Read(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Bytes) == 0 || got.ETag == "" {
		t.Fatal("the picture that arrived after the miss did not replace it")
	}

	// And now that there is one, it is left alone.
	refresher.EnsureGravatar(ctx, userID, "a@example.com")

	if asks != 1 {
		t.Errorf("the provider was asked %d times, want the stored picture to be kept", asks)
	}
}

// TestAFreshMissIsNotRetried is the other half: the miss cache still does its
// job, or every sign-in costs an outbound request again.
func TestAFreshMissIsNotRetried(t *testing.T) {
	ctx := context.Background()
	avatars, db, userID := newStore(t)

	asks := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asks++
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(provider.Close)

	refresher := &Refresher{
		Store:    avatars,
		Client:   provider.Client(),
		Gravatar: true,
		Run:      func(work func()) { work() },
		gravatar: func(string) string { return provider.URL + "/avatar" },
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_avatars (user_id, type, etag, source, fetched_at)
		VALUES (?, '', '', ?, ?)`, userID, SourceGravatar, avatars.now()-60); err != nil {
		t.Fatal(err)
	}

	refresher.EnsureGravatar(ctx, userID, "a@example.com")

	if asks != 0 {
		t.Errorf("a miss recorded a minute ago cost %d outbound requests", asks)
	}
}

// TestABackfillAsksOnceAboutEverybodyWhoWasNeverAsked covers the accounts that
// predate the feature.
func TestABackfillAsksOnceAboutEverybodyWhoWasNeverAsked(t *testing.T) {
	ctx := context.Background()
	avatars, db, first := newStore(t)

	result, err := db.ExecContext(ctx,
		`INSERT INTO users (email, name, created_at, updated_at) VALUES ('b@example.com', 'B', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := result.LastInsertId()

	asked := []string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(square(t, 64, "png"))
	}))
	t.Cleanup(provider.Close)

	refresher := &Refresher{
		Store:    avatars,
		Client:   provider.Client(),
		Gravatar: true,
		Run:      func(work func()) { work() },
		gravatar: func(email string) string { return provider.URL + "/" + email },
	}

	// The first was asked about a minute ago; only the second is outstanding.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_avatars (user_id, type, etag, source, fetched_at)
		VALUES (?, '', '', ?, ?)`, first, SourceGravatar, avatars.now()-60); err != nil {
		t.Fatal(err)
	}

	people := func(context.Context) ([]Person, error) {
		return []Person{{ID: first, Email: "a@example.com"}, {ID: second, Email: "b@example.com"}}, nil
	}

	count, failed, err := refresher.Backfill(ctx, people)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if count != 1 || failed != 0 || len(asked) != 1 || asked[0] != "/b@example.com" {
		t.Fatalf("the backfill asked about %v, want only the account with no answer on file", asked)
	}

	// Running it again asks about nobody, so it is safe to run twice.
	if again, _, err := refresher.Backfill(ctx, people); err != nil || again != 0 {
		t.Errorf("a second backfill asked about %d accounts (%v), want none", again, err)
	}
}

// TestABackfillCountsWhatItCouldNotReach keeps a run where every fetch failed
// from printing a count and exiting clean.
func TestABackfillCountsWhatItCouldNotReach(t *testing.T) {
	ctx := context.Background()
	avatars, _, userID := newStore(t)

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)

	refresher := &Refresher{
		Store:    avatars,
		Client:   provider.Client(),
		Gravatar: true,
		gravatar: func(string) string { return provider.URL + "/avatar" },
	}

	asked, failed, err := refresher.Backfill(ctx, func(context.Context) ([]Person, error) {
		return []Person{{ID: userID, Email: "a@example.com"}}, nil
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}

	if asked != 0 || failed != 1 {
		t.Errorf("a provider that refused reported asked=%d failed=%d, want 0 and 1", asked, failed)
	}

	// And nothing was written, so the next run tries again rather than
	// recording a temporary failure as a fact about a person.
	state, err := avatars.State(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}

	if state.Asked {
		t.Error("a failed fetch was recorded as an answer")
	}
}

// TestABackfillRefusesWhenItCannotFetch keeps a command that did nothing from
// reporting that it did.
func TestABackfillRefusesWhenItCannotFetch(t *testing.T) {
	avatars, _, _ := newStore(t)

	off := &Refresher{Store: avatars, Client: http.DefaultClient, Gravatar: false}

	if _, _, err := off.Backfill(context.Background(), func(context.Context) ([]Person, error) {
		return nil, nil
	}); err == nil || !strings.Contains(err.Error(), "Gravatar") {
		t.Errorf("a backfill with Gravatar switched off answered %v, want it named", err)
	}
}
