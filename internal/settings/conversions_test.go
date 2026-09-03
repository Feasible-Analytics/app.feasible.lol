//
// conversions_test.go
// End-to-end form coverage for goal, property, and funnel settings.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// conversionPost submits one settings form through the same CSRF-aware handler as the browser.
func conversionPost(t *testing.T, handler *Handler, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

// TestConversionsPageManagesGoalsPropertiesAndFunnels covers the complete reachable settings workflow.
func TestConversionsPageManagesGoalsPropertiesAndFunnels(t *testing.T) {
	handler, manager := newHandler(t)
	handler.Role = func(*http.Request, sites.Site) teams.Role { return teams.RoleOwner }
	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	first, err := goals.Create(context.Background(), account.Writer(), goals.Goal{
		SiteID: 1, Kind: goals.KindPage, DisplayName: "Pricing viewed", PagePattern: "/pricing",
	}, handler.now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := goals.Create(context.Background(), account.Writer(), goals.Goal{
		SiteID: 1, Kind: goals.KindEvent, DisplayName: "Purchased", EventName: "Purchase",
	}, handler.now())
	if err != nil {
		t.Fatal(err)
	}

	response := get(t, handler, conversionsPath("example.com"))
	if response.Code != http.StatusOK {
		t.Fatalf("conversion settings answered %d", response.Code)
	}
	// The screen, and the chrome that says where the rest of the product is.
	// Account settings and billing are menu rows, so they are asserted at the
	// URLs that menu draws.
	for _, text := range []string{
		"Goals", "Custom properties", "Funnels", "Pricing viewed", "Purchased", "Scroll depth",
		`href="/dashboard/example.com"`,
		`href="/sites?team_id=1&amp;site_context=example.com"`,
		`href="/settings"`,
		`href="/billing?team=1"`,
	} {
		if !strings.Contains(response.Body.String(), text) {
			t.Errorf("conversion settings do not contain %q", text)
		}
	}

	update := conversionPost(t, handler, conversionsPath("example.com")+"/goals/update", url.Values{
		"goal_id": {formatID(first.ID)}, "kind": {string(goals.KindScroll)}, "display_name": {"Read pricing"},
		"page_pattern": {"/pricing"}, "scroll_depth": {"75"},
		"property_name": {"plan", "", ""}, "property_value": {"growth", "", ""},
	})
	if update.Code != http.StatusSeeOther {
		t.Fatalf("goal update answered %d", update.Code)
	}
	updated, err := goals.Get(context.Background(), account.Reader(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Kind != goals.KindScroll || updated.ScrollDepth != 75 || updated.PagePattern != "/pricing" ||
		len(updated.Properties) != 1 || updated.Properties[0].Name != "plan" {
		t.Fatalf("updated goal = %+v", updated)
	}
	if updated.CreatedAt != first.CreatedAt {
		t.Fatalf("goal creation time moved from %d to %d", first.CreatedAt, updated.CreatedAt)
	}

	property := conversionPost(t, handler, conversionsPath("example.com")+"/properties/allow", url.Values{
		"name": {"plan"}, "scope": {string(goals.ScopeSession)},
	})
	if property.Code != http.StatusSeeOther {
		t.Fatalf("property enable answered %d", property.Code)
	}
	properties, err := goals.Allowed(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(properties) != 1 || properties[0].Name != "plan" || properties[0].Scope != goals.ScopeSession {
		t.Fatalf("allowed properties = %+v", properties)
	}

	duplicate := conversionPost(t, handler, conversionsPath("example.com")+"/funnels/save", url.Values{
		"name": {"Broken checkout"}, "mode": {"strict"},
		"goal_id": {formatID(first.ID), formatID(first.ID)},
	})
	if duplicate.Code != http.StatusSeeOther || !strings.Contains(duplicate.Header().Get("Location"), "appears+more+than+once") {
		t.Fatalf("duplicate funnel response = %d %q", duplicate.Code, duplicate.Header().Get("Location"))
	}

	created := conversionPost(t, handler, conversionsPath("example.com")+"/funnels/save", url.Values{
		"name": {"Checkout"}, "mode": {"sequential"},
		"goal_id": {formatID(first.ID), formatID(second.ID)},
	})
	if created.Code != http.StatusSeeOther {
		t.Fatalf("funnel create answered %d", created.Code)
	}
	funnels, err := goals.ListFunnels(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(funnels) != 1 || funnels[0].Name != "Checkout" || len(funnels[0].Steps) != 2 {
		t.Fatalf("funnels = %+v", funnels)
	}

	blockedDelete := conversionPost(t, handler, conversionsPath("example.com")+"/goals/delete", url.Values{
		"goal_id": {formatID(first.ID)},
	})
	if blockedDelete.Code != http.StatusSeeOther || !strings.Contains(blockedDelete.Header().Get("Location"), "remove+it+from+the+funnel+first") {
		t.Fatalf("goal-in-funnel delete response = %d %q", blockedDelete.Code, blockedDelete.Header().Get("Location"))
	}
	if _, err := goals.Get(context.Background(), account.Reader(), first.ID); err != nil {
		t.Fatalf("goal used by a funnel was deleted: %v", err)
	}
}

// TestConversionMutationRequiresCSRF proves a rejected form token cannot change account definitions.
func TestConversionMutationRequiresCSRF(t *testing.T) {
	handler, manager := newHandler(t)
	handler.CheckCSRF = func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "invalid form token", http.StatusForbidden)
		return false
	}

	response := conversionPost(t, handler, conversionsPath("example.com")+"/goals/create", url.Values{
		"kind": {string(goals.KindEvent)}, "event_name": {"Signup"},
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("rejected CSRF form answered %d", response.Code)
	}

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	list, err := goals.List(context.Background(), account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rejected CSRF request created %d goals", len(list))
	}
}

// TestAutomaticGoalsOnlyAllowSafeRenaming proves the UI and the handler keep
// tracker-managed matching rules immutable even when a forged form submits them.
func TestAutomaticGoalsOnlyAllowSafeRenaming(t *testing.T) {
	handler, manager := newHandler(t)
	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	automatic, err := goals.EnsureAutomatic(context.Background(), account.Writer(), 1, handler.now())
	if err != nil {
		t.Fatal(err)
	}
	goal := automatic[0]

	page := get(t, handler, conversionsPath("example.com")).Body.String()
	start := strings.Index(page, ">"+goal.Label()+"<")
	if start < 0 {
		t.Fatalf("automatic goal %q is not rendered", goal.Label())
	}
	end := strings.Index(page[start:], "</details>")
	if end < 0 {
		t.Fatal("automatic goal card is incomplete")
	}
	card := page[start : start+end]
	if !strings.Contains(card, "You can change only the name shown in reports") {
		t.Fatal("automatic goal does not explain its safe edit boundary")
	}
	for _, forbidden := range []string{"Goal type", "Property constraints", "Revenue goal", "Delete goal"} {
		if strings.Contains(card, forbidden) {
			t.Errorf("automatic goal card exposes %q", forbidden)
		}
	}

	response := conversionPost(t, handler, conversionsPath("example.com")+"/goals/update", url.Values{
		"goal_id": {formatID(goal.ID)}, "display_name": {"Missing pages"},
		"kind": {string(goals.KindPage)}, "page_pattern": {"/forged"},
		"is_revenue": {"1"}, "currency": {"USD"},
		"property_name": {"plan"}, "property_value": {"enterprise"},
	})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("automatic goal rename answered %d", response.Code)
	}
	updated, err := goals.Get(context.Background(), account.Reader(), goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DisplayName != "Missing pages" || updated.Kind != goal.Kind || updated.EventName != goal.EventName ||
		updated.PagePattern != goal.PagePattern || updated.IsRevenue != goal.IsRevenue || len(updated.Properties) != 0 {
		t.Fatalf("automatic goal definition changed through a forged edit: %+v", updated)
	}

	response = conversionPost(t, handler, conversionsPath("example.com")+"/goals/delete", url.Values{
		"goal_id": {formatID(goal.ID)},
	})
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "Automatic+goals+cannot+be+deleted") {
		t.Fatalf("automatic goal delete response = %d %q", response.Code, response.Header().Get("Location"))
	}
	if _, err := goals.Get(context.Background(), account.Reader(), goal.ID); err != nil {
		t.Fatalf("automatic goal was deleted: %v", err)
	}
}

// TestUnseenPropertiesDoesNotRescopeConfiguredNames keeps bulk enable additive.
func TestUnseenPropertiesDoesNotRescopeConfiguredNames(t *testing.T) {
	result := unseenProperties([]string{"plan", "campaign", "region"}, []goals.Property{
		{Name: "plan", Scope: goals.ScopeSession},
		{Name: "region", Scope: goals.ScopeEvent},
	})

	if len(result) != 1 || result[0] != "campaign" {
		t.Fatalf("unseen properties = %v, want only campaign", result)
	}
}

// formatID renders a database identifier for an HTML form value.
func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}
