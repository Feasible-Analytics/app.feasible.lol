//
// pipeline_test.go
// Tests for URL parsing, the seven acquisition parameters, and the ordered derive.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// derive runs the pipeline over one payload and returns the debug view, which
// carries every field the event would have been written with.
func derive(t testing.TB, h *harness, body string, headers map[string]string) Debug {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", visitors[0].userAgent)
	req.Header.Set("X-Forwarded-For", visitors[0].ip)
	req.Header.Set(HeaderDebug, "true")

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("debug request returned %d: %s", recorder.Code, recorder.Body.String())
	}

	var debug Debug
	if err := json.NewDecoder(recorder.Body).Decode(&debug); err != nil {
		t.Fatal(err)
	}

	return debug
}

// event body helper, so the tests read as the URL under test rather than as
// JSON punctuation.
func pageview(url string) string {
	return `{"n":"pageview","u":"` + url + `","d":"example.com"}`
}

// TestQueryParametersAreStripped is a privacy guarantee, not a tidiness one. A
// site that puts a session token or an email address in its query string must
// not have us keep it, and the customer cannot un-store it afterwards.
func TestQueryParametersAreStripped(t *testing.T) {
	h := newHandlerHarness(t)

	debug := derive(t, h, pageview("https://example.com/account?email=someone@example.com&session=secret"), nil)

	if debug.Pathname != "/account" {
		t.Fatalf("pathname = %q, want /account with no query string", debug.Pathname)
	}
	if strings.Contains(debug.Pathname, "email") || strings.Contains(debug.Pathname, "secret") {
		t.Fatal("a query parameter survived into the stored path")
	}
}

// TestAcquisitionParameters checks the seven we recognise, and only those seven.
func TestAcquisitionParameters(t *testing.T) {
	h := newHandlerHarness(t)

	url := "https://example.com/landing?utm_source=Newsletter&utm_medium=email&" +
		"utm_campaign=Spring%20Launch&utm_content=header-link&utm_term=web+analytics&unrelated=keepout"

	debug := derive(t, h, pageview(url), nil)

	if debug.UTMSource != "Newsletter" {
		t.Errorf("utm_source = %q, want Newsletter", debug.UTMSource)
	}
	if debug.UTMMedium != "email" {
		t.Errorf("utm_medium = %q, want email", debug.UTMMedium)
	}

	// URI decoding is what makes utm_source=Android%20App display as
	// "Android App" rather than with the escape in it.
	if debug.UTMCampaign != "Spring Launch" {
		t.Errorf("utm_campaign = %q, want %q", debug.UTMCampaign, "Spring Launch")
	}
	if debug.UTMContent != "header-link" {
		t.Errorf("utm_content = %q, want header-link", debug.UTMContent)
	}
	if debug.UTMTerm != "web analytics" {
		t.Errorf("utm_term = %q, want %q", debug.UTMTerm, "web analytics")
	}
	if debug.Channel != "Email" {
		t.Errorf("channel = %q, want Email", debug.Channel)
	}
}

// TestRefAndSourceAliases covers the two short forms the established tracker
// supports, which somebody migrating will already have links in the wild for.
func TestRefAndSourceAliases(t *testing.T) {
	h := newHandlerHarness(t)

	for _, param := range []string{"ref", "source", "utm_source"} {
		debug := derive(t, h, pageview("https://example.com/?"+param+"=partner-site"), nil)

		if debug.UTMSource != "partner-site" {
			t.Errorf("%s: utm_source = %q, want partner-site", param, debug.UTMSource)
		}
	}
}

// TestClickIDKeepsOnlyTheParameterName is the GDPR line. A click id is a unique
// per-click identifier and is not ours to keep without consent, but knowing one
// was there is what separates a paid click from an organic one.
func TestClickIDKeepsOnlyTheParameterName(t *testing.T) {
	h := newHandlerHarness(t)

	debug := derive(t, h, pageview("https://example.com/?gclid=Cj0KCQiA-SOMETHING-UNIQUE"), nil)

	if debug.ClickIDParam != "gclid" {
		t.Fatalf("click_id_param = %q, want gclid", debug.ClickIDParam)
	}

	// The value must not appear in any derived field.
	encoded, err := json.Marshal(debug)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "UNIQUE") {
		t.Fatalf("the click id value survived into the derived event: %s", encoded)
	}
}

// TestGclidImpliesPaidSearch checks the click id alone is enough. Auto-tagged
// advertising often carries no medium at all.
func TestGclidImpliesPaidSearch(t *testing.T) {
	h := newHandlerHarness(t)

	body := `{"n":"pageview","u":"https://example.com/?gclid=abc","d":"example.com","r":"https://www.google.com/"}`
	debug := derive(t, h, body, nil)

	if debug.Channel != "Paid Search" {
		t.Fatalf("channel = %q, want Paid Search", debug.Channel)
	}
}

// TestHostnameDefaults checks the (none) sentinel. It is a fingerprint input, so
// an empty string there would collide with a genuinely missing hostname.
func TestHostnameDefaults(t *testing.T) {
	h := newHandlerHarness(t)

	debug := derive(t, h, pageview("/just-a-path"), nil)

	if debug.Hostname != NoneHostname {
		t.Fatalf("hostname = %q, want %q", debug.Hostname, NoneHostname)
	}
	if debug.RootDomain != NoneHostname {
		t.Fatalf("root_domain = %q, want %q", debug.RootDomain, NoneHostname)
	}
}

// TestTrailingSlashDoesNotSplitAPage checks one page is one row. A trailing
// slash on anything but the root would otherwise double every path on the site.
func TestTrailingSlashDoesNotSplitAPage(t *testing.T) {
	h := newHandlerHarness(t)

	with := derive(t, h, pageview("https://example.com/pricing/"), nil)
	without := derive(t, h, pageview("https://example.com/pricing"), nil)

	if with.Pathname != without.Pathname {
		t.Fatalf("a trailing slash split the page: %q vs %q", with.Pathname, without.Pathname)
	}

	// The root itself keeps its slash, because "" is not a path.
	root := derive(t, h, pageview("https://example.com/"), nil)
	if root.Pathname != "/" {
		t.Fatalf("root pathname = %q, want /", root.Pathname)
	}
}

// TestOverlongPathIsTruncatedAndCounted checks the limit is on the path,
// excluding the domain and query string, and that the cut is reported.
func TestOverlongPathIsTruncatedAndCounted(t *testing.T) {
	h := newHandlerHarness(t)

	long := "https://example.com/" + strings.Repeat("a", MaxURLLength+100) + "?ignored=1"
	debug := derive(t, h, pageview(long), nil)

	if len(debug.Pathname) != MaxURLLength {
		t.Fatalf("pathname is %d characters, want %d", len(debug.Pathname), MaxURLLength)
	}
	if !debug.Truncation.URLTruncated {
		t.Fatal("the truncation was not reported")
	}
}

// TestSubdomainSharesTheVisitor is the fingerprint property, checked through the
// whole pipeline rather than the hash function alone: the derive step has to
// pass the registrable domain, not the hostname.
func TestSubdomainSharesTheVisitor(t *testing.T) {
	h := newHandlerHarness(t)

	root := derive(t, h, pageview("https://example.com/"), nil)
	sub := derive(t, h, pageview("https://app.example.com/"), nil)

	if root.UserID != sub.UserID {
		t.Fatalf("example.com hashed to %d and app.example.com to %d; subdomains must share a visitor",
			root.UserID, sub.UserID)
	}
	if root.RootDomain != "example.com" || sub.RootDomain != "example.com" {
		t.Fatalf("root domains = %q and %q, want example.com for both", root.RootDomain, sub.RootDomain)
	}
}

// TestDifferentAddressIsADifferentVisitor is the other half of the fingerprint
// reaching the pipeline correctly.
func TestDifferentAddressIsADifferentVisitor(t *testing.T) {
	h := newHandlerHarness(t)

	first := derive(t, h, pageview("https://example.com/"), nil)
	second := derive(t, h, pageview("https://example.com/"), map[string]string{
		"X-Forwarded-For": visitors[1].ip,
	})

	if first.UserID == second.UserID {
		t.Fatal("two addresses produced the same visitor id")
	}
}

// TestLanguageComesFromTheHeader checks the first tag of the browser's
// preference order is taken and the rest is discarded.
func TestLanguageComesFromTheHeader(t *testing.T) {
	h := newHandlerHarness(t)

	debug := derive(t, h, pageview("https://example.com/"), map[string]string{
		"Accept-Language": "en-GB,en;q=0.9,fr;q=0.8",
	})

	if debug.Language != "en-GB" {
		t.Fatalf("language = %q, want en-GB", debug.Language)
	}
}

// TestServerSideOverridesAttribution is the thing the incumbent will not do. A
// delayed or offline conversion has no referrer of its own and would otherwise
// be Direct forever.
func TestServerSideOverridesAttribution(t *testing.T) {
	h := newHandlerHarness(t)

	body := `{"n":"purchase","u":"https://example.com/thanks","d":"example.com",
	          "referrer":"https://www.google.com/","utm_source":"google","utm_medium":"cpc","utm_campaign":"spring"}`

	debug := derive(t, h, body, nil)

	if debug.Source != "Google" {
		t.Errorf("source = %q, want Google", debug.Source)
	}
	if debug.Channel != "Paid Search" {
		t.Errorf("channel = %q, want Paid Search", debug.Channel)
	}
	if debug.UTMCampaign != "spring" {
		t.Errorf("utm_campaign = %q, want spring", debug.UTMCampaign)
	}
}

// TestDirectTrafficStoresAnEmptySource checks Direct lands on dimension id 0,
// which every schema column already defaults to. A second synonym for it would
// split the largest bucket on the Sources tab across two rows.
func TestDirectTrafficStoresAnEmptySource(t *testing.T) {
	h := newHandlerHarness(t)

	debug := derive(t, h, pageview("https://example.com/"), nil)

	if debug.Source != "" {
		t.Fatalf("source = %q, want empty for direct traffic", debug.Source)
	}
	if debug.Channel != "Direct" {
		t.Fatalf("channel = %q, want Direct", debug.Channel)
	}
}

// TestDormantAccountIsDropped checks the grace period actually expires. A lapse
// costs dashboard access first and ingestion much later, but it does eventually
// stop.
func TestDormantAccountIsDropped(t *testing.T) {
	h := newHandlerHarness(t)

	site, ok := h.service.Sites.Lookup("example.com")
	if !ok {
		t.Fatal("the fixture site is missing")
	}
	site.AcceptTrafficUntil = fixtureStart.Unix() - 1
	h.service.Sites.Set(site)

	recorder := post(t, h, "text/plain", validBody, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", recorder.Code)
	}
	if got := recorder.Header().Get(HeaderDropped); got != ReasonAccountDormant {
		t.Fatalf("dropped header = %q, want %q", got, ReasonAccountDormant)
	}
}

// TestIPShieldRunsWhileTheAddressExists checks the customer's blocked-IP list is
// applied in the one place the raw address still exists.
func TestIPShieldRunsWhileTheAddressExists(t *testing.T) {
	h := newHandlerHarness(t)
	h.service.Pipeline.Shield = blockEverything{}

	recorder := post(t, h, "text/plain", validBody, nil)

	if got := recorder.Header().Get(HeaderDropped); got != ReasonShieldIP {
		t.Fatalf("dropped header = %q, want %q", got, ReasonShieldIP)
	}
}

// blockEverything is an IPShield that refuses every address, which is enough to
// prove the check runs at the right point in the sequence.
type blockEverything struct{}

// Blocked always blocks.
func (blockEverything) Blocked(int64, netip.Addr) bool { return true }

// TestDeriveNeedsNoGeoDatabase checks the whole pipeline runs with geolocation
// absent, which is what every fresh install looks like before the country
// database is in place.
func TestDeriveNeedsNoGeoDatabase(t *testing.T) {
	h := newHandlerHarness(t)

	debug := derive(t, h, pageview("https://example.com/"), nil)

	if debug.Country != "" {
		t.Fatalf("country = %q with no database, want empty", debug.Country)
	}
	if debug.UserID == 0 {
		t.Fatal("the event was not derived at all")
	}

	// The buffer still accepts it, so a missing optional data file costs a
	// dimension rather than an event.
	if err := h.service.Buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}
