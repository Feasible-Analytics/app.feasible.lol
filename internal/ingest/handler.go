//
// handler.go
// POST /api/event: durable 202s, retryable storage failures, and one-curl debug.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"encoding/json"
	"errors"
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

	// Limiter caps how fast one source address may send without ever persisting
	// that address outside process memory.
	Limiter *RateLimiter

	// Durable makes a successful response wait for the account transaction. A
	// 202 is a durability acknowledgement, not merely a parsing acknowledgement.
	Durable bool

	// Observer, when set, is handed the derived view of every request. It is
	// what turns the counters into a health panel that can name the hostname,
	// the header and the tracker version behind a number. A nil Observer costs
	// one comparison per event.
	Observer Observer
}

// observe hands one request to the health observer, if there is one. It is a
// method rather than an inline call because every exit path in ServeHTTP has to
// report — an observation only emitted on the happy path would produce a health
// panel that is blind to exactly the requests somebody opens it to explain.
func (h *Handler) observe(payload *Payload, result Result, userAgent string, accepted, pending bool) {
	if h.Observer == nil {
		return
	}

	h.Observer.Observe(Observation{
		SiteID:         result.Debug.SiteID,
		AccountID:      result.Debug.AccountID,
		ReceivedAt:     result.Debug.Timestamp,
		Debug:          result.Debug,
		DropReason:     result.Debug.DropReason,
		Accepted:       accepted,
		Pending:        pending,
		UserAgent:      userAgent,
		TrackerVersion: payload.TrackerVersion(),
		Truncation:     result.Truncation,
	})
}

// ServeHTTP accepts one event. Policy drops answer 202 because retrying cannot
// change them; dependency and storage failures answer 503 so the browser keeps
// its durable copy for a later attempt.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The tracker is a cross-origin request from every site we serve, so the
	// endpoint is open by definition. `connect-src` in the site's own CSP is
	// what actually controls who may call it.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, x-debug-request")
	w.Header().Set("Access-Control-Expose-Headers", HeaderDropped)
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

	// Apply the per-source ceiling before parsing and derivation. It remains a
	// named 202 drop because a browser beacon cannot act usefully on a 429.
	if h.Limiter != nil && !h.Limiter.Allow(ResolveClientIP(r, h.Pipeline.Trusted).Addr) {
		h.drop(w, 0, "", ReasonRateLimited)
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

	if errors.Is(err, ErrSaltUnavailable) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "ingest identity service is temporarily unavailable", http.StatusServiceUnavailable)
		return
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

	hostnameSuspect := result.DropReason == ReasonHostnameNotAllowed && result.Event != nil

	if (result.DropReason != "" && !hostnameSuspect) || result.Event == nil {
		// A derive that produced no event and no reason is a bug on our side
		// rather than anything the sender did, and it still answers 202: a
		// beacon given a 4xx retries, and the retry would fail the same way.
		reason := result.DropReason
		if reason == "" {
			reason = ReasonInternalError
		}

		result.Debug.DropReason = reason
		h.observe(payload, result, r.Header.Get("User-Agent"), false, false)

		h.drop(w, result.Debug.SiteID, payload.Domain, reason)
		return
	}

	event := result.Event

	if h.Durable {
		if err := h.Buffer.AddAndWait(r.Context(), *event); err != nil {
			if h.Log != nil {
				h.Log.Error("event could not be persisted before the response", "domain", payload.Domain, "error", err)
			}

			http.Error(w, "event could not be persisted", http.StatusServiceUnavailable)
			return
		}
	} else {
		h.Buffer.Add(*event)
	}

	// A classified event is still written, with its reason attached, so a
	// customer can toggle it back on. Count and report the classification only
	// after the same durability boundary as every other accepted event.
	if event.BotReason != "" {
		w.Header().Set(HeaderDropped, event.BotReason)
		h.Counters.Dropped(event.SiteID, event.BotReason)
	}
	h.Counters.Accepted(event.SiteID)
	h.observe(payload, result, r.Header.Get("User-Agent"), false, true)

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
// check is narrow on purpose: it only fires when the resolved request arrived
// straight from a datacentre address without a usable forwarded address, which
// is exactly the shape of a misconfigured API integration.
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
	if client.Source == SourceSocket {
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
