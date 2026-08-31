//
// handler.go
// POST /api/stats/:domain/query — the single endpoint the dashboard runs on.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package statsapi is the HTTP surface of the query engine. It is one endpoint
// rather than a handler per report, because every report in the product is the
// same request with different metrics and dimensions: one request struct, one
// compiler, one response shape. Twenty handlers is twenty places for the same
// number to be counted twenty slightly different ways.
package statsapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// Pattern is the route this handler is mounted on. It is a constant so that the
// path and the name the handler reads the domain out of cannot drift apart.
const Pattern = "POST /api/stats/{domain}/query"

// MaxBodyBytes bounds a request body. A query is a few hundred bytes; anything
// approaching this is either a mistake or an attempt to make the JSON decoder
// the most expensive thing on the box.
const MaxBodyBytes = 1 << 20

// Handler answers stats queries for one shard.
type Handler struct {
	// Sites resolves a domain to a site and the account that owns it. It is
	// the same in-memory snapshot the ingest path reads, so a dashboard query
	// never touches control.db either.
	Sites *sites.Cache

	// Accounts hands out the per-account database handles.
	Accounts *accounts.Manager

	Log *logger.Logger

	// Now is the clock every date range is resolved against, injectable so a
	// test can ask what "today" returns without waiting for tomorrow.
	Now func() time.Time

	// live holds the answers to reports whose range reaches today. A finished
	// period is never held: it cannot change, and it is already answered from
	// the summary tables in single-digit milliseconds.
	live *cache
}

// New builds a handler over the site cache and the account manager.
func New(cache *sites.Cache, manager *accounts.Manager, log *logger.Logger) *Handler {
	return &Handler{Sites: cache, Accounts: manager, Log: log, live: newCache(CacheTTL, CacheEntries)}
}

// now reads the handler's clock.
func (h *Handler) now() time.Time {
	if h.Now == nil {
		return time.Now().UTC()
	}

	return h.Now()
}

// request is the wire form. It is a struct of its own rather than query.Query
// because the two differ in exactly one place — a caller names a site by
// domain, and the engine works in site ids — and because unknown fields are
// refused here, which needs a type that lists every field a caller may send.
type request struct {
	SiteID     string           `json:"site_id"`
	Metrics    []string         `json:"metrics"`
	DateRange  query.DateRange  `json:"date_range"`
	Dimensions []string         `json:"dimensions"`
	Filters    []query.Filter   `json:"filters"`
	OrderBy    []query.Order    `json:"order_by"`
	Pagination query.Pagination `json:"pagination"`
	Include    query.Include    `json:"include"`
	Timezone   string           `json:"timezone"`
	SampleRate float64          `json:"sample_rate"`

	// Currency is the ISO 4217 code the money metrics are totalled in. It is
	// optional: with one currency in the data the compiler resolves it, and
	// with several it refuses rather than adding them together.
	Currency string `json:"currency"`
}

// errorBody is what a failure looks like. One field, always the same shape, so
// a client can read the reason without branching on the status code.
type errorBody struct {
	Error string `json:"error"`
}

// ServeHTTP answers one query.
//
// This handler carries no authentication of its own. The public API mounts this
// exact instance behind an API key as POST /api/v2/query, resolving the site and
// checking the key's team before delegating — which is why the public endpoint
// can never disagree with the dashboard about a number. On its own path it is
// still open, so anybody who can reach the port can read any site's stats: the
// dashboard's session check belongs in front of it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.fail(w, http.StatusMethodNotAllowed, "POST a query to this endpoint")
		return
	}

	domain := h.domain(r)
	if domain == "" {
		h.fail(w, http.StatusBadRequest, "the URL must name a site, as /api/stats/<domain>/query")
		return
	}

	site, ok := h.Sites.Lookup(domain)
	if !ok {
		// Not found rather than forbidden: this endpoint has no authentication
		// in front of it yet, and once it does, the answer for a site somebody
		// may not read should be the same as for one that does not exist.
		h.fail(w, http.StatusNotFound, "no site is registered for "+domain)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		h.fail(w, http.StatusBadRequest, "could not read the request body")
		return
	}

	parsed, err := decode(body)
	if err != nil {
		h.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// A body that names a different site than the URL is a mistake worth
	// catching: answering for one of the two would silently return another
	// site's numbers.
	if parsed.SiteID != "" && sites.Normalise(parsed.SiteID) != sites.Normalise(domain) {
		h.fail(w, http.StatusBadRequest, "site_id in the body does not match the site in the URL")
		return
	}

	// A dashboard refreshes itself and is left open, so the same handful of
	// live reports arrive over and over with a few new events between them.
	key := cacheKey(sites.Normalise(domain), body)

	if held, ok := h.live.get(key); ok {
		h.writeBody(w, http.StatusOK, held)
		return
	}

	account, err := h.Accounts.Open(r.Context(), site.AccountID)
	if err != nil {
		h.internal(w, "open account", err)
		return
	}

	engine := query.New(account.Reader())
	if h.Now != nil {
		engine.Now = h.Now
	}

	result, err := engine.Run(r.Context(), parsed.toQuery(site))
	if err != nil {
		var callerError *query.Error
		if errors.As(err, &callerError) {
			h.fail(w, http.StatusBadRequest, callerError.Message)
			return
		}

		h.internal(w, "run query", err)
		return
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		h.internal(w, "encode result", err)
		return
	}

	if h.stillRunning(result) {
		h.live.put(key, encoded)
	}

	h.writeBody(w, http.StatusOK, encoded)
}

// stillRunning reports whether a report's period has not finished, which is the
// only kind worth holding. The bound comes from the echoed query rather than
// from the request, because that is the window the engine actually resolved —
// a preset, a relative range and an explicit pair of dates all end up there in
// one form.
func (h *Handler) stillRunning(result *query.Result) bool {
	if len(result.Query.DateRange) != 2 {
		return false
	}

	end, err := time.Parse(time.RFC3339, result.Query.DateRange[1])
	if err != nil {
		return false
	}

	return end.After(h.now())
}

// domain reads the site out of the URL, falling back to parsing the path so the
// handler still works if it is ever mounted somewhere without a named wildcard.
func (h *Handler) domain(r *http.Request) string {
	if value := r.PathValue("domain"); value != "" {
		return value
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, part := range parts {
		if part == "stats" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	return ""
}

// decode reads the request body, refusing anything it does not recognise. A
// misspelt field that is silently ignored is a filter that never applied and a
// number nobody can explain, so an unknown key is a 400 naming the key.
func decode(body []byte) (*request, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()

	parsed := &request{}
	if err := decoder.Decode(parsed); err != nil {
		return nil, readableJSONError(err)
	}

	return parsed, nil
}

// readableJSONError turns a decoder failure into something a caller can act on.
func readableJSONError(err error) error {
	message := err.Error()

	// The decoder's own wording for an unknown key is already the most useful
	// sentence we could write, so it is passed through rather than replaced.
	if strings.Contains(message, "unknown field") {
		return errors.New(message + " — check the spelling; unknown fields are refused rather than ignored")
	}

	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return errors.New("the request body is not valid JSON")
	}

	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return errors.New("the field " + mismatch.Field + " has the wrong type")
	}

	return errors.New(message)
}

// toQuery turns the wire request into the engine's query, defaulting the
// timezone to the site's own. The site's timezone is the right default because
// a day boundary belongs to the site owner, not to whoever is looking at the
// dashboard from another continent.
func (r *request) toQuery(site sites.Site) query.Query {
	timezone := r.Timezone
	if timezone == "" {
		timezone = site.Timezone
	}

	return query.Query{
		SiteIDs:    []int64{site.ID},
		Metrics:    r.Metrics,
		Dimensions: r.Dimensions,
		Filters:    r.Filters,
		DateRange:  r.DateRange,
		Timezone:   timezone,
		OrderBy:    r.OrderBy,
		Pagination: r.Pagination,
		Include:    r.Include,
		SampleRate: r.SampleRate,
		Currency:   r.Currency,
	}
}

// fail answers a caller's mistake with the reason.
func (h *Handler) fail(w http.ResponseWriter, status int, message string) {
	h.write(w, status, errorBody{Error: message})
}

// internal answers our mistake. The detail goes to our log, because the caller
// can do nothing with a SQLite error and we can do nothing with a bug report
// that does not name one.
func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	if h.Log != nil {
		h.Log.Error("stats query failed", "step", what, "error", err)
	}

	h.write(w, http.StatusInternalServerError, errorBody{Error: "the query could not be answered"})
}

// write encodes a response body.
func (h *Handler) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil && h.Log != nil {
		h.Log.Error("stats response could not be written", "error", err)
	}
}

// writeBody sends an already-encoded response. A held answer is bytes rather
// than a struct, so re-encoding it on every hit would spend most of what the
// cache saves.
func (h *Handler) writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if _, err := w.Write(append(body, '\n')); err != nil && h.Log != nil {
		h.Log.Error("stats response could not be written", "error", err)
	}
}
