//
// api.go
// The public API surface: what it is made of and how it answers.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package publicapi is the API customers and integrations call. It is three
// things behind one authentication layer: the v2 query endpoint, the v1
// compatibility shims that let an existing integration migrate by changing one
// hostname, and the sites provisioning endpoints.
//
// Two decisions run through all of it.
//
// The first is that every one of these is included in every plan and every
// build. The incumbent gates their stats API behind their most expensive tier
// and their sites API behind an enterprise contract, and their self-hosted build
// inherited the same check — so people running it on their own hardware got a
// subscription prompt on their own instance. There is no plan check in this
// package, and adding one would be a bug.
//
// The second is that a caller's mistake is always a 400 with a sentence naming
// what was wrong, never a 500. A 500 is a page somebody has to read our logs to
// explain, and the incumbent's breakdown endpoint returns one for `page=foo`
// because the value goes straight into an integer parse. Every parameter in this
// package is parsed by a function that returns a message.
package publicapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/statsapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/webhooks"
)

// MaxBodyBytes bounds a request body. It matches the internal endpoint's limit
// so that a request which works against one works against the other.
const MaxBodyBytes = statsapi.MaxBodyBytes

// API holds everything the public endpoints need. It is one struct rather than
// a package of free functions because the handlers share a clock, a logger and
// four stores, and threading those through every signature would make each
// handler's own logic the smallest part of it.
type API struct {
	// Keys authenticates the bearer token on every request.
	Keys *apikeys.Store

	// Limiter counts requests per key.
	Limiter *apikeys.Limiter

	// Access is the account lock. Every route below reads or writes one
	// account's data and none of them is how somebody pays us, so the check
	// sits in front of the whole mux rather than on the endpoints that looked
	// most like reports — a key that can still answer "how many visitors did I
	// have" is a lock that does not lock. It is nil on an install with no
	// billing, which locks nothing.
	Access *access.Gate

	// Sites is the in-memory routing snapshot. Reading it rather than
	// control.db keeps an authenticated query off the shared write lock.
	Sites *sites.Cache

	// Control is the control database, for the provisioning endpoints that
	// genuinely have to write to it.
	Control *ControlStore

	// Accounts hands out per-account analytics handles.
	Accounts *accounts.Manager

	// Webhooks and Dispatcher back the webhook management endpoints.
	Webhooks   *webhooks.Store
	Dispatcher *webhooks.Dispatcher

	// Goals, Funnels, Shields and Annotations are the features another part of
	// the product owns. They are interfaces and may be nil: where one is, the
	// endpoint still exists and answers with a clear "not available yet"
	// rather than a 404, because a route that vanishes looks like a bug in the
	// caller's URL and a route that explains itself does not.
	Goals       GoalStore
	Funnels     FunnelStore
	Shields     ShieldStore
	Annotations AnnotationStore

	// BaseURL is the URL people actually type. Tracker snippets and shared-link
	// URLs are built from it, so it has to be the public address rather than
	// the bind address — a snippet pointing at 127.0.0.1 works on the machine
	// that generated it and nowhere else.
	BaseURL string

	Log *logger.Logger

	// SampleThreshold is how many event rows a query may be estimated to read
	// before it is answered from a sample. It is carried here so the public
	// endpoint and the dashboard sample at the same size — an API that
	// estimated where the dashboard was exact would be two answers to one
	// question.
	SampleThreshold int64

	// Now is the clock every date range resolves against, injectable so a test
	// can ask what "today" returns without waiting for tomorrow.
	Now func() time.Time

	// statsOnce and stats hold the internal query handler, built on first use
	// so that constructing an API costs nothing and a process that never serves
	// a query never builds one.
	statsOnce sync.Once
	stats     *statsapi.Handler
}

// now reads the API's clock.
func (a *API) now() time.Time {
	if a.Now == nil {
		return time.Now().UTC()
	}

	return a.Now()
}

// Routes returns the whole public surface, already authenticated.
//
// Every route is registered with its method in the pattern, so a GET against a
// POST-only endpoint is a 405 from the mux rather than a handler that has to
// remember to check. The authentication wrapper goes around the mux rather than
// around each handler for the same reason: a route added later is authenticated
// by construction rather than by whoever adds it remembering to.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	// The Stats API. v2 is the endpoint everything new should use; the v1
	// routes are the compatibility surface an existing integration already
	// points at.
	mux.HandleFunc("POST /api/v2/query", a.handleQueryV2)
	mux.HandleFunc("GET /api/v1/stats/aggregate", a.handleAggregate)
	mux.HandleFunc("GET /api/v1/stats/timeseries", a.handleTimeseries)
	mux.HandleFunc("GET /api/v1/stats/breakdown", a.handleBreakdown)
	mux.HandleFunc("GET /api/v1/stats/realtime/visitors", a.handleRealtimeVisitors)

	// The Sites API.
	mux.HandleFunc("GET /api/v1/sites", a.handleListSites)
	mux.HandleFunc("POST /api/v1/sites", a.handleCreateSite)
	mux.HandleFunc("GET /api/v1/sites/goals", a.handleListGoals)
	mux.HandleFunc("PUT /api/v1/sites/goals", a.handleCreateGoal)
	mux.HandleFunc("DELETE /api/v1/sites/goals/{goal_id}", a.handleDeleteGoal)
	mux.HandleFunc("GET /api/v1/sites/shared-links", a.handleListSharedLinks)
	mux.HandleFunc("PUT /api/v1/sites/shared-links", a.handleCreateSharedLink)
	mux.HandleFunc("DELETE /api/v1/sites/shared-links/{link_id}", a.handleDeleteSharedLink)
	mux.HandleFunc("GET /api/v1/sites/guests", a.handleListGuests)
	mux.HandleFunc("PUT /api/v1/sites/guests", a.handleCreateGuest)
	mux.HandleFunc("DELETE /api/v1/sites/guests/{guest_id}", a.handleDeleteGuest)
	mux.HandleFunc("GET /api/v1/sites/custom-props", a.handleListCustomProps)
	mux.HandleFunc("PUT /api/v1/sites/custom-props", a.handleCreateCustomProp)
	mux.HandleFunc("DELETE /api/v1/sites/custom-props/{prop_id}", a.handleDeleteCustomProp)
	mux.HandleFunc("GET /api/v1/teams/memberships", a.handleListMemberships)
	mux.HandleFunc("PUT /api/v1/teams/memberships", a.handleCreateMembership)
	mux.HandleFunc("DELETE /api/v1/teams/memberships/{membership_id}", a.handleDeleteMembership)

	// The per-site routes come last so that the literal paths above win: a
	// wildcard segment would otherwise swallow /api/v1/sites/goals.
	mux.HandleFunc("GET /api/v1/sites/{site_id}", a.handleGetSite)
	mux.HandleFunc("PUT /api/v1/sites/{site_id}", a.handleUpdateSite)
	mux.HandleFunc("DELETE /api/v1/sites/{site_id}", a.handleDeleteSite)
	mux.HandleFunc("GET /api/v1/sites/{site_id}/tracker", a.handleGetTracker)
	mux.HandleFunc("PUT /api/v1/sites/{site_id}/tracker", a.handleUpdateTracker)

	// Webhooks.
	mux.HandleFunc("GET /api/v1/webhooks", a.handleListWebhooks)
	mux.HandleFunc("POST /api/v1/webhooks", a.handleCreateWebhook)
	mux.HandleFunc("GET /api/v1/webhooks/event-types", a.handleWebhookEventTypes)
	mux.HandleFunc("GET /api/v1/webhooks/{webhook_id}", a.handleGetWebhook)
	mux.HandleFunc("PUT /api/v1/webhooks/{webhook_id}", a.handleUpdateWebhook)
	mux.HandleFunc("DELETE /api/v1/webhooks/{webhook_id}", a.handleDeleteWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/{webhook_id}/rotate-secret", a.handleRotateWebhookSecret)
	mux.HandleFunc("GET /api/v1/webhooks/{webhook_id}/deliveries", a.handleListDeliveries)
	mux.HandleFunc("POST /api/v1/webhooks/deliveries/{delivery_id}/redeliver", a.handleRedeliver)

	return a.authenticated(mux)
}

// errorBody is what a failure looks like. One field, always the same shape, so
// a client can read the reason without branching on the status code — and the
// same shape the internal endpoint uses, so a v2 caller sees one error format
// whichever handler answered.
type errorBody struct {
	Error string `json:"error"`
}

// fail answers a caller's mistake with the reason. Every refusal in this
// package goes through it so that no handler can invent its own error shape.
func (a *API) fail(w http.ResponseWriter, status int, message string) {
	a.write(w, status, errorBody{Error: message})
}

// internal answers our mistake. The detail goes to our log, because the caller
// can do nothing with a SQLite error and we can do nothing with a bug report
// that does not name one.
func (a *API) internal(w http.ResponseWriter, what string, err error) {
	if a.Log != nil {
		a.Log.Error("public api failed", "step", what, "error", err)
	}

	a.write(w, http.StatusInternalServerError, errorBody{Error: "the request could not be completed"})
}

// write encodes a response body.
func (a *API) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil && a.Log != nil {
		a.Log.Error("public api response could not be written", "error", err)
	}
}

// resolveSite turns a site identifier from a caller into the site it names,
// answering with the right refusal when it does not.
//
// A site the key's team does not own is reported as not found rather than as
// forbidden. Distinguishing the two turns the API into an oracle for which
// domains are registered with us, which is a fact about somebody else's
// customer list.
func (a *API) resolveSite(w http.ResponseWriter, key *apikeys.Key, identifier string) (sites.Site, bool) {
	if identifier == "" {
		a.fail(w, http.StatusBadRequest, "site_id is required — pass the site's domain")
		return sites.Site{}, false
	}

	site, ok := a.Sites.Lookup(identifier)
	if !ok || site.AccountID != key.TeamID {
		a.fail(w, http.StatusNotFound, "no site named "+identifier+" is available to this key")
		return sites.Site{}, false
	}

	return site, true
}

// Query answers one query against a site's account database.
//
// It is the one path every stats answer goes through — v2, all four v1 shims
// and every MCP tool — because two ways to run a query is two answers to "how
// many visitors did I have", and no way to tell which one is wrong. It is
// exported because the MCP server is a second front end onto exactly this.
func (a *API) Query(ctx context.Context, site sites.Site, q query.Query) (*query.Result, error) {
	account, err := a.Accounts.Open(ctx, site.AccountID)
	if err != nil {
		return nil, err
	}

	engine := query.New(account.Reader())
	engine.Now = a.now

	return engine.Run(ctx, q)
}

// Clock reports the time this API resolves date ranges against. The MCP tools
// resolve some ranges themselves — a comparison window has to be known before
// the second query is built — and they must use the same clock, or the two
// halves of a comparison land on different days.
func (a *API) Clock() time.Time {
	return a.now()
}

// runQuery is the request-scoped spelling the HTTP handlers use.
func (a *API) runQuery(r *http.Request, site sites.Site, q query.Query) (*query.Result, error) {
	return a.Query(r.Context(), site, q)
}

// answerQueryError turns an engine failure into a response. A *query.Error is
// the caller's mistake and carries a message written for them; anything else is
// ours and must not leak.
func (a *API) answerQueryError(w http.ResponseWriter, err error) {
	var callerError *query.Error
	if errors.As(err, &callerError) {
		a.fail(w, http.StatusBadRequest, callerError.Message)
		return
	}

	a.internal(w, "run query", err)
}
