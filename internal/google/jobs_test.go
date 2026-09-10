//
// jobs_test.go
// A nightly top-up that leaves no trace, and one bad grant that stops nothing.
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
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// searchStub is a stand-in for the Search Analytics endpoint that records which
// days were asked for.
type searchStub struct {
	mu   sync.Mutex
	days []string
	rows int
}

// handler answers every day with one row, and remembers the date it was asked
// for so a test can assert on the window rather than on the row count alone.
func (s *searchStub) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StartDate string `json:"startDate"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode search request: %v", err)
		}

		s.mu.Lock()
		s.days = append(s.days, body.StartDate)
		rows := s.rows
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		if rows == 0 {
			if _, err := w.Write([]byte(`{"rows":[]}`)); err != nil {
				t.Errorf("write search response: %v", err)
			}

			return
		}

		if _, err := w.Write([]byte(`{"rows":[{"keys":["widgets","/","usa","DESKTOP"],
			"clicks":3,"impressions":30,"position":4.5}]}`)); err != nil {
			t.Errorf("write search response: %v", err)
		}
	})
}

// asked returns the days the stub was called for.
func (s *searchStub) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.days...)
}

// newWorkers builds the workers over one account holding one site.
func newWorkers(t *testing.T, stub *searchStub, now time.Time) (*Workers, *accounts.Account) {
	t.Helper()

	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)

	original := SearchAPI
	SearchAPI = server.URL
	t.Cleanup(func() { SearchAPI = original })

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

	app, _ := NewApp("id", "secret", "https://example.com")

	return &Workers{
		Accounts: manager, Sites: cache, App: app,
		Now: func() time.Time { return now },
	}, account
}

// storeGrant saves a Search Console connection whose access token is still
// valid, so a test never reaches the token endpoint.
func storeGrant(t *testing.T, account *accounts.Account, property string, now time.Time) {
	t.Helper()

	connection := Connection{
		SiteID: 1, AccountID: 1, Provider: ProviderSearchConsole, Property: property,
		RefreshToken: "refresh", AccessToken: "valid",
		ExpiresAt: now.Add(time.Hour).Unix(), Status: StatusConnected,
	}

	if err := SaveConnection(context.Background(), account.Writer(), connection, now); err != nil {
		t.Fatal(err)
	}
}

// TestTheNightlyTopUpLeavesNoImportRow is the whole reason the refresh is not
// the backfill. A row a night would bury the customer's own imports under a
// year of noise within a year, and nobody would ever read one of them.
func TestTheNightlyTopUpLeavesNoImportRow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	stub := &searchStub{rows: 1}
	workers, account := newWorkers(t, stub, now)

	storeGrant(t, account, "sc-domain:example.com", now)

	outcome, err := workers.RunSync(ctx, jobs.Job{})
	if err != nil {
		t.Fatal(err)
	}

	if outcome.Handled != 1 {
		t.Fatalf("handled %d sites, want the one connected site", outcome.Handled)
	}

	records, err := dataio.ListImports(ctx, account.Reader(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 0 {
		t.Fatalf("%d import rows, want the top-up to leave none", len(records))
	}

	if days := stub.asked(); len(days) != SearchConsoleRefreshDays {
		t.Fatalf("asked for %d days, want %d re-read each night", len(days), SearchConsoleRefreshDays)
	}
}

// TestTheTopUpStopsBeforeGooglePublishes keeps the refresh off the days Google
// has not released. Asking for them costs a request each and answers nothing.
func TestTheTopUpStopsBeforeGooglePublishes(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	stub := &searchStub{rows: 1}
	workers, account := newWorkers(t, stub, now)

	storeGrant(t, account, "sc-domain:example.com", now)

	if _, err := workers.RunSync(context.Background(), jobs.Job{}); err != nil {
		t.Fatal(err)
	}

	days := stub.asked()
	latest := days[len(days)-1]

	if latest != SearchConsoleThrough(now).Format("2006-01-02") {
		t.Errorf("the newest day asked for was %q, want the last day Google has probably published", latest)
	}

	if latest >= now.Format("2006-01-02") {
		t.Errorf("the newest day asked for was %q, which is today or later", latest)
	}
}

// TestASiteWithNoPropertyIsSkippedNotFailed covers the half-finished setup. It
// is not an error, and reporting it as one would turn every nightly run into a
// failure for as long as one customer had not finished connecting.
func TestASiteWithNoPropertyIsSkippedNotFailed(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	stub := &searchStub{rows: 1}
	workers, account := newWorkers(t, stub, now)

	storeGrant(t, account, "", now)

	outcome, err := workers.RunSync(context.Background(), jobs.Job{})
	if err != nil {
		t.Fatalf("an unconfigured site failed the run: %v", err)
	}

	if outcome.Handled != 0 || outcome.Skipped != 1 {
		t.Fatalf("handled %d skipped %d, want the site skipped", outcome.Handled, outcome.Skipped)
	}

	if outcome.Note == "" {
		t.Error("a run that did nothing said nothing about why")
	}

	if len(stub.asked()) != 0 {
		t.Error("Google was called for a site with no property chosen")
	}
}

// TestARunThatDoesNothingSaysWhy is the rule the whole job system is built on:
// a handler reporting success without work and without a reason is recorded as
// a failure, because it is indistinguishable from a job that has quietly died.
func TestARunThatDoesNothingSaysWhy(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	workers, _ := newWorkers(t, &searchStub{}, now)
	workers.App = nil

	outcome, err := workers.RunSync(context.Background(), jobs.Job{})
	if err != nil {
		t.Fatal(err)
	}

	if err := outcome.Validate(); err != nil {
		t.Fatalf("the run reported nothing and gave no reason: %v", err)
	}
}

// TestAnExpiredGrantIsSkippedRatherThanRetried keeps a customer who revoked
// access from producing a failure every night for ever. The settings screen is
// already showing them a reconnect button; the job has nothing to add.
func TestAnExpiredGrantIsSkippedRatherThanRetried(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	stub := &searchStub{rows: 1}
	workers, account := newWorkers(t, stub, now)

	storeGrant(t, account, "sc-domain:example.com", now)

	if err := MarkNeedsReconnect(ctx, account.Writer(), 1, ProviderSearchConsole, "revoked", now); err != nil {
		t.Fatal(err)
	}

	outcome, err := workers.RunSync(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("a lapsed grant failed the whole run: %v", err)
	}

	if outcome.Skipped != 1 {
		t.Errorf("skipped %d, want the lapsed grant left alone", outcome.Skipped)
	}
}

// TestABackfillRecordsItsFailure is the rule for the visible half: every
// failure lands on the import row, so the customer reads a reason on the
// imports screen rather than watching a progress bar stop.
func TestABackfillRecordsItsFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	workers, account := newWorkers(t, &searchStub{}, now)

	record, err := dataio.CreateImport(ctx, account.Writer(), 1, dataio.SourceSearchConsole, "sc-domain:example.com", now)
	if err != nil {
		t.Fatal(err)
	}

	// No grant was stored, so the site is not connected any more.
	args, err := json.Marshal(ImportArgs{AccountID: 1, SiteID: 1, ImportID: record.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := workers.RunImport(ctx, jobs.Job{ID: 7, Args: args}); err == nil {
		t.Fatal("an import with no connection reported success")
	}

	stored, err := dataio.GetImportByID(ctx, account.Reader(), record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Status != dataio.StatusFailed {
		t.Fatalf("status = %q, want the failure on the row", stored.Status)
	}

	if !strings.Contains(stored.Failure, "connect") {
		t.Errorf("failure reads %q, which does not tell the customer what to do", stored.Failure)
	}
}

// TestABackfillKeepsItsOwnWindow covers a retry a day later. A window derived
// from the clock would slide forward between attempts and skip whichever day
// the cursor had already passed.
func TestABackfillKeepsItsOwnWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	stub := &searchStub{rows: 1}
	workers, account := newWorkers(t, stub, now)

	storeGrant(t, account, "sc-domain:example.com", now)

	record, err := dataio.CreateImport(ctx, account.Writer(), 1, dataio.SourceSearchConsole, "sc-domain:example.com", now)
	if err != nil {
		t.Fatal(err)
	}

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	args, err := json.Marshal(ImportArgs{
		AccountID: 1, SiteID: 1, ImportID: record.ID, From: from.Unix(), To: to.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := workers.RunImport(ctx, jobs.Job{ID: 8, Args: args}); err != nil {
		t.Fatal(err)
	}

	days := stub.asked()

	if len(days) != 3 || days[0] != "2026-08-01" || days[2] != "2026-08-03" {
		t.Fatalf("asked for %v, want exactly the days the job carried", days)
	}

	stored, err := dataio.GetImportByID(ctx, account.Reader(), record.ID)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Status != dataio.StatusCompleted {
		t.Fatalf("status = %q, want a finished import", stored.Status)
	}

	if stored.RowsWritten != 3 {
		t.Errorf("rows written = %d, want one per day", stored.RowsWritten)
	}
}
