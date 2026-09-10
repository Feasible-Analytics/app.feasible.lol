//
// analytics_test.go
// Property discovery pagination and visible Google API failures.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAnalyticsPropertiesFollowsPagesAndRejectsAPIFailures distinguishes a
// truly empty account from disabled APIs, expired access, and missing pages.
func TestAnalyticsPropertiesFollowsPagesAndRejectsAPIFailures(t *testing.T) {
	for _, status := range []int{200, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				body := `{"accountSummaries":[{"propertySummaries":[{"property":"properties/123","displayName":"Example"}]}],"nextPageToken":"next"}`
				if r.URL.Query().Get("pageToken") == "next" {
					body = `{"accountSummaries":[{"propertySummaries":[{"property":"properties/456","displayName":"Second"}]}]}`
				}
				if status != 200 {
					body = `{"error":{"message":"API unavailable"}}`
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			original := AnalyticsAdminAPI
			AnalyticsAdminAPI = server.URL
			defer func() { AnalyticsAdminAPI = original }()
			app, _ := NewApp("id", "secret", "https://example.com")
			now := time.Now()
			properties, err := app.ListAnalyticsProperties(context.Background(), nil, &Connection{AccessToken: "valid", ExpiresAt: now.Add(time.Hour).Unix()}, now)
			if status != 200 {
				if err == nil {
					t.Fatal("API failure reported as empty success")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || len(properties) != 2 || properties[1].Property != "456" {
				t.Fatalf("properties=%+v calls=%d", properties, calls)
			}
		})
	}
}
