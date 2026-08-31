//
// handler_test.go
// Tests for the public endpoint: always 202, never silent, debuggable in one curl.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newHandlerHarness builds a fully wired service for the endpoint tests. It is
// the same wiring the real process uses, so a test here exercises the handler
// against the real pipeline rather than a stand-in.
func newHandlerHarness(t testing.TB) *harness {
	t.Helper()

	dir := t.TempDir()

	return newHarness(t, newControl(t, dir), filepath.Join(dir, "shard"), nil)
}

// post sends a body with a content type and the headers a browser would send.
func post(t testing.TB, h *harness, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", visitors[0].userAgent)
	req.Header.Set("X-Forwarded-For", visitors[0].ip)

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	return recorder
}

// validBody is one ordinary pageview for the fixture site.
const validBody = `{"n":"pageview","u":"https://example.com/pricing","d":"example.com"}`

// TestTextPlainIsAccepted is the compatibility requirement. The official
// trackers send text/plain precisely because it avoids a CORS preflight, and an
// endpoint that rejected it would break every existing integration.
func TestTextPlainIsAccepted(t *testing.T) {
	h := newHandlerHarness(t)

	for _, contentType := range []string{"text/plain", "application/json", "text/plain;charset=UTF-8", ""} {
		recorder := post(t, h, contentType, validBody, nil)

		if recorder.Code != http.StatusAccepted {
			t.Errorf("content type %q: status %d, want 202: %s", contentType, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get(HeaderDropped); got != "" {
			t.Errorf("content type %q: dropped as %q", contentType, got)
		}
	}
}

// TestUnsupportedContentTypeIsRefused checks the one shape we will not read,
// which is a caller sending something that is not an event at all.
func TestUnsupportedContentTypeIsRefused(t *testing.T) {
	h := newHandlerHarness(t)

	if code := post(t, h, "application/xml", validBody, nil).Code; code != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", code)
	}
}

// TestUnknownSiteIsDroppedWithAReason is the core promise: still 202, because a
// beacon can do nothing with a 4xx except retry, but never silent.
func TestUnknownSiteIsDroppedWithAReason(t *testing.T) {
	h := newHandlerHarness(t)

	body := `{"n":"pageview","u":"https://nobody.example/","d":"nobody.example"}`
	recorder := post(t, h, "text/plain", body, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", recorder.Code)
	}
	if got := recorder.Header().Get(HeaderDropped); got != ReasonUnknownSite {
		t.Fatalf("dropped header = %q, want %q", got, ReasonUnknownSite)
	}

	// And it is counted, because a header nobody reads is not visibility.
	snapshot := h.service.Counters.Snapshot()
	if len(snapshot.Dropped) != 1 || snapshot.Dropped[0].Reason != ReasonUnknownSite {
		t.Fatalf("counters = %+v, want one unknown_site drop", snapshot.Dropped)
	}
}

// TestDropReasonsAreAClosedSet checks nothing outside the documented list can
// reach a response. A free-text reason would make the header and the health
// panel unqueryable.
func TestDropReasonsAreAClosedSet(t *testing.T) {
	known := map[string]struct{}{}
	for _, reason := range Reasons {
		known[reason] = struct{}{}
	}

	if len(known) != len(Reasons) {
		t.Fatal("the reason list contains a duplicate")
	}

	for _, reason := range []string{ReasonBot, ReasonDatacenterIP, ReasonReferrerSpam} {
		if !IsClassification(reason) {
			t.Errorf("%q should be a classification, not a deletion", reason)
		}
	}
	for _, reason := range []string{ReasonUnknownSite, ReasonRateLimited, ReasonShieldIP, ReasonNoSessionForEngage} {
		if IsClassification(reason) {
			t.Errorf("%q should be a real drop, not a classification", reason)
		}
	}
}

// TestBotIsClassifiedNotDeleted checks the row is still written. Deleting bot
// traffic before storing it means a wrongly-classified visitor is gone forever
// and a self-hoster is frozen at whatever list their build shipped with.
func TestBotIsClassifiedNotDeleted(t *testing.T) {
	h := newHandlerHarness(t)

	recorder := post(t, h, "text/plain", validBody, map[string]string{
		"User-Agent": "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	})

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", recorder.Code)
	}
	if got := recorder.Header().Get(HeaderDropped); got != ReasonBot {
		t.Fatalf("dropped header = %q, want %q", got, ReasonBot)
	}

	if err := h.service.Buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	var stored int64
	if err := account.Reader().QueryRow(
		"SELECT COUNT(*) FROM events WHERE bot_reason_id = (SELECT id FROM dim_bot_reason WHERE value = 'bot')",
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	if stored != 1 {
		t.Fatalf("stored %d classified rows, want 1 — the row must survive with its reason attached", stored)
	}
}

// TestDebugRequestReturnsEverything checks the one-curl answer to "why is this
// event wrong". It returns 200 with the resolved address, which header it came
// from and every derived field, and writes nothing.
func TestDebugRequestReturnsEverything(t *testing.T) {
	h := newHandlerHarness(t)

	body := `{"n":"pageview","u":"https://example.com/pricing?utm_source=newsletter&utm_medium=email","d":"example.com","r":"https://www.google.com/"}`
	recorder := post(t, h, "text/plain", body, map[string]string{HeaderDebug: "true"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	var debug Debug
	if err := json.NewDecoder(recorder.Body).Decode(&debug); err != nil {
		t.Fatal(err)
	}

	if debug.ClientIP != visitors[0].ip {
		t.Errorf("client_ip = %q, want %q", debug.ClientIP, visitors[0].ip)
	}
	if debug.ClientIPSource != SourceForwardedFor {
		t.Errorf("client_ip_source = %q, want %q", debug.ClientIPSource, SourceForwardedFor)
	}
	if debug.SiteID != 1 || debug.AccountID != 1 {
		t.Errorf("site/account = %d/%d, want 1/1", debug.SiteID, debug.AccountID)
	}
	if debug.UserID == 0 {
		t.Error("the fingerprint is missing from the debug view")
	}
	if debug.RootDomain != "example.com" {
		t.Errorf("root_domain = %q, want example.com", debug.RootDomain)
	}
	if debug.Pathname != "/pricing" {
		t.Errorf("pathname = %q, want /pricing", debug.Pathname)
	}
	if debug.Channel != "Email" {
		t.Errorf("channel = %q, want Email", debug.Channel)
	}
	if debug.Browser != "Chrome" || debug.OS != "macOS" {
		t.Errorf("browser/os = %q/%q, want Chrome/macOS", debug.Browser, debug.OS)
	}

	// A debug request writes nothing, so it is safe to run against production
	// and safe to hand to a customer.
	if err := h.service.Buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.eventCount(t); got != 0 {
		t.Fatalf("a debug request wrote %d events, want 0", got)
	}
}

// TestServerSideCallerIsToldWhatIsMissing is the incumbent's single most common
// support burden, across four separate public issues: a datacentre caller with
// no forwarded address is silently filed as a bot instead of being told.
func TestServerSideCallerIsToldWhatIsMissing(t *testing.T) {
	h := newHandlerHarness(t)
	h.service.Bots.SetDatacenterRanges([]string{"192.0.2.0/24"})

	req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "text/plain")
	req.RemoteAddr = "192.0.2.44:41000"

	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", recorder.Code)
	}

	message := recorder.Body.String()
	for _, want := range []string{HeaderForwardedFor, "User-Agent"} {
		if !strings.Contains(message, want) {
			t.Errorf("the error does not mention %s: %s", want, message)
		}
	}
}

// TestBrowserFromADatacentreIsNotRefused checks the narrowness of that rule. A
// visitor behind a corporate proxy in a datacentre range is a real person, and
// refusing them would be worse than the problem being solved.
func TestBrowserFromADatacentreIsNotRefused(t *testing.T) {
	h := newHandlerHarness(t)
	h.service.Bots.SetDatacenterRanges([]string{"192.0.2.0/24"})

	req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", visitors[0].userAgent)
	req.Header.Set(HeaderForwardedFor, "203.0.113.9")
	req.RemoteAddr = "192.0.2.44:41000"

	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
}

// TestVPNTrafficIsCountedNotDropped checks commercial VPN exits stay real
// traffic. They are datacentre addresses carrying real people, and the
// incumbent dropped months of genuine Mullvad and Proton users this way.
func TestVPNTrafficIsCountedNotDropped(t *testing.T) {
	h := newHandlerHarness(t)
	h.service.Bots.SetDatacenterRanges([]string{"203.0.113.0/24"})

	recorder := post(t, h, "text/plain", validBody, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", recorder.Code)
	}
	if got := recorder.Header().Get(HeaderDropped); got != ReasonDatacenterIP {
		t.Fatalf("dropped header = %q, want %q", got, ReasonDatacenterIP)
	}

	if err := h.service.Buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	var country string
	if err := account.Reader().QueryRow(`
		SELECT c.value FROM sessions s JOIN dim_country c ON c.id = s.country_id
	`).Scan(&country); err != nil {
		t.Fatal(err)
	}

	if country != "Anonymous VPN Service" {
		t.Fatalf("country = %q, want the anonymous VPN bucket", country)
	}
}

// TestEngagementWithNoSessionIsNotWritten checks the one event that can be
// deferred indefinitely. It has no page of its own, so it cannot open a visit.
func TestEngagementWithNoSessionIsNotWritten(t *testing.T) {
	h := newHandlerHarness(t)

	body := `{"n":"engagement","u":"https://example.com/","d":"example.com","e":5000,"sd":40}`
	if code := post(t, h, "text/plain", body, nil).Code; code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", code)
	}

	if err := h.service.Buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := h.eventCount(t); got != 0 {
		t.Fatalf("wrote %d events for an orphaned ping, want 0", got)
	}
}

// TestTruncationIsCounted checks the promise that nothing is cut silently. The
// count is what reaches the customer's ingestion health panel.
func TestTruncationIsCounted(t *testing.T) {
	h := newHandlerHarness(t)

	props := map[string]string{}
	for i := 0; i < MaxProps+5; i++ {
		props[string(rune('a'+i%26))+strings.Repeat("x", i)] = "value"
	}

	encoded, err := json.Marshal(map[string]any{
		"n": "signup",
		"u": "https://example.com/",
		"d": "example.com",
		"p": props,
	})
	if err != nil {
		t.Fatal(err)
	}

	if code := post(t, h, "text/plain", string(encoded), nil).Code; code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", code)
	}

	var dropped int64
	for _, count := range h.service.Counters.Snapshot().Truncations {
		if count.Reason == TruncationProps {
			dropped = count.Count
		}
	}

	if dropped != 5 {
		t.Fatalf("counted %d dropped properties, want 5", dropped)
	}
}

// TestPreflightIsAnswered checks the CORS path. The endpoint is cross-origin by
// definition — it is called from every site we serve — and connect-src in the
// site's own policy is what actually controls access.
func TestPreflightIsAnswered(t *testing.T) {
	h := newHandlerHarness(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/event", nil)
	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("the preflight did not allow the origin")
	}
}

// TestMalformedBodyIsRefused checks a broken caller is told rather than given a
// 202 it will read as success. The person who can fix it is reading the reply.
func TestMalformedBodyIsRefused(t *testing.T) {
	h := newHandlerHarness(t)

	recorder := post(t, h, "text/plain", `{"n":`, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", recorder.Code)
	}
}

// TestSizeTriggeredFlushSurvivesTheRequestEnding drives the production flush
// path over a real server, where the request context is cancelled the instant
// the 202 is written. The buffer's write must not be tied to it: if it is, only
// the ticker ever writes and every size-triggered batch fails and requeues.
func TestSizeTriggeredFlushSurvivesTheRequestEnding(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, newControl(t, dir), filepath.Join(dir, "shard"), nil)

	// A production-shaped buffer: small enough that the size trigger fires,
	// with an interval long enough that only the size trigger can run.
	var flushErr error

	h.service.Buffer = NewBuffer(NewDirect(h.service.Writer), 2, time.Hour)
	h.service.Buffer.OnError = func(err error) { flushErr = err }
	h.service.Handler.Buffer = h.service.Buffer

	server := httptest.NewServer(h.service.Handler)
	defer server.Close()

	h.clock = fixtureStart

	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"n":"pageview","u":"https://example.com/p%d","d":"example.com"}`, i)

		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/event", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("User-Agent", visitors[0].userAgent)
		req.Header.Set("X-Forwarded-For", visitors[0].ip)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// The flush is detached from the request by design, so the test waits for
	// it rather than for a tick that will not come for an hour.
	deadline := time.Now().Add(5 * time.Second)
	for h.eventCount(t) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := h.eventCount(t); got != 2 {
		t.Fatalf("the size-triggered flush wrote %d of 2 events (buffer holds %d, error: %v)",
			got, h.service.Buffer.Len(), flushErr)
	}
}

// TestInternalFailureIsStillA202 checks our own outage does not become the
// sender's error. A salt store that will not open makes the fingerprint
// impossible, and a 4xx would have every tracker retrying a request that cannot
// succeed.
func TestInternalFailureIsStillA202(t *testing.T) {
	h := newHandlerHarness(t)

	key, err := salts.LoadKey(t.TempDir(), fixtureSaltKey)
	if err != nil {
		t.Fatal(err)
	}

	// A store over a database that is already closed is the cheapest honest
	// version of "the salts are unavailable".
	closed, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}

	broken, err := salts.NewStore(closed, key)
	if err != nil {
		t.Fatal(err)
	}
	h.service.Pipeline.Salts = broken

	recorder := post(t, h, "text/plain", validBody, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 — our failure must never reach the tracker as a 4xx", recorder.Code)
	}
	if got := recorder.Header().Get(HeaderDropped); got != ReasonInternalError {
		t.Fatalf("dropped header = %q, want %q", got, ReasonInternalError)
	}

	var counted int64
	for _, count := range h.service.Counters.Snapshot().Dropped {
		if count.Reason == ReasonInternalError {
			counted = count.Count
		}
	}
	if counted != 1 {
		t.Fatalf("counted %d internal failures, want 1 — an outage nobody counts is an outage nobody fixes", counted)
	}
}

// TestUnreadablePropsAreDroppedWithAReason checks a props object we cannot read
// is a counted drop rather than a 400. The sender is a beacon: a status code it
// cannot act on produces a retry that fails in exactly the same way.
func TestUnreadablePropsAreDroppedWithAReason(t *testing.T) {
	h := newHandlerHarness(t)

	body := `{"n":"pageview","u":"https://example.com/","d":"example.com","p":"{not json"}`
	recorder := post(t, h, "text/plain", body, nil)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202", recorder.Code)
	}
	if got := recorder.Header().Get(HeaderDropped); got != ReasonInvalidPayload {
		t.Fatalf("dropped header = %q, want %q", got, ReasonInvalidPayload)
	}

	var counted int64
	for _, count := range h.service.Counters.Snapshot().Dropped {
		if count.Reason == ReasonInvalidPayload {
			counted = count.Count
		}
	}
	if counted != 1 {
		t.Fatalf("counted %d invalid payloads, want 1", counted)
	}
}

// TestDebugAnswersADroppedEvent is the one curl a customer runs, and they run
// it precisely because their event did not count. Answering it with the reason
// and nothing else answers the easy half of the question.
func TestDebugAnswersADroppedEvent(t *testing.T) {
	h := newHandlerHarness(t)
	h.service.Pipeline.Shield = blockEverything{}

	body := `{"n":"pageview","u":"https://example.com/pricing?utm_source=newsletter&utm_medium=email","d":"example.com","r":"https://www.google.com/"}`
	recorder := post(t, h, "text/plain", body, map[string]string{HeaderDebug: "true"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	var debug Debug
	if err := json.NewDecoder(recorder.Body).Decode(&debug); err != nil {
		t.Fatal(err)
	}

	if debug.DropReason != ReasonShieldIP {
		t.Fatalf("drop_reason = %q, want %q", debug.DropReason, ReasonShieldIP)
	}

	// Everything derived up to the drop has to be there, or the answer is "it
	// was dropped" and the customer is no further forward.
	if debug.UserID == 0 {
		t.Error("the fingerprint is missing from a dropped event's debug view")
	}
	if debug.Pathname != "/pricing" {
		t.Errorf("pathname = %q, want /pricing", debug.Pathname)
	}
	if debug.Channel != "Email" {
		t.Errorf("channel = %q, want Email", debug.Channel)
	}
	if debug.Browser != "Chrome" {
		t.Errorf("browser = %q, want Chrome", debug.Browser)
	}

	// An unknown domain never reaches a site id, and the debug view still has
	// to describe what we saw.
	unknown := post(t, h, "text/plain", `{"n":"pageview","u":"https://nobody.example/x","d":"nobody.example"}`,
		map[string]string{HeaderDebug: "true"})

	var missing Debug
	if err := json.NewDecoder(unknown.Body).Decode(&missing); err != nil {
		t.Fatal(err)
	}

	if missing.DropReason != ReasonUnknownSite {
		t.Fatalf("drop_reason = %q, want %q", missing.DropReason, ReasonUnknownSite)
	}
	if missing.Hostname != "nobody.example" || missing.Pathname != "/x" {
		t.Fatalf("hostname/pathname = %q/%q, want nobody.example and /x", missing.Hostname, missing.Pathname)
	}
	if missing.UserID == 0 {
		t.Error("the fingerprint is missing from an unknown site's debug view")
	}
}

// TestGetIsRefused checks the endpoint says what it wants rather than silently
// doing nothing.
func TestGetIsRefused(t *testing.T) {
	h := newHandlerHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/api/event", nil)
	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", recorder.Code)
	}
}
