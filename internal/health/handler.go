//
// handler.go
// The health endpoint, the Allow button, and the test event that round-trips.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// The routes this package answers on.
const (
	PanelPattern     = "/api/sites/{domain}/health"
	AllowPattern     = "/api/sites/{domain}/health/allow-hostname"
	TestEventPattern = "/api/sites/{domain}/health/test-event"
)

// TestEventTimeout bounds the round trip. A test that hangs is worse than one
// that fails: a spinner tells somebody nothing, and a timeout with the address
// it tried tells them their proxy is not answering.
const TestEventTimeout = 10 * time.Second

// TestEventPath is the path a test event is posted to. It is the real ingest
// endpoint, not an internal shortcut, because the entire value of the button is
// that it exercises the same path a visitor's browser uses — the proxy, the
// headers, the derivation and the response — rather than proving that a
// function in this binary can be called.
const TestEventPath = "/api/event"

// Handler serves the ingestion health panel.
type Handler struct {
	Store *Store
	Log   *logger.Logger

	// BaseURL is where a test event is sent. It is the install's public URL so
	// that the round trip goes through whatever is in front of us.
	BaseURL string

	// Client sends the test event, injectable so a test can point it at a
	// fixture rather than at the network.
	Client *http.Client
}

// New builds the handler.
func New(store *Store, baseURL string, log *logger.Logger) *Handler {
	return &Handler{
		Store:   store,
		Log:     log,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: TestEventTimeout},
	}
}

// ServeHTTP routes one request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	if domain == "" {
		h.fail(w, http.StatusBadRequest, "the URL must name a site")

		return
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "/allow-hostname"):
		h.allowHostname(w, r, domain)

	case strings.HasSuffix(r.URL.Path, "/test-event"):
		h.testEvent(w, r, domain)

	default:
		h.panel(w, r, domain)
	}
}

// panel answers the whole health picture for one site.
func (h *Handler) panel(w http.ResponseWriter, r *http.Request, domain string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		h.fail(w, http.StatusMethodNotAllowed, "GET the health panel from this endpoint")

		return
	}

	panel, err := h.Store.Panel(r.Context(), domain)
	if errors.Is(err, ErrUnknownSite) {
		h.fail(w, http.StatusNotFound, "no site is registered for "+domain)

		return
	}
	if err != nil {
		h.internal(w, "read health panel", err)

		return
	}

	h.write(w, http.StatusOK, panel)
}

// allowHostname is the one-click remedy behind the unknown-hostname warning.
func (h *Handler) allowHostname(w http.ResponseWriter, r *http.Request, domain string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.fail(w, http.StatusMethodNotAllowed, "POST a hostname to this endpoint")

		return
	}

	var body struct {
		Hostname string `json:"hostname"`
	}

	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		h.fail(w, http.StatusBadRequest, "the request body must be {\"hostname\": \"...\"}")

		return
	}

	err := h.Store.AllowHostname(r.Context(), domain, body.Hostname)
	if errors.Is(err, ErrUnknownSite) {
		h.fail(w, http.StatusNotFound, "no site is registered for "+domain)

		return
	}
	if err != nil {
		h.internal(w, "allow hostname", err)

		return
	}

	panel, err := h.Store.Panel(r.Context(), domain)
	if err != nil {
		h.internal(w, "read health panel", err)

		return
	}

	// The whole panel comes back rather than an acknowledgement, so the warning
	// disappears from the screen in the same round trip as the click. A button
	// that needs a reload to show its effect is a button people press twice.
	h.write(w, http.StatusOK, panel)
}

// TestEventResult is what the test-event button reports back.
type TestEventResult struct {
	// OK is whether the round trip completed at all. It is separate from the
	// derived fields because "the endpoint did not answer" and "the endpoint
	// answered and dropped it" are entirely different problems.
	OK bool `json:"ok"`

	// URL is where the event was sent, so a failure names the address that
	// failed rather than leaving somebody to guess which hostname we used.
	URL string `json:"url"`

	Status int `json:"status"`

	// DroppedReason is the value of the response's dropped header, empty when
	// the event was accepted.
	DroppedReason string `json:"dropped_reason,omitempty"`

	// Derived is the whole debug view the endpoint answered with: the resolved
	// address, the header it came from, the fingerprint inputs, the parsed
	// user agent and the geolocation. This is the curl output, in the UI.
	Derived map[string]any `json:"derived,omitempty"`

	// Error is why the round trip failed, in a sentence.
	Error string `json:"error,omitempty"`
}

// testEvent sends one event through the public endpoint and reports what came
// back.
//
// It uses the debug header, so nothing is written and the button is safe to
// press against a production site as many times as somebody likes. What comes
// back is everything the pipeline derived — which is exactly the output a
// customer would otherwise have to produce by hand with curl, and which nobody
// should have to.
func (h *Handler) testEvent(w http.ResponseWriter, r *http.Request, domain string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.fail(w, http.StatusMethodNotAllowed, "POST to send a test event")

		return
	}

	result := h.Check(r.Context(), domain)

	// The result is a 200 whatever happened, because a failed round trip is a
	// successful answer to the question that was asked. A 502 here would make
	// the front end show its own error and hide the diagnosis.
	h.write(w, http.StatusOK, result)
}

// Check performs the round trip and reports what came back. It is exported
// because the server-rendered health screen presses the same button as the API
// does, and two implementations of "send a test event" would eventually derive
// two different answers.
func (h *Handler) Check(ctx context.Context, domain string) TestEventResult {
	target := h.BaseURL + TestEventPath

	result := TestEventResult{URL: target}

	payload, err := json.Marshal(map[string]any{
		"n": ingest.EventPageview,
		"d": domain,

		// The path names itself so that a test event which does get written by
		// mistake is obvious in the pages report rather than looking like real
		// traffic. Nothing is written on this path today, and that is one
		// header away from changing.
		"u": "https://" + domain + "/__feasible_test_event",
		"r": "",
		"v": CurrentTrackerVersion,
	})
	if err != nil {
		result.Error = fmt.Sprintf("the test event could not be built: %v", err)

		return result
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		result.Error = fmt.Sprintf("%s is not a URL we can send to: %v", target, err)

		return result
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(ingest.HeaderDebug, "true")
	request.Header.Set("User-Agent", "feasible-health-check/1.0")

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: TestEventTimeout}
	}

	response, err := client.Do(request)
	if err != nil {
		result.Error = fmt.Sprintf("nothing answered at %s: %v — check that the address in your "+
			"configuration is the one your proxy actually serves", target, err)

		return result
	}
	defer func() { _ = response.Body.Close() }()

	result.Status = response.StatusCode
	result.DroppedReason = response.Header.Get(ingest.HeaderDropped)

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		result.Error = fmt.Sprintf("%s answered %d but the body could not be read: %v", target, response.StatusCode, err)

		return result
	}

	if err := json.Unmarshal(body, &result.Derived); err != nil {
		result.Error = fmt.Sprintf("%s answered %d with something that is not our debug output. "+
			"Something in front of us — a proxy, a login page, a firewall — is answering instead.",
			target, response.StatusCode)

		return result
	}

	if reason, ok := result.Derived["drop_reason"].(string); ok && reason != "" {
		result.DroppedReason = reason
	}

	result.OK = response.StatusCode == http.StatusOK

	return result
}

// fail answers a caller's mistake with the reason.
func (h *Handler) fail(w http.ResponseWriter, status int, message string) {
	h.write(w, status, map[string]string{"error": message})
}

// internal answers our mistake. The detail goes to our log, because the caller
// can do nothing with a SQLite error and we can do nothing with a bug report
// that does not name one.
func (h *Handler) internal(w http.ResponseWriter, what string, err error) {
	if h.Log != nil {
		h.Log.Error("a health request failed", "step", what, "error", err)
	}

	h.write(w, http.StatusInternalServerError, map[string]string{"error": "the request could not be answered"})
}

// write encodes a response body.
func (h *Handler) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil && h.Log != nil {
		h.Log.Error("a health response could not be written", "error", err)
	}
}
