//
// analytics.go
// Selecting a GA4 property and importing an explicit historical window.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/google"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// loadAnalyticsProperties provides the authenticated property picker. Discovery
// errors stay visible instead of making a successful OAuth grant look absent.
func (h *Handler) loadAnalyticsProperties(r *http.Request, account *accounts.Account, data *page) {
	if data.GA4 == nil || data.GA4.NeedsReconnect() {
		return
	}
	// Import progress refreshes this page frequently. A running migration has
	// already selected its property and must not spend quota listing it again.
	for _, record := range data.Imports {
		if record.Source == dataio.SourceGA4 && (record.Status == dataio.StatusPending || record.Status == dataio.StatusRunning) {
			return
		}
	}
	properties, err := h.Google.ListAnalyticsProperties(r.Context(), account.Writer(), data.GA4, h.now())
	if err != nil {
		data.AnalyticsPropertyError = err.Error()
		return
	}
	if len(properties) == 0 {
		data.AnalyticsPropertyError = tr(r, "auth.ga4.no_properties")
		return
	}
	data.AnalyticsProperties = properties
}

// analyticsImport validates the chosen property against Google and reserves the
// requested range atomically before enqueueing. Repeated submits and overlapping
// migrations cannot silently add a second copy of the same historical traffic.
func (h *Handler) analyticsImport(w http.ResponseWriter, r *http.Request, site sites.Site) {
	location, err := time.LoadLocation(site.Timezone)
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}
	from, fromErr := time.ParseInLocation("2006-01-02", r.PostFormValue("from"), location)
	to, toErr := time.ParseInLocation("2006-01-02", r.PostFormValue("to"), location)
	today := h.now().In(location).Format("2006-01-02")
	if fromErr != nil || toErr != nil || from.After(to) || from.Year() < 2005 || to.Format("2006-01-02") >= today {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.ga4.invalid_dates"))
		return
	}
	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer lease.Release() //nolint:errcheck // preserve the import result
	account := lease.Account
	connection, err := google.GetConnection(r.Context(), account.Reader(), site.ID, google.ProviderGA4)
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}
	if connection == nil || connection.NeedsReconnect() {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.google.error_not_connected"))
		return
	}
	available, err := h.Google.ListAnalyticsProperties(r.Context(), account.Writer(), connection, h.now())
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}
	property := strings.TrimSpace(r.PostFormValue("property"))
	found := false
	for _, candidate := range available {
		if candidate.Property == property {
			found = true
			break
		}
	}
	if !found {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.google.error_unknown_property"))
		return
	}
	if h.Jobs == nil {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.ga4.queue_unavailable"))
		return
	}
	// One INSERT guards and reserves the range, including active imports whose
	// final coverage is not known yet. Search data has its own separate tables.
	var importID int64
	err = account.Writer().QueryRowContext(r.Context(), `
 INSERT INTO imports (site_id, source, label, status, created_at, range_start, range_end)
 SELECT ?, 'ga4', ?, 'pending', ?, ?, ?
 WHERE NOT EXISTS (
  SELECT 1 FROM imports WHERE site_id = ? AND source != 'search_console'
  AND (status IN ('pending', 'running') OR
   (status = 'completed' AND range_start < ? AND range_end >= ?))
 ) AND NOT EXISTS (SELECT 1 FROM events WHERE site_id = ? AND timestamp < ?)
 AND NOT EXISTS (SELECT 1 FROM rollup_visitors WHERE site_id = ? AND grain IN (0, 1) AND bucket < ?)
 RETURNING id`, site.ID, "GA4 "+property+" · "+from.Format("2006-01-02")+" – "+to.Format("2006-01-02"),
		h.now().Unix(), from.Unix(), to.AddDate(0, 0, 1).Unix()-1, site.ID, to.AddDate(0, 0, 1).Unix(), from.Unix(),
		site.ID, to.AddDate(0, 0, 1).Unix(), site.ID, to.AddDate(0, 0, 1).Unix()).Scan(&importID)
	if errors.Is(err, sql.ErrNoRows) {
		h.redirect(w, r, site.Domain, "imports", "", tr(r, "auth.ga4.overlap"))
		return
	}
	if err != nil {
		h.redirect(w, r, site.Domain, "imports", "", err.Error())
		return
	}
	connection.Property = property
	err = google.SaveConnection(r.Context(), account.Writer(), *connection, h.now())
	if err == nil {
		_, err = h.Jobs.EnqueueOwned(r.Context(), site.AccountID, jobs.QueueImports, jobs.KindGA4Import,
			google.ImportArgs{AccountID: site.AccountID, SiteID: site.ID, ImportID: importID, Property: property, From: from.Unix(), To: to.Unix()},
			fmt.Sprintf("account-%d-ga4-import-%d", site.AccountID, importID))
	}
	if err != nil {
		failure := dataio.FailImport(r.Context(), account.Writer(), importID, err.Error(), h.now())
		h.redirect(w, r, site.Domain, "imports", "", errors.Join(err, failure).Error())
		return
	}
	h.redirect(w, r, site.Domain, "imports", tr(r, "auth.ga4.queued"), "")
}
