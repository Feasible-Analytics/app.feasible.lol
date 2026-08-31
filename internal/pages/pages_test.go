//
// pages_test.go
// Every server-rendered screen: it renders, it prices correctly, and it carries the address.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package pages

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/billing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// pagesNow is the clock the screens render at.
var pagesNow = time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

// newHandler builds the pages over a real control database with one team.
func newHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()

	control, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	stamp := pagesNow.Unix()

	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	handler := &Handler{
		Billing: &billing.Service{
			Store:   billing.NewStore(control),
			Plans:   billing.Plans{Monthly: "price_monthly", Yearly: "price_yearly"},
			BaseURL: "https://feasible.lol",
		},
		Lifecycle:  lifecycle.NewStore(control),
		Usage:      usage.NewStore(control),
		SalesEmail: "sales@feasible.lol",
		Now:        func() time.Time { return pagesNow },
	}

	return handler, control
}

// render issues one request through the whole route table.
func render(t *testing.T, handler *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	mux := http.NewServeMux()
	handler.Routes(mux)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	return recorder
}

// TestEveryPageRenders walks the whole route table. A billing screen that fails
// to render is a customer who cannot pay us.
func TestEveryPageRenders(t *testing.T) {
	handler, _ := newHandler(t)

	paths := []string{
		"/pricing",
		"/billing?team=1",
		"/billing/upgrade",
		"/billing/done",
		"/billing/export",
		"/docs",
		"/legal/privacy",
		"/legal/terms",
		"/legal/dpa",
	}

	for _, doc := range documentation {
		paths = append(paths, "/docs/"+doc.Slug)
	}

	for _, path := range paths {
		response := render(t, handler, path)

		if response.Code != http.StatusOK {
			t.Errorf("%s answered %d", path, response.Code)
			continue
		}

		body := response.Body.String()

		if !strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("%s is not a complete document", path)
		}

		// The legal entity is required in the footer of every page.
		for _, line := range []string{"Cloudmanic Labs, LLC", "901 Brutscher Street, D112", "Newberg, OR 97132", "United States"} {
			if !strings.Contains(body, line) {
				t.Errorf("%s is missing %q from the footer", path, line)
			}
		}
	}
}

// TestPricingStatesBothPrices is the one thing the page exists to say.
func TestPricingStatesBothPrices(t *testing.T) {
	handler, _ := newHandler(t)

	body := render(t, handler, "/pricing").Body.String()

	for _, want := range []string{"$9.99", "$100", "1,000,000 pageviews", "Unlimited sites", "Pro-rata refund within 30 days"} {
		if !strings.Contains(body, want) {
			t.Errorf("the pricing page never says %q", want)
		}
	}
}

// TestPricingPublishesTheLifecycleTimetable is the "we would rather tell you
// before you buy" promise. All four phases have to be on the page somebody reads
// while deciding.
func TestPricingPublishesTheLifecycleTimetable(t *testing.T) {
	handler, _ := newHandler(t)

	body := render(t, handler, "/pricing").Body.String()

	for _, want := range []string{"Days 0 – 30", "Days 30 – 60", "Days 60 – 90", "Day 90", "we keep collecting", "permanently delete"} {
		if !strings.Contains(body, want) {
			t.Errorf("the pricing page never mentions %q", want)
		}
	}
}

// TestBillingShowsTheUsageMeter is the in-app meter the specification asks for.
func TestBillingShowsTheUsageMeter(t *testing.T) {
	handler, control := newHandler(t)

	if _, err := control.Exec(`
		INSERT INTO usage_counters (team_id, period, pageviews, custom_events, updated_at)
		VALUES (1, '2026-03', 700000, 20000, ?)
	`, pagesNow.Unix()); err != nil {
		t.Fatal(err)
	}

	body := render(t, handler, "/billing?team=1").Body.String()

	for _, want := range []string{"720,000", "1,000,000", "72%", "Usage this month"} {
		if !strings.Contains(body, want) {
			t.Errorf("the billing page never shows %q", want)
		}
	}

	// Past the 70% rung, the enterprise conversation appears.
	if !strings.Contains(body, "sales@feasible.lol") {
		t.Error("the billing page does not offer the enterprise contact at 72% of the plan")
	}
}

// TestBillingBannerNamesTheDates is the same "nobody is ever surprised" rule the
// emails follow, applied to the screen.
func TestBillingBannerNamesTheDates(t *testing.T) {
	handler, control := newHandler(t)

	state := lifecycle.State{Trigger: lifecycle.TriggerLapse, StartedAt: pagesNow.Add(-40 * lifecycle.Day)}

	if err := lifecycle.NewStore(control).Save(context.Background(), 1, state); err != nil {
		t.Fatal(err)
	}

	body := render(t, handler, "/billing?team=1").Body.String()

	for _, phase := range []lifecycle.Phase{lifecycle.PhaseLocked, lifecycle.PhaseDormant, lifecycle.PhaseDeleted} {
		want := state.Boundary(phase).Format("2 January 2006")

		if !strings.Contains(body, want) {
			t.Errorf("the billing page never names %s, the %s boundary", want, phase)
		}
	}

	if !strings.Contains(body, "still collecting") {
		t.Error("a locked account is not told that collection is continuing")
	}
}

// TestBillingAlwaysOffersTheExport is the portability guarantee on the screen: it
// has to be reachable in every phase, including from a locked account.
func TestBillingAlwaysOffersTheExport(t *testing.T) {
	handler, control := newHandler(t)

	for _, day := range []int{0, 35, 70, 89} {
		state := lifecycle.State{Trigger: lifecycle.TriggerLapse, StartedAt: pagesNow.Add(-time.Duration(day) * lifecycle.Day)}

		if err := lifecycle.NewStore(control).Save(context.Background(), 1, state); err != nil {
			t.Fatal(err)
		}

		body := render(t, handler, "/billing?team=1").Body.String()

		if !strings.Contains(body, "/billing/export") {
			t.Errorf("day %d has no export link", day)
		}
	}
}

// TestCheckoutWithoutAProviderExplainsItself covers the self-hosted install. It
// must say so plainly rather than failing with a stack trace, because nothing
// about the software the customer is running is limited by it.
func TestCheckoutWithoutAProviderExplainsItself(t *testing.T) {
	handler, _ := newHandler(t)

	mux := http.NewServeMux()
	handler.Routes(mux)

	request := httptest.NewRequest(http.MethodPost, "/billing/checkout", strings.NewReader("plan=monthly&team=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "cannot take payments") {
		t.Errorf("the page does not explain why checkout is unavailable: %s", recorder.Body.String())
	}
}

// TestDocsCoverEveryRequiredTopic is the documentation scope from the
// specification, asserted rather than trusted to a reviewer.
func TestDocsCoverEveryRequiredTopic(t *testing.T) {
	required := []string{
		"installation", "integrations", "script-options", "proxying",
		"metrics", "goals-funnels", "custom-properties", "api",
		"self-hosting", "privacy",
	}

	for _, slug := range required {
		if _, ok := findDoc(documentation, slug); !ok {
			t.Errorf("there is no documentation page for %q", slug)
		}
	}
}

// TestTheDocsExplainTheSurprisingMetrics is the specific list the specification
// calls out: the four results that make somebody think the numbers are wrong.
func TestTheDocsExplainTheSurprisingMetrics(t *testing.T) {
	page, ok := findDoc(documentation, "metrics")
	if !ok {
		t.Fatal("there is no metrics page")
	}

	body := string(page.Body)

	for _, want := range []string{
		"Visitors are not additive",
		"Unique visitors can exceed pageviews",
		"Attribution is frozen at the start of a visit",
		"Goals do not backfill",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the metric definitions never explain %q", want)
		}
	}
}

// TestThePrivacyDocIsHonestAboutPseudonymity is the differentiator, and the one
// paragraph most likely to be softened by somebody later.
func TestThePrivacyDocIsHonestAboutPseudonymity(t *testing.T) {
	page, ok := findDoc(documentation, "privacy")
	if !ok {
		t.Fatal("there is no privacy page")
	}

	body := string(page.Body)

	if !strings.Contains(body, "pseudonymous, not anonymous") {
		t.Error("the privacy documentation does not state that the identifier is pseudonymous rather than anonymous")
	}
	if !strings.Contains(body, "brute-force") {
		t.Error("the privacy documentation does not explain why it is pseudonymous")
	}
}

// TestTheLegalPagesNameTheController is a GDPR requirement: the controller's
// identity and contact details have to be in the policy itself, not only in a
// footer.
func TestTheLegalPagesNameTheController(t *testing.T) {
	for _, slug := range []string{"privacy", "terms", "dpa"} {
		page, ok := findDoc(legal, slug)
		if !ok {
			t.Fatalf("there is no legal page for %q", slug)
		}

		body := string(page.Body)

		if !strings.Contains(body, "Cloudmanic Labs, LLC") {
			t.Errorf("the %s page does not name the entity", slug)
		}
		if !strings.Contains(body, "901 Brutscher Street, D112") {
			t.Errorf("the %s page does not give the postal address", slug)
		}
	}
}

// TestNoCompetitorIsNamed keeps the repository's rule. The docs cover the same
// topics as the incumbent's and say so in our own words, but naming anybody is
// off limits.
func TestNoCompetitorIsNamed(t *testing.T) {
	banned := []string{"plausible", "fathom", "matomo", "simple analytics", "google analytics", "paddle"}

	for _, set := range [][]Doc{documentation, legal} {
		for _, doc := range set {
			body := strings.ToLower(string(doc.Body))

			for _, name := range banned {
				if strings.Contains(body, name) {
					t.Errorf("%s names %q", doc.Slug, name)
				}
			}
		}
	}
}

// TestAnUnknownDocIs404 makes sure a bad link produces a 404 rather than an
// empty page that looks like the content is missing.
func TestAnUnknownDocIs404(t *testing.T) {
	handler, _ := newHandler(t)

	if code := render(t, handler, "/docs/not-a-page").Code; code != http.StatusNotFound {
		t.Errorf("an unknown documentation page answered %d", code)
	}
	if code := render(t, handler, "/legal/not-a-page").Code; code != http.StatusNotFound {
		t.Errorf("an unknown legal page answered %d", code)
	}
}

// TestStylesheetIsServed checks the one asset these pages depend on. A billing
// screen with no stylesheet still works, and looks like a broken deploy.
func TestStylesheetIsServed(t *testing.T) {
	handler, _ := newHandler(t)

	response := render(t, handler, "/billing/assets/pages.css")

	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("content type is %q", got)
	}
}

// TestThousands checks the formatting the meter and the tables use.
func TestThousands(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1_000: "1,000", 1_234_567: "1,234,567"}

	for input, want := range cases {
		if got := thousands(input); got != want {
			t.Errorf("%d became %q, want %q", input, got, want)
		}
	}
}
