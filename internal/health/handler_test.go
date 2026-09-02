//
// handler_test.go
// The endpoint, the Allow button and the test event that round-trips.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// TestTheTestEventReportsWhatWasDerived is the acceptance criterion.
//
// The value of the button is that it goes through the real endpoint — the
// proxy, the headers, the whole pipeline — and comes back with the derived
// event. That is the curl output a customer would otherwise have to produce by
// hand, and nobody should have to.
func TestTheTestEventReportsWhatWasDerived(t *testing.T) {
	f := newFixture(t)

	var seen *http.Request

	// A stand-in for the real ingest endpoint, answering the debug view the
	// pipeline would.
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ingest.Debug{
			ClientIP:       "203.0.113.4",
			ClientIPSource: clientip.SourceForwardedFor,
			SiteDomain:     f.domain,
			Hostname:       f.domain,
			Country:        "GB",
		})
	}))

	t.Cleanup(endpoint.Close)

	handler := New(f.store, endpoint.URL, nil)

	result := handler.Check(context.Background(), f.domain)

	if !result.OK {
		t.Fatalf("the round trip failed: %+v", result)
	}

	if seen == nil {
		t.Fatal("the endpoint was never called")
	}

	if !ingest.IsDebugRequest(seen) {
		t.Fatal("the test event was sent without the debug header, so it would have been written")
	}

	if result.Derived["client_ip_source"] != clientip.SourceForwardedFor {
		t.Fatalf("the derived event is %+v", result.Derived)
	}

	if !strings.HasSuffix(result.URL, TestEventPath) {
		t.Fatalf("the result names %q rather than the ingest endpoint", result.URL)
	}
}

// TestAFailedRoundTripNamesTheAddressItTried checks the diagnosis somebody
// actually needs: which URL did not answer.
func TestAFailedRoundTripNamesTheAddressItTried(t *testing.T) {
	f := newFixture(t)

	// A port nothing is listening on.
	handler := New(f.store, "http://127.0.0.1:1", nil)

	result := handler.Check(context.Background(), f.domain)

	if result.OK {
		t.Fatal("a round trip to a closed port reported success")
	}

	if !strings.Contains(result.Error, "127.0.0.1:1") {
		t.Fatalf("the failure does not name the address: %q", result.Error)
	}
}

// TestSomethingElseAnsweringIsSaidPlainly checks the case where a proxy, a
// login page or a firewall answers instead of us — which looks like success
// from a status code alone.
func TestSomethingElseAnsweringIsSaidPlainly(t *testing.T) {
	f := newFixture(t)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>Please sign in</body></html>"))
	}))

	t.Cleanup(endpoint.Close)

	result := New(f.store, endpoint.URL, nil).Check(context.Background(), f.domain)

	if result.OK {
		t.Fatal("an HTML login page counted as a successful test event")
	}

	if !strings.Contains(result.Error, "in front of us") {
		t.Fatalf("the failure does not explain what happened: %q", result.Error)
	}
}

// TestADroppedTestEventIsReportedAsSuchRatherThanAsAFailure checks the
// distinction between "the endpoint did not answer" and "the endpoint answered
// and would have dropped it" — entirely different problems.
func TestADroppedTestEventIsReportedAsSuchRatherThanAsAFailure(t *testing.T) {
	f := newFixture(t)

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(ingest.HeaderDropped, ingest.ReasonHostnameNotAllowed)
		_ = json.NewEncoder(w).Encode(ingest.Debug{DropReason: ingest.ReasonHostnameNotAllowed})
	}))

	t.Cleanup(endpoint.Close)

	result := New(f.store, endpoint.URL, nil).Check(context.Background(), f.domain)

	if !result.OK {
		t.Fatal("a dropped test event was reported as a failed round trip")
	}

	if result.DroppedReason != ingest.ReasonHostnameNotAllowed {
		t.Fatalf("the drop reason is %q", result.DroppedReason)
	}
}

// TestThePanelEndpointAnswersJSON checks the API the front end reads.
func TestThePanelEndpointAnswersJSON(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: clientip.SourceCloudflare}})

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	handler := New(f.store, "http://localhost:19300", nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/sites/"+f.domain+"/health", nil)
	request.SetPathValue("domain", f.domain)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the panel answered %d: %s", recorder.Code, recorder.Body.String())
	}

	var panel Panel
	if err := json.Unmarshal(recorder.Body.Bytes(), &panel); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if panel.Accepted != 1 || panel.Domain != f.domain {
		t.Fatalf("the panel is %+v", panel)
	}
}

// TestTheAllowEndpointReturnsTheUpdatedPanel checks that the warning disappears
// in the same round trip as the click. A button that needs a reload to show its
// effect is a button people press twice.
func TestTheAllowEndpointReturnsTheUpdatedPanel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "staging.other.example"}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	handler := New(f.store, "http://localhost:19300", nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/api/sites/"+f.domain+"/health/allow-hostname",
		strings.NewReader(`{"hostname":"staging.other.example"}`))
	request.SetPathValue("domain", f.domain)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the allow endpoint answered %d: %s", recorder.Code, recorder.Body.String())
	}

	var panel Panel
	if err := json.Unmarshal(recorder.Body.Bytes(), &panel); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, warning := range panel.Warnings {
		if warning.Code == WarnUnknownHostname {
			t.Fatal("the warning survived the Allow button in the same response")
		}
	}

	if len(panel.AllowedHostnames) != 2 {
		t.Fatalf("the allow-list is %v, want the hostname and the site's own domain", panel.AllowedHostnames)
	}
}

// TestAnUnknownSiteAnswers404 checks the endpoint's answer for a domain nobody
// registered.
func TestAnUnknownSiteAnswers404(t *testing.T) {
	f := newFixture(t)

	handler := New(f.store, "http://localhost:19300", nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/sites/nobody.example/health", nil)
	request.SetPathValue("domain", "nobody.example")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("an unknown site answered %d", recorder.Code)
	}
}
