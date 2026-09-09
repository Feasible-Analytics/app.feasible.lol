//
// searchconsole_test.go
// Offering only properties a grant can read, and guessing the obvious one.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// connected builds a grant whose access token is still valid, so a test reaches
// the API call rather than the refresh path.
func connected(t *testing.T, now time.Time) *Connection {
	t.Helper()

	return &Connection{
		SiteID: 1, AccountID: 1, Provider: ProviderSearchConsole,
		AccessToken: "valid", ExpiresAt: now.Add(time.Hour).Unix(), Status: StatusConnected,
	}
}

// TestUnverifiedPropertiesAreNotOffered covers a setup step somebody walks
// through once. Google lists properties the grant can see but cannot read data
// from; offering one produces a connection that authorises cleanly and then
// imports nothing, with no error anywhere to explain it.
func TestUnverifiedPropertiesAreNotOffered(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer valid" {
			t.Errorf("Authorization = %q, want the stored access token", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"siteEntry":[
			{"siteUrl":"sc-domain:example.com","permissionLevel":"siteOwner"},
			{"siteUrl":"https://other.test/","permissionLevel":"siteUnverifiedUser"},
			{"siteUrl":"https://team.test/","permissionLevel":"siteFullUser"}]}`)); err != nil {
			t.Errorf("write sites response: %v", err)
		}
	}))
	defer server.Close()

	original := SearchAPI
	SearchAPI = server.URL
	defer func() { SearchAPI = original }()

	app, _ := NewApp("id", "secret", "https://example.com")

	properties, err := app.ListSearchProperties(ctx, account.Writer(), connected(t, now), now)
	if err != nil {
		t.Fatal(err)
	}

	if len(properties) != 2 {
		t.Fatalf("%d properties offered, want the two this grant can read", len(properties))
	}

	for _, property := range properties {
		if property.Permission == unverifiedPermission {
			t.Errorf("%q was offered even though the grant cannot read it", property.URL)
		}
	}
}

// TestGoogleErrorIsReported keeps a refused list from looking like an empty
// one. An account with no properties and an account we were not allowed to ask
// need different sentences on screen.
func TestGoogleErrorIsReported(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"error":{"code":403,"message":"insufficient permissions"}}`)); err != nil {
			t.Errorf("write error response: %v", err)
		}
	}))
	defer server.Close()

	original := SearchAPI
	SearchAPI = server.URL
	defer func() { SearchAPI = original }()

	app, _ := NewApp("id", "secret", "https://example.com")

	if _, err := app.ListSearchProperties(ctx, account.Writer(), connected(t, now), now); err == nil {
		t.Fatal("a refusal came back as an empty list, which reads as an account with no properties")
	}
}

// TestMatchSearchPropertyPrefersTheDomainProperty is the guess that saves
// somebody reading a list of every property their Google account can see. A
// domain property wins because it covers every subdomain and scheme, which is
// what a site owner almost always wants.
func TestMatchSearchPropertyPrefersTheDomainProperty(t *testing.T) {
	available := []SearchProperty{
		{URL: "https://www.example.com/", Permission: "siteOwner"},
		{URL: "sc-domain:example.com", Permission: "siteOwner"},
		{URL: "sc-domain:unrelated.test", Permission: "siteOwner"},
	}

	matched, ok := MatchSearchProperty("example.com", available)
	if !ok {
		t.Fatal("no property matched a domain that is plainly in the list")
	}

	if matched.URL != "sc-domain:example.com" {
		t.Fatalf("matched %q, want the domain property", matched.URL)
	}
}

// TestMatchSearchPropertyFallsBackToAPrefix covers the common case of an owner
// who only ever verified the URL prefix.
func TestMatchSearchPropertyFallsBackToAPrefix(t *testing.T) {
	available := []SearchProperty{{URL: "https://www.example.com/", Permission: "siteOwner"}}

	matched, ok := MatchSearchProperty("example.com", available)
	if !ok || matched.URL != "https://www.example.com/" {
		t.Fatalf("matched %v %v, want the www prefix to match the bare domain", matched, ok)
	}
}

// TestMatchSearchPropertyRefusesToGuess is what sends somebody to the picker.
// Importing another site's search history because the names looked similar is
// the one outcome worth several extra clicks to avoid.
func TestMatchSearchPropertyRefusesToGuess(t *testing.T) {
	available := []SearchProperty{
		{URL: "sc-domain:notexample.com", Permission: "siteOwner"},
		{URL: "https://example.com.evil.test/", Permission: "siteOwner"},
	}

	if matched, ok := MatchSearchProperty("example.com", available); ok {
		t.Fatalf("guessed %q for a domain that is not in the list", matched.URL)
	}

	if _, ok := MatchSearchProperty("", available); ok {
		t.Fatal("an empty domain matched something")
	}
}

// TestAccountEmailNeverBlocksAConnection covers a grant issued without the
// address scope. The address is shown beside the property and nothing depends
// on it, so a failure leaves it blank rather than failing the connection.
func TestAccountEmailNeverBlocksAConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	original := UserInfoAPI
	UserInfoAPI = server.URL
	defer func() { UserInfoAPI = original }()

	app, _ := NewApp("id", "secret", "https://example.com")

	if email := app.AccountEmail(context.Background(), "token"); email != "" {
		t.Fatalf("email = %q, want an empty answer rather than a failure", email)
	}
}

// TestBackfillWindowStopsBeforeToday keeps the last days of a first import from
// reporting zero rows written. Google has not published them yet, and an import
// that ends on several empty days reads as a broken connection.
func TestBackfillWindowStopsBeforeToday(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	from, to := BackfillWindow(now)

	if !to.Before(now.Add(-24 * time.Hour)) {
		t.Errorf("the window ends at %v, which is inside the period Google has not published", to)
	}

	if !from.Before(to.AddDate(-1, 0, 0)) {
		t.Errorf("the window starts at %v, which is less than the year Search Console still holds", from)
	}
}

// TestScopesAskForTheAddress keeps the settings screen able to say which Google
// account a connection belongs to. Somebody with several logins always asks.
func TestScopesAskForTheAddress(t *testing.T) {
	for provider, wanted := range map[string]string{
		ProviderSearchConsole: ScopeSearchConsole,
		ProviderGA4:           ScopeAnalytics,
	} {
		scopes := ScopesFor(provider)

		if !strings.Contains(scopes, wanted) {
			t.Errorf("%s scopes %q do not include its own read scope", provider, scopes)
		}

		if !strings.Contains(scopes, ScopeEmail) {
			t.Errorf("%s scopes %q do not ask for the address", provider, scopes)
		}
	}
}
