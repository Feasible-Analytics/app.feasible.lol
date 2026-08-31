//
// sites_test.go
// Provisioning: create a site, configure it, share it, and take it away again.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSiteLifecycle walks the whole of what an agency provisioning a client
// does, in one test, because the steps only mean anything in sequence: creating
// a site that cannot then be queried is not a working create.
func TestSiteLifecycle(t *testing.T) {
	h := newHarness(t)

	status, body := h.post(t, "/api/v1/sites",
		`{"domain":"Fresh.Example.COM","display_name":"Fresh","timezone":"America/Los_Angeles"}`)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d (%s)", status, body)
	}

	created := decode(t, body)

	// The domain is normalised on the way in, so that a site created as
	// "WWW.Example.com" and a tracker snippet that says "example.com" are one
	// site rather than two — registering them separately is a silent, total data
	// loss for whichever one is not in the routing map.
	if created["domain"] != "fresh.example.com" {
		t.Errorf("domain = %v, want it normalised", created["domain"])
	}

	if created["timezone"] != "America/Los_Angeles" {
		t.Errorf("timezone = %v", created["timezone"])
	}

	// The routing snapshot has to be rebuilt immediately: a script installed the
	// moment after this call returns must already be accepted, and waiting out a
	// refresh interval looks exactly like a broken snippet.
	status, body = h.post(t, "/api/v2/query",
		`{"site_id":"fresh.example.com","metrics":["visitors"],"date_range":"7d"}`)
	if status != http.StatusOK {
		t.Fatalf("the new site was not queryable straight away: %d (%s)", status, body)
	}

	status, body = h.get(t, "/api/v1/sites/fresh.example.com")
	if status != http.StatusOK {
		t.Fatalf("get status = %d (%s)", status, body)
	}

	status, body = h.do(t, http.MethodPut, "/api/v1/sites/fresh.example.com",
		`{"display_name":"Renamed","is_public":true}`, h.Key)
	if status != http.StatusOK {
		t.Fatalf("update status = %d (%s)", status, body)
	}

	updated := decode(t, body)

	if updated["display_name"] != "Renamed" || updated["is_public"] != true {
		t.Errorf("update did not take: %s", body)
	}

	// A field left out of an update is untouched, which is why every optional
	// field is a pointer. With plain fields, renaming a site would also reset
	// its timezone and silently move every past day.
	if updated["timezone"] != "America/Los_Angeles" {
		t.Errorf("an unmentioned field was overwritten: %s", body)
	}

	status, body = h.do(t, http.MethodDelete, "/api/v1/sites/fresh.example.com", "", h.Key)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d (%s)", status, body)
	}

	if status, _ := h.get(t, "/api/v1/sites/fresh.example.com"); status != http.StatusNotFound {
		t.Fatalf("the deleted site still answers: %d", status)
	}
}

// TestDuplicateDomainIsAConflict checks that registering a domain twice is a
// status the caller can act on rather than a driver error.
func TestDuplicateDomainIsAConflict(t *testing.T) {
	h := newHarness(t)

	status, body := h.post(t, "/api/v1/sites", `{"domain":"example.com"}`)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", status, body)
	}
}

// TestListSitesPaginatesAtAHundred checks the default page size and that the
// listing only ever shows the key's own team.
func TestListSitesPaginatesAtAHundred(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/sites")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	answer := decode(t, body)

	meta := answer["meta"].(map[string]any)
	if meta["limit"].(float64) != DefaultPageSize {
		t.Errorf("default limit = %v, want %d", meta["limit"], DefaultPageSize)
	}

	list := answer["sites"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d sites, want only this team's one", len(list))
	}

	if list[0].(map[string]any)["domain"] != "example.com" {
		t.Errorf("listed somebody else's site: %s", body)
	}
}

// TestTrackerConfigRoundTrips checks the per-site script configuration and the
// snippet built from it, which is the thing a customer actually copies.
func TestTrackerConfigRoundTrips(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/sites/example.com/tracker")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	status, body = h.do(t, http.MethodPut, "/api/v1/sites/example.com/tracker",
		`{"hash_routing":true,"outbound_links":true,"excluded_pages":"/admin/**","file_types":"pdf,zip",
		  "api_endpoint":"https://stats.example.com/api/event"}`, h.Key)
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	answer := decode(t, body)

	snippet, _ := answer["snippet"].(string)

	for _, expected := range []string{
		`data-domain="example.com"`,
		`data-hash`,
		`data-exclude="/admin/**"`,
		`data-api="https://stats.example.com/api/event"`,
	} {
		if !strings.Contains(snippet, expected) {
			t.Errorf("snippet is missing %s: %s", expected, snippet)
		}
	}

	status, body = h.get(t, "/api/v1/sites/example.com/tracker")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	config := decode(t, body)["config"].(map[string]any)
	if config["hash_routing"] != true || config["file_types"] != "pdf,zip" {
		t.Errorf("the configuration did not persist: %s", body)
	}
}

// TestCustomPropertiesAreAnAllowList checks the property allow list, including
// that declaring the same property twice is a success. An integration that
// declares its properties on every deploy must not have to remember which ones
// it declared last time.
func TestCustomPropertiesAreAnAllowList(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < 2; i++ {
		status, body := h.do(t, http.MethodPut, "/api/v1/sites/custom-props",
			`{"site_id":"example.com","key":"plan"}`, h.Key)
		if status != http.StatusCreated {
			t.Fatalf("attempt %d: status = %d (%s)", i+1, status, body)
		}
	}

	status, body := h.get(t, "/api/v1/sites/custom-props?site_id=example.com")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Props []struct {
			ID  int64  `json:"id"`
			Key string `json:"key"`
		} `json:"custom_props"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	if len(answer.Props) != 1 || answer.Props[0].Key != "plan" {
		t.Fatalf("props = %+v, want exactly one", answer.Props)
	}

	status, body = h.do(t, http.MethodDelete,
		"/api/v1/sites/custom-props/"+itoa(answer.Props[0].ID)+"?site_id=example.com", "", h.Key)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d (%s)", status, body)
	}
}

// TestSharedLinkIsUnguessable checks that a link's slug is random rather than
// derived from its name. A shared link is a capability — anybody with the URL
// sees the dashboard — so a slug anybody can guess from the site's name is a
// public dashboard nobody meant to publish.
func TestSharedLinkIsUnguessable(t *testing.T) {
	h := newHarness(t)

	first := h.createSharedLink(t, "Client view")
	second := h.createSharedLink(t, "Client view")

	if first == second {
		t.Fatalf("two links with the same name got the same slug: %q", first)
	}

	if strings.Contains(strings.ToLower(first), "client") {
		t.Errorf("the slug is derived from the name: %q", first)
	}

	if len(first) < 16 {
		t.Errorf("slug %q is too short to be unguessable", first)
	}

	status, body := h.get(t, "/api/v1/sites/shared-links?site_id=example.com")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	if !strings.Contains(string(body), "https://example.test/share/") {
		t.Errorf("a listed link must carry the URL somebody opens: %s", body)
	}
}

// createSharedLink mints a link and returns its slug.
func (h *harness) createSharedLink(t *testing.T, name string) string {
	t.Helper()

	status, body := h.do(t, http.MethodPut, "/api/v1/sites/shared-links",
		`{"site_id":"example.com","name":"`+name+`"}`, h.Key)
	if status != http.StatusCreated {
		t.Fatalf("status = %d (%s)", status, body)
	}

	slug, _ := decode(t, body)["slug"].(string)

	return slug
}

// TestGuestsAndMemberships checks the two access endpoints, including the
// refusal for somebody who has no account. Creating one as a side effect of an
// API call would mint a passwordless account in somebody else's name.
func TestGuestsAndMemberships(t *testing.T) {
	h := newHarness(t)

	status, body := h.do(t, http.MethodPut, "/api/v1/sites/guests",
		`{"site_id":"example.com","email":"guest@example.test","role":"guest_viewer"}`, h.Key)
	if status != http.StatusCreated {
		t.Fatalf("status = %d (%s)", status, body)
	}

	status, body = h.do(t, http.MethodPut, "/api/v1/sites/guests",
		`{"site_id":"example.com","email":"nobody@example.test"}`, h.Key)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d for an address with no account, want 404 (%s)", status, body)
	}

	status, body = h.get(t, "/api/v1/sites/guests?site_id=example.com")
	if status != http.StatusOK || !strings.Contains(string(body), "guest@example.test") {
		t.Fatalf("status = %d (%s)", status, body)
	}

	status, body = h.get(t, "/api/v1/teams/memberships")
	if status != http.StatusOK || !strings.Contains(string(body), "owner@example.test") {
		t.Fatalf("status = %d (%s)", status, body)
	}
}

// TestLastOwnerCannotBeRemoved checks the one membership rule that is not a
// preference. A team with no owner cannot be administered by anybody, billing
// included, and the only recovery is a support ticket to us.
func TestLastOwnerCannotBeRemoved(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/teams/memberships")
	if status != http.StatusOK {
		t.Fatalf("status = %d (%s)", status, body)
	}

	var answer struct {
		Memberships []struct {
			ID   int64  `json:"id"`
			Role string `json:"role"`
		} `json:"memberships"`
	}

	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}

	status, body = h.do(t, http.MethodDelete,
		"/api/v1/teams/memberships/"+itoa(answer.Memberships[0].ID), "", h.Key)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", status, body)
	}
}

// TestGoalsSayTheyAreMissingRatherThanVanishing checks the seam where this API
// meets a feature it does not own.
//
// A 404 would tell an integrator their URL is wrong and send them to check
// their own code. A 501 naming the feature tells them the truth, which is that
// the route is right and the thing behind it is not built.
func TestGoalsSayTheyAreMissingRatherThanVanishing(t *testing.T) {
	h := newHarness(t)

	status, body := h.get(t, "/api/v1/sites/goals?site_id=example.com")
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", status, body)
	}

	message, _ := decode(t, body)["error"].(string)
	if !strings.Contains(message, "goals") {
		t.Errorf("the refusal must name the feature: %q", message)
	}
}

// itoa renders an id for a URL.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}

	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}

	return digits
}
