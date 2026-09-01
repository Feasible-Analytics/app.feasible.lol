//
// goals.go
// The goals endpoints, which exist before the feature behind them does.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"net/http"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// handleListGoals lists a site's conversions.
func (a *API) handleListGoals(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesRead)
	if !ok {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	if a.Goals == nil {
		a.notImplemented(w, "goals")
		return
	}

	goals, err := a.Goals.ListGoals(r.Context(), site.ID)
	if err != nil {
		a.answerStoreError(w, "list goals", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"goals": goals})
}

// goalRequest is the body of a goal creation. Exactly one of the two kinds is
// set, which is checked here rather than left to the store, so the error names
// the field a caller has to change.
type goalRequest struct {
	SiteID      string         `json:"site_id"`
	Kind        string         `json:"kind"`
	DisplayName string         `json:"display_name"`
	EventName   string         `json:"event_name"`
	PagePath    string         `json:"page_path"`
	ScrollDepth int            `json:"scroll_depth"`
	Currency    string         `json:"currency"`
	Properties  []GoalProperty `json:"properties"`
}

// handleCreateGoal registers a conversion.
func (a *API) handleCreateGoal(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	var request goalRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}

	// The validation runs even when the feature is missing, so that an
	// integration written today against a build without goals still finds out
	// its request body is wrong, rather than having that error waiting for it
	// on the day the feature arrives.
	goal, err := validateGoal(request)
	if err != nil {
		a.refuse(w, err)
		return
	}

	if a.Goals == nil {
		a.notImplemented(w, "goals")
		return
	}

	created, err := a.Goals.CreateGoal(r.Context(), site.ID, *goal)
	if err != nil {
		a.answerStoreError(w, "create goal", err)
		return
	}

	a.write(w, http.StatusCreated, created)
}

// handleUpdateGoal replaces one configured conversion in place.
func (a *API) handleUpdateGoal(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok || !a.requireScope(w, key, apikeys.ScopeSitesProvision) || !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}
	var request goalRequest
	if !a.decodeBody(w, r, &request) {
		return
	}
	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}
	id, ok := a.idFromPath(w, r, "goal_id")
	if !ok {
		return
	}
	goal, err := validateGoal(request)
	if err != nil {
		a.refuse(w, err)
		return
	}
	updater, ok := a.Goals.(GoalUpdater)
	if !ok {
		a.notImplemented(w, "goal updates")
		return
	}
	updated, err := updater.UpdateGoal(r.Context(), site.ID, id, *goal)
	if err != nil {
		a.answerStoreError(w, "update goal", err)
		return
	}
	a.write(w, http.StatusOK, updated)
}

// funnelRequest is the site-scoped funnel management payload.
type funnelRequest struct {
	SiteID      string       `json:"site_id"`
	Name        string       `json:"name"`
	StrictOrder bool         `json:"strict_order"`
	Steps       []FunnelStep `json:"steps"`
}

// handleListFunnels lists a site's configured funnels.
func (a *API) handleListFunnels(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesRead)
	if !ok || !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}
	if a.Funnels == nil {
		a.notImplemented(w, "funnels")
		return
	}
	list, err := a.Funnels.ListFunnels(r.Context(), site.ID)
	if err != nil {
		a.answerStoreError(w, "list funnels", err)
		return
	}
	a.write(w, http.StatusOK, map[string]any{"funnels": list})
}

// handleCreateFunnel stores a new ordered funnel.
func (a *API) handleCreateFunnel(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok || !a.requireScope(w, key, apikeys.ScopeSitesProvision) || !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}
	var request funnelRequest
	if !a.decodeBody(w, r, &request) {
		return
	}
	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}
	manager, ok := a.Funnels.(FunnelManager)
	if !ok {
		a.notImplemented(w, "funnel management")
		return
	}
	created, err := manager.CreateFunnel(r.Context(), site.ID, Funnel{Name: strings.TrimSpace(request.Name), StrictOrder: request.StrictOrder, Steps: request.Steps})
	if err != nil {
		a.answerStoreError(w, "create funnel", err)
		return
	}
	a.write(w, http.StatusCreated, created)
}

// handleUpdateFunnel replaces one site's ordered funnel.
func (a *API) handleUpdateFunnel(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok || !a.requireScope(w, key, apikeys.ScopeSitesProvision) || !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}
	var request funnelRequest
	if !a.decodeBody(w, r, &request) {
		return
	}
	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}
	id, ok := a.idFromPath(w, r, "funnel_id")
	if !ok {
		return
	}
	manager, ok := a.Funnels.(FunnelManager)
	if !ok {
		a.notImplemented(w, "funnel management")
		return
	}
	updated, err := manager.UpdateFunnel(r.Context(), site.ID, id, Funnel{Name: strings.TrimSpace(request.Name), StrictOrder: request.StrictOrder, Steps: request.Steps})
	if err != nil {
		a.answerStoreError(w, "update funnel", err)
		return
	}
	a.write(w, http.StatusOK, updated)
}

// handleDeleteFunnel removes one site's funnel definition.
func (a *API) handleDeleteFunnel(w http.ResponseWriter, r *http.Request) {
	_, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}
	id, ok := a.idFromPath(w, r, "funnel_id")
	if !ok {
		return
	}
	manager, ok := a.Funnels.(FunnelManager)
	if !ok {
		a.notImplemented(w, "funnel management")
		return
	}
	if err := manager.DeleteFunnel(r.Context(), site.ID, id); err != nil {
		a.answerStoreError(w, "delete funnel", err)
		return
	}
	a.write(w, http.StatusOK, map[string]any{"deleted": true})
}

// validateGoal checks that a goal names exactly one thing to count.
func validateGoal(request goalRequest) (*Goal, error) {
	eventName := strings.TrimSpace(request.EventName)
	pagePath := strings.TrimSpace(request.PagePath)

	kind := strings.TrimSpace(request.Kind)
	if kind == "scroll" {
		if request.ScrollDepth < 1 || request.ScrollDepth > 100 {
			return nil, badParam("scroll_depth must be between 1 and 100")
		}
	} else if eventName == "" && pagePath == "" {
		return nil, badParam("a goal needs either event_name or page_path")
	}

	if kind != "scroll" && eventName != "" && pagePath != "" {
		return nil, badParam("a goal counts either an event or a page, not both — send one of event_name or page_path")
	}

	if pagePath != "" && !strings.HasPrefix(pagePath, "/") {
		return nil, badParam("page_path must start with a slash, for example /thanks")
	}

	if request.Currency != "" && len(request.Currency) != 3 {
		return nil, badParam("currency must be a three-letter ISO code, for example USD")
	}

	displayName := strings.TrimSpace(request.DisplayName)
	if displayName == "" {
		displayName = eventName + pagePath
		if kind == "scroll" {
			displayName = "Scroll depth"
		}
	}
	if kind == "" {
		if eventName != "" {
			kind = "event"
		} else {
			kind = "page"
		}
	}

	return &Goal{
		Kind:        kind,
		DisplayName: displayName,
		EventName:   eventName,
		PagePath:    pagePath,
		ScrollDepth: request.ScrollDepth,
		Currency:    strings.ToUpper(request.Currency),
		IsRevenue:   request.Currency != "",
		Properties:  request.Properties,
	}, nil
}

// handleDeleteGoal removes a conversion.
func (a *API) handleDeleteGoal(w http.ResponseWriter, r *http.Request) {
	_, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "goal_id")
	if !ok {
		return
	}

	if a.Goals == nil {
		a.notImplemented(w, "goals")
		return
	}

	if err := a.Goals.DeleteGoal(r.Context(), site.ID, id); err != nil {
		a.answerStoreError(w, "delete goal", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"deleted": true})
}

// notImplemented answers for a feature this build does not carry yet.
//
// It is a 501 rather than a 404 on purpose. A 404 tells an integrator their URL
// is wrong and sends them to check their own code; a 501 naming the feature
// tells them the truth, which is that the route is right and the thing behind it
// is not built.
func (a *API) notImplemented(w http.ResponseWriter, feature string) {
	a.write(w, http.StatusNotImplemented, errorBody{Error: unavailable(feature)})
}
