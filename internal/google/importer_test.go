//
// importer_test.go
// GA4 pagination, replay, and the historical totals used by dashboards.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// ga4TestServer returns two short pages per shape, forcing the importer to use
// Google's rowCount rather than assuming a page shorter than its limit is final.
func ga4TestServer(t *testing.T, badMetric bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer valid" {
			t.Error("missing grant")
		}
		var request struct {
			Dimensions []struct {
				Name string `json:"name"`
			} `json:"dimensions"`
			Metrics []struct {
				Name string `json:"name"`
			} `json:"metrics"`
			Offset string `json:"offset"`
			Ranges []struct {
				Start string `json:"startDate"`
				End   string `json:"endDate"`
			} `json:"dateRanges"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		for _, metric := range request.Metrics {
			if metric.Name == "bounces" {
				t.Error("unsupported bounces metric")
			}
		}
		date := strings.ReplaceAll(request.Ranges[0].Start, "-", "")
		if request.Offset == "1" {
			date = strings.ReplaceAll(request.Ranges[0].End, "-", "")
		}
		values := map[string]string{"date": date, "hostName": "example.com", "pagePath": "/guide", "sessionSource": "Google", "sessionMedium": "organic", "sessionCampaignName": "spring", "sessionDefaultChannelGroup": "Organic Search", "countryId": "US", "region": "Oregon", "city": "Portland", "deviceCategory": "desktop", "browser": "Chrome", "operatingSystem": "Macintosh", "landingPage": "/guide"}
		dims := []map[string]string{}
		for _, dimension := range request.Dimensions {
			dims = append(dims, map[string]string{"value": values[dimension.Name]})
		}
		metrics := []map[string]string{{"value": "10"}, {"value": "12"}, {"value": "30"}, {"value": "8"}, {"value": "12.5"}}
		if badMetric && request.Offset == "1" {
			metrics[0]["value"] = "NaN"
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"rowCount": 2, "rows": []any{map[string]any{"dimensionValues": dims, "metricValues": metrics}}}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	original := AnalyticsAPI
	AnalyticsAPI = server.URL
	t.Cleanup(func() { AnalyticsAPI = original })
	return server
}

// TestGA4ReplayAndDashboardTotals verifies a restarted import replays all pages
// without duplicating daily totals, and the normal dashboard query reads them.
func TestGA4ReplayAndDashboardTotals(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	ga4TestServer(t, false)
	app, _ := NewApp("id", "secret", "https://example.com")
	connection := &Connection{SiteID: 1, Provider: ProviderGA4, Property: "123", AccessToken: "valid", ExpiresAt: now.Add(time.Hour).Unix()}
	record, err := dataio.CreateImport(ctx, account.Writer(), 1, dataio.SourceGA4, "123", now)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	for attempt := 0; attempt < 2; attempt++ {
		// Simulate the stale cursor and row count a restarted worker reads from disk.
		if attempt == 1 {
			record.Cursor = "2026-08"
			record.RowsWritten = 16
		}
		if err := app.GA4Import(ctx, account.Writer(), account.Intern, record, connection, from, to, time.UTC, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}
		stored, err := dataio.GetImportByID(ctx, account.Reader(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != dataio.StatusCompleted || stored.RowsWritten != 16 || len(stored.Dimensions) == 0 {
			t.Fatalf("incomplete import: %+v", stored)
		}
		engine := query.New(account.Reader())
		result, err := engine.Run(ctx, query.Query{SiteIDs: []int64{1}, Metrics: []string{"visitors", "visits", "pageviews", "bounce_rate"}, Timezone: "UTC", DateRange: query.DateRange{Preset: query.RangeCustom, Start: from, End: to.AddDate(0, 0, 1).Add(-time.Second)}})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Results) != 1 {
			t.Fatalf("results: %+v", result.Results)
		}
		for i, want := range []float64{20, 24, 60, 100.0 / 3} {
			if got := result.Results[0].Metrics[i]; got < want-0.01 || got > want+0.01 {
				t.Fatalf("attempt %d metric %d = %v, want %v", attempt, i, got, want)
			}
		}
	}
}

// TestGA4RejectsInvalidMetrics ensures corrupt API values cannot be silently
// converted into zero visitors in an otherwise successful historical import.
func TestGA4RejectsInvalidMetrics(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	ga4TestServer(t, true)
	now := time.Now()
	record, err := dataio.CreateImport(ctx, account.Writer(), 1, dataio.SourceGA4, "123", now)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := NewApp("id", "secret", "https://example.com")
	err = app.GA4Import(ctx, account.Writer(), account.Intern, record, &Connection{Property: "123", AccessToken: "valid", ExpiresAt: now.Add(time.Hour).Unix()}, now, now, time.UTC, func() time.Time { return now })
	if err == nil || !strings.Contains(err.Error(), "invalid totalUsers") {
		t.Fatalf("error = %v", err)
	}
}
