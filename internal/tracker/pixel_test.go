//
// pixel_test.go
// Tests for the noscript 1x1 pixel fallback.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package tracker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// recorderHandler stands in for the ingest endpoint, keeping the body it was
// handed so a test can assert on the event rather than on the pixel.
type recorderHandler struct {
	body    []byte
	headers http.Header
	status  int
	dropped string
}

// ServeHTTP records the request and answers the way the real endpoint would.
func (h *recorderHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.body, _ = io.ReadAll(r.Body)
	h.headers = r.Header.Clone()

	if h.dropped != "" {
		w.Header().Set(ingest.HeaderDropped, h.dropped)
	}

	status := h.status
	if status == 0 {
		status = http.StatusAccepted
	}

	w.WriteHeader(status)
}

// fetchPixel issues one pixel request and returns both sides of it.
func fetchPixel(t *testing.T, target, referer string, events *recorderHandler) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, target, nil)
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (test)")

	recorder := httptest.NewRecorder()
	(&Pixel{Events: events}).ServeHTTP(recorder, request)

	return recorder
}

// decodeEvent reads the event the pixel forwarded.
func decodeEvent(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("the forwarded body is not JSON: %v (%s)", err, body)
	}

	return event
}

// TestPixelTakesTheURLFromTheReferer is what makes the fallback a copy-paste
// one-liner: the browser tells us which page loaded the image, so the customer
// does not have to template a URL into every page of their site.
func TestPixelTakesTheURLFromTheReferer(t *testing.T) {
	events := &recorderHandler{}

	recorder := fetchPixel(t, PixelPath+"?d=example.com", "https://example.com/pricing", events)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}

	if got := recorder.Header().Get("Content-Type"); got != "image/gif" {
		t.Fatalf("content type %q, want image/gif", got)
	}

	if body := recorder.Body.Bytes(); len(body) != len(pixelGIF) {
		t.Fatalf("the response is %d bytes, want a %d byte GIF", len(body), len(pixelGIF))
	}

	event := decodeEvent(t, events.body)

	if event["u"] != "https://example.com/pricing" {
		t.Errorf("url is %v, want the referer", event["u"])
	}
	if event["d"] != "example.com" {
		t.Errorf("domain is %v", event["d"])
	}
	if event["n"] != ingest.EventPageview {
		t.Errorf("event name is %v, want a pageview", event["n"])
	}
}

// TestPixelForwardsAsAnEvent checks the content type the ingest endpoint
// insists on, and that the visitor's own user agent survives the hop — without
// it every noscript visit would be classified as whatever the server looks like.
func TestPixelForwardsAsAnEvent(t *testing.T) {
	events := &recorderHandler{}

	fetchPixel(t, PixelPath+"?d=example.com&u=https://example.com/&r=https://news.example&n=Signup&p=%7B%22plan%22%3A%22pro%22%7D", "", events)

	if got := events.headers.Get("Content-Type"); got != "text/plain" {
		t.Errorf("content type %q, want text/plain", got)
	}
	if got := events.headers.Get("User-Agent"); got != "Mozilla/5.0 (test)" {
		t.Errorf("user agent %q did not survive", got)
	}

	event := decodeEvent(t, events.body)

	if event["n"] != "Signup" {
		t.Errorf("event name is %v", event["n"])
	}
	if event["r"] != "https://news.example" {
		t.Errorf("referrer is %v", event["r"])
	}
	if event["p"] != `{"plan":"pro"}` {
		t.Errorf("props are %v", event["p"])
	}
}

// TestPixelAlwaysReturnsTheImage is the rule that a request we cannot make
// sense of still gets a picture. A broken-image icon on a customer's page is a
// visible defect they did not cause.
func TestPixelAlwaysReturnsTheImage(t *testing.T) {
	events := &recorderHandler{}

	for _, target := range []string{PixelPath, PixelPath + "?d=example.com"} {
		recorder := fetchPixel(t, target, "", events)

		if recorder.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", target, recorder.Code)
		}

		// Never fail silently: the image comes back, and the reason rides
		// along in the same header the scripted endpoint uses.
		if recorder.Header().Get(ingest.HeaderDropped) == "" {
			t.Errorf("%s: no reason was reported for the drop", target)
		}
	}
}

// TestPixelReportsWhyIngestRefused passes the endpoint's own verdict back out,
// so somebody debugging a snippet with curl sees it.
func TestPixelReportsWhyIngestRefused(t *testing.T) {
	events := &recorderHandler{dropped: "bot"}

	recorder := fetchPixel(t, PixelPath+"?d=example.com&u=https://example.com/", "", events)

	if got := recorder.Header().Get(ingest.HeaderDropped); got != "bot" {
		t.Fatalf("dropped header is %q, want the ingest reason", got)
	}
}

// TestPixelIsNeverCached. A cached pixel is a visitor counted once and then
// never again for as long as the entry lives, which looks exactly like a site
// losing all of its returning traffic.
func TestPixelIsNeverCached(t *testing.T) {
	recorder := fetchPixel(t, PixelPath+"?d=example.com", "https://example.com/", &recorderHandler{})

	if got := recorder.Header().Get("Cache-Control"); got == "" || !contains(got, "no-store") {
		t.Fatalf("cache-control is %q", got)
	}
}

// contains is a local substring check, kept here so the test file needs no
// import for one call.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
