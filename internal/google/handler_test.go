//
// handler_test.go
// Four answers to one request, and a share that is told none of them.
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
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// testSite is the one site every handler assertion below reads.
var testSite = sites.Site{ID: 1, AccountID: 1, TeamID: 1, Domain: "example.com", Timezone: "UTC"}

// newHandler builds a handler over a migrated account database, with the site
// already in the routing snapshot.
func newHandler(t *testing.T, available bool) (*Handler, *accounts.Account) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	cache := sites.NewEmpty()
	cache.Set(testSite)

	handler := &Handler{
		Sites: cache, Accounts: manager, Available: available,
		Authorize: func(*http.Request, sites.Site) error { return nil },
		Now:       func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) },
	}

	return handler, account
}

// ask runs one report request and decodes the answer.
func ask(t *testing.T, handler *Handler, query string) (*httptest.ResponseRecorder, SearchReport) {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "/api/sites/example.com/search-console/report?"+query, nil)
	request.SetPathValue("domain", "example.com")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var report SearchReport
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode report: %v (%s)", err, recorder.Body.String())
		}
	}

	return recorder, report
}

// TestAnInstallWithNoCredentialsSaysSo is what removes the card from a
// self-hosted build that can never answer. A connect button leading to
// invalid_client is worse than no card at all.
func TestAnInstallWithNoCredentialsSaysSo(t *testing.T) {
	handler, _ := newHandler(t, false)

	recorder, report := ask(t, handler, "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	if report.Status != StatusUnavailable {
		t.Errorf("status = %q, want %q", report.Status, StatusUnavailable)
	}
}

// TestAnUnconnectedSiteGetsAButton separates two answers that look identical
// from a distance: a feature this install cannot offer, and one this site has
// simply not turned on.
func TestAnUnconnectedSiteGetsAButton(t *testing.T) {
	handler, _ := newHandler(t, true)

	_, report := ask(t, handler, "")

	if report.Status != StatusNotConnected {
		t.Errorf("status = %q, want %q", report.Status, StatusNotConnected)
	}
}

// TestAGrantWithNoPropertyAsksForOne covers the half-finished setup. The
// authorisation worked and nobody has said which property to read, which is a
// different sentence from "not connected" and a different button.
func TestAGrantWithNoPropertyAsksForOne(t *testing.T) {
	handler, account := newHandler(t, true)
	now := time.Unix(1_800_000_000, 0)

	connection := Connection{SiteID: 1, AccountID: 1, Provider: ProviderSearchConsole,
		RefreshToken: "refresh", Status: StatusConnected}

	if err := SaveConnection(context.Background(), account.Writer(), connection, now); err != nil {
		t.Fatal(err)
	}

	_, report := ask(t, handler, "")

	if report.Status != StatusNoProperty {
		t.Errorf("status = %q, want %q", report.Status, StatusNoProperty)
	}
}

// TestAConnectedSiteGetsItsRows is the ordinary answer, and it carries the
// property so a site with both a domain property and a URL prefix can tell
// which one produced these numbers.
func TestAConnectedSiteGetsItsRows(t *testing.T) {
	ctx := context.Background()
	handler, account := newHandler(t, true)
	now := time.Unix(1_800_000_000, 0)

	connection := Connection{SiteID: 1, AccountID: 1, Provider: ProviderSearchConsole,
		Property: "sc-domain:example.com", RefreshToken: "refresh", Status: StatusConnected}

	if err := SaveConnection(ctx, account.Writer(), connection, now); err != nil {
		t.Fatal(err)
	}

	seedSearch(t, account.Writer(), 1,
		day{timestamp: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC).Unix(),
			query: "widgets", clicks: 4, impressions: 40, position: 3},
	)

	_, report := ask(t, handler, "")

	if report.Status != StatusReady {
		t.Fatalf("status = %q, want %q", report.Status, StatusReady)
	}

	if len(report.Rows) != 1 || report.Rows[0].Value != "widgets" {
		t.Fatalf("rows = %v", report.Rows)
	}

	if report.Property != "sc-domain:example.com" {
		t.Errorf("property = %q, want the one these figures came from", report.Property)
	}

	if report.UpdatedThrough != "2026-09-05" {
		t.Errorf("updated through %q, want the last stored day", report.UpdatedThrough)
	}
}

// TestAnExpiredGrantStillShowsWhatWeHave keeps stored rows on screen while
// saying they have stopped being topped up. Hiding true numbers because the
// grant lapsed would look like the history was lost.
func TestAnExpiredGrantStillShowsWhatWeHave(t *testing.T) {
	ctx := context.Background()
	handler, account := newHandler(t, true)
	now := time.Unix(1_800_000_000, 0)

	connection := Connection{SiteID: 1, AccountID: 1, Provider: ProviderSearchConsole,
		Property: "sc-domain:example.com", RefreshToken: "refresh", Status: StatusConnected}

	if err := SaveConnection(ctx, account.Writer(), connection, now); err != nil {
		t.Fatal(err)
	}

	if err := MarkNeedsReconnect(ctx, account.Writer(), 1, ProviderSearchConsole, "revoked", now); err != nil {
		t.Fatal(err)
	}

	seedSearch(t, account.Writer(), 1,
		day{timestamp: time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC).Unix(),
			query: "widgets", clicks: 4, impressions: 40, position: 3},
	)

	_, report := ask(t, handler, "")

	if report.Status != StatusNotConnected {
		t.Errorf("status = %q, want the reader told the numbers have stopped moving", report.Status)
	}

	if len(report.Rows) != 1 {
		t.Errorf("%d rows, want the history we already hold kept on screen", len(report.Rows))
	}
}

// TestAnUnknownDimensionIsRefused keeps a caller-supplied string out of the SQL
// and gives the caller a sentence rather than a five hundred.
func TestAnUnknownDimensionIsRefused(t *testing.T) {
	handler, _ := newHandler(t, true)

	recorder, _ := ask(t, handler, "dimension=browser")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

// TestAnUnauthorisedReaderIsNotToldTheSiteExists keeps the refusal and the
// missing-site answer identical, so the difference between them cannot be used
// to enumerate the domains on an install.
func TestAnUnauthorisedReaderIsNotToldTheSiteExists(t *testing.T) {
	handler, _ := newHandler(t, true)
	handler.Authorize = func(*http.Request, sites.Site) error { return http.ErrNoCookie }

	recorder, _ := ask(t, handler, "")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the same 404 an unregistered domain gets", recorder.Code)
	}
}

// TestNoAuthorizerDenies is the safe default. A newly mounted handler must not
// be able to become public by omission.
func TestNoAuthorizerDenies(t *testing.T) {
	handler, _ := newHandler(t, true)
	handler.Authorize = nil

	recorder, _ := ask(t, handler, "")

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}
