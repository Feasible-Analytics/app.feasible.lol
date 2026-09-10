//
// searchconsole.go
// Listing the Search Console properties a grant can read, and picking one.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// UserInfoAPI answers the address of the Google account behind a grant. It is a
// variable for the same reason the other endpoints are: a test points the whole
// package at a local server.
var UserInfoAPI = "https://www.googleapis.com/oauth2/v3/userinfo"

// SearchConsoleRetention is how far back Search Console will answer for.
// A backfill asks for exactly this and no more, because Google silently returns
// nothing for older days and a progress bar that crawls through empty months is
// indistinguishable from one that is broken.
const SearchConsoleRetention = 16 * 30 * 24 * time.Hour

// SearchConsoleRefreshDays is how many recent days the nightly job re-fetches.
//
// Google revises its own figures for several days after the fact, so a job that
// only ever fetched yesterday would leave every day permanently holding the
// first, lowest number Google gave. The day rows upsert, so re-fetching costs
// one request each and cannot double-count.
const SearchConsoleRefreshDays = 5

// unverifiedPermission is the level Google reports for a property the grant can
// see listed but cannot read data from. Offering one in the picker produces a
// connection that authorises cleanly and then imports nothing.
const unverifiedPermission = "siteUnverifiedUser"

// SearchProperty is one Search Console property a grant may choose.
type SearchProperty struct {
	// URL is the property identifier Google's API takes: either a URL prefix
	// such as "https://example.com/" or a domain property, "sc-domain:example.com".
	URL string `json:"url"`

	// Permission is Google's own permission level, carried so the picker can
	// say why a property it can see is not offered.
	Permission string `json:"permission"`
}

// searchSitesResponse is the wire form of the property list.
type searchSitesResponse struct {
	SiteEntry []struct {
		SiteURL         string `json:"siteUrl"`
		PermissionLevel string `json:"permissionLevel"`
	} `json:"siteEntry"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ListSearchProperties reads every Search Console property this grant can read
// data from, most likely first.
//
// Properties the grant cannot read are dropped rather than shown greyed out.
// The list is a setup step somebody walks through once, and an option that
// exists but cannot be chosen is a support question in waiting.
func (a *App) ListSearchProperties(ctx context.Context, db *sql.DB, connection *Connection, now time.Time) ([]SearchProperty, error) {
	token, err := a.AccessToken(ctx, db, connection, now)
	if err != nil {
		return nil, err
	}

	var parsed searchSitesResponse
	if err := a.getJSON(ctx, SearchAPI+"/webmasters/v3/sites", token, &parsed); err != nil {
		return nil, err
	}

	if parsed.Error != nil {
		return nil, fmt.Errorf("google answered: %s", parsed.Error.Message)
	}

	properties := make([]SearchProperty, 0, len(parsed.SiteEntry))

	for _, entry := range parsed.SiteEntry {
		if entry.PermissionLevel == unverifiedPermission {
			continue
		}

		properties = append(properties, SearchProperty{URL: entry.SiteURL, Permission: entry.PermissionLevel})
	}

	return properties, nil
}

// userInfoResponse is the wire form of the address lookup.
type userInfoResponse struct {
	Email string `json:"email"`
}

// AccountEmail reads which Google account authorised a grant, for display.
//
// A failure is not an error worth stopping a connection over: the address is
// shown beside the property on the settings screen and nothing depends on it,
// so a grant issued without the address scope simply shows no address.
func (a *App) AccountEmail(ctx context.Context, token string) string {
	var parsed userInfoResponse
	if err := a.getJSON(ctx, UserInfoAPI, token, &parsed); err != nil {
		return ""
	}

	return parsed.Email
}

// MatchSearchProperty picks the property a site most likely means.
//
// Somebody connecting analytics for one domain should not have to read a list
// of every property their Google account can see to find the one that matches
// the site they are already looking at. A domain property wins over a URL
// prefix because it covers every subdomain and scheme, which is what a site
// owner almost always wants. No match returns false, and the picker then asks.
func MatchSearchProperty(domain string, properties []SearchProperty) (SearchProperty, bool) {
	wanted := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "www.")
	if wanted == "" {
		return SearchProperty{}, false
	}

	var prefix SearchProperty
	var havePrefix bool

	for _, property := range properties {
		if strings.EqualFold(property.URL, "sc-domain:"+wanted) {
			return property, true
		}

		if havePrefix {
			continue
		}

		if host := propertyHost(property.URL); host != "" && host == wanted {
			prefix, havePrefix = property, true
		}
	}

	return prefix, havePrefix
}

// propertyHost reads the hostname out of a URL-prefix property, without the
// leading "www." so that it compares against a site domain the same way the
// referrer resolver compares hosts.
func propertyHost(property string) string {
	if strings.HasPrefix(property, "sc-domain:") {
		return strings.TrimPrefix(strings.ToLower(property), "sc-domain:")
	}

	rest := property
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(strings.ToLower(rest), scheme) {
			rest = rest[len(scheme):]
			break
		}
	}

	host, _, _ := strings.Cut(rest, "/")

	return strings.TrimPrefix(strings.ToLower(host), "www.")
}

// SearchConsoleThrough is the last day Google is likely to have figures for.
//
// Asking for anything later is not an error — Google answers with no rows — but
// it makes an import report zero rows written for its final days, which reads
// as a broken connection rather than as data that has not been published yet.
func SearchConsoleThrough(now time.Time) time.Time {
	return now.UTC().Add(-SearchConsoleDelay)
}

// BackfillWindow is the range a first import asks for: everything Search
// Console still holds, up to the last day it has probably published.
func BackfillWindow(now time.Time) (from, to time.Time) {
	to = SearchConsoleThrough(now)

	return to.Add(-SearchConsoleRetention), to
}

// getJSON performs one authorised GET and decodes the body.
func (a *App) getJSON(ctx context.Context, endpoint, token string, into any) (err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("google: build request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+token)

	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("google: %s could not be reached: %w", endpoint, err)
	}
	defer closeResource(response.Body, &err, "response")

	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("google: reading %s: %w", endpoint, err)
	}

	// HTTP failures must remain failures: decoding Google's error object into a
	// property-list struct would otherwise report an empty successful account.
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		return fmt.Errorf("google: API answered %d: %s", response.StatusCode, failure.Error.Message)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("google: %s did not answer with JSON: %w", endpoint, err)
	}

	return nil
}
