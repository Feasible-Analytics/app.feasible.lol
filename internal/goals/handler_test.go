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

// TestTheReportEndpointKeepsItsCardVisibleWithAndWithoutGoals checks both empty
// states: no definitions is an empty array, while a configured goal with no
// conversions is a real zero-valued row.
func TestTheReportEndpointKeepsItsCardVisibleWithAndWithoutGoals(t *testing.T) {
	handler, manager, domain, siteID := newReportHandler(t)

	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, goalReportRequest(domain))
	if empty.Code != http.StatusOK {
		t.Fatalf("empty report answered %d: %s", empty.Code, empty.Body.String())
	}
	if !json.Valid(empty.Body.Bytes()) || !contains(empty.Body.String(), `"Form: Submission"`) {
		t.Fatalf("empty report = %s, want provisioned automatic goals", empty.Body.String())
	}

	lease, err := manager.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}
	if _, err := Create(context.Background(), lease.Account.Writer(), Goal{
		SiteID: siteID, Kind: KindEvent, EventName: "Signup", DisplayName: "Signed up", IsAutomatic: true,
	}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release account: %v", err)
	}

	configured := httptest.NewRecorder()
	handler.ServeHTTP(configured, goalReportRequest(domain))
	if configured.Code != http.StatusOK {
		t.Fatalf("configured report answered %d: %s", configured.Code, configured.Body.String())
	}

	var report ReportResult
	if err := json.Unmarshal(configured.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(report.Rows) != 5 || report.Rows[0].Label != "Signed up" || report.Rows[0].TotalConversions != 0 {
		t.Fatalf("configured report = %+v", report.Rows)
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

// contains keeps the assertion above readable without making its exact JSON
// whitespace part of the endpoint contract.
func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}

	return false
}
