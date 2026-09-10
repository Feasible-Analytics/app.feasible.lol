//
// analytics_test.go
// GA4 property validation, date reservations, and the settings form.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/google"
)

// TestAnalyticsSelectionValidatesAndReservesHistory exercises the customer form
// through the settings router, including forged properties and double submits.
func TestAnalyticsSelectionValidatesAndReservesHistory(t *testing.T) {
	for _, scenario := range []string{"valid", "unknown_property", "reversed_dates", "future_dates", "overlap", "native_overlap", "no_connection"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			handler, manager := newHandler(t)
			handler.Google, _ = google.NewApp("id", "secret", "https://example.com")
			account, err := manager.Open(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write([]byte(`{"accountSummaries":[{"propertySummaries":[{"property":"properties/123","displayName":"Example property"}]}]}`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			original := google.AnalyticsAdminAPI
			google.AnalyticsAdminAPI = server.URL
			defer func() { google.AnalyticsAdminAPI = original }()
			now := handler.now()
			if scenario != "no_connection" {
				if err := google.SaveConnection(ctx, account.Writer(), google.Connection{SiteID: 1, AccountID: 1, Provider: google.ProviderGA4, AccessToken: "valid", ExpiresAt: now.Add(time.Hour).Unix()}, now); err != nil {
					t.Fatal(err)
				}
				page := get(t, handler, "/settings/sites/example.com/imports")
				if !strings.Contains(page.Body.String(), "Example property") || !strings.Contains(page.Body.String(), "ga4-from") {
					t.Fatal("missing Analytics property/date form")
				}
			}
			values := url.Values{"provider": {"ga4"}, "property": {"123"}, "from": {"2026-08-01"}, "to": {"2026-08-02"}}
			switch scenario {
			case "unknown_property":
				values.Set("property", "456")
			case "reversed_dates":
				values.Set("from", "2026-08-03")
			case "future_dates":
				values.Set("to", "2099-01-01")
			case "overlap":
				record, err := dataio.CreateImport(ctx, account.Writer(), 1, dataio.SourcePlausible, "existing", now)
				if err != nil {
					t.Fatal(err)
				}
				if err := dataio.CompleteImport(ctx, account.Writer(), record.ID, nil, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC).Unix(), 0, now); err != nil {
					t.Fatal(err)
				}
			case "native_overlap":
				if _, err := account.Writer().ExecContext(ctx, "INSERT INTO rollup_visitors (site_id,grain,bucket,dimension,value_id) VALUES (1,0,?,0,0)", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix()); err != nil {
					t.Fatal(err)
				}
			}
			handler.ServeHTTP(httptest.NewRecorder(), postForm(t, "/settings/sites/example.com/google/property", values))
			// Repeating a valid request must leave exactly one pending import.
			if scenario == "valid" {
				handler.ServeHTTP(httptest.NewRecorder(), postForm(t, "/settings/sites/example.com/google/property", values))
			}
			records, err := dataio.ListImports(ctx, account.Reader(), 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, record := range records {
				if record.Source == dataio.SourceGA4 {
					count++
				}
			}
			want := 0
			if scenario == "valid" {
				want = 1
			}
			if count != want {
				t.Fatalf("GA4 imports=%d want %d: %+v", count, want, records)
			}
		})
	}
}
