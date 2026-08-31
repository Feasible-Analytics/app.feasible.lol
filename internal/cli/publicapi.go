//
// publicapi.go
// Assembling the public API, the MCP server and the webhook worker.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mcp"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/webhooks"
)

// publicStack is everything the public surface is made of, assembled once and
// handed to whichever command needs it.
//
// It is one struct because the pieces are genuinely one thing: the MCP server
// is a second front end onto the same API, the webhook dispatcher is what the
// provisioning endpoints publish through, and the OAuth endpoints exist only to
// hand out tokens the MCP endpoint accepts. Building them separately in `serve`
// and again in `mcp` is how the two would end up subtly different.
type publicStack struct {
	API      *publicapi.API
	MCP      *mcp.Server
	Handler  *mcp.Handler
	OAuth    *mcp.OAuth
	Webhooks *webhooks.Store
	Worker   *webhooks.Worker
	Keys     *apikeys.Store
}

// buildPublic assembles the stack over an already-open control database.
//
// Nothing here is gated on a plan. The Stats API, the Sites API, the MCP server
// and webhooks are in every build and every plan, which is the whole point: the
// incumbent gates their stats API behind their most expensive tier, their
// self-hosted build inherited the check, and people running it on their own
// hardware were shown a subscription prompt on their own instance. A plan check
// in this function would be a bug.
func buildPublic(e *env, control *sql.DB, cache *sites.Cache, manager *accounts.Manager, gate *access.Gate) *publicStack {
	keys := apikeys.NewStore(control)
	hooks := webhooks.NewStore(control)
	dispatcher := webhooks.NewDispatcher(hooks)

	// A locked account's numbers must not leave by the back door either. An
	// outbound goal conversion or traffic spike is the same data the API is
	// about to refuse, arriving at a customer's endpoint on a schedule.
	dispatcher.Blocked = gate.Blocked

	api := &publicapi.API{
		Keys:       keys,
		Limiter:    apikeys.NewLimiter(e.cfg.API.RateLimit),
		Access:     gate,
		Sites:      cache,
		Control:    publicapi.NewControlStore(control),
		Accounts:   manager,
		Webhooks:   hooks,
		Dispatcher: dispatcher,
		BaseURL:    e.cfg.App.BaseURL,
		Log:        e.log,
	}

	server := mcp.New(api, e.log)

	oauth := &mcp.OAuth{DB: control, Keys: keys, BaseURL: e.cfg.App.BaseURL}

	worker := webhooks.NewWorker(hooks, e.cfg.API.WebhookTimeout)
	worker.Log = func(message string, args ...any) { e.log.Info(message, args...) }
	worker.Notifier = &loggingNotifier{log: e.log}

	return &publicStack{
		API:      api,
		MCP:      server,
		Handler:  &mcp.Handler{Server: server, Authenticate: oauth.Authenticate, ResourceMetadataURL: e.cfg.App.BaseURL + mcp.PathProtectedResourceMetadata},
		OAuth:    oauth,
		Webhooks: hooks,
		Worker:   worker,
		Keys:     keys,
	}
}

// mount adds the public surface to a mux.
//
// The API is mounted on two subtree patterns rather than at the root because
// the same process also answers /api/event, the tracker script and the health
// probes, none of which take an API key. Mounting the authenticated handler at
// the root would put a bearer-token check in front of the event endpoint, which
// is every customer's traffic — and, now that the same wrapper also carries the
// account lock, would stop a locked account being collected at all.
func (p *publicStack) mount(mux *http.ServeMux) {
	routes := p.API.Routes()

	mux.Handle("/api/v1/", routes)
	mux.Handle("/api/v2/", routes)

	mux.Handle(mcp.Path, p.Handler)

	p.OAuth.Routes(mux)
}

// startWorker runs the webhook delivery loop and the rate limiter's sweeper
// until the context is cancelled.
//
// The delivery loop runs in the app process rather than a separate one because a
// self-hoster's whole deployment is this binary — and it is a goroutine rather
// than something on a request path because a customer endpoint that never
// answers must cost a worker its timeout and cost event collection nothing.
func (p *publicStack) startWorker(ctx context.Context, e *env) {
	go p.Worker.Run(ctx, e.cfg.API.WebhookPoll)
	go sweepLimiter(ctx, p.API.Limiter)
}

// sweepLimiter drops the rate-limit counters whose window has passed. Without
// it, a process that has served a million one-off keys holds a million counters
// for as long as it runs.
func sweepLimiter(ctx context.Context, limiter *apikeys.Limiter) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			limiter.Sweep()
		}
	}
}

// loggingNotifier is the interim destination for the "your webhook is failing"
// notices.
//
// It logs rather than emails because the mail transport is not wired into this
// process yet. Logging is not good enough on its own — the customer cannot read
// our logs — but it is the difference between a warning that exists and one that
// does not, and swapping this for the mail sender is a one-line change here.
type loggingNotifier struct {
	log *logger.Logger
}

// WebhookFailing records that an endpoint is on its way to being disabled.
func (n *loggingNotifier) WebhookFailing(_ context.Context, endpoint *webhooks.Endpoint, failures, disableAfter int) error {
	n.log.Warn("webhook endpoint is failing",
		"endpoint", endpoint.ID, "team", endpoint.TeamID, "url", endpoint.URL,
		"consecutive_failures", failures, "disabled_after", disableAfter)

	return nil
}

// WebhookDisabled records that we have stopped trying.
func (n *loggingNotifier) WebhookDisabled(_ context.Context, endpoint *webhooks.Endpoint, reason string) error {
	n.log.Warn("webhook endpoint disabled",
		"endpoint", endpoint.ID, "team", endpoint.TeamID, "url", endpoint.URL, "reason", reason)

	return nil
}
