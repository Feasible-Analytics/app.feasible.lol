//
// auth.go
// Bearer authentication and the hourly rate limit, in front of every route.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// contextKey is this package's own key type, so a value stored here can never
// collide with one another package stored under the same name.
type contextKey struct{ name string }

// keyContextKey carries the authenticated key down to the handlers.
var keyContextKey = &contextKey{"apikey"}

// Rate limit headers. They are the widely-understood spelling rather than
// anything of our own: a client library that already knows how to back off
// should not have to learn our names to do it.
const (
	headerLimit     = "X-RateLimit-Limit"
	headerRemaining = "X-RateLimit-Remaining"
	headerReset     = "X-RateLimit-Reset"
)

// KeyFrom reads the authenticated key out of a request context. It is exported
// because the MCP server mounts handlers that were authenticated by this
// package's middleware and needs the same key.
func KeyFrom(ctx context.Context) (*apikeys.Key, bool) {
	key, ok := ctx.Value(keyContextKey).(*apikeys.Key)

	return key, ok
}

// WithKey puts an authenticated key into a context. It exists so the MCP
// server, whose bearer token is an OAuth access token rather than an API key,
// can present the same authenticated identity to the shared handlers.
func WithKey(ctx context.Context, key *apikeys.Key) context.Context {
	return context.WithValue(ctx, keyContextKey, key)
}

// authenticated wraps the mux with the credential check and the rate limit.
//
// Both run before routing rather than inside each handler, so a route added
// later is protected by construction. The order matters: authentication first,
// because a rate limit keyed on an unauthenticated caller would let anybody
// exhaust somebody else's budget by guessing their key id.
func (a *API) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, err := bearerToken(r)
		if err != nil {
			a.unauthorised(w, err.Error())
			return
		}

		key, err := a.Keys.Authenticate(r.Context(), presented)
		if err != nil {
			switch {
			case errors.Is(err, apikeys.ErrMalformed):
				a.unauthorised(w, "the Authorization header does not carry a "+apikeys.Prefix+" API key")
			case errors.Is(err, apikeys.ErrNotFound):
				a.unauthorised(w, "this API key is not valid — it may have been revoked")
			default:
				a.internal(w, "authenticate", err)
			}

			return
		}

		decision := a.Limiter.Allow(key)

		w.Header().Set(headerLimit, strconv.Itoa(decision.Limit))
		w.Header().Set(headerRemaining, strconv.Itoa(decision.Remaining))
		w.Header().Set(headerReset, strconv.FormatInt(decision.ResetsAt.Unix(), 10))

		if !decision.Allowed {
			// Retry-After is not optional here. A client told only "no" retries
			// immediately, which turns a rate limit into a tight loop against
			// the thing it was supposed to protect.
			w.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryAfter().Seconds())))
			a.fail(w, http.StatusTooManyRequests, fmt.Sprintf(
				"rate limit of %d requests per hour reached — it resets at %s",
				decision.Limit, decision.ResetsAt.Format("15:04:05 MST")))

			return
		}

		next.ServeHTTP(w, r.WithContext(WithKey(r.Context(), key)))
	})
}

// unauthorised answers a missing or bad credential, naming the scheme so a
// caller who sent the wrong header shape can see what was expected.
func (a *API) unauthorised(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="feasible"`)
	a.fail(w, http.StatusUnauthorized, message)
}

// bearerToken pulls the credential out of the request.
//
// Only the Authorization header is read. A key in the query string ends up in
// access logs, browser history and referrer headers, and supporting it "just for
// convenience" is how a customer's key ends up in somebody else's log file.
func bearerToken(r *http.Request) (string, error) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return "", errors.New("this endpoint needs an API key: send Authorization: Bearer <key>")
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("the Authorization header must be of the form: Bearer <key>")
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("the Authorization header carries no key")
	}

	return token, nil
}

// requireScope refuses a key that is not allowed to do something. A scoped key
// is opt-in — an unscoped key does everything — so this only ever refuses
// somebody who deliberately narrowed their own key.
func (a *API) requireScope(w http.ResponseWriter, key *apikeys.Key, scope string) bool {
	if key.Allows(scope) {
		return true
	}

	a.fail(w, http.StatusForbidden, "this API key does not carry the "+scope+" scope")

	return false
}
