//
// handler.go
// GET /api/sites/:domain/search-console/report — the dashboard's search card.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// ReportPattern is the route the search card reads.
const ReportPattern = "GET /api/sites/{domain}/search-console/report"

// MaxSearchText bounds the reader's filter text. A keyword is a few words; a
// megabyte of it is somebody making the LIKE scan the expensive part of the box.
const MaxSearchText = 200

// Handler answers the search card.
//
// It is deliberately not reachable through a shared link or a public dashboard.
// A share can be pinned to a segment — one country, one campaign — and the
// stored search rows carry none of the dimensions those segments filter on, so
// there is no honest way to narrow them to match. Serving them anyway would let
// a link scoped to a slice of a site hand over the whole site's keyword list.
type Handler struct {
	Sites    *sites.Cache
	Accounts *accounts.Manager
	Log      *logger.Logger

	// Available reports whether this install has a Google application at all.
	// With none, the card hides itself rather than offering a connect button
	// that leads to invalid_client.
	Available bool

	// Authorize proves the caller holds a session that may read this site.
	// Nil denies: a newly-mounted handler cannot accidentally become public.
	Authorize func(*http.Request, sites.Site) error

	// Now is the clock relative ranges resolve against, injectable for tests.
	Now func() time.Time
}

// now reads the handler's clock.
func (h *Handler) now() time.Time {
	if h.Now == nil {
		return time.Now().UTC()
	}

	return h.Now()
}

// ServeHTTP answers one search report.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "GET a search report from this endpoint")
		return
	}

	domain := strings.TrimSpace(r.PathValue("domain"))
	if domain == "" {
		h.fail(w, http.StatusBadRequest, "the URL must name a site")
		return
	}

	site, ok := h.Sites.Lookup(domain)
	if !ok {
		h.fail(w, http.StatusNotFound, "no site is registered for "+domain)
		return
	}

	if h.Authorize == nil {
		h.fail(w, http.StatusUnauthorized, "an authenticated session is required")
		return
	}

	if err := h.Authorize(r, site); err != nil {
		h.fail(w, http.StatusNotFound, "no site is registered for "+domain)
		return
	}

	// Answered before the account is opened. An install with no credentials can
	// never have a row to read, and the card only needs to be told once.
	if !h.Available {
		h.write(w, &SearchReport{Status: StatusUnavailable, Rows: []SearchRow{}})
		return
	}

	request, err := decodeSearchRequest(r)
	if err != nil {
		h.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	request.SiteID = site.ID

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.internal(w, "open account", err)
		return
	}
	defer lease.Release() //nolint:errcheck // the report is more useful than an unlock error

	connection, err := GetConnection(r.Context(), lease.Account.Reader(), site.ID, ProviderSearchConsole)
	if err != nil {
		h.internal(w, "read connection", err)
		return
	}

	switch {
	case connection == nil:
		h.write(w, &SearchReport{Status: StatusNotConnected, Rows: []SearchRow{}})
		return
	case strings.TrimSpace(connection.Property) == "":
		h.write(w, &SearchReport{Status: StatusNoProperty, Rows: []SearchRow{}})
		return
	}

	location, err := time.LoadLocation(site.Timezone)
	if err != nil {
		h.internal(w, "load site timezone", err)
		return
	}

	resolved, err := h.resolve(r, lease, site, location, request.rawRange)
	if err != nil {
		var callerError *query.Error
		if errors.As(err, &callerError) {
			h.fail(w, http.StatusBadRequest, callerError.Message)
			return
		}

		h.internal(w, "resolve date range", err)
		return
	}

	request.Start, request.End = resolved.Start, resolved.End
	request.Location = location

	report, err := SearchReportRun(r.Context(), lease.Account.Reader(), request.SearchReportRequest)
	if err != nil {
		h.internal(w, "read search report", err)
		return
	}

	report.Property = connection.Property

	if connection.NeedsReconnect() {
		// The stored rows are still true, so they are still shown. The status
		// is what lets the card say the numbers have stopped being topped up
		// rather than letting them quietly go stale.
		report.Status = StatusNotConnected
	}

	h.write(w, report)
}

// resolve turns the requested range into absolute bounds in the site's zone.
func (h *Handler) resolve(r *http.Request, lease *accounts.Lease, site sites.Site,
	location *time.Location, dateRange query.DateRange) (query.Resolved, error) {

	var earliest time.Time

	if dateRange.NeedsEarliest() {
		// Only "all" pays for this. The earliest imported day is the right
		// floor here rather than the site's first event: search history
		// routinely reaches back further than the tracker does.
		first, err := EarliestSearchDay(r.Context(), lease.Account.Reader(), site.ID)
		if err != nil {
			return query.Resolved{}, err
		}

		earliest = first
	}

	return dateRange.Resolve(h.now(), location, earliest)
}

// searchRequest is the decoded query string plus the range it named.
type searchRequest struct {
	SearchReportRequest
	rawRange query.DateRange
}

// decodeSearchRequest reads the query string. The date range stays JSON because
// it already has a strict decoder, and a second grammar here would accept
// ranges the rest of the dashboard refuses.
func decodeSearchRequest(r *http.Request) (searchRequest, error) {
	values := r.URL.Query()
	request := searchRequest{}

	raw := strings.TrimSpace(values.Get("date_range"))
	if raw == "" {
		raw = `"28d"`
	}

	if err := json.Unmarshal([]byte(raw), &request.rawRange); err != nil {
		return searchRequest{}, err
	}

	request.Dimension = strings.TrimSpace(values.Get("dimension"))
	if request.Dimension == "" {
		request.Dimension = DimensionQuery
	}

	if _, ok := searchColumns[request.Dimension]; !ok {
		return searchRequest{}, errors.New("dimension must be query, page, country or device")
	}

	search := strings.TrimSpace(values.Get("search"))
	if len(search) > MaxSearchText {
		return searchRequest{}, errors.New("the search text is longer than " + strconv.Itoa(MaxSearchText) + " characters")
	}
	request.Search = search

	if limit := strings.TrimSpace(values.Get("limit")); limit != "" {
		parsed, err := strconv.Atoi(limit)
		if err != nil {
			return searchRequest{}, errors.New("limit must be a whole number")
		}

		request.Limit = parsed
	}

	return request, nil
}

// write sends one report.
func (h *Handler) write(w http.ResponseWriter, report *SearchReport) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if err := json.NewEncoder(w).Encode(report); err != nil && h.Log != nil {
		h.Log.Warn("a search report could not be written", "error", err)
	}
}

// fail answers a refusal in the envelope every dashboard read shares.
func (h *Handler) fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil && h.Log != nil {
		h.Log.Warn("a search report refusal could not be written", "error", err)
	}
}

// internal logs the detail and tells the caller only that it failed. Database
// text in a response body is how a schema ends up in somebody's browser console.
func (h *Handler) internal(w http.ResponseWriter, operation string, err error) {
	if h.Log != nil {
		h.Log.Error("search report failed", "operation", operation, "error", err)
	}

	h.fail(w, http.StatusInternalServerError, "the search report could not be produced")
}
