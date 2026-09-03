//
// handler_test.go
// The dashboard goals report endpoint.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newReportHandler builds an endpoint over real control and account databases.
func newReportHandler(t *testing.T) (*Handler, *accounts.Manager, string, int64) {
	t.Helper()

	dir := t.TempDir()
	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.System()); err != nil {
		t.Fatalf("migrate control: %v", err)
	}

	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	team, err := control.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', ?, ?)`, now.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}
	teamID, _ := team.LastInsertId()

	domain := "goals.example"
	created, err := control.Exec(`INSERT INTO sites (account_id, domain, timezone, created_at, updated_at) VALUES (?, ?, 'UTC', ?, ?)`, teamID, domain, now.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}
	siteID, _ := created.LastInsertId()

	cache := sites.New(control)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh sites: %v", err)
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close accounts: %v", err)
		}
	})

	handler := NewHandler(cache, manager, nil)
	handler.Now = func() time.Time { return now }
	handler.Authorize = func(*http.Request, sites.Site) (Authorization, error) { return Authorization{}, nil }
	lease, err := manager.Acquire(context.Background(), teamID)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if _, err := EnsureAutomatic(context.Background(), lease.Account.Writer(), siteID, now); err != nil {
		t.Fatalf("provision automatic goals: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release account: %v", err)
	}

	return handler, manager, domain, siteID
}

// goalReportRequest builds the request shape a mounted mux supplies.
func goalReportRequest(domain string) *http.Request {
	request := httptest.NewRequest(http.MethodGet,
		"/api/sites/"+domain+`/goals/report?date_range=%2228d%22`, nil)
	request.SetPathValue("domain", domain)

	return request
}

// TestTheReportEndpointCountsTheGoalsItDoesNotList is the contract the empty
// states are built on. The dashboard lists only goals that converted, so an
// empty row list on its own cannot say whether the site has no goals or has
// goals that did not fire — and those two need opposite advice. The counts are
// what tell them apart, so they must survive the wire.
func TestTheReportEndpointCountsTheGoalsItDoesNotList(t *testing.T) {
	handler, manager, domain, siteID := newReportHandler(t)

	decode := func(t *testing.T, what string) ReportResult {
		t.Helper()

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, goalReportRequest(domain))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s report answered %d: %s", what, recorder.Code, recorder.Body.String())
		}

		var report ReportResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode %s report: %v", what, err)
		}

		return report
	}

	// The fixture serves no events at all, so none of the four provisioned
	// automatic goals has converted.
	automatic := decode(t, "automatic")
	if len(automatic.Rows) != 0 {
		t.Errorf("unconverted report lists %d rows, want none", len(automatic.Rows))
	}
	if automatic.Configured != 4 || automatic.ConfiguredAutomatic != 4 {
		t.Errorf("unconverted report counted %d goals, %d automatic, want 4 and 4",
			automatic.Configured, automatic.ConfiguredAutomatic)
	}

	lease, err := manager.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if _, err := Create(context.Background(), lease.Account.Writer(), Goal{
		SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Signed up",
	}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release account: %v", err)
	}

	// A goal somebody configured themselves is still not listed until it
	// converts, but it does move the site off "every goal here is one of ours".
	business := decode(t, "configured")
	if len(business.Rows) != 0 {
		t.Errorf("configured report lists %d rows, want none", len(business.Rows))
	}
	if business.Configured != 5 || business.ConfiguredAutomatic != 4 {
		t.Errorf("configured report counted %d goals, %d automatic, want 5 and 4",
			business.Configured, business.ConfiguredAutomatic)
	}
}

// TestJourneyEndpointDecodesTypedContinuation verifies the dashboard's full
// Explore wire contract reaches the typed journey engine with shared filters.
func TestJourneyEndpointDecodesTypedContinuation(t *testing.T) {
	handler, _, domain, _ := newReportHandler(t)
	request := httptest.NewRequest(http.MethodGet,
		"/api/sites/"+domain+`/journey?anchor_type=event&anchor=Signup&direction=backward&grouping=prefix&trail=%5B%7B%22type%22%3A%22page%22%2C%22value%22%3A%22%2Fpricing%22%7D%5D&filters=%5B%5D&exact=true`, nil)
	request.SetPathValue("domain", domain)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("journey answered %d: %s", recorder.Code, recorder.Body.String())
	}
	var result JourneyResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Anchor.Type != "event" || result.Anchor.Value != "Signup" || result.Direction != "backward" {
		t.Fatalf("typed anchor = %+v direction %q", result.Anchor, result.Direction)
	}
	if len(result.Trail) != 1 || result.Trail[0].Value != "/pricing" || result.Steps == nil {
		t.Fatalf("journey continuation = trail %+v steps %+v", result.Trail, result.Steps)
	}
}

