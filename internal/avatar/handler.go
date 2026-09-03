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
)

// Pattern is the route the stored picture is served from. The path carries the
// user id and the ETag carries the version, so the URL is stable while the
// picture behind it is not.
const Pattern = "GET /app/avatar/{user}"

// fetchTimeout bounds one provider fetch. The work already runs away from the
// request that triggered it, so this is only about not holding a connection and
// a goroutine open for a provider that has stopped answering.
const fetchTimeout = 10 * time.Second

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

	// Log records a failed fetch. A picture nobody could fetch is not worth an
	// error to the person signing in, but it is worth us knowing about.
	Log interface {
		Warn(msg string, args ...any)
	}

	// Run is how the fetch leaves the request. It is a field so a test can make
	// it synchronous; left nil it spawns, because a sign-in must never wait on
	// somebody else's server.
	Run func(func())
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
// It is called on sign-in and on reconnect, which is the cheapest trigger that
// keeps the picture current: somebody who changes their Google photo sees it
// here the next time they sign in. It never returns an error to its caller,
// because a picture is not worth failing a sign-in over.
func (r *Refresher) FromGoogle(ctx context.Context, userID int64, pictureURL string) {
	if r == nil || r.Store == nil || r.Client == nil || strings.TrimSpace(pictureURL) == "" {
		return
	}

	// The request's context is cancelled the moment the redirect is written,
	// which is before the fetch could finish.
	detached := context.WithoutCancel(ctx)

	r.dispatch(func() { r.store(detached, userID, SourceGoogle, pictureURL) })
}

// EnsureGravatar fetches the Gravatar for an address, unless this person
// already has a picture or has already been looked up.
//
// The already-looked-up check is what stops a mailbox with no Gravatar
// producing an outbound request on every sign-in. A person who later creates
// one is picked up when the stored answer is cleared, which is what linking a
// Google account does.
func (r *Refresher) EnsureGravatar(ctx context.Context, userID int64, email string, fetched bool) {
	if r == nil || r.Store == nil || r.Client == nil || !r.Gravatar || fetched {
		return
	}

	detached := context.WithoutCancel(ctx)
	url := GravatarURL(email)

	r.dispatch(func() { r.store(detached, userID, SourceGravatar, url) })
}

// store performs one fetch and writes whatever it learned, including nothing.
func (r *Refresher) store(ctx context.Context, userID int64, source, url string) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	picture, err := Fetch(ctx, r.Client, url)
	if err != nil {
		// A miss and a failure are both remembered, because retrying a broken
		// picture on every sign-in is a request that never starts succeeding.
		// A failure is logged; a miss is an ordinary answer.
		if !errors.Is(err, ErrNoImage) && r.Log != nil {
			r.Log.Warn("could not fetch an account picture", "source", source, "user", userID, "error", err)
		}

		if err := r.Store.RememberMiss(ctx, userID); err != nil && r.Log != nil {
			r.Log.Warn("could not record a missing account picture", "user", userID, "error", err)
		}

		return
	}

	if err := r.Store.Save(ctx, userID, source, picture); err != nil && r.Log != nil {
		r.Log.Warn("could not store an account picture", "source", source, "user", userID, "error", err)
	}
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
// The cache header is long because the bytes behind a URL only change when
// somebody changes their picture, and the ETag catches that: a browser
// revalidates and is told either 304 or the new image. It is private because
// the picture belongs to one account and a shared cache must not hold it.
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
	w.Header().Set("Cache-Control", "private, max-age=86400, must-revalidate")
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
