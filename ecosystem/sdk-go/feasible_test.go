//
// feasible_test.go
// Everything the client promises, proved against a real HTTP server on an ephemeral port.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package feasible

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// uuidV4 is the only shape the server accepts in the idempotency field.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// capture is what one recorded request looked like on the wire. The body is
// kept as bytes rather than a decoded map so a test can assert on the exact
// keys, which is the part of the contract that cannot drift without breaking
// every existing integration.
type capture struct {
	body        []byte
	contentType string
	forwarded   string
	userAgent   string
	debug       string
}

// newTestClient builds a client pointed at a test server, with a backoff short
// enough that the retry tests run in milliseconds rather than seconds.
func newTestClient(t *testing.T, host string, mutate func(*Options)) *Client {
	t.Helper()

	opts := Options{
		Domain:      "example.com",
		Host:        host,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  2 * time.Millisecond,
	}

	if mutate != nil {
		mutate(&opts)
	}

	client, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return client
}

// recordingServer answers every request with the given handler and hands back
// what it saw. It is a real listener because the transport, the headers and the
// content type are exactly what these tests exist to check, and a stubbed
// RoundTripper would let a bug in any of them through.
func recordingServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]capture) {
	t.Helper()

	seen := make([]capture, 0, 4)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		seen = append(seen, capture{
			body:        body,
			contentType: r.Header.Get("Content-Type"),
			forwarded:   r.Header.Get("X-Forwarded-For"),
			userAgent:   r.Header.Get("User-Agent"),
			debug:       r.Header.Get(HeaderDebug),
		})

		handler(w, r)
	}))

	t.Cleanup(server.Close)

	return server, &seen
}

// keysOf lists the top-level JSON keys of a body, sorted. Asserting the whole
// key set rather than a handful of fields is what catches an accidental null or
// a stray zero-valued key being added to the wire shape.
func keysOf(t *testing.T, body []byte) []string {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v (%s)", err, body)
	}

	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// decode reads a captured body into a map for value assertions.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body is not valid JSON: %v (%s)", err, body)
	}

	return out
}

// TestPageviewWire pins the minimal pageview: three keys, nothing else, and the
// two headers the endpoint refuses a server-side call without.
func TestPageviewWire(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)

	visitor := NewVisitor("203.0.113.9", "Mozilla/5.0 (Macintosh)")

	result, err := client.Pageview(context.Background(), visitor, "https://example.com/pricing")
	if err != nil {
		t.Fatalf("Pageview: %v", err)
	}

	if result.StatusCode != http.StatusAccepted || result.Attempts != 1 || result.Dropped() {
		t.Fatalf("unexpected result: %+v", result)
	}

	if len(*seen) != 1 {
		t.Fatalf("want 1 request, got %d", len(*seen))
	}

	got := (*seen)[0]

	if want := []string{"d", "k", "n", "u"}; strings.Join(keysOf(t, got.body), ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keysOf(t, got.body), want)
	}

	body := decode(t, got.body)
	if body["n"] != "pageview" || body["u"] != "https://example.com/pricing" || body["d"] != "example.com" {
		t.Fatalf("unexpected body: %v", body)
	}

	if key, _ := body["k"].(string); !uuidV4.MatchString(key) {
		t.Fatalf("k = %q, want a UUID v4", body["k"])
	}

	if got.contentType != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got.contentType)
	}

	if got.forwarded != "203.0.113.9" {
		t.Fatalf("X-Forwarded-For = %q, want the visitor's address", got.forwarded)
	}

	if got.userAgent != "Mozilla/5.0 (Macintosh)" {
		t.Fatalf("User-Agent = %q, want the visitor's own", got.userAgent)
	}
}

// TestCustomEventWire pins the rich shape: props, revenue and the server-side
// attribution overrides that keep a delayed conversion out of Direct.
func TestCustomEventWire(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)

	interactive := false

	event := NewEvent("Purchase", "https://example.com/checkout", NewVisitor("203.0.113.9", "curl/8.4.0"))
	event.Props = map[string]any{"plan": "annual", "seats": 4}
	event.Revenue = &Revenue{Amount: 99.5, Currency: "usd"}
	event.Title = "Checkout"
	event.Referrer = "https://news.example/post"
	event.Interactive = &interactive
	event.Attribution = Attribution{
		Referrer:    "https://news.example/post",
		UTMSource:   "newsletter",
		UTMMedium:   "email",
		UTMCampaign: "spring",
		UTMContent:  "cta-a",
		UTMTerm:     "analytics",
	}

	if _, err := client.Send(context.Background(), event); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := (*seen)[0]

	want := []string{"$", "d", "i", "k", "n", "p", "r", "referrer", "t", "u", "utm_campaign", "utm_content", "utm_medium", "utm_source", "utm_term"}
	if strings.Join(keysOf(t, got.body), ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keysOf(t, got.body), want)
	}

	body := decode(t, got.body)

	props, ok := body["p"].(map[string]any)
	if !ok || props["plan"] != "annual" || props["seats"] != float64(4) {
		t.Fatalf("props = %v", body["p"])
	}

	revenue, ok := body["$"].(map[string]any)
	if !ok || revenue["amount"] != 99.5 || revenue["currency"] != "usd" {
		t.Fatalf("revenue = %v", body["$"])
	}

	if body["i"] != false {
		t.Fatalf("i = %v, want an explicit false", body["i"])
	}

	if body["utm_source"] != "newsletter" || body["referrer"] != "https://news.example/post" {
		t.Fatalf("attribution overrides missing: %v", body)
	}
}

// TestValidation is the heart of this SDK. A missing IP or User-Agent is the
// mistake that makes a site look like it has no traffic, so it has to be a
// typed error that names the field and never a request on the wire.
func TestValidation(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)

	tests := []struct {
		name      string
		event     *Event
		wantErr   error
		wantField string
	}{
		{
			name:      "missing client IP",
			event:     NewPageview("https://example.com/", NewVisitor("", "curl/8.4.0")),
			wantErr:   ErrMissingClientIP,
			wantField: "Event.Visitor.IP",
		},
		{
			name:      "blank client IP",
			event:     NewPageview("https://example.com/", NewVisitor("   ", "curl/8.4.0")),
			wantErr:   ErrMissingClientIP,
			wantField: "Event.Visitor.IP",
		},
		{
			name:      "missing user agent",
			event:     NewPageview("https://example.com/", NewVisitor("203.0.113.9", "")),
			wantErr:   ErrMissingUserAgent,
			wantField: "Event.Visitor.UserAgent",
		},
		{
			name:      "missing name",
			event:     NewEvent("", "https://example.com/", NewVisitor("203.0.113.9", "curl/8.4.0")),
			wantErr:   ErrMissingName,
			wantField: "Event.Name",
		},
		{
			name:      "missing url",
			event:     NewEvent("Signup", "", NewVisitor("203.0.113.9", "curl/8.4.0")),
			wantErr:   ErrMissingURL,
			wantField: "Event.URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.Send(context.Background(), test.event)
			if err == nil {
				t.Fatal("want an error, got nil")
			}

			if !errors.Is(err, test.wantErr) {
				t.Fatalf("errors.Is(%v, %v) = false", err, test.wantErr)
			}

			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("want a *ValidationError, got %T", err)
			}

			if validation.Field != test.wantField {
				t.Fatalf("Field = %q, want %q", validation.Field, test.wantField)
			}

			if validation.Reason == "" {
				t.Fatal("a validation error must say why the field matters")
			}
		})
	}

	if len(*seen) != 0 {
		t.Fatalf("a refused event must never reach the wire, got %d request(s)", len(*seen))
	}
}

// TestNewRequiresDomain proves the site identifier is checked once at
// construction rather than on every event, which is where a configuration
// mistake belongs.
func TestNewRequiresDomain(t *testing.T) {
	_, err := New(Options{})
	if !errors.Is(err, ErrMissingDomain) {
		t.Fatalf("New without a domain: %v, want ErrMissingDomain", err)
	}
}

// TestNoOpMode covers both ways in: the constructor flag and the environment
// variable a CI run sets. Nothing may leave the process, the call must succeed,
// and the events must be readable back for assertions.
func TestNoOpMode(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	tests := []struct {
		name   string
		mutate func(*Options)
		env    string
	}{
		{name: "constructor flag", mutate: func(o *Options) { o.Disabled = true }},
		{name: "environment variable", env: "1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.env != "" {
				t.Setenv(DisabledEnv, test.env)
			}

			client := newTestClient(t, server.URL, test.mutate)

			if !client.Disabled() {
				t.Fatal("client should be in no-op mode")
			}

			visitor := NewVisitor("203.0.113.9", "curl/8.4.0")

			result, err := client.Track(context.Background(), visitor, "Signup", "https://example.com/join")
			if err != nil {
				t.Fatalf("Track: %v", err)
			}

			if !result.Skipped || result.StatusCode != 0 {
				t.Fatalf("unexpected result: %+v", result)
			}

			recorded := client.Recorded()
			if len(recorded) != 1 || recorded[0].Name != "Signup" || recorded[0].Visitor.IP != "203.0.113.9" {
				t.Fatalf("recorded = %+v", recorded)
			}

			client.Reset()

			if len(client.Recorded()) != 0 {
				t.Fatal("Reset should clear the recorded events")
			}
		})
	}

	if len(*seen) != 0 {
		t.Fatalf("no-op mode sent %d request(s)", len(*seen))
	}
}

// TestNoOpModeStillValidates keeps the guard rail honest. A test suite that
// never sends anything is exactly where a missing IP would hide until the code
// reached production.
func TestNoOpModeStillValidates(t *testing.T) {
	client := newTestClient(t, "https://example.invalid", func(o *Options) { o.Disabled = true })

	_, err := client.Send(context.Background(), NewPageview("https://example.com/", Visitor{}))
	if !errors.Is(err, ErrMissingClientIP) {
		t.Fatalf("no-op mode must still refuse a missing IP, got %v", err)
	}
}

// TestRetryOnServerError proves the backoff loop retries what is worth retrying
// and reports how many attempts it took.
func TestRetryOnServerError(t *testing.T) {
	var calls int32

	server, _ := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)

	result, err := client.Pageview(context.Background(), NewVisitor("203.0.113.9", "curl/8.4.0"), "https://example.com/")
	if err != nil {
		t.Fatalf("Pageview: %v", err)
	}

	if result.Attempts != 3 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("attempts = %d, calls = %d, want 3 and 3", result.Attempts, calls)
	}
}

// TestIdempotencyKeySurvivesRetry is what makes a retry safe. The server dedupes
// on "k", so a 5xx that arrived after the event was committed must be resent
// with the same key — and two different events must never share one.
func TestIdempotencyKeySurvivesRetry(t *testing.T) {
	var calls int32

	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)
	visitor := NewVisitor("203.0.113.9", "curl/8.4.0")

	if _, err := client.Pageview(context.Background(), visitor, "https://example.com/"); err != nil {
		t.Fatalf("Pageview: %v", err)
	}

	if _, err := client.Pageview(context.Background(), visitor, "https://example.com/"); err != nil {
		t.Fatalf("second Pageview: %v", err)
	}

	if len(*seen) != 3 {
		t.Fatalf("want 3 requests (one retried, one fresh), got %d", len(*seen))
	}

	first, _ := decode(t, (*seen)[0].body)["k"].(string)
	retried, _ := decode(t, (*seen)[1].body)["k"].(string)
	fresh, _ := decode(t, (*seen)[2].body)["k"].(string)

	if !uuidV4.MatchString(first) {
		t.Fatalf("k = %q, want a UUID v4", first)
	}

	if retried != first {
		t.Fatalf("retry sent k = %q, want the original %q", retried, first)
	}

	if fresh == first {
		t.Fatalf("a second event reused k = %q", fresh)
	}
}

// TestRetryExhausted proves a permanently unhappy server ends in an APIError
// carrying the status and the attempt count rather than an endless loop.
func TestRetryExhausted(t *testing.T) {
	var calls int32

	server, _ := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	client := newTestClient(t, server.URL, nil)

	_, err := client.Pageview(context.Background(), NewVisitor("203.0.113.9", "curl/8.4.0"), "https://example.com/")

	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("want an *APIError, got %v", err)
	}

	if api.StatusCode != http.StatusTooManyRequests || api.Attempts != 3 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("api = %+v, calls = %d", api, calls)
	}
}

// TestNoRetryOnBadRequest is the other half of the retry contract. A 400 is the
// caller's bug — a missing header, a malformed body — and sending it again
// produces the same 400 while hiding the message that explains it.
func TestNoRetryOnBadRequest(t *testing.T) {
	var calls int32

	server, _ := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "this request arrived from a datacentre address with no X-Forwarded-For", http.StatusBadRequest)
	})

	client := newTestClient(t, server.URL, nil)

	_, err := client.Pageview(context.Background(), NewVisitor("203.0.113.9", "curl/8.4.0"), "https://example.com/")

	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("want an *APIError, got %v", err)
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("a 400 was retried %d times", calls-1)
	}

	if !strings.Contains(api.Body, "datacentre address") {
		t.Fatalf("the server's own sentence was lost: %q", api.Body)
	}
}

// TestDroppedIsNotAFailure proves a classified event comes back as a readable
// reason on a successful result, sent once. Retrying a drop would duplicate a
// decision the server already made.
func TestDroppedIsNotAFailure(t *testing.T) {
	var calls int32

	server, _ := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set(HeaderDropped, "bot:datacenter")
		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)

	result, err := client.Pageview(context.Background(), NewVisitor("203.0.113.9", "curl/8.4.0"), "https://example.com/")
	if err != nil {
		t.Fatalf("a drop must not be an error: %v", err)
	}

	if !result.Dropped() || result.DropReason != "bot:datacenter" {
		t.Fatalf("result = %+v, want the drop reason surfaced", result)
	}

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("a dropped 202 was retried %d times", calls-1)
	}
}

// TestRetryOnTransportError proves a connection that never reached a server is
// tried again: nothing was counted, so nothing can be duplicated.
func TestRetryOnTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := server.URL
	server.Close()

	client := newTestClient(t, host, func(o *Options) { o.Attempts = 2 })

	_, err := client.Pageview(context.Background(), NewVisitor("203.0.113.9", "curl/8.4.0"), "https://example.com/")
	if err == nil {
		t.Fatal("want a transport error against a closed listener")
	}

	if !strings.Contains(err.Error(), "attempt 2") {
		t.Fatalf("error should name the last attempt: %v", err)
	}
}

// TestDebugRequest proves the escape hatch: the debug header goes out and the
// server's derived event comes back untouched, which is what makes "my numbers
// look wrong" answerable in one call.
func TestDebugRequest(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderDebug) != "true" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"site_id":1,"client_ip_source":"x-forwarded-for","drop_reason":""}`))
	})

	client := newTestClient(t, server.URL, nil)

	raw, err := client.Debug(context.Background(), NewPageview("https://example.com/", NewVisitor("203.0.113.9", "curl/8.4.0")))
	if err != nil {
		t.Fatalf("Debug: %v", err)
	}

	var derived map[string]any
	if err := json.Unmarshal(raw, &derived); err != nil {
		t.Fatalf("debug response is not JSON: %v", err)
	}

	if derived["client_ip_source"] != "x-forwarded-for" {
		t.Fatalf("derived = %v", derived)
	}

	if (*seen)[0].debug != "true" {
		t.Fatal("the debug header was not sent")
	}
}

// TestFromRequest walks the precedence the ingest server itself uses. The first
// entry of X-Forwarded-For is the case worth pinning: taking the last one
// reports the nearest proxy as the visitor.
func TestFromRequest(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		remoteAddr string
		wantIP     string
	}{
		{
			name:       "cloudflare wins",
			headers:    map[string]string{"CF-Connecting-IP": "198.51.100.5", "X-Forwarded-For": "192.0.2.5"},
			remoteAddr: "10.0.0.1:4444",
			wantIP:     "198.51.100.5",
		},
		{
			name:       "first forwarded entry is the client",
			headers:    map[string]string{"X-Forwarded-For": "192.0.2.5, 10.0.0.7, 10.0.0.8"},
			remoteAddr: "10.0.0.1:4444",
			wantIP:     "192.0.2.5",
		},
		{
			name:       "socket address is the fallback",
			remoteAddr: "203.0.113.9:51234",
			wantIP:     "203.0.113.9",
		},
		{
			name:       "ipv6 socket address loses its port",
			remoteAddr: "[2001:db8::1]:51234",
			wantIP:     "2001:db8::1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
			req.RemoteAddr = test.remoteAddr
			req.Header.Set("User-Agent", "Mozilla/5.0 (X11)")

			for name, value := range test.headers {
				req.Header.Set(name, value)
			}

			visitor := FromRequest(req)

			if visitor.IP != test.wantIP {
				t.Fatalf("IP = %q, want %q", visitor.IP, test.wantIP)
			}

			if visitor.UserAgent != "Mozilla/5.0 (X11)" {
				t.Fatalf("UserAgent = %q", visitor.UserAgent)
			}
		})
	}
}

// TestFromRequestEndToEnd proves the helper and the sender agree: what the
// inbound request carried is what the outbound request carries.
func TestFromRequestEndToEnd(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL, nil)

	inbound := httptest.NewRequest(http.MethodGet, "https://example.com/pricing", nil)
	inbound.RemoteAddr = "10.0.0.1:4444"
	inbound.Header.Set("X-Forwarded-For", "192.0.2.5, 10.0.0.7")
	inbound.Header.Set("User-Agent", "Mozilla/5.0 (iPhone)")

	if _, err := client.Pageview(context.Background(), FromRequest(inbound), "https://example.com/pricing"); err != nil {
		t.Fatalf("Pageview: %v", err)
	}

	got := (*seen)[0]

	if got.forwarded != "192.0.2.5" || got.userAgent != "Mozilla/5.0 (iPhone)" {
		t.Fatalf("forwarded = %q, ua = %q", got.forwarded, got.userAgent)
	}
}

// TestEndpointAndDomainOverride proves the endpoint is derived from the host
// once, and that a process reporting for several sites can override the domain
// per event.
func TestEndpointAndDomainOverride(t *testing.T) {
	server, seen := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	client := newTestClient(t, server.URL+"/", nil)

	if client.Endpoint() != server.URL+"/api/event" {
		t.Fatalf("Endpoint = %q", client.Endpoint())
	}

	event := NewPageview("https://other.example/", NewVisitor("203.0.113.9", "curl/8.4.0"))
	event.Domain = "other.example"

	if _, err := client.Send(context.Background(), event); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if decode(t, (*seen)[0].body)["d"] != "other.example" {
		t.Fatalf("domain override ignored: %s", (*seen)[0].body)
	}
}

// TestContextCancellationStopsRetries proves the retry loop belongs to the
// caller's context. A handler that has already given up should not be held open
// by a backoff nobody is waiting for.
func TestContextCancellationStopsRetries(t *testing.T) {
	server, _ := recordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	client := newTestClient(t, server.URL, func(o *Options) {
		o.BaseBackoff = 250 * time.Millisecond
		o.MaxBackoff = time.Second
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Pageview(ctx, NewVisitor("203.0.113.9", "curl/8.4.0"), "https://example.com/")
	if err == nil {
		t.Fatal("want an error when the context ends mid-retry")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error to survive wrapping, got %v", err)
	}
}

// TestPropHelperAllocates covers the small convenience that would otherwise be
// a nil-map panic at the first call site that used it.
func TestPropHelperAllocates(t *testing.T) {
	event := NewEvent("Signup", "https://example.com/", NewVisitor("203.0.113.9", "curl/8.4.0")).
		WithProp("plan", "annual").
		WithProp("trial", true)

	if len(event.Props) != 2 || event.Props["plan"] != "annual" || event.Props["trial"] != true {
		t.Fatalf("props = %v", event.Props)
	}
}
