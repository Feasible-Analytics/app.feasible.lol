//
// analytics.go
// Listing the GA4 properties available to a read-only grant.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AnalyticsAdminAPI is injectable so property discovery can be tested offline.
var AnalyticsAdminAPI = "https://analyticsadmin.googleapis.com"

// AnalyticsProperty identifies a readable GA4 property without guessing from a
// site's domain: a property may collect several websites or mobile apps.
type AnalyticsProperty struct {
	Property    string `json:"property"`
	DisplayName string `json:"displayName"`
}

// ListAnalyticsProperties follows every account-summary page using the existing
// analytics.readonly grant. Only Google-returned numeric property IDs are offered.
func (a *App) ListAnalyticsProperties(ctx context.Context, db *sql.DB, connection *Connection, now time.Time) ([]AnalyticsProperty, error) {
	token, err := a.AccessToken(ctx, db, connection, now)
	if err != nil {
		return nil, err
	}
	var properties []AnalyticsProperty
	seen := map[string]bool{}
	pageToken := ""
	for {
		var response struct {
			Accounts []struct {
				Properties []AnalyticsProperty `json:"propertySummaries"`
			} `json:"accountSummaries"`
			Next string `json:"nextPageToken"`
		}
		endpoint := AnalyticsAdminAPI + "/v1beta/accountSummaries?pageSize=200&pageToken=" + url.QueryEscape(pageToken)
		if err := a.getJSON(ctx, endpoint, token, &response); err != nil {
			return nil, err
		}
		for _, account := range response.Accounts {
			for _, property := range account.Properties {
				id, ok := strings.CutPrefix(property.Property, "properties/")
				if _, err := strconv.ParseUint(id, 10, 64); !ok || err != nil {
					return nil, fmt.Errorf("google: invalid Analytics property resource")
				}
				property.Property = id
				properties = append(properties, property)
			}
		}
		if response.Next == "" {
			return properties, nil
		}
		if seen[response.Next] {
			return nil, fmt.Errorf("google: repeated Analytics property page")
		}
		seen[response.Next] = true
		pageToken = response.Next
	}
}
