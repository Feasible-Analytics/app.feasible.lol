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
	"database/sql"
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
// Overrides replace the value the server answers for a report dimension.
func ga4TestServer(t *testing.T, badMetric bool, overrides map[string]string) *httptest.Server {
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
		for name, value := range overrides {
			values[name] = value
		}
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
	ga4TestServer(t, false, nil)
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
	ga4TestServer(t, true, nil)
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

// TestAPIImportNamesMatchLiveCollection runs the API import once per mapping
// and checks each value lands on the name live collection stores for the same
// thing, so a report over imported and collected days has one row per browser,
// system and source.
func TestAPIImportNamesMatchLiveCollection(t *testing.T) {
	cases := []struct {
		dimension string
		value     string
		stored    string
		want      string
	}{
		{"sessionSource", "(direct)", "visit:source", ""},
		{"sessionSource", "google", "visit:source", "Google"},
		{"sessionSource", "m.facebook.com", "visit:source", "Facebook"},
		{"sessionSource", "en.wikipedia.org", "visit:source", "wikipedia.org"},
		{"sessionSource", "newsletter", "visit:source", "Newsletter"},
		{"sessionSource", "producthunt", "visit:source", "producthunt"},
		{"sessionMedium", "(none)", "visit:utm_medium", ""},
		{"sessionMedium", "organic", "visit:utm_medium", ""},
		{"sessionMedium", "referral", "visit:utm_medium", ""},
		{"sessionMedium", "cpc", "visit:utm_medium", "cpc"},
		{"sessionCampaignName", "(organic)", "visit:utm_campaign", ""},
		{"sessionCampaignName", "(referral)", "visit:utm_campaign", ""},
		{"sessionCampaignName", "(direct)", "visit:utm_campaign", ""},
		{"sessionCampaignName", "(not set)", "visit:utm_campaign", ""},
		{"sessionCampaignName", "(cross-network)", "visit:utm_campaign", ""},
		{"sessionDefaultChannelGroup", "organic search", "visit:channel", "Organic Search"},
		{"countryId", "us", "visit:country", "US"},
		{"countryId", "(not set)", "visit:country", ""},
		{"region", "(not set)", "visit:region", ""},
		{"city", "(not set)", "visit:city", ""},
		{"deviceCategory", "desktop", "visit:device", "Desktop"},
		{"deviceCategory", "mobile", "visit:device", "Mobile"},
		{"deviceCategory", "tablet", "visit:device", "Tablet"},
		{"deviceCategory", "smart tv", "visit:device", "TV"},
		{"browser", "Edge", "visit:browser", "Microsoft Edge"},
		{"browser", "Android Webview", "visit:browser", "Mobile App"},
		{"browser", "Safari (in-app)", "visit:browser", "Mobile App"},
		{"browser", "YaBrowser", "visit:browser", "Yandex Browser"},
		{"browser", "Opera Mini", "visit:browser", "Opera"},
		{"browser", "(not set)", "visit:browser", ""},
		{"operatingSystem", "Macintosh", "visit:os", "macOS"},
		{"operatingSystem", "Linux", "visit:os", "GNU/Linux"},
		{"operatingSystem", "(not set)", "visit:os", ""},
		{"hostName", "www.example.com", "event:hostname", "example.com"},
		{"hostName", "(not set)", "event:hostname", "(none)"},
		{"pagePath", "/café", "event:page", "/caf%C3%A9"},
		{"pagePath", "(not set)", "event:page", ""},
		{"landingPage", "/über", "visit:entry_page", "/%C3%BCber"},
		{"landingPage", "(not set)", "visit:entry_page", ""},
	}

	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	for _, tc := range cases {
		account := newAccount(t)
		ga4TestServer(t, false, map[string]string{tc.dimension: tc.value})

		app, _ := NewApp("id", "secret", "https://example.com")
		connection := &Connection{SiteID: 1, Provider: ProviderGA4, Property: "123", AccessToken: "valid", ExpiresAt: now.Add(time.Hour).Unix()}

		record, err := dataio.CreateImport(ctx, account.Writer(), 1, dataio.SourceGA4, "123", now)
		if err != nil {
			t.Fatal(err)
		}

		if err := app.GA4Import(ctx, account.Writer(), account.Intern, record, connection, from, to, time.UTC, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}

		got := storedValues(t, account.Reader(), record.ID, tc.stored)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s %q stored as %s %q, want %q", tc.dimension, tc.value, tc.stored, got, tc.want)
		}
	}
}

// storedValues lists the distinct values one dimension holds across the rows
// of an import that carry it.
func storedValues(t *testing.T, db *sql.DB, importID int64, dimension string) []string {
	t.Helper()

	column, ok := query.ImportedColumn(dimension)
	if !ok {
		t.Fatalf("%s is not an imported dimension", dimension)
	}

	table, ok := query.ImportedInterned(dimension)
	if !ok {
		t.Fatalf("%s has no dimension table", dimension)
	}

	bit, err := query.ImportedCoverage([]string{dimension})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query("SELECT DISTINCT COALESCE(d.value, '') FROM imported_rollups r "+
		"LEFT JOIN "+table.Table()+" d ON d.id = r."+column+" "+
		"WHERE r.import_id = ? AND (r.covered & ?) != 0 ORDER BY 1", importID, int64(bit))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return values
}
