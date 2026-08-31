//
// hooks.go
// The webhook management endpoints, including the delivery log and redelivery.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"net/http"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/webhooks"
)

// requireWebhooks refuses when the feature is not wired into this process.
func (a *API) requireWebhooks(w http.ResponseWriter, r *http.Request) (int64, bool) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return 0, false
	}

	if !a.requireScope(w, key, apikeys.ScopeWebhooks) {
		return 0, false
	}

	if a.Webhooks == nil || a.Dispatcher == nil {
		a.notImplemented(w, "webhooks")
		return 0, false
	}

	return key.TeamID, true
}

// handleWebhookEventTypes lists what can be subscribed to. It is its own
// endpoint because the alternative is a customer discovering the list by
// subscribing to a name and waiting to see whether anything ever arrives.
func (a *API) handleWebhookEventTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireWebhooks(w, r); !ok {
		return
	}

	a.write(w, http.StatusOK, map[string]any{"event_types": webhooks.EventTypes()})
}

// createWebhookRequest is the body of an endpoint registration.
type createWebhookRequest struct {
	URL         string   `json:"url"`
	SiteID      string   `json:"site_id"`
	Description string   `json:"description"`
	EventTypes  []string `json:"event_types"`
}

// handleCreateWebhook registers a destination and returns its signing secret,
// which is the only time the secret is ever shown.
func (a *API) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	var request createWebhookRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	var siteID *int64

	if strings.TrimSpace(request.SiteID) != "" {
		key, _ := KeyFrom(r.Context())

		site, ok := a.resolveSite(w, key, request.SiteID)
		if !ok {
			return
		}

		siteID = &site.ID
	}

	endpoint, err := a.Webhooks.Create(r.Context(), teamID, siteID, request.URL, request.Description, request.EventTypes)
	if err != nil {
		// Everything Create refuses is something the caller wrote: a bad URL, a
		// scheme we will not send secrets over, an event type that does not
		// exist. None of them is a 500.
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	a.write(w, http.StatusCreated, endpoint)
}

// handleListWebhooks lists a team's endpoints.
func (a *API) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	endpoints, err := a.Webhooks.List(r.Context(), teamID)
	if err != nil {
		a.answerStoreError(w, "list webhooks", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"webhooks": endpoints})
}

// handleGetWebhook reads one endpoint.
func (a *API) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "webhook_id")
	if !ok {
		return
	}

	endpoint, err := a.Webhooks.Get(r.Context(), teamID, id)
	if err != nil {
		a.answerStoreError(w, "get webhook", err)
		return
	}

	a.write(w, http.StatusOK, endpoint)
}

// updateWebhookRequest is the body of an endpoint update. Pointers again, so
// that leaving a field out means "leave it alone" rather than "set it to empty".
type updateWebhookRequest struct {
	URL         *string   `json:"url"`
	Description *string   `json:"description"`
	EventTypes  *[]string `json:"event_types"`
	Enabled     *bool     `json:"enabled"`
}

// handleUpdateWebhook changes an endpoint.
func (a *API) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "webhook_id")
	if !ok {
		return
	}

	var request updateWebhookRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	endpoint, err := a.Webhooks.Update(r.Context(), teamID, id, request.URL, request.Description, request.EventTypes, request.Enabled)
	if err != nil {
		if isWebhookNotFound(err) {
			a.fail(w, http.StatusNotFound, "no such webhook endpoint")
			return
		}

		a.fail(w, http.StatusBadRequest, err.Error())

		return
	}

	a.write(w, http.StatusOK, endpoint)
}

// isWebhookNotFound reports whether a store error is a missing row.
func isWebhookNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), webhooks.ErrNotFound.Error())
}

// handleDeleteWebhook removes an endpoint and its log.
func (a *API) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "webhook_id")
	if !ok {
		return
	}

	if err := a.Webhooks.Delete(r.Context(), teamID, id); err != nil {
		a.answerStoreError(w, "delete webhook", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleRotateWebhookSecret mints a new signing secret, keeping the old one
// valid for a grace period so that rotating does not break the deliveries in
// flight against a receiver that has not redeployed yet.
func (a *API) handleRotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "webhook_id")
	if !ok {
		return
	}

	endpoint, err := a.Webhooks.Rotate(r.Context(), teamID, id)
	if err != nil {
		a.answerStoreError(w, "rotate webhook secret", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{
		"webhook":                           endpoint,
		"previous_secret_valid_for_seconds": int(webhooks.RotationGrace.Seconds()),
	})
}

// handleListDeliveries reads the log for one endpoint.
func (a *API) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "webhook_id")
	if !ok {
		return
	}

	limit, page, ok := a.pageParams(w, r)
	if !ok {
		return
	}

	deliveries, err := a.Webhooks.Deliveries(r.Context(), teamID, id, limit, (page-1)*limit)
	if err != nil {
		a.answerStoreError(w, "webhook deliveries", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{
		"deliveries": deliveries,
		"meta":       map[string]any{"page": page, "limit": limit},
	})
}

// handleRedeliver queues an existing delivery again.
//
// It answers 202 rather than 200: nothing has been delivered by the time this
// returns, and saying otherwise would have somebody refreshing their receiver's
// log looking for a request that has not been made yet.
func (a *API) handleRedeliver(w http.ResponseWriter, r *http.Request) {
	teamID, ok := a.requireWebhooks(w, r)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "delivery_id")
	if !ok {
		return
	}

	delivery, err := a.Dispatcher.Redeliver(r.Context(), teamID, id)
	if err != nil {
		a.answerStoreError(w, "redeliver", err)
		return
	}

	a.write(w, http.StatusAccepted, delivery)
}
