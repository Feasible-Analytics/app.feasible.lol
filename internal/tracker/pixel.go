//
// pixel.go
// The noscript fallback: a 1x1 GIF that records a pageview for a browser with no JavaScript.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package tracker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// PixelPath is where the fallback lives.
//
// It sits under /api/event on purpose. Every proxy configuration we document
// already forwards that prefix to the ingest tier, so the pixel needs no new
// rule anywhere — and a fallback that only works once somebody edits their
// reverse proxy is a fallback most people never get.
const PixelPath = "/api/event/pixel.gif"

// pixelGIF is a 1x1 transparent GIF, the smallest image every browser has
// understood for thirty years. It is a literal rather than a generated image
// because it never changes and forty-three bytes of table is cheaper than a
// dependency.
var pixelGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, // GIF89a
	0x01, 0x00, 0x01, 0x00, // 1x1
	0x80, 0x00, 0x00, // a two-entry global colour table follows
	0x00, 0x00, 0x00, 0xff, 0xff, 0xff, // black, white
	0x21, 0xf9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, // colour 0 is transparent
	0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, // image descriptor
	0x02, 0x02, 0x44, 0x01, 0x00, // the one pixel
	0x3b, // trailer
}

// Pixel serves the noscript fallback by translating a GET into the same event
// the script would have posted.
//
// It wraps the ingest handler rather than reaching into the pipeline, so a
// pixel event and a scripted event go through byte-identical parsing,
// classification and storage. A second path into ingestion is a second set of
// rules that drift apart, and the drift is only ever discovered as two numbers
// that should match and do not.
type Pixel struct {
	// Events is the ingest endpoint's handler.
	Events http.Handler
}

// ServeHTTP records the event and answers with the image.
//
// The image is returned whatever happens, including for a request we could make
// no sense of. A broken-image icon on a customer's page is a visible defect
// they did not cause; the reason we dropped it goes in the same header the
// scripted endpoint uses, so nothing fails silently either.
func (p *Pixel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET the pixel from this endpoint", http.StatusMethodNotAllowed)
		return
	}

	if reason := p.record(r); reason != "" {
		w.Header().Set(ingest.HeaderDropped, reason)
	}

	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Content-Length", "43")

	// Nothing about a beacon may be cached. A cached pixel is a visit that is
	// counted once and then never again for as long as the entry lives, which
	// looks exactly like a site losing its returning visitors.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}

	_, _ = w.Write(pixelGIF)
}

// record builds the event and hands it to the ingest endpoint, returning the
// reason it was not counted, or the empty string.
func (p *Pixel) record(r *http.Request) string {
	if p.Events == nil {
		return "no ingest endpoint"
	}

	body, reason := pixelPayload(r)
	if reason != "" {
		return reason
	}

	// The synthetic request keeps the original's context, address and headers,
	// because every one of them is an input to the derived event: the address
	// becomes the country and half the visitor fingerprint, and the user agent
	// becomes the browser and the bot decision.
	proxied := r.Clone(r.Context())
	proxied.Method = http.MethodPost
	proxied.URL = &url.URL{Path: "/api/event"}
	proxied.RequestURI = ""
	proxied.Body = io.NopCloser(bytes.NewReader(body))
	proxied.ContentLength = int64(len(body))
	proxied.Header.Set("Content-Type", "text/plain")
	proxied.Header.Del("If-None-Match")

	captured := &capture{header: http.Header{}}
	p.Events.ServeHTTP(captured, proxied)

	if dropped := captured.header.Get(ingest.HeaderDropped); dropped != "" {
		return dropped
	}

	if captured.status >= 400 {
		return strings.TrimSpace(captured.body.String())
	}

	return ""
}

// pixelPayload turns the query string into an event body.
//
// The page URL comes from the Referer header when the snippet did not spell it
// out, which is the whole reason this works as a copy-paste one-liner: the
// browser tells us which page the image was loaded from, so the customer does
// not have to template the URL into every page of their site.
//
// The consequence is that the referrer itself is unknowable here — the header
// is already spent on the page URL — so a noscript visitor is Direct unless the
// snippet passes `r` explicitly. That is a real limit of the technique and is
// documented rather than papered over.
func pixelPayload(r *http.Request) ([]byte, string) {
	query := r.URL.Query()

	domain := strings.TrimSpace(query.Get("d"))
	if domain == "" {
		return nil, "pixel is missing d (the site domain)"
	}

	page := strings.TrimSpace(query.Get("u"))
	if page == "" {
		page = strings.TrimSpace(r.Referer())
	}
	if page == "" {
		return nil, "pixel has neither u nor a Referer header to take the page URL from"
	}

	name := strings.TrimSpace(query.Get("n"))
	if name == "" {
		name = ingest.EventPageview
	}

	event := map[string]any{"n": name, "u": page, "d": domain}

	if referrer := strings.TrimSpace(query.Get("r")); referrer != "" {
		event["r"] = referrer
	}

	// Properties arrive as a JSON object in one parameter. The ingest parser
	// already unwraps a JSON document that arrived as a string, so passing it
	// through untouched is both correct and one fewer place to disagree about
	// what a property may hold.
	if props := strings.TrimSpace(query.Get("p")); props != "" {
		event["p"] = props
	}

	body, err := json.Marshal(event)
	if err != nil {
		return nil, "pixel parameters could not be encoded"
	}

	return body, ""
}

// capture is a ResponseWriter that keeps the status and headers and throws the
// body away. The pixel's own response is fixed, so the only thing worth
// carrying out of the ingest handler is why it said no.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header exposes the captured header map.
func (c *capture) Header() http.Header { return c.header }

// WriteHeader records the status the ingest handler chose.
func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// Write keeps a bounded amount of the body, which is the error text when the
// handler refused the event. The cap is there because this is memory allocated
// per request on a path an attacker can call.
func (c *capture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}

	if c.body.Len() < 512 {
		c.body.Write(p)
	}

	return len(p), nil
}
