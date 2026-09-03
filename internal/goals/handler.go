//
// handler.go
// The goals report endpoint used by the dashboard.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

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

// Dashboard route patterns expose conversions, properties, funnels, and
// journey exploration over one authorization and query-string vocabulary.
const (
	ReportPattern         = "GET /api/sites/{domain}/goals/report"
	GoalsPattern          = "GET /api/sites/{domain}/goals"
	PropertiesPattern     = "GET /api/sites/{domain}/properties"
	PropertyReportPattern = "GET /api/sites/{domain}/properties/{property}/report"
	FunnelsPattern        = "GET /api/sites/{domain}/funnels"
	FunnelReportPattern   = "GET /api/sites/{domain}/funnels/{funnel_id}/report"
	JourneyPattern        = "GET /api/sites/{domain}/journey"
)

// Handler answers goal reports from the same account database and query engine
// that power every other dashboard number.
type Handler struct {
	Sites    *sites.Cache
	Accounts *accounts.Manager
	Log      *logger.Logger

	// Authorize proves the caller may read the site and supplies any immutable
	// filters pinned to a public or shared dashboard.
	Authorize func(*http.Request, sites.Site) (Authorization, error)

	// Now is the clock relative ranges resolve against, injectable for tests.
	Now func() time.Time
}

// Authorization is the server-owned portion of a goals report request.
type Authorization struct {
	PinnedFilters []query.Filter
}

// AuthorizationError is a refusal the caller can act on.
type AuthorizationError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *AuthorizationError) Error() string {
	return e.Message
}

// Refuse builds a caller-facing authorization failure.
func Refuse(status int, message string) error {
	return &AuthorizationError{Status: status, Message: message}
}

// NewHandler builds the goals report endpoint.
func NewHandler(cache *sites.Cache, manager *accounts.Manager, log *logger.Logger) *Handler {
	return &Handler{Sites: cache, Accounts: manager, Log: log}
}

// ServeHTTP validates, authorizes, and answers one conversion-platform read.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "GET a goals report from this endpoint")
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
		h.fail(w, http.StatusUnauthorized, "an authenticated session or validated sharing capability is required")
		return
	}

	authorization, err := h.Authorize(r, site)
	if err != nil {
		var refusal *AuthorizationError
		if errors.As(err, &refusal) {
			h.fail(w, refusal.Status, refusal.Message)
			return
		}

		h.internal(w, "authorize", err)
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.internal(w, "open account", err)
		return
	}
	defer lease.Release() //nolint:errcheck // report failures are more useful than an unlock error

	engine := query.New(lease.Account.Reader())
	if h.Now != nil {
		engine.Now = h.Now
	}

	if err := h.serveRead(w, r, site, authorization, lease, engine); err != nil {
		var callerError *query.Error
		var goalError *Error
		switch {
		case errors.Is(err, ErrNotFound):
			h.fail(w, http.StatusNotFound, "the requested conversion resource does not exist")
		case errors.As(err, &callerError) || errors.As(err, &goalError):
			h.fail(w, http.StatusBadRequest, err.Error())
		default:
			h.internal(w, "read conversion dashboard", err)
		}
	}
}

// serveRead dispatches one already-authorized site request.
func (h *Handler) serveRead(w http.ResponseWriter, r *http.Request, site sites.Site, authorization Authorization, lease *accounts.Lease, engine *query.Engine) error {
	common, err := decodeReportRequest(r)
	if err != nil {
		return err
	}
	common.SiteID = site.ID
	common.Filters = append(append([]query.Filter(nil), authorization.PinnedFilters...), common.Filters...)
	if common.Timezone == "" {
		common.Timezone = site.Timezone
	}

	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/goals/report"):
		common.IncludeEmptyAutomatic = true
		result, err := Report(r.Context(), lease.Account.Reader(), engine, common)
		if err != nil {
			return err
		}
		h.write(w, http.StatusOK, result)

	case strings.HasSuffix(path, "/goals"):
		list, err := List(r.Context(), lease.Account.Reader(), site.ID)
		if err != nil {
			return err
		}
		h.write(w, http.StatusOK, map[string]any{"goals": list})

	case strings.HasSuffix(path, "/properties"):
		list, err := Allowed(r.Context(), lease.Account.Reader(), site.ID)
		if err != nil {
			return err
		}
		h.write(w, http.StatusOK, map[string]any{"properties": list})

	case strings.Contains(path, "/properties/") && strings.HasSuffix(path, "/report"):
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		result, err := PropertyReport(r.Context(), lease.Account.Reader(), engine, PropertyReportRequest{
			SiteID: site.ID, Name: r.PathValue("property"), DateRange: common.DateRange,
			Timezone: common.Timezone, Filters: common.Filters, Exact: common.Exact, Limit: limit,
		})
		if err != nil {
			return err
		}
		h.write(w, http.StatusOK, result)

	case strings.HasSuffix(path, "/funnels"):
		list, err := ListFunnels(r.Context(), lease.Account.Reader(), site.ID)
		if err != nil {
			return err
		}
		h.write(w, http.StatusOK, map[string]any{"funnels": list})

	case strings.Contains(path, "/funnels/") && strings.HasSuffix(path, "/report"):
		id, err := strconv.ParseInt(r.PathValue("funnel_id"), 10, 64)
		if err != nil || id < 1 {
			return invalid("a funnel id must be a positive whole number")
		}
		result, err := RunFunnel(r.Context(), lease.Account.Reader(), engine, FunnelRequest{
			FunnelID: id, DateRange: common.DateRange, Timezone: common.Timezone,
			Filters: common.Filters, Exact: common.Exact,
		})
		if err != nil {
			return err
		}
		if result.Funnel.SiteID != site.ID {
			return ErrNotFound
		}
		h.write(w, http.StatusOK, result)

	case strings.HasSuffix(path, "/journey"):
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var trail []JourneyAnchor
		if raw := strings.TrimSpace(r.URL.Query().Get("trail")); raw != "" {
			if err := json.Unmarshal([]byte(raw), &trail); err != nil {
				return invalid("trail must be a JSON array of anchors")
			}
		}
		anchor := r.URL.Query().Get("anchor")
		if anchor == "" {
			anchor = r.URL.Query().Get("page")
		}
		result, err := Journey(r.Context(), lease.Account.Reader(), engine, JourneyRequest{
			SiteID: site.ID, DateRange: common.DateRange, Timezone: common.Timezone,
			Page: r.URL.Query().Get("page"), Limit: limit, AnchorType: r.URL.Query().Get("anchor_type"),
			Anchor: anchor, Direction: r.URL.Query().Get("direction"), Trail: trail,
			Grouping: r.URL.Query().Get("grouping"), Filters: common.Filters, Exact: common.Exact,
		})
		if err != nil {
			return err
		}
		h.write(w, http.StatusOK, result)

	default:
		return ErrNotFound
	}
	return nil
}

// decodeReportRequest reads the compact query-string wire form. The date range
// and filters remain JSON because both already have strict JSON decoders and a
// second ad-hoc grammar here would accept different questions than /api/stats.
func decodeReportRequest(r *http.Request) (ReportRequest, error) {
	values := r.URL.Query()
	request := ReportRequest{}

	rawRange := strings.TrimSpace(values.Get("date_range"))
	if rawRange == "" {
		rawRange = `"28d"`
	}
	if err := json.Unmarshal([]byte(rawRange), &request.DateRange); err != nil {
		return ReportRequest{}, err
	}

	rawFilters := strings.TrimSpace(values.Get("filters"))
	if rawFilters != "" {
		if err := json.Unmarshal([]byte(rawFilters), &request.Filters); err != nil {
			return ReportRequest{}, err
		}
	}

	request.Timezone = strings.TrimSpace(values.Get("timezone"))
	request.Exact, _ = strconv.ParseBool(values.Get("exact"))

	return request, nil
}

// fail answers a caller's mistake with its reason.
func (h *Handler) fail(w http.ResponseWriter, status int, message string) {
	h.write(w, status, map[string]string{"error": message})
}

// internal logs private detail and gives the caller a stable failure sentence.
func (h *Handler) internal(w http.ResponseWriter, step string, err error) {
	if h.Log != nil {
		h.Log.Error("a goals report request failed", "step", step, "error", err)
	}

	h.fail(w, http.StatusInternalServerError, "the goals report could not be answered")
}

// write encodes one JSON response.
func (h *Handler) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil && h.Log != nil {
		h.Log.Error("a goals report response could not be written", "error", err)
	}
}
