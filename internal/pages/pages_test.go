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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/billing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// pagesNow is the clock the screens render at.
var pagesNow = time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

// applyPagesSystemSchema applies the complete merged control chain so page
// fixtures render against the same M9 and M8 schema as the runtime.
func applyPagesSystemSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}
}

// newHandler builds the pages over a real system database with one team.
func newHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()

	control, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	applyPagesSystemSchema(t, control)

	stamp := pagesNow.Unix()

	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	handler := &Handler{
		Billing: &billing.Service{
			Store:         billing.NewStore(control),
			Plans:         billing.Plans{Product: "prod_test", Monthly: "price_monthly", Yearly: "price_yearly"},
			BaseURL:       "https://feasible.lol",
			WebhookSecret: "whsec_test",
		},
		Lifecycle:  lifecycle.NewStore(control),
		Usage:      usage.NewStore(control),
		SalesEmail: "sales@feasible.lol",
		Hosted:     true,
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
		"/billing/done?team=1",
		"/billing/export?team=1",
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

// TestLayoutNavigationPreservesTheSelectedTeam keeps ordinary navigation from
// silently switching a multi-team user back to their default billing account.
func TestLayoutNavigationPreservesTheSelectedTeam(t *testing.T) {
	handler, _ := newHandler(t)
	handler.CurrentAccount = func(*http.Request) (Account, error) {
		return Account{ID: 27, Email: "billing@example.com"}, nil
	}

	body := render(t, handler, "/pricing?team=27").Body.String()
	if count := strings.Count(body, `href="/pricing?team=27"`); count != 2 {
		t.Errorf("selected-team pricing navigation appeared %d times, want header and footer: %s", count, body)
	}
	if !strings.Contains(body, `href="/billing?team=27"`) {
		t.Errorf("selected-team billing navigation is missing: %s", body)
	}
}

// TestWritePublicBrowserFixtures renders every public GET page through the real
// route table for the serverless Chromium matrix. Normal Go test runs skip the
// artifact write; the browser suite supplies a temporary output directory and
// removes it after loading the manifest.
func TestWritePublicBrowserFixtures(t *testing.T) {
	outputDir := os.Getenv("FEASIBLE_PUBLIC_FIXTURE_DIR")
	if outputDir == "" {
		t.Skip("FEASIBLE_PUBLIC_FIXTURE_DIR is only set by the serverless browser suite")
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}

	handler, _ := newHandler(t)
	paths := []string{
		"/pricing",
		"/billing?team=1",
		"/billing/upgrade",
		"/billing/done?team=1",
		"/billing/export?team=1",
		"/docs",
		"/legal/privacy",
		"/legal/terms",
		"/legal/dpa",
	}
	for _, doc := range documentation {
		paths = append(paths, "/docs/"+doc.Slug)
	}

	type fixture struct {
		Path string `json:"path"`
		File string `json:"file"`
	}
	fixtures := make([]fixture, 0, len(paths))
	for index, path := range paths {
		response := render(t, handler, path)
		if response.Code != http.StatusOK {
			t.Fatalf("render browser fixture %s: status %d", path, response.Code)
		}
		name := fmt.Sprintf("%03d.html", index)
		if err := os.WriteFile(filepath.Join(outputDir, name), response.Body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture{Path: path, File: name})
	}

	manifest, err := json.Marshal(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPublicHeaderHasNarrowViewportContainment checks the static layout
// contract used by browser-capable smoke checks at 320px and 390px.
func TestPublicHeaderHasNarrowViewportContainment(t *testing.T) {
	handler, _ := newHandler(t)
	page := render(t, handler, "/pricing").Body.String()
	if !strings.Contains(page, `name="viewport" content="width=device-width, initial-scale=1"`) {
		t.Fatal("public layout is missing its device-width viewport")
	}

	css := render(t, handler, "/billing/assets/pages.css").Body.String()
	for _, want := range []string{
		"header.top .wrap {", "flex-wrap: wrap", "@media (max-width: 520px)",
		"header.top .brand { flex-basis: 100%; }", "header.top nav { width: 100%",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("narrow header CSS is missing %q", want)
		}
	}
	if strings.Contains(css, "letter-spacing: -") {
		t.Fatal("public CSS still uses negative letter spacing")
	}
}

// TestLegalPagesIdentifyTheRunningOperator proves hosted pages retain the
// service entity while self-hosted privacy and DPA pages name the configured
// operator and never misidentify Cloudmanic as that deployment's processor.
func TestLegalPagesIdentifyTheRunningOperator(t *testing.T) {
	handler, _ := newHandler(t)
	handler.Hosted = false
	handler.OperatorName = "Example Operator, Inc."
	handler.OperatorAddress = "123 Example Street\nPortland, OR"
	handler.OperatorEmail = "privacy@example.test"

	for _, path := range []string{"/legal/privacy", "/legal/dpa"} {
		body := render(t, handler, path).Body.String()
		for _, want := range []string{"Example Operator, Inc.", "123 Example Street", "privacy@example.test"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s self-hosted body is missing %q", path, want)
			}
		}
		if strings.Contains(body, "<strong>Processor:</strong> Cloudmanic") || strings.Contains(body, "<strong>Cloudmanic Labs, LLC</strong>") {
			t.Errorf("%s identifies Cloudmanic as the self-hosted operator", path)
		}
	}

	handler.Hosted = true
	for _, path := range []string{"/legal/privacy", "/legal/dpa"} {
		body := render(t, handler, path).Body.String()
		if !strings.Contains(body, "Cloudmanic Labs, LLC") {
			t.Errorf("%s hosted body lost the service entity", path)
		}
	}
}

// TestWebhookDocsDescribeDecimalInt64DeliveryIDs keeps the public request and
// header examples aligned with the INTEGER PRIMARY KEY implementation.
func TestWebhookDocsDescribeDecimalInt64DeliveryIDs(t *testing.T) {
	body := strings.ToLower(string(mustFind(t, documentation, "webhooks").Body))
	for _, want := range []string{"feasible-delivery: 1842", "decimal signed 64-bit integer id", "{delivery_id}"} {
		if !strings.Contains(body, want) {
			t.Errorf("webhook docs are missing %q", want)
		}
	}
	if strings.Contains(body, "feasible-delivery: 91c4") {
		t.Fatal("webhook docs still show the delivery id as a hexadecimal or UUID-like value")
	}
}

// TestPublicLanguageChoicePersistsWithoutMislabelingFallback proves public
// handlers use the shared Apply path. German is supported by account screens
// but the public pages catalogue and embedded prose are English-only, so the
// preference is remembered while the document truthfully remains lang=en.
func TestPublicLanguageChoicePersistsWithoutMislabelingFallback(t *testing.T) {
	handler, _ := newHandler(t)
	mux := http.NewServeMux()
	handler.Routes(mux)

	for _, path := range []string{"/pricing?lang=de", "/docs/shields?lang=de", "/legal/privacy?lang=de"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s answered %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), `<html lang="en">`) {
			t.Errorf("%s labelled English fallback content as another language", path)
		}

		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != i18n.CookieName || cookies[0].Value != "de" {
			t.Errorf("%s did not persist the explicit language choice: %v", path, cookies)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/pricing", nil)
	req.AddCookie(&http.Cookie{Name: i18n.CookieName, Value: "de"})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	if !strings.Contains(recorder.Body.String(), `<html lang="en">`) {
		t.Fatal("a persisted partial locale mislabeled the next page's English fallback")
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

// TestSignedOutPricingStartsAuthentication keeps the public pricing page useful
// without exposing checkout itself to an unauthenticated browser.
func TestSignedOutPricingStartsAuthentication(t *testing.T) {
	handler, _ := newHandler(t)

	body := render(t, handler, "/pricing").Body.String()
	for _, want := range []string{
		"/register?next=%2Fpricing%3Fplan%3Dmonthly",
		"/login?next=%2Fpricing%3Fplan%3Dmonthly",
		"/register?next=%2Fpricing%3Fplan%3Dyearly",
		"/login?next=%2Fpricing%3Fplan%3Dyearly",
		"Create an account",
		"Sign in to upgrade",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("signed-out pricing is missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `action="/billing/checkout"`) {
		t.Error("signed-out pricing rendered a checkout form")
	}
}

// TestPricingRestoresTheChosenPlanAfterAuthentication makes the safe GET
// destination visibly retain intent while checkout itself remains POST-only.
func TestPricingRestoresTheChosenPlanAfterAuthentication(t *testing.T) {
	handler, _ := newHandler(t)

	body := render(t, handler, "/pricing?plan=yearly&team=1").Body.String()
	if !strings.Contains(body, "Continue yearly") || strings.Contains(body, "Continue monthly") {
		t.Fatalf("yearly purchase intent was not restored on pricing: %s", body)
	}
}

// TestPortalSessionCreationIsPostOnly prevents email links, prefetchers, and
// cross-site navigation from creating provider sessions as a GET side effect.
func TestPortalSessionCreationIsPostOnly(t *testing.T) {
	handler, _ := newHandler(t)
	mux := http.NewServeMux()
	handler.Routes(mux)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/billing/portal", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /billing/portal answered %d, want 405", response.Code)
	}
}

// TestBillingCopyQualifiesManagedPaymentsResponsibilities keeps the pricing
// page and contract honest about both the merchant-of-record entity and the tax
// obligations that remain with the seller outside Managed Payments coverage.
func TestBillingCopyQualifiesManagedPaymentsResponsibilities(t *testing.T) {
	handler, _ := newHandler(t)

	pricing := render(t, handler, "/pricing").Body.String()
	terms, ok := findDoc(legal, "terms")
	if !ok {
		t.Fatal("there is no legal page for terms")
	}

	for name, body := range map[string]string{
		"pricing": pricing,
		"terms":   string(terms.Body),
	} {
		body = strings.Join(strings.Fields(body), " ")
		for _, want := range []string{"Stripe Managed Payments", "Sold through Link, LLC", "merchant of record", "seller taxes", "does not handle them"} {
			if !strings.Contains(body, want) {
				t.Errorf("the %s page never says %q", name, want)
			}
		}

		for _, stale := range []string{"We are the merchant of record", "Cloudmanic Labs, LLC appears on your invoice"} {
			if strings.Contains(body, stale) {
				t.Errorf("the %s page still says %q", name, stale)
			}
		}
	}
}

// TestRefundCopyDefersToLinkWhereRequired prevents our voluntary policy from
// being presented as the only or controlling policy for Sold through Link
// transactions.
func TestRefundCopyDefersToLinkWhereRequired(t *testing.T) {
	handler, _ := newHandler(t)

	pricing := render(t, handler, "/pricing").Body.String()
	terms, ok := findDoc(legal, "terms")
	if !ok {
		t.Fatal("there is no legal page for terms")
	}

	for name, body := range map[string]string{"pricing": pricing, "terms": string(terms.Body)} {
		for _, want := range []string{"Link support", "refund policy controls", "applicable law"} {
			if !strings.Contains(body, want) {
				t.Errorf("the %s page never says %q", name, want)
			}
		}
	}
}

// TestCheckoutReturnDistinguishesPaidAndPending makes sure a browser redirect
// cannot promise access before an asynchronous payment has settled.
func TestCheckoutReturnDistinguishesPaidAndPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "unpaid"
		if strings.HasSuffix(r.URL.Path, "/cs_paid") {
			status = "paid"
		}
		teamID := "2"
		if strings.HasSuffix(r.URL.Path, "/cs_other") {
			teamID = "1"
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"%s","object":"checkout.session","mode":"subscription","payment_status":%q,"metadata":{"feasible_team_id":%q}}`, strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), status, teamID)
	}))
	t.Cleanup(server.Close)

	handler, _ := newHandler(t)
	client := stripe.New("sk_test_fake")
	client.BaseURL = server.URL
	handler.Billing.Stripe = client
	handler.CurrentAccount = func(*http.Request) (Account, error) {
		return Account{ID: 2, Email: "owner@example.com"}, nil
	}

	paid := render(t, handler, "/billing/done?team=2&session=cs_paid").Body.String()
	if !strings.Contains(paid, "Payment confirmed") || !strings.Contains(paid, "signed notification") {
		t.Fatalf("paid checkout copy is not conclusive but webhook-gated: %s", paid)
	}
	if !strings.Contains(paid, "/billing?team=2") {
		t.Fatalf("paid checkout return lost selected team: %s", paid)
	}

	pending := render(t, handler, "/billing/done?team=2&session=cs_pending").Body.String()
	for _, want := range []string{"Payment processing", "has not settled yet", "current state"} {
		if !strings.Contains(pending, want) {
			t.Errorf("pending checkout copy is missing %q: %s", want, pending)
		}
	}
	if strings.Contains(pending, "account is active") || strings.Contains(pending, "payment went through") {
		t.Errorf("pending checkout promises paid access: %s", pending)
	}
	if !strings.Contains(pending, "/billing?team=2") || !strings.Contains(pending, "/pricing?team=2") {
		t.Errorf("pending retry links lost selected team: %s", pending)
	}

	other := render(t, handler, "/billing/done?team=2&session=cs_other")
	if other.Code != http.StatusNotFound {
		t.Errorf("another account's checkout return answered %d, want 404", other.Code)
	}
}

// TestBillingShowsPaymentStateAlongsideProviderStatus makes an active Stripe
// subscription with a failed asynchronous payment understandable to customers
// and support instead of displaying the misleading word "active" alone.
func TestBillingShowsPaymentStateAlongsideProviderStatus(t *testing.T) {
	handler, control := newHandler(t)
	stamp := pagesNow.Unix()

	if _, err := control.Exec(`
		INSERT INTO subscriptions
			(team_id, stripe_subscription_id, status, plan, stripe_price_id,
			 payment_state, created_at, updated_at)
		VALUES (1, 'sub_async', 'active', 'monthly', 'price_monthly', 'failed', ?, ?)
	`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	body := render(t, handler, "/billing?team=1").Body.String()
	for _, want := range []string{"Status", "active", "Payment", "failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("billing page is missing %q: %s", want, body)
		}
	}
}

// TestBillingExplainsACompedAccount ensures a complimentary account does not
// look like a broken subscription or invite its owner to buy access they
// already have.
func TestBillingExplainsACompedAccount(t *testing.T) {
	handler, control := newHandler(t)

	if _, err := control.Exec(`
		INSERT INTO account_comps (team_id, owner_email, comped_at)
		VALUES (1, 'owner@example.com', ?)
	`, pagesNow.Unix()); err != nil {
		t.Fatal(err)
	}

	body := render(t, handler, "/billing?team=1").Body.String()
	for _, want := range []string{"Complimentary account", "fully active", "does not need a card"} {
		if !strings.Contains(body, want) {
			t.Errorf("comped billing page is missing %q: %s", want, body)
		}
	}

	for _, unwanted := range []string{`action="/billing/checkout"`, "Renews on", "No card on file"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("comped billing page contains %q: %s", unwanted, body)
		}
	}
}

// TestPricingPublishesTheLifecycleTimetable is the "we would rather tell you
// before you buy" promise. All four phases have to be on the page somebody reads
// while deciding.
func TestPricingPublishesTheLifecycleTimetable(t *testing.T) {
	handler, _ := newHandler(t)

	body := render(t, handler, "/pricing").Body.String()

	for _, want := range []string{"Days 0 – 30", "Days 30 – 60", "Days 60 – 90", "Day 90", "we keep collecting", "live systems", "outside the application", "retried hourly"} {
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

		if !strings.Contains(body, "/billing/export?team=1") {
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

// TestInjectedAccountAccessProtectsBillingAndCSRF verifies the package-level
// contract used by auth: account routes require its middleware, forged account
// ids are ignored, and authenticated POSTs run its CSRF check first.
func TestInjectedAccountAccessProtectsBillingAndCSRF(t *testing.T) {
	handler, _ := newHandler(t)
	handler.RequireAccount = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test-Session") != "verified" {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
	handler.CurrentAccount = func(*http.Request) (Account, error) {
		return Account{ID: 1, Email: "owner@example.com"}, nil
	}
	handler.FormToken = func(http.ResponseWriter, *http.Request) string { return "trusted-token" }
	handler.ValidateForm = func(w http.ResponseWriter, r *http.Request) bool {
		if r.PostFormValue("csrf_token") == "trusted-token" {
			return true
		}

		http.Error(w, "bad token", http.StatusForbidden)

		return false
	}

	mux := http.NewServeMux()
	handler.Routes(mux)

	signedOut := httptest.NewRecorder()
	mux.ServeHTTP(signedOut, httptest.NewRequest(http.MethodGet, "/billing?team=999", nil))
	if signedOut.Code != http.StatusFound || !strings.HasPrefix(signedOut.Header().Get("Location"), "/login?next=") {
		t.Fatalf("signed-out billing answered %d and redirected to %q", signedOut.Code, signedOut.Header().Get("Location"))
	}

	signedInRequest := httptest.NewRequest(http.MethodGet, "/billing?team=999", nil)
	signedInRequest.Header.Set("X-Test-Session", "verified")
	signedIn := httptest.NewRecorder()
	mux.ServeHTTP(signedIn, signedInRequest)
	if signedIn.Code != http.StatusOK {
		t.Fatalf("signed-in billing answered %d: %s", signedIn.Code, signedIn.Body.String())
	}
	if !strings.Contains(signedIn.Body.String(), "Example Co") || strings.Contains(signedIn.Body.String(), "Account 999") {
		t.Errorf("billing did not resolve the authenticated account: %s", signedIn.Body.String())
	}

	for name, token := range map[string]string{"missing": "", "mismatched": "forged-token"} {
		t.Run(name+" csrf", func(t *testing.T) {
			form := url.Values{"plan": {"monthly"}, "team": {"999"}}
			if token != "" {
				form.Set("csrf_token", token)
			}

			request := httptest.NewRequest(http.MethodPost, "/billing/checkout", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("X-Test-Session", "verified")
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if response.Code != http.StatusForbidden {
				t.Errorf("%s token answered %d, want 403", name, response.Code)
			}
		})
	}
}

// TestCheckoutUsesAuthenticatedAccountAndEmail proves that caller-controlled
// team and email fields cannot change the Stripe session being created.
func TestCheckoutUsesAuthenticatedAccountAndEmail(t *testing.T) {
	var posted url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Stripe form: %v", err)
		}
		posted = r.PostForm

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_authenticated","url":"https://checkout.example/session"}`))
	}))
	t.Cleanup(server.Close)

	handler, control := newHandler(t)
	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (2, 'Second Team', ?, ?)`, pagesNow.Unix(), pagesNow.Unix()); err != nil {
		t.Fatal(err)
	}
	client := stripe.New("sk_test_fake")
	client.BaseURL = server.URL
	handler.Billing.Stripe = client
	handler.CurrentAccount = func(*http.Request) (Account, error) {
		return Account{ID: 2, Email: "owner@example.com"}, nil
	}
	handler.ValidateForm = func(http.ResponseWriter, *http.Request) bool { return true }

	mux := http.NewServeMux()
	handler.Routes(mux)

	form := url.Values{
		"plan":  {"monthly"},
		"team":  {"999"},
		"email": {"attacker@example.com"},
	}
	request := httptest.NewRequest(http.MethodPost, "/billing/checkout", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("checkout answered %d: %s", response.Code, response.Body.String())
	}
	if got := posted.Get("metadata[feasible_team_id]"); got != "2" {
		t.Errorf("Stripe team metadata is %q, want authenticated team 2", got)
	}
	if got := posted.Get("customer_email"); got != "owner@example.com" {
		t.Errorf("Stripe customer email is %q, want authenticated owner", got)
	}
	for field, want := range map[string]string{
		"success_url": "https://feasible.lol/billing/done?session={CHECKOUT_SESSION_ID}&team=2",
		"cancel_url":  "https://feasible.lol/pricing?plan=monthly&team=2",
	} {
		if got := posted.Get(field); got != want {
			t.Errorf("Stripe %s is %q, want %q", field, got, want)
		}
	}
}

// TestPortalPreservesTheAuthenticatedTeamInItsReturnURL covers a non-default
// team. The form's forged selector is ignored, and Stripe returns to the exact
// membership-selected account instead of silently reopening the default team.
func TestPortalPreservesTheAuthenticatedTeamInItsReturnURL(t *testing.T) {
	var posted url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Stripe form: %v", err)
		}
		posted = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bps_selected","url":"https://billing.example/session"}`))
	}))
	t.Cleanup(server.Close)

	handler, control := newHandler(t)
	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (2, 'Second Team', ?, ?)`, pagesNow.Unix(), pagesNow.Unix()); err != nil {
		t.Fatal(err)
	}
	applied, err := handler.Billing.Store.SaveReconciled(context.Background(), billing.Subscription{
		TeamID: 2, CustomerID: "cus_second", SubscriptionID: "sub_second",
		Status: stripe.StatusActive, Plan: "monthly", PriceID: "price_monthly",
		PaymentState: billing.PaymentPaid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("seeding the second team's mirror was rejected by the ordering guard")
	}
	client := stripe.New("sk_test_fake")
	client.BaseURL = server.URL
	handler.Billing.Stripe = client
	handler.CurrentAccount = func(*http.Request) (Account, error) {
		return Account{ID: 2, Email: "owner@example.com"}, nil
	}
	handler.ValidateForm = func(http.ResponseWriter, *http.Request) bool { return true }

	mux := http.NewServeMux()
	handler.Routes(mux)
	form := url.Values{"team": {"999"}}
	request := httptest.NewRequest(http.MethodPost, "/billing/portal", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "https://billing.example/session" {
		t.Fatalf("portal answered %d at %q: %s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if got := posted.Get("customer"); got != "cus_second" {
		t.Errorf("portal customer is %q, want selected account customer", got)
	}
	if got := posted.Get("return_url"); got != "https://feasible.lol/billing?team=2" {
		t.Errorf("portal return URL is %q", got)
	}
}

// TestDocsCoverEveryRequiredTopic is the documentation scope from the
// specification, asserted rather than trusted to a reviewer.
func TestDocsCoverEveryRequiredTopic(t *testing.T) {
	required := []string{
		"installation", "integrations", "script-options", "proxying",
		"dashboard", "metrics", "goals-funnels", "custom-properties",
		"shields", "import-export", "api", "webhooks", "mcp", "sdks",
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

// TestDeletionCopyMatchesOperationalRetention prevents the legal pages from
// promising that the application controls storage operated outside itself.
func TestDeletionCopyMatchesOperationalRetention(t *testing.T) {
	for _, page := range []Doc{
		mustFind(t, documentation, "privacy"),
		mustFind(t, legal, "privacy"),
		mustFind(t, legal, "terms"),
		mustFind(t, legal, "dpa"),
	} {
		body := strings.ToLower(strings.Join(strings.Fields(string(page.Body)), " "))
		for _, required := range []string{"live", "hourly", "outside"} {
			if !strings.Contains(body, required) {
				t.Errorf("%s does not state day-90 %s behavior", page.Slug, required)
			}
		}
		for _, contradiction := range []string{"60 seconds", "replica", "replication"} {
			if strings.Contains(body, contradiction) {
				t.Errorf("%s still claims application-owned storage behavior with %q", page.Slug, contradiction)
			}
		}
	}

	privacy := strings.ToLower(string(mustFind(t, legal, "privacy").Body))
	for _, want := range []string{"payment-provider customer identifier", "minimal tombstone"} {
		if !strings.Contains(privacy, want) {
			t.Errorf("legal privacy copy does not disclose %q", want)
		}
	}
}

// TestPathCleaningDocsDiscloseTheLegacyBoundary keeps the reversible new-write
// guarantee from becoming an impossible recovery promise for upgraded data.
func TestPathCleaningDocsDiscloseTheLegacyBoundary(t *testing.T) {
	body := strings.ToLower(strings.Join(strings.Fields(string(mustFind(t, documentation, "shields").Body)), " "))
	for _, want := range []string{"new traffic keeps its original", "older versions rewrote paths", "cannot be reconstructed"} {
		if !strings.Contains(body, want) {
			t.Errorf("path cleaning docs do not disclose %q", want)
		}
	}
	if strings.Contains(body, "every original path comes back") {
		t.Fatal("path cleaning docs still promise recovery of already-lost originals")
	}
}

// TestDPALinksPublicSubprocessors keeps the legal inventory on the public
// website while retaining the documented hosted and self-hosted boundaries.
func TestDPALinksPublicSubprocessors(t *testing.T) {
	body := strings.ToLower(string(mustFind(t, legal, "dpa").Body))

	for _, want := range []string{
		"https://feasible.lol/legal/subprocessors",
		"legal entities",
		"data category",
		"tls terminates",
		"private-network http",
		"self-hosted operators choose and control",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DPA does not describe %q", want)
		}
	}
	if strings.Contains(body, "no sub-processor receives visitor analytics") {
		t.Fatal("DPA still denies the infrastructure subprocessors receive visitor analytics")
	}
}

// TestProxyDocsRequireHeaderSanitation makes the trusted-proxy boundary
// actionable. The allow-list authenticates the socket peer, while the edge
// still has to remove higher-precedence headers supplied by that peer's client.
func TestProxyDocsRequireHeaderSanitation(t *testing.T) {
	for _, slug := range []string{"proxying", "self-hosting"} {
		body := string(mustFind(t, documentation, slug).Body)
		for _, want := range []string{"FEASIBLE_INGEST_TRUSTED_PROXIES", "strip or overwrite", "right to left"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not state proxy requirement %q", slug, want)
			}
		}
	}
}

// TestNoCompetitorIsNamed keeps comparisons out of the documentation. The one
// exception is the script-options page, where the exact Plausible identifiers
// are part of the migration API and cannot be documented under another name.
func TestNoCompetitorIsNamed(t *testing.T) {
	banned := []string{"plausible", "fathom", "matomo", "simple analytics", "google analytics", "paddle"}

	for _, set := range [][]Doc{documentation, legal} {
		for _, doc := range set {
			body := strings.ToLower(string(doc.Body))

			for _, name := range banned {
				if doc.Slug == "script-options" && name == "plausible" {
					continue
				}

				if strings.Contains(body, name) {
					t.Errorf("%s names %q", doc.Slug, name)
				}
			}
		}
	}
}

// TestTheDocsQuoteTheRealScriptPath is the cheapest guard there is against the
// commonest kind of documentation rot: a snippet somebody can copy, paste, and
// get a 404 from. The path is read from the package that serves it rather than
// written down twice.
func TestTheDocsQuoteTheRealScriptPath(t *testing.T) {
	var quoted bool

	for _, doc := range documentation {
		body := string(doc.Body)

		if strings.Contains(body, tracker.PathLegacy) {
			quoted = true
		}

		// Any other filename under the script prefix is a URL the tracker
		// handler answers with a 404, and a snippet nobody can install.
		for _, fragment := range []string{"/js/f.js", "/js/script.min.js", "/js/feasible.js"} {
			if strings.Contains(body, fragment) {
				t.Errorf("%s tells people to load %s, which is not served", doc.Slug, fragment)
			}
		}
	}

	if !quoted {
		t.Errorf("no documentation page contains the install snippet's script path %q", tracker.PathLegacy)
	}
}

// TestEveryInternalDocLinkResolves walks the cross-references. A link between
// two pages of a documentation set is exactly the kind of thing that rots
// silently: renaming a page leaves a 404 that nobody clicks until a customer
// does.
func TestEveryInternalDocLinkResolves(t *testing.T) {
	link := regexp.MustCompile(`href="/(docs|legal)/([a-z0-9-]+)"`)

	for _, set := range [][]Doc{documentation, legal} {
		for _, doc := range set {
			for _, match := range link.FindAllStringSubmatch(string(doc.Body), -1) {
				target := documentation
				if match[1] == "legal" {
					if match[2] == "subprocessors" {
						continue
					}
					target = legal
				}

				if _, ok := findDoc(target, match[2]); !ok {
					t.Errorf("%s links to /%s/%s, which does not exist", doc.Slug, match[1], match[2])
				}
			}
		}
	}
}

// TestThePrivacyDocsDescribeSharedDailySaltDerivation keeps the public privacy
// story aligned with the stateless implementation used by every ingester.
func TestThePrivacyDocsDescribeSharedDailySaltDerivation(t *testing.T) {
	for _, page := range []Doc{mustFind(t, documentation, "privacy"), mustFind(t, legal, "privacy"), mustFind(t, legal, "dpa")} {
		body := strings.ToLower(string(page.Body))
		for _, disclosure := range []string{"shared", "utc", "not stored"} {
			if !strings.Contains(body, disclosure) {
				t.Errorf("%s does not disclose shared salt detail %q", page.Slug, disclosure)
			}
		}
	}
}

// mustFind fetches one page or fails the test naming it.
func mustFind(t *testing.T, set []Doc, slug string) Doc {
	t.Helper()

	doc, ok := findDoc(set, slug)
	if !ok {
		t.Fatalf("there is no page for %q", slug)
	}

	return doc
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

// TestDocumentationContentHasReusableMobileOverflowRules guards both narrow
// regressions with the shared prose container: tables scroll inside the article
// and long inline code may wrap without widening the 390px viewport.
func TestDocumentationContentHasReusableMobileOverflowRules(t *testing.T) {
	body, err := assetFS.ReadFile("assets/pages.css")
	if err != nil {
		t.Fatal(err)
	}

	css := string(body)
	for _, rule := range []string{
		".docs article { min-width: 0; overflow-wrap: anywhere; }",
		".docs article table",
		"overflow-x: auto",
		"word-break: break-word",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("documentation stylesheet is missing responsive rule %q", rule)
		}
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
