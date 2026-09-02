//
// handler.go
// The annotations endpoint the graph reads its markers from.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package annotations

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// The routes this package answers on. Two patterns rather than one because the
// collection and a single note take different methods, and a mux that matches
// both on one pattern has to re-derive which it got from the path.
const (
	CollectionPattern = "/api/sites/{domain}/annotations"
	ItemPattern       = "/api/sites/{domain}/annotations/{id}"
)

// MaxBodyBytes bounds a request. An annotation is a sentence; anything
// approaching this is a mistake or an attempt to make the JSON decoder the most
// expensive thing on the box.
const MaxBodyBytes = 8 * 1024

// Handler serves annotations for one shard.
type Handler struct {
	Store *Store

	// Sites is the same in-memory routing snapshot the ingest and stats paths
	// read, so listing a site's markers never touches system.db.
	Sites *sites.Cache

	// Identity is supplied by the authenticated application guard. Annotation
	// authorship is derived from this principal and never from request JSON.
	Identity func(*http.Request) (userID int64, name string, ok bool)

	Log *logger.Logger
}

// New builds the handler.
func New(store *Store, cache *sites.Cache, log *logger.Logger) *Handler {
	return &Handler{Store: store, Sites: cache, Log: log}
}

// ServeHTTP answers one annotation request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	if domain == "" {
		h.fail(w, http.StatusBadRequest, "the URL must name a site")

		return
	}

	site, ok := h.Sites.Lookup(domain)
	if !ok {
		h.fail(w, http.StatusNotFound, "no site is registered for "+domain)

		return
	}

	switch r.Method {
	case http.MethodGet:
		h.list(w, r, site)

	case http.MethodPost:
		h.create(w, r, site)

	case http.MethodDelete:
		h.remove(w, r, site)

	case http.MethodPut, http.MethodPatch:
		h.update(w, r, site)

	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		h.fail(w, http.StatusMethodNotAllowed, "that method is not supported here")
	}
}

// list answers the markers in a date range.
func (h *Handler) list(w http.ResponseWriter, r *http.Request, site sites.Site) {
	query := r.URL.Query()

	found, err := h.Store.List(r.Context(), site.AccountID, site.ID,
		strings.TrimSpace(query.Get("from")), strings.TrimSpace(query.Get("to")))
	if err != nil {
		h.internal(w, "list annotations", err)

		return
	}

	h.write(w, http.StatusOK, map[string]any{"annotations": found})
}

// create writes a new marker.
func (h *Handler) create(w http.ResponseWriter, r *http.Request, site sites.Site) {
	annotation, ok := h.decode(w, r)
	if !ok {
		return
	}

	annotation.SiteID = site.ID

	if h.Identity == nil {
		h.fail(w, http.StatusUnauthorized, "an authenticated session is required")
		return
	}

	userID, name, ok := h.Identity(r)
	if !ok {
		h.fail(w, http.StatusUnauthorized, "an authenticated session is required")
		return
	}

	annotation.AuthorUserID = userID
	annotation.AuthorName = name

	created, err := h.Store.Create(r.Context(), site.AccountID, annotation)
	if err != nil {
		h.failOrInternal(w, "create annotation", err)

		return
	}

	h.write(w, http.StatusCreated, created)
}

// update edits an existing marker.
func (h *Handler) update(w http.ResponseWriter, r *http.Request, site sites.Site) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}

	annotation, ok := h.decode(w, r)
	if !ok {
		return
	}

	annotation.ID = id
	annotation.SiteID = site.ID

	if err := h.Store.Update(r.Context(), site.AccountID, annotation); err != nil {
		h.failOrInternal(w, "update annotation", err)

		return
	}

	// The response is the stored row, not the decoded input: the input has no
	// author or timestamps, and a client that replaced its copy with it would
	// lose both until the next list.
	updated, err := h.Store.Get(r.Context(), site.AccountID, site.ID, id)
	if err != nil {
		h.failOrInternal(w, "read annotation", err)

		return
	}

	h.write(w, http.StatusOK, updated)
}

// remove deletes a marker.
func (h *Handler) remove(w http.ResponseWriter, r *http.Request, site sites.Site) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}

	if err := h.Store.Delete(r.Context(), site.AccountID, site.ID, id); err != nil {
		h.failOrInternal(w, "delete annotation", err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// id reads the annotation id from the path.
func (h *Handler) id(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		h.fail(w, http.StatusBadRequest, "the URL must name an annotation")

		return 0, false
	}

	return id, true
}

// decode reads the request body, refusing anything it does not recognise. A
// misspelt field that is silently ignored is a note stored with a blank date,
// so an unknown key is a 400 naming the key.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request) (Annotation, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		h.fail(w, http.StatusBadRequest, "could not read the request body")

		return Annotation{}, false
	}

	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()

	var input struct {
		ShownOn string `json:"shown_on"`
		Body    string `json:"body"`
	}
	if err := decoder.Decode(&input); err != nil {
		h.fail(w, http.StatusBadRequest, err.Error())

		return Annotation{}, false
	}

	return Annotation{ShownOn: input.ShownOn, Body: input.Body}, true
}

// failOrInternal maps a store error onto a status. A validation failure is the
// caller's and carries its own sentence; anything else is ours and does not.
func (h *Handler) failOrInternal(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		h.fail(w, http.StatusBadRequest, err.Error())

	case errors.Is(err, ErrNotFound):
		h.fail(w, http.StatusNotFound, "no such annotation")

	default:
		h.internal(w, what, err)
	}
}

// fail answers a caller's mistake with the reason.
func (h *Handler) fail(w http.ResponseWriter, status int, message string) {
	h.write(w, status, map[string]string{"error": message})
}

// internal answers our mistake. The detail goes to our log, because the caller
// can do nothing with a SQLite error and we can do nothing with a bug report
// that does not name one.
func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	if h.Log != nil {
		h.Log.Error("an annotation request failed", "step", what, "error", err)
	}

	h.write(w, http.StatusInternalServerError, map[string]string{"error": "the request could not be answered"})
}

// write encodes a response body.
func (h *Handler) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil && h.Log != nil {
		h.Log.Error("an annotation response could not be written", "error", err)
	}
}
