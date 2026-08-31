//
// gate.go
// Blocking the dashboard for a locked account, without touching control.db per request.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package access decides whether a request may see an account's reports.
//
// It exists as its own package, and as middleware rather than as a check inside
// the dashboard, for two reasons. The dashboard and the stats API are separate
// handlers owned elsewhere, and a lock that only one of them honoured would be
// no lock at all — the API is where the numbers actually come from. And the
// answer has to be available without a database read per request, because the
// stats endpoint is on the path of every card on every dashboard.
package access

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// RefreshInterval is how often the locked set is rebuilt. Fifteen seconds
// matches the routing map: it is the gap between paying and the dashboard
// coming back, and it is short enough that nobody files a bug about it.
const RefreshInterval = 15 * time.Second

// Reason is why an account is locked. There are exactly two, and they are told
// apart because the pages a customer sees are different: one says "pay us" and
// the other says "talk to us".
type Reason string

// The two lock reasons.
const (
	// ReasonLifecycle is a trial that ended or a subscription that stopped
	// paying. It clears the moment a payment succeeds.
	ReasonLifecycle Reason = "lifecycle"

	// ReasonVolume is two consecutive months over the plan with no reply. It
	// clears the moment usage comes back into range, or somebody replies.
	ReasonVolume Reason = "volume"
)

// Gate holds the set of locked accounts and answers whether a request may
// proceed.
type Gate struct {
	Lifecycle *lifecycle.Store
	Usage     *usage.Store
	Sites     *sites.Cache
	Log       *logger.Logger

	// Now is injectable so a test can lock and unlock an account by moving the
	// clock rather than by waiting for a phase boundary.
	Now func() time.Time

	// locked is swapped whole rather than mutated, so a lookup is one atomic
	// load and one map read with no lock on the hot path.
	locked atomic.Pointer[map[int64]Reason]
}

// New builds a gate. Nothing is loaded yet, so a process can construct one
// before it has decided whether it will serve traffic; until the first refresh
// the gate allows everything, which is the right failure direction — locking
// paying customers out because a query has not run yet would be far worse than
// briefly showing a lapsed account its own reports.
func New(lifecycleStore *lifecycle.Store, usageStore *usage.Store, siteCache *sites.Cache, log *logger.Logger) *Gate {
	gate := &Gate{Lifecycle: lifecycleStore, Usage: usageStore, Sites: siteCache, Log: log}
	gate.locked.Store(&map[int64]Reason{})

	return gate
}

// now returns the gate's clock.
func (g *Gate) now() time.Time {
	if g.Now == nil {
		return time.Now().UTC()
	}

	return g.Now().UTC()
}

// Refresh rebuilds the locked set from both sources.
func (g *Gate) Refresh(ctx context.Context) error {
	locked := map[int64]Reason{}

	if g.Lifecycle != nil {
		ids, err := g.Lifecycle.LockedTeams(ctx, g.now())
		if err != nil {
			return err
		}

		for _, id := range ids {
			locked[id] = ReasonLifecycle
		}
	}

	if g.Usage != nil {
		ids, err := g.Usage.LockedTeams(ctx)
		if err != nil {
			return err
		}

		// A lifecycle lock wins when both apply. It is the one the customer has
		// to resolve first, and telling somebody to email sales when their
		// account is thirty days from deletion would be actively unhelpful.
		for _, id := range ids {
			if _, exists := locked[id]; !exists {
				locked[id] = ReasonVolume
			}
		}
	}

	g.locked.Store(&locked)

	return nil
}

// Run refreshes on a ticker until the context is cancelled.
func (g *Gate) Run(ctx context.Context) {
	if err := g.Refresh(ctx); err != nil && g.Log != nil {
		g.Log.Error("access gate refresh failed", "error", err)
	}

	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.Refresh(ctx); err != nil && g.Log != nil {
				g.Log.Error("access gate refresh failed", "error", err)
			}
		}
	}
}

// Locked reports whether an account's reports are blocked, and why.
func (g *Gate) Locked(accountID int64) (Reason, bool) {
	reason, ok := (*g.locked.Load())[accountID]

	return reason, ok
}

// Count reports how many accounts are locked, for the health panel. A number
// that jumps is either a billing outage or a bug in the sweeper, and both are
// worth seeing before the support tickets arrive.
func (g *Gate) Count() int {
	return len(*g.locked.Load())
}

// Set puts an account into the locked map without reading the database. It
// exists for tests and for the path that has just locked an account and should
// not have to wait out a refresh interval.
func (g *Gate) Set(accountID int64, reason Reason) {
	current := *g.locked.Load()

	next := make(map[int64]Reason, len(current)+1)
	for id, existing := range current {
		next[id] = existing
	}
	next[accountID] = reason

	g.locked.Store(&next)
}

// Protect wraps a handler so that a locked account's requests are refused.
//
// The domain is read out of the URL rather than from a session, which is
// deliberate: this has to work identically for the dashboard, the stats API, a
// public shared link and an API key, and the site is the one thing all four
// name. A request for a domain we do not serve is passed through untouched, so
// the handler underneath still answers its own 404 rather than this one
// inventing a different error for the same condition.
func (g *Gate) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain := domainFrom(r)
		if domain == "" {
			next.ServeHTTP(w, r)
			return
		}

		site, known := g.Sites.Lookup(domain)
		if !known {
			next.ServeHTTP(w, r)
			return
		}

		reason, locked := g.Locked(site.AccountID)
		if !locked {
			next.ServeHTTP(w, r)
			return
		}

		g.refuse(w, r, reason)
	})
}

// refuse answers a locked request. The status is 402 Payment Required, which is
// the one status code that says exactly this and is otherwise almost unused —
// a 403 would be indistinguishable from a permissions bug in a support ticket.
func (g *Gate) refuse(w http.ResponseWriter, r *http.Request, reason Reason) {
	message, action := "Your dashboard is locked.", "/billing"

	switch reason {
	case ReasonLifecycle:
		message = "Your dashboard is locked because this account is not currently paying. " +
			"We are still collecting your data, and everything comes back the moment you upgrade. " +
			"Your exports and settings stay open."
	case ReasonVolume:
		message = "Your dashboard is locked because this account has been over its included volume " +
			"for two months and we have not heard back. Collection has not stopped and nothing has been deleted. " +
			"Reply to our email, or write to us, and it unlocks immediately."
	}

	w.Header().Set("Cache-Control", "no-store")

	// The stats API is consumed by JavaScript that expects JSON, and handing it
	// an HTML error page produces a parse error in a console instead of a
	// message on screen.
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  message,
			"reason": string(reason),
			"action": action,
		})

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusPaymentRequired)

	_, _ = w.Write([]byte(lockedPage(message, action)))
}

// domainFrom reads the site out of a request. Go's pattern matching fills the
// path value for a route that declares one; the manual parse covers the
// dashboard, whose whole subtree is one wildcard route.
func domainFrom(r *http.Request) string {
	if value := r.PathValue("domain"); value != "" {
		return value
	}

	trimmed := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(trimmed, "/")

	if len(parts) >= 2 && parts[0] == "dashboard" {
		return parts[1]
	}

	return ""
}

// wantsJSON reports whether the caller is a program rather than a browser.
func wantsJSON(r *http.Request) bool {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		return true
	}

	return strings.HasPrefix(r.URL.Path, "/api/")
}

// lockedPage is the page a person sees. It is deliberately plain and carries
// the two links that matter — pay, and get your data — because a locked screen
// with no way forward is the thing customers write angry reviews about.
func lockedPage(message, action string) string {
	return `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Dashboard locked</title>
<style>
body{margin:0;background:#f4f5f7;color:#1c2024;font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
.card{max-width:560px;margin:12vh auto;background:#fff;border:1px solid #e3e5e8;border-radius:12px;padding:32px}
h1{margin:0 0 12px;font-size:22px}
p{color:#31363c}
a.btn{display:inline-block;margin-top:8px;margin-right:8px;padding:11px 20px;border-radius:8px;background:#1f6feb;color:#fff;text-decoration:none;font-weight:600}
a.alt{background:#fff;color:#1c2024;border:1px solid #d3d6da}
</style></head>
<body><div class="card">
<h1>Your dashboard is locked</h1>
<p>` + message + `</p>
<a class="btn" href="` + action + `">Open billing</a>
<a class="btn alt" href="/billing/export">Download your data</a>
</div></body></html>`
}
