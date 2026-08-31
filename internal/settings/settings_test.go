//
// settings_test.go
// The three screens render, and the Google section hides itself when it must.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/google"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pathclean"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/shields"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newHandler builds a handler over a temporary install holding one site.
func newHandler(t *testing.T) (*Handler, *accounts.Manager) {
	t.Helper()

	ctx := context.Background()
	dataDir := t.TempDir()

	control, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(ctx, control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	exec(t, control, "INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', 0, 0)")
	exec(t, control, "INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', 0, 0)")

	siteCache := sites.New(control)
	if err := siteCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	manager := accounts.NewManager(dataDir)
	t.Cleanup(func() { manager.CloseAll() })

	if _, err := manager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}

	trusted, err := ingest.ParseTrustedProxies(nil)
	if err != nil {
		t.Fatal(err)
	}

	return &Handler{
		Sites:    siteCache,
		Accounts: manager,
		Jobs:     jobs.NewClient(control),
		DataDir:  dataDir,
		Trusted:  trusted,
		Shields:  shields.New(siteCache, manager),
		Paths:    pathclean.New(siteCache, manager),
		Now:      func() time.Time { return time.Unix(1_800_000_000, 0) },
	}, manager
}

// exec runs one statement or fails the test.
func exec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

// get fetches one page.
func get(t *testing.T, handler *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = "203.0.113.14:41234"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

// TestScreensRender checks each page comes back whole. The templates are parsed
// at start-up, so a broken one is a panic in this test rather than a blank page
// somebody discovers in production.
func TestScreensRender(t *testing.T) {
	handler, _ := newHandler(t)

	for _, tc := range []struct{ path, want string }{
		{"/settings/example.com/shields", "Blocked addresses"},
		{"/settings/example.com/paths", "Path cleaning"},
		{"/settings/example.com/imports", "Import &amp; export"},
	} {
		response := get(t, handler, tc.path)

		if response.Code != http.StatusOK {
			t.Fatalf("%s answered %d", tc.path, response.Code)
		}

		if !strings.Contains(response.Body.String(), tc.want) {
			t.Errorf("%s does not contain %q", tc.path, tc.want)
		}
	}
}

// TestShieldsPageShowsTheResolvedAddress covers the one-click promise: the page
// has to show the customer the address their own traffic arrives on, or
// "block my own traffic" is a hunt through a third-party site.
func TestShieldsPageShowsTheResolvedAddress(t *testing.T) {
	handler, _ := newHandler(t)

	body := get(t, handler, "/settings/example.com/shields").Body.String()

	if !strings.Contains(body, "203.0.113.14") {
		t.Fatal("the resolved address is not on the page")
	}

	if !strings.Contains(body, "Block my own traffic") {
		t.Fatal("there is no one-click button for the customer's own address")
	}
}

// TestShieldsPageWarnsAboutALANAddress covers the self-hosting trap: behind a
// proxy that does not forward X-Forwarded-For, the address on this page is the
// customer's router, and a rule built on it blocks nothing at all.
func TestShieldsPageWarnsAboutALANAddress(t *testing.T) {
	handler, _ := newHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/settings/example.com/shields", nil)
	request.RemoteAddr = "192.168.178.1:41234"

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	body := recorder.Body.String()

	if !strings.Contains(body, "not your public address") {
		t.Fatal("no warning was shown for a private address")
	}

	if !strings.Contains(body, "X-Forwarded-For") {
		t.Fatal("the warning does not name the header the customer has to fix")
	}

	if strings.Contains(body, "Block my own traffic") {
		t.Fatal("a one-click rule was offered on an address that would block nothing")
	}
}

// TestGoogleSectionHidesItself is what an install with no OAuth client sees. A
// button that sends somebody to Google and comes back with invalid_client is
// worse than no button.
func TestGoogleSectionHidesItself(t *testing.T) {
	handler, _ := newHandler(t)

	body := get(t, handler, "/settings/example.com/imports").Body.String()

	if strings.Contains(body, "Connect Analytics") {
		t.Fatal("the Google section is offered on an install with no OAuth client")
	}

	app, ok := google.NewApp("id", "secret", "https://example.com")
	if !ok {
		t.Fatal("a complete client was reported as unconfigured")
	}
	handler.Google = app

	body = get(t, handler, "/settings/example.com/imports").Body.String()

	if !strings.Contains(body, "Connect Analytics") {
		t.Fatal("the Google section is hidden on an install that has credentials")
	}

	// The delay has to be stated, or every new customer files the same bug
	// about an empty Search Console report.
	if !strings.Contains(body, "24 to 36 hours") {
		t.Fatal("the Search Console delay is not mentioned anywhere on the page")
	}
}

// TestPathPreviewDoesNotSave checks the preview button. A regular expression
// that eats half a site's URLs has to be visible before it is stored, not
// after.
func TestPathPreviewDoesNotSave(t *testing.T) {
	handler, manager := newHandler(t)

	form := "action=preview&pattern=%5E%2Fusers%2F%5B%5E%2F%5D%2B%24&replacement=%2Fusers%2F%3Aid&label=Users&enabled-0=on"

	request := httptest.NewRequest(http.MethodPost, "/settings/example.com/paths/save", strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the preview answered %d", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "Preview") {
		t.Fatal("the preview section was not rendered")
	}

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rules, err := pathclean.List(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(rules) != 0 {
		t.Fatalf("the preview stored %d rules — nothing should be saved until Save is pressed", len(rules))
	}
}
