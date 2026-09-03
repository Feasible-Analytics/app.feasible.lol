//
// handler.go
// Serving a stored account picture, and deciding when to go and get one.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package avatar

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// Pattern is the route the stored picture is served from. The path carries the
// user id and the ETag carries the version, so the URL is stable while the
// picture behind it is not.
const Pattern = "GET /app/avatar/{user}"

// FetchTimeout bounds one provider fetch. The work already runs away from the
// request that triggered it, so this is only about not holding a connection and
// a goroutine open for a provider that has stopped answering.
const FetchTimeout = 10 * time.Second

// firstPaintWait is how long a Google sign-in pauses for the picture before
// redirecting. Google's image host answers in tens of milliseconds, so this is
// usually the whole fetch; when it is not, the sign-in carries on and the
// picture appears on the next page load rather than the first.
const firstPaintWait = 1500 * time.Millisecond

// Refresher fetches and stores pictures. It is a struct rather than a function
// so the client and the Gravatar switch are configured once, at startup.
type Refresher struct {
	Store *Store

	// Client is the guarded outbound client. Nil disables fetching entirely,
	// which is what a deployment with no outbound access gets.
	Client *http.Client

	// Gravatar decides whether an address with no Google picture is looked up.
	// It is a switch because the lookup tells a third party the hash of an
	// address, and a self-hoster may reasonably not want that. No customer IP
	// is involved either way — the request is ours, not the viewer's.
	Gravatar bool

	Log *logger.Logger

	// Run is how the fetch leaves the request. It is a field so a test can make
	// it synchronous; left nil it spawns, because a sign-in must never wait on
	// somebody else's server.
	Run func(func())

	// gravatar builds the lookup URL. It is a seam so the tests can answer as
	// Gravatar without any of them reaching the real one.
	gravatar func(string) string
}

// dispatch sends the fetch away from the request.
func (r *Refresher) dispatch(work func()) {
	if r.Run != nil {
		r.Run(work)
		return
	}

	go work()
}

// FromGoogle stores the picture Google returned with the profile.
//
// It runs on every Google sign-in, which is the cheapest trigger that notices
// somebody changing their photo. It waits a moment before returning so the
// picture is usually already there on the page the sign-in redirects to; past
// that it carries on in the background and lands on the next load. The wait is
// bounded and its result ignored, because a picture is never worth failing or
// delaying a sign-in over.
func (r *Refresher) FromGoogle(ctx context.Context, userID int64, pictureURL string) {
	if r == nil || r.Store == nil || r.Client == nil || strings.TrimSpace(pictureURL) == "" {
		return
	}

	// The request's context is cancelled the moment the redirect is written,
	// which is before the fetch could finish.
	detached := context.WithoutCancel(ctx)
	done := make(chan struct{})

	r.dispatch(func() {
		defer close(done)
		r.store(detached, userID, SourceGoogle, pictureURL)
	})

	select {
	case <-done:
	case <-time.After(firstPaintWait):
	}
}

// EnsureGravatar looks up the Gravatar for an address, unless a provider has
// already answered for this person.
//
// That one guard covers both cases worth covering: somebody who already has a
// Google picture keeps it, and a mailbox with no Gravatar is not asked about on
// every sign-in for ever. It is deliberately not "did they use Google" —
// signing in with Google says nothing about whether Google has a photo.
func (r *Refresher) EnsureGravatar(ctx context.Context, userID int64, email string) {
	if r == nil || r.Store == nil || r.Client == nil || !r.Gravatar {
		return
	}

	state, err := r.Store.State(ctx, userID)
	if err != nil {
		r.warn("could not read an account picture", userID, SourceGravatar, err)
		return
	}

	if state.Asked {
		return
	}

	detached := context.WithoutCancel(ctx)
	build := r.gravatar
	if build == nil {
		build = GravatarURL
	}
	url := build(email)

	r.dispatch(func() { r.store(detached, userID, SourceGravatar, url) })
}

// State reports what is known about a person's picture without loading it, so
// a page can build the URL to it. A store that is not configured answers as
// though nobody has one, which leaves the letter circle.
func (r *Refresher) State(ctx context.Context, userID int64) State {
	if r == nil || r.Store == nil {
		return State{}
	}

	state, err := r.Store.State(ctx, userID)
	if err != nil {
		r.warn("could not read an account picture", userID, "", err)

		return State{}
	}

	return state
}

// store performs one fetch and writes what it learned.
//
// The three outcomes are deliberately different. A picture is stored. A clean
// "no picture" is remembered, so the provider is not asked again. Anything else
// — a timeout, a rate limit, a provider having a bad day — is logged and
// written nowhere, so the next sign-in tries again rather than recording a
// temporary failure as a permanent fact about a person.
func (r *Refresher) store(ctx context.Context, userID int64, source, url string) {
	fetch, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	picture, err := Fetch(fetch, r.Client, url)

	switch {
	case errors.Is(err, ErrNoImage):
		// The write runs on the outer context: the fetch deadline has very
		// likely just expired, and the one failure the miss cache exists for is
		// the one where it would then never be recorded.
		if err := r.Store.RememberMiss(ctx, userID, source); err != nil {
			r.warn("could not record a missing account picture", userID, source, err)
		}

	case err != nil:
		r.warn("could not fetch an account picture", userID, source, err)

	default:
		if err := r.Store.Save(ctx, userID, source, picture); err != nil {
			r.warn("could not store an account picture", userID, source, err)
		}
	}
}

// warn records what went wrong. A picture nobody could fetch is not worth an
// error to the person signing in, but a provider that has started refusing us
// is worth knowing about before somebody reports it.
func (r *Refresher) warn(message string, userID int64, source string, err error) {
	if r.Log == nil {
		return
	}

	r.Log.Warn(message, "source", source, "user", userID, "error", err)
}

// Handler serves a stored picture.
type Handler struct {
	Store *Store

	// Authorise reports whether this request may read this person's picture.
	// It is supplied rather than assumed because the session lives in another
	// package and a picture is still account data.
	Authorise func(*http.Request, int64) bool
}

// URL is where a person's picture is served from, or empty when they have none.
// The ETag is not in the path: a stable URL is what lets a browser reuse what
// it already has, and the validator is what tells it whether it still can.
func URL(userID int64, etag string) string {
	if etag == "" {
		return ""
	}

	return "/app/avatar/" + strconv.FormatInt(userID, 10)
}

// ServeHTTP answers one picture request.
//
// The URL is stable and the bytes behind it are not, so the browser revalidates
// every time and is answered with a 304 when it already holds the picture. A
// max-age would be cheaper and would leave a changed one stale for as long as
// it lasted. It is private because the picture belongs to one account.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil || userID < 1 {
		http.NotFound(w, r)
		return
	}

	if h.Authorise == nil || !h.Authorise(r, userID) {
		http.NotFound(w, r)
		return
	}

	picture, err := h.Store.Read(r.Context(), userID)
	if err != nil || len(picture.Bytes) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("ETag", picture.ETag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Type", picture.Type)

	if matches(r.Header.Get("If-None-Match"), picture.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(picture.Bytes)))
	_, _ = w.Write(picture.Bytes)
}

// matches reports whether a browser already holds this version. If-None-Match
// is a list, and a proxy may have weakened the tag on the way through, so both
// are compared with the weakness prefix removed.
func matches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}

	return false
}
