//
// stats.go
// POST /api/v2/query — the internal endpoint, with a key in front of it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/statsapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// stats builds the internal query handler once, and hands the same instance to
// every request.
//
// The public endpoint is genuinely the internal handler with authentication in
// front, not a second implementation of it. Two handlers over one query engine
// would drift — a validation added to one, a default changed in the other — and
// the dashboard and the API would start disagreeing about the same number for
// reasons nobody could reproduce.
func (a *API) statsHandler() *statsapi.Handler {
	a.statsOnce.Do(func() {
		handler := statsapi.New(a.Sites, a.Accounts, a.Log)
		handler.Now = a.now
		handler.Authorize = statsapi.AllowAll
		a.stats = handler
	})

	return a.stats
}

// siteRef is the one field this handler reads before delegating. It is decoded
// with a permissive struct rather than the full request type because the strict
// decode — the one that refuses unknown fields — is the internal handler's job,
// and doing it twice would report the same error from two places.
type siteRef struct {
	SiteID string `json:"site_id"`
}

// handleQueryV2 authenticates, authorises the named site and then hands the
// request to the internal handler unchanged.
//
// The body is read here and put back before delegating, because the site has to
// be known before we can decide whether this key may read it, and the internal
// handler needs the same bytes afterwards. Rewriting the path value rather than
// reimplementing the handler is what keeps "the same handler" literally true.
func (a *API) handleQueryV2(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeStatsRead) {
		return
	}
	if !a.requirePermission(w, key, teams.PermViewDashboard) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		a.fail(w, http.StatusBadRequest, "could not read the request body")
		return
	}

	var ref siteRef
	if err := json.Unmarshal(body, &ref); err != nil {
		// The full decode below would report this too, but it would report it
		// after a site lookup that cannot work, so the message would be about
		// the site rather than about the JSON.
		a.fail(w, http.StatusBadRequest, "the request body is not valid JSON")
		return
	}

	site, ok := a.resolveSite(w, key, ref.SiteID)
	if !ok {
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.SetPathValue("domain", site.Domain)

	a.statsHandler().ServeHTTP(w, r)
}

// authorisedSites lists the sites a key may read, in domain order. It is shared
// by the sites listing endpoint and by the MCP `list_sites` tool, so the two can
// never disagree about what a key can see. The order is stable because an
// unordered list breaks pagination: page two of a shuffled list is not the rest
// of page one.
func (a *API) authorisedSites(key *apikeys.Key) []sites.Site {
	owned := []sites.Site{}

	for _, domain := range a.Sites.Domains() {
		site, ok := a.Sites.Lookup(domain)
		if ok && site.TeamID == key.TeamID {
			owned = append(owned, site)
		}
	}

	sort.Slice(owned, func(i, j int) bool { return owned[i].Domain < owned[j].Domain })

	return owned
}
