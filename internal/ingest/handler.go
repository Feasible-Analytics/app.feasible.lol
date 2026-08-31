//
// handler.go
// POST /api/event: always 202, never silent, and debuggable in one curl.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// HeaderDropped carries the reason an event was not counted. Answering 202 and
// saying nothing is what makes analytics data loss invisible for weeks; the
// status code has to stay 202 so a beacon does not retry, so the explanation
// goes in a header instead.
const HeaderDropped = "x-feasible-dropped"

// HeaderDebug asks for the derived event back instead of a write. It exists so
// that "my numbers look wrong" can be answered with one curl by the person who
// owns the proxy, rather than by us reading logs they cannot see.
const HeaderDebug = "X-Debug-Request"

// IsDebugRequest reports whether this request wants the derived event back
// instead of a write. Both the handler and the pipeline ask, because the answer
// changes how far the pipeline derives a dropped event: a debug request is
// worth the extra work, an ordinary one is not.
func IsDebugRequest(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(HeaderDebug)), "true")
}

// Handler serves the public ingest endpoint.
type Handler struct {
	Pipeline *Pipeline
	Buffer   *Buffer
	Counters *Counters
	Log      *logger.Logger
}

// ServeHTTP accepts one event. It answers 202 for everything it understood,
// including events it decided to drop, because the sender is a beacon that
// cannot do anything useful with a failure and would only retry.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The tracker is a cross-origin request from every site we serve, so the
	// endpoint is open by definition. `connect-src` in the site's own CSP is
	// what actually controls who may call it.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, x-debug-request")
	w.Header().Set("Access-Control-Max-Age", "86400")

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "POST an event to this endpoint", http.StatusMethodNotAllowed)
		return
	}

	if !acceptableContentType(r.Header.Get("Content-Type")) {
		// text/plain is not a nicety. The official trackers use it precisely
		// because it avoids a CORS preflight, and rejecting it breaks every
		// existing integration on the internet.
		http.Error(w, "content type must be application/json or text/plain", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}

	payload, err := ParsePayload(body)
	if err != nil {
		// A malformed body is a programming error in whatever sent it, and the
		// only person who can fix it is reading this response.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A server-side caller that forgot to forward the visitor's address and
	// user agent gets told exactly that. Silently classifying it as a
	// datacentre bot instead is the single most common support burden the
	// incumbent has, across four separate public issues.
	if problem := h.serverSideCallerProblem(r); problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}

	result, err := h.Pipeline.Derive(r.Context(), r, payload)
	if err != nil && h.Log != nil {
		// The sender can do nothing about a salt store that will not open or a
		// props object it cannot see, so the detail belongs in our log and only
		// the reason travels back.
		h.Log.Error("event could not be derived", "domain", payload.Domain, "error", err)
	}

	// The debug view is answered before anything is written, so running it
	// against production is free of side effects and safe to hand to a customer.
	// It is answered even when the event was dropped or failed to derive: the
	// one curl somebody runs is the one about an event that did not count, and
	// it has to come back with the drop reason and everything derived so far.
	if IsDebugRequest(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result.Debug)
		return
	}

	if result.Truncation.Any() {
		// Never silently truncate. The count is what appears on the ingestion
		// health panel, and it is the whole difference between our behaviour
		// and the mystery of a thirty-first property that simply vanishes.
		h.Counters.Truncated(result.Debug.SiteID, result.Truncation)
	}

	if result.DropReason != "" || result.Event == nil {
		// A derive that produced no event and no reason is a bug on our side
		// rather than anything the sender did, and it still answers 202: a
		// beacon given a 4xx retries, and the retry would fail the same way.
		reason := result.DropReason
		if reason == "" {
			reason = ReasonInternalError
		}

		h.drop(w, result.Debug.SiteID, payload.Domain, reason)
		return
	}

	event := result.Event

	// A classified event is still written, with its reason attached, so a
	// customer can toggle it back on. What the sender is told is the same
	// either way: here is what we thought this was.
	if event.BotReason != "" {
		w.Header().Set(HeaderDropped, event.BotReason)
		h.Counters.Dropped(event.SiteID, event.BotReason)
	}

	h.Buffer.Add(*event)
	h.Counters.Accepted(event.SiteID)

	if h.Log != nil {
		h.Log.EventReceived(payload.Domain, itoa(event.SiteID), itoa(int64(event.Shard)), event.BotReason)

		if h.Log.TraceEventsEnabled() {
			h.Log.TraceEvent(
				"domain", payload.Domain,
				"site", event.SiteID,
				"shard", event.Shard,
				"name", event.Name,
				"user_id", event.UserID,
				"pathname", event.Pathname,
				"source", event.Source,
				"channel", event.Channel,
				"country", event.Country,
				"browser", event.Browser,
				"os", event.OS,
				"device", event.DeviceType,
				"client_ip_source", result.Debug.ClientIPSource,
			)
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

// drop answers a dropped event. It still returns 202 — a beacon that got a 4xx
// would retry and change nothing — with the reason in a header and a counter
// the customer can see.
func (h *Handler) drop(w http.ResponseWriter, siteID int64, domain, reason string) {
	w.Header().Set(HeaderDropped, reason)
	h.Counters.Dropped(siteID, reason)

	if h.Log != nil {
		h.Log.EventReceived(domain, itoa(siteID), "", reason)
	}

	w.WriteHeader(http.StatusAccepted)
}

// serverSideCallerProblem returns the message to send a server-side caller that
// is missing what we need, or the empty string when the request is fine. The
// check is narrow on purpose: it only fires when the request arrived straight
// from a datacentre address with no forwarding header at all, which is exactly
// the shape of an API integration and nothing like a real visitor.
func (h *Handler) serverSideCallerProblem(r *http.Request) string {
	if h.Pipeline == nil || h.Pipeline.Bots == nil {
		return ""
	}

	client := ResolveClientIP(r, h.Pipeline.Trusted)
	if client.Source != SourceSocket {
		return ""
	}

	if !h.Pipeline.Bots.IsDatacenterIP(client.Addr) {
		return ""
	}

	var missing []string
	if r.Header.Get(HeaderForwardedFor) == "" && r.Header.Get(HeaderCFConnectingIP) == "" {
		missing = append(missing, HeaderForwardedFor)
	}
	if r.Header.Get("User-Agent") == "" {
		missing = append(missing, "User-Agent")
	}

	if len(missing) == 0 {
		return ""
	}

	return "this request arrived from a datacentre address with no " +
		strings.Join(missing, " and no ") +
		". A server-side caller must forward the visitor's real " + HeaderForwardedFor +
		" and User-Agent, or every event will be attributed to your server rather than to the visitor."
}

// acceptableContentType reports whether we will read this body. Both types are
// accepted and an absent type is accepted too, because a beacon sent with
// navigator.sendBeacon does not always set one and refusing it would lose the
// last pageview of every visit.
func acceptableContentType(value string) bool {
	if value == "" {
		return true
	}

	media, _, _ := strings.Cut(value, ";")
	media = strings.ToLower(strings.TrimSpace(media))

	switch media {
	case "application/json", "text/plain", "application/x-www-form-urlencoded":
		return true
	}

	return false
}

// itoa formats an id for a log line without pulling strconv into every call
// site's imports.
func itoa(value int64) string {
	if value == 0 {
		return ""
	}

	var buf [20]byte
	i := len(buf)
	negative := value < 0
	if negative {
		value = -value
	}

	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}

	if negative {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}
