//
// google_test.go
// One grant per site, and a refusal that asks for a reconnect instead of retrying.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// newAccount opens a migrated account database in a temporary directory.
func newAccount(t *testing.T) *accounts.Account {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { manager.CloseAll() })

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	return account
}

// TestNoCredentialsHidesTheFeature is the behaviour an install with no Google
// application gets. It is not an error and it is not a broken button: the
// features are simply absent, and the process says so once at start-up.
func TestNoCredentialsHidesTheFeature(t *testing.T) {
	if _, ok := NewApp("", "", "https://example.com"); ok {
		t.Error("an empty client was reported as configured")
	}

	if _, ok := NewApp("id", "", "https://example.com"); ok {
		t.Error("a client with no secret was reported as configured")
	}

	app, ok := NewApp("id", "secret", "https://example.com/")
	if !ok {
		t.Fatal("a complete client was reported as unconfigured")
	}

	if app.RedirectURL != "https://example.com"+CallbackPath {
		t.Fatalf("redirect URI = %q, want it derived from the base URL", app.RedirectURL)
	}
}

// TestTokensAreStoredPerSite is the fix for a real incident. On an incumbent's
// self-hosted build, connecting a second site with the same Google account
// invalidated the first site's refresh token and it was never root-caused. The
// shape that allows that is one token row shared by every site a Google account
// touches; here the row is keyed on the site and the provider, so two sites
// hold two independent grants.
func TestTokensAreStoredPerSite(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	first := Connection{SiteID: 1, AccountID: 1, Provider: ProviderGA4,
		GoogleEmail: "owner@example.com", Property: "111", RefreshToken: "refresh-one", Status: StatusConnected}

	second := Connection{SiteID: 2, AccountID: 1, Provider: ProviderGA4,
		GoogleEmail: "owner@example.com", Property: "222", RefreshToken: "refresh-two", Status: StatusConnected}

	for _, connection := range []Connection{first, second} {
		if err := SaveConnection(ctx, account.Writer(), connection, now); err != nil {
			t.Fatal(err)
		}
	}

	stored, err := GetConnection(ctx, account.Reader(), 1, ProviderGA4)
	if err != nil {
		t.Fatal(err)
	}

	if stored.RefreshToken != "refresh-one" {
		t.Fatalf("site 1 holds %q — connecting a second site overwrote the first site's grant", stored.RefreshToken)
	}

	// Disconnecting one site must not reach the other.
	if err := DeleteConnection(ctx, account.Writer(), 2, ProviderGA4); err != nil {
		t.Fatal(err)
	}

	stored, err = GetConnection(ctx, account.Reader(), 1, ProviderGA4)
	if err != nil {
		t.Fatal(err)
	}

	if stored == nil || stored.RefreshToken != "refresh-one" {
		t.Fatal("disconnecting one site removed another site's grant")
	}

	// The two providers are separate grants too: a customer may want their
	// history imported without handing over their search data.
	if search, err := GetConnection(ctx, account.Reader(), 1, ProviderSearchConsole); err != nil || search != nil {
		t.Fatalf("a Search Console grant appeared from an Analytics one: %v %v", search, err)
	}
}

// TestInvalidGrantAsksForAReconnect covers the only refresh failure a retry
// cannot fix. Google has stopped honouring the grant, so the connection is
// marked as needing a reconnect and the customer gets a button — rather than a
// nightly job that fails identically with nobody watching.
func TestInvalidGrantAsksForAReconnect(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer server.Close()

	original := TokenURL
	TokenURL = server.URL
	defer func() { TokenURL = original }()

	app, _ := NewApp("id", "secret", "https://example.com")

	connection := Connection{SiteID: 1, AccountID: 1, Provider: ProviderGA4,
		RefreshToken: "stale", Status: StatusConnected}

	if err := SaveConnection(ctx, account.Writer(), connection, now); err != nil {
		t.Fatal(err)
	}

	_, err := app.AccessToken(ctx, account.Writer(), &connection, now)
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("refresh error = %v, want ErrInvalidGrant", err)
	}

	stored, err := GetConnection(ctx, account.Reader(), 1, ProviderGA4)
	if err != nil {
		t.Fatal(err)
	}

	if !stored.NeedsReconnect() {
		t.Fatal("the connection was left looking healthy after Google refused it")
	}

	if stored.Failure == "" {
		t.Fatal("no reason was recorded, so the settings page has nothing to show")
	}
}

// TestRefreshKeepsTheRefreshToken covers a quiet way to disconnect a site. A
// refresh response usually omits the refresh token, and overwriting the stored
// one with an empty string would break the connection on its first successful
// refresh.
func TestRefreshKeepsTheRefreshToken(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"fresh","expires_in":3600,"scope":"` + ScopeAnalytics + `"}`))
	}))
	defer server.Close()

	original := TokenURL
	TokenURL = server.URL
	defer func() { TokenURL = original }()

	app, _ := NewApp("id", "secret", "https://example.com")

	token, err := app.Refresh(ctx, "keep-me", now)
	if err != nil {
		t.Fatal(err)
	}

	if token.RefreshToken != "keep-me" {
		t.Fatalf("refresh token = %q, want the stored one to survive a response that omitted it", token.RefreshToken)
	}

	if token.AccessToken != "fresh" {
		t.Fatalf("access token = %q", token.AccessToken)
	}

	if !token.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expiry = %v, want an hour from now", token.ExpiresAt)
	}
}

// TestSearchConsoleDelayIsStated keeps the notice in the binary. Google's own
// data is a day to a day and a half behind, so today and usually yesterday are
// legitimately empty — and every new customer files the same bug if nothing on
// the page says so.
func TestSearchConsoleDelayIsStated(t *testing.T) {
	if SearchConsoleDelay < 24*time.Hour {
		t.Error("the stated delay is shorter than Google's own")
	}

	if SearchConsoleDelayNotice == "" {
		t.Error("there is no sentence to show, so the delay is invisible to the customer")
	}
}

// TestMonthsBetweenIsResumable covers the cursor arithmetic. Restarting a
// half-finished import from the beginning would write every earlier month
// twice, and no later check could tell which copy was the duplicate.
func TestMonthsBetweenIsResumable(t *testing.T) {
	from := time.Date(2025, 11, 15, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)

	months := monthsBetween(from, to)

	if len(months) != 4 {
		t.Fatalf("%d months, want 4", len(months))
	}

	if months[0].start != "2025-11-15" {
		t.Errorf("the first month starts at %q, want the range's own start", months[0].start)
	}

	if months[len(months)-1].end != "2026-02-03" {
		t.Errorf("the last month ends at %q, want the range's own end", months[len(months)-1].end)
	}

	// The labels have to sort the way the cursor compares them, or a resume
	// re-runs work it has already done.
	for i := 1; i < len(months); i++ {
		if months[i-1].label >= months[i].label {
			t.Fatalf("labels %q and %q do not sort forwards", months[i-1].label, months[i].label)
		}
	}
}
