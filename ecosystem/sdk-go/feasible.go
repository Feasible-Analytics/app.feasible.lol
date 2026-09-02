//
// feasible.go
// The server-side client for feasible.lol: the two headers you cannot forget, and a retry that knows what not to retry.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package feasible

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultHost is the hosted service. A self-hoster passes their own host and
// nothing else changes, which is the whole reason the endpoint path is not
// configurable: there is only one, and inventing a setting for it would only
// create something to get wrong.
const DefaultHost = "https://app.feasible.lol"

// The knobs a caller may turn, with the values that are right for almost
// everybody. Five seconds is longer than the endpoint has ever needed and short
// enough that an unreachable host cannot hold a request handler open.
const (
	DefaultTimeout     = 5 * time.Second
	DefaultAttempts    = 3
	DefaultBaseBackoff = 100 * time.Millisecond
	DefaultMaxBackoff  = 2 * time.Second
)

// The headers that carry the answer back out of the ingest endpoint. The drop
// header is a classification, not a failure, so it is surfaced on the result
// rather than turned into an error.
const (
	HeaderDropped = "x-feasible-dropped"
	HeaderDebug   = "X-Debug-Request"
)

// DisabledEnv turns the client into a recorder without sending anything. It
// exists so a CI run, a seed script or a local development server can exercise
// the real code path without a single packet leaving the machine.
const DisabledEnv = "FEASIBLE_DISABLED"

// The reasons a call can be refused before a request is ever built. They are
// sentinels so a caller can branch on the specific mistake with errors.Is,
// which matters most for the first two: they are the mistakes that make a site
// look like it has no traffic.
var (
	ErrMissingClientIP  = errors.New("the visitor's client IP is required")
	ErrMissingUserAgent = errors.New("the visitor's User-Agent is required")
	ErrMissingName      = errors.New("an event name is required")
	ErrMissingURL       = errors.New("an event URL is required")
	ErrMissingDomain    = errors.New("a site domain is required")
	ErrInvalidCurrency  = errors.New("the revenue currency must be a three-letter ISO 4217 code")
)

// ValidationError is a refusal to send something the server would only reject
// or misattribute. It names the field and says why the field matters, because
// the person reading this message is the only person who can fix it and they
// are usually reading it for the first time.
type ValidationError struct {
	// Field is the parameter that was missing, spelled as the caller wrote it.
	Field string

	// Reason says what goes wrong when it is missing, in a sentence.
	Reason string

	err error
}

// Error renders the field, the rule and the consequence in one line, so the
// message is useful in a log where nobody will click through to documentation.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("feasible: %s: %v. %s", e.Field, e.err, e.Reason)
}

// Unwrap exposes the sentinel so errors.Is finds it. Callers branch on the
// sentinel and print the message; both need to work off the same error value.
func (e *ValidationError) Unwrap() error {
	return e.err
}

// APIError is a response the server understood and refused. It carries the body
// verbatim because the endpoint answers a 400 with a sentence naming what is
// wrong, and paraphrasing that sentence would lose the only diagnosis there is.
type APIError struct {
	StatusCode int
	Body       string
	Attempts   int
}

// Error keeps the server's own words. A wrapped or reworded 400 is a support
// conversation that starts from nothing.
func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("feasible: server returned %d after %d attempt(s)", e.StatusCode, e.Attempts)
	}

	return fmt.Sprintf("feasible: server returned %d after %d attempt(s): %s", e.StatusCode, e.Attempts, e.Body)
}

// Visitor is who an event is about. It is a separate type, and a required
// argument everywhere an event is constructed, for one reason: a server-side
// call that forwards neither the visitor's address nor their User-Agent arrives
// at the endpoint looking exactly like a datacentre bot, and the event is
// dropped. Making it a field on an options struct would make it forgettable,
// so it is a parameter instead.
type Visitor struct {
	// IP is the visitor's address, sent as the first entry of X-Forwarded-For.
	IP string

	// UserAgent is the visitor's User-Agent header, sent verbatim. It is what
	// the browser, device and operating system columns are derived from.
	UserAgent string
}

// NewVisitor is the explicit form, for a caller holding the two values already
// — a queue worker replaying a conversion recorded hours earlier, say, where
// there is no live request to read them from.
func NewVisitor(ip, userAgent string) Visitor {
	return Visitor{IP: ip, UserAgent: userAgent}
}

// FromRequest lifts the visitor out of an inbound request: CF-Connecting-IP,
// then the first entry of X-Forwarded-For, then the socket address.
//
// This SDK has no trusted-proxy configuration. The helper is safe only behind
// an application edge that strips client-supplied forwarding headers and writes
// its own; a directly exposed app must pass the socket address explicitly.
func FromRequest(r *http.Request) Visitor {
	if r == nil {
		return Visitor{}
	}

	visitor := Visitor{UserAgent: r.Header.Get("User-Agent")}

	if addr := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); addr != "" {
		visitor.IP = addr
		return visitor
	}

	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if first = strings.TrimSpace(first); first != "" {
			visitor.IP = first
			return visitor
		}
	}

	visitor.IP = hostOnly(r.RemoteAddr)

	return visitor
}

// hostOnly strips the port from a socket address in either family. RemoteAddr
// always carries one, and sending "203.0.113.9:54321" as an address produces an
// event with no country at all.
func hostOnly(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}

	return value
}

// Revenue is the money an event reports. The currency is an ISO 4217 code and
// the amount is in major units — 9.99 is nine dollars and ninety-nine cents —
// because that is what a payment provider hands you and converting at the call
// site is where the factor-of-a-hundred bugs come from.
type Revenue struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// currencyCode upper-cases a currency and reports whether it is the three-letter
// shape the server stores. The check is here because the server ignores a
// revenue object whose currency it cannot read, and revenue that is silently
// zero is the hardest kind of missing data to notice. Upper-casing means "usd"
// and "USD" do not become two rows on the same report.
func currencyCode(value string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(value))

	if len(code) != 3 {
		return code, false
	}

	for _, char := range code {
		if char < 'A' || char > 'Z' {
			return code, false
		}
	}

	return code, true
}

// Attribution overrides where a conversion came from. A delayed or offline
// conversion has no referrer of its own, so without these it is Direct forever
// and the campaign that actually paid for it gets no credit. The server applies
// them to any event that carries them.
type Attribution struct {
	Referrer    string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMContent  string
	UTMTerm     string
}

// Event is one thing that happened. Build it with NewPageview or NewEvent so
// the visitor cannot be left out, then set whatever else applies directly on
// the struct.
type Event struct {
	// Name is "pageview" for a pageview and anything else for a custom event.
	Name string

	// URL is the full URL of the page the event happened on. It is what the
	// page, entry page and exit page dimensions are derived from.
	URL string

	// Visitor carries the two values the server cannot guess.
	Visitor Visitor

	// Domain overrides the client's site domain, for a process that reports
	// for more than one site.
	Domain string

	// Referrer is the referring URL as the browser reported it. Use
	// Attribution instead when there is no browser in the story.
	Referrer string

	// Title is the page title.
	Title string

	// Props are custom properties. Thirty at most; names are capped at 300
	// characters and values at 2000, and anything past those limits is counted
	// and reported by the server rather than silently dropped.
	Props map[string]any

	// Revenue is optional money attached to the event.
	Revenue *Revenue

	// Interactive is a pointer so that "not set" and "explicitly false" stay
	// different: absent means an ordinary interaction, and false is how a
	// background event avoids ending somebody's bounce.
	Interactive *bool

	// ScrollDepth is a percentage from 0 to 100.
	ScrollDepth *float64

	// EngagementTime is milliseconds of engaged time, capped by the server at
	// one day.
	EngagementTime *int64

	// ViewportWidth is the viewport width in CSS pixels, which the server
	// buckets into a screen size.
	ViewportWidth *int

	// Attribution is the server-side override set.
	Attribution Attribution
}

// NewPageview builds a pageview. The visitor is the second argument rather than
// a field so that the compiler, not the ingest endpoint three environments
// later, is what tells you it is missing.
func NewPageview(pageURL string, visitor Visitor) *Event {
	return &Event{Name: "pageview", URL: pageURL, Visitor: visitor}
}

// NewEvent builds a custom event. Same rule as NewPageview: the visitor is a
// parameter because it is the one thing a server-side caller forgets.
func NewEvent(name, pageURL string, visitor Visitor) *Event {
	return &Event{Name: name, URL: pageURL, Visitor: visitor}
}

// WithProp adds one property, allocating the map on first use so a caller never
// has to.
func (e *Event) WithProp(name string, value any) *Event {
	if e.Props == nil {
		e.Props = make(map[string]any, 4)
	}

	e.Props[name] = value

	return e
}

// Options configures a client. The zero value of every field is a working
// default except Domain, which has no sensible default and is required.
type Options struct {
	// Domain is the site as registered — the site id every event carries.
	Domain string

	// Host is the analytics host, without a path. Defaults to DefaultHost.
	Host string

	// Timeout bounds one attempt, not the whole call. Defaults to five seconds.
	Timeout time.Duration

	// Attempts is the total number of tries including the first. Defaults to
	// three; one disables retrying.
	Attempts int

	// BaseBackoff and MaxBackoff bound the wait between attempts.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration

	// Disabled turns off sending entirely. FEASIBLE_DISABLED=1 does the same
	// thing from the environment, so a test suite does not have to thread a
	// flag through its own configuration to get there.
	Disabled bool

	// HTTPClient replaces the client's own. Supply one to share a transport
	// with the rest of an application; leave it nil to get a transport tuned
	// for this endpoint.
	HTTPClient *http.Client
}

// Result is what a delivered event tells you. It is not an error type: a 202
// carrying a drop reason is a classification the server made, and a caller who
// wants to know can log DropReason without having to unwrap anything.
type Result struct {
	// StatusCode is the final HTTP status, or zero when nothing was sent.
	StatusCode int

	// DropReason is the x-feasible-dropped header, empty when the event
	// counted. A value here is not a failure and must not be retried.
	DropReason string

	// Attempts is how many requests it took.
	Attempts int

	// Skipped reports that the client is in no-op mode and nothing was sent.
	Skipped bool
}

// Dropped reports whether the server classified this event away. It reads
// better than comparing a string at every call site.
func (r *Result) Dropped() bool {
	return r != nil && r.DropReason != ""
}

// Client sends events. It is safe for concurrent use and is meant to be built
// once for the life of a process, because the value of reusing it is the
// connection pool underneath.
//
// Two things are required on every event and neither can be guessed by the
// server: the visitor's IP and their User-Agent. A request from your server
// carrying neither arrives from a datacentre address with no visitor in it, and
// the endpoint answers 400 rather than quietly attributing the visit to your
// hosting provider. That is why they are constructor parameters on Event and
// not optional fields, and why FromRequest exists.
type Client struct {
	domain      string
	endpoint    string
	http        *http.Client
	attempts    int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	disabled    bool

	mu       sync.Mutex
	recorded []Event
}

// New builds a client. It returns an error rather than panicking on a missing
// domain because the domain usually comes from configuration, and a process
// that cannot report analytics should log and carry on rather than die.
func New(opts Options) (*Client, error) {
	domain := strings.TrimSpace(opts.Domain)
	if domain == "" {
		return nil, &ValidationError{
			Field:  "Options.Domain",
			Reason: "It is the site identifier every event carries, exactly as the site is registered.",
			err:    ErrMissingDomain,
		}
	}

	host := strings.TrimSpace(opts.Host)
	if host == "" {
		host = DefaultHost
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	attempts := opts.Attempts
	if attempts <= 0 {
		attempts = DefaultAttempts
	}

	baseBackoff := opts.BaseBackoff
	if baseBackoff <= 0 {
		baseBackoff = DefaultBaseBackoff
	}

	maxBackoff := opts.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = DefaultMaxBackoff
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout, Transport: newTransport()}
	}

	return &Client{
		domain:      domain,
		endpoint:    strings.TrimRight(host, "/") + "/api/event",
		http:        httpClient,
		attempts:    attempts,
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
		disabled:    opts.Disabled || envDisabled(),
	}, nil
}

// newTransport is a transport sized for one endpoint. The default transport
// allows two idle connections per host, which is enough to make a busy web
// server open a fresh connection — and a fresh TLS handshake — for a large
// share of its events.
func newTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second

	return transport
}

// envDisabled reads the no-op switch. It accepts the obvious spellings because
// the value is typed into a CI configuration by hand and "true" failing where
// "1" works is a wasted afternoon.
func envDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(DisabledEnv))) {
	case "1", "true", "yes", "on":
		return true
	}

	return false
}

// Disabled reports whether this client is in no-op mode, so an application can
// say so at startup instead of leaving somebody to wonder where the data went.
func (c *Client) Disabled() bool {
	return c.disabled
}

// Endpoint is the URL events are posted to. It is exposed for the log line an
// application prints at startup, which is the cheapest way to catch a host that
// was configured wrong.
func (c *Client) Endpoint() string {
	return c.endpoint
}

// Recorded returns the events a no-op client accepted, oldest first. This is
// how a test asserts that the code under test reported what it should have,
// with no network and no stub server.
func (c *Client) Recorded() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]Event, len(c.recorded))
	copy(out, c.recorded)

	return out
}

// Reset clears the recorded events, so one test's assertions cannot be
// satisfied by another test's events.
func (c *Client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recorded = nil
}

// Pageview reports a pageview in one call, for the common case where there is
// nothing to attach. The visitor comes first because it is the argument that
// must not be an afterthought.
func (c *Client) Pageview(ctx context.Context, visitor Visitor, pageURL string) (*Result, error) {
	return c.Send(ctx, NewPageview(pageURL, visitor))
}

// Track reports a custom event in one call. Anything richer than a name and a
// URL is built with NewEvent and sent through Send.
func (c *Client) Track(ctx context.Context, visitor Visitor, name, pageURL string) (*Result, error) {
	return c.Send(ctx, NewEvent(name, pageURL, visitor))
}

// Send delivers one event. Validation runs before the no-op check on purpose:
// a test suite that never sends anything is exactly where a missing IP would
// otherwise hide until production.
func (c *Client) Send(ctx context.Context, event *Event) (*Result, error) {
	payload, err := c.payload(event)
	if err != nil {
		return nil, err
	}

	if c.disabled {
		c.mu.Lock()
		c.recorded = append(c.recorded, *event)
		c.mu.Unlock()

		return &Result{Skipped: true}, nil
	}

	result, _, err := c.deliver(ctx, event, payload, false)

	return result, err
}

// Debug asks the server what it would derive from this event and returns that
// JSON instead of writing anything. It is free of side effects and safe to run
// against production, which is what makes it the first thing to reach for when
// somebody says their numbers look wrong.
func (c *Client) Debug(ctx context.Context, event *Event) (json.RawMessage, error) {
	payload, err := c.payload(event)
	if err != nil {
		return nil, err
	}

	if c.disabled {
		return nil, errors.New("feasible: Debug needs a live request and this client is in no-op mode")
	}

	_, body, err := c.deliver(ctx, event, payload, true)
	if err != nil {
		return nil, err
	}

	return json.RawMessage(body), nil
}

// wirePayload is the request body. The single-letter keys are the established
// wire shape, and the field order here is the key order on the wire, which
// makes a captured body diffable by eye.
type wirePayload struct {
	Key            string         `json:"k"`
	Name           string         `json:"n"`
	URL            string         `json:"u"`
	Domain         string         `json:"d"`
	Referrer       string         `json:"r,omitempty"`
	Props          map[string]any `json:"p,omitempty"`
	Title          string         `json:"t,omitempty"`
	Interactive    *bool          `json:"i,omitempty"`
	ScrollDepth    *float64       `json:"sd,omitempty"`
	EngagementTime *int64         `json:"e,omitempty"`
	ViewportWidth  *int           `json:"w,omitempty"`
	Revenue        *Revenue       `json:"$,omitempty"`

	OverrideReferrer    string `json:"referrer,omitempty"`
	OverrideUTMSource   string `json:"utm_source,omitempty"`
	OverrideUTMMedium   string `json:"utm_medium,omitempty"`
	OverrideUTMCampaign string `json:"utm_campaign,omitempty"`
	OverrideUTMContent  string `json:"utm_content,omitempty"`
	OverrideUTMTerm     string `json:"utm_term,omitempty"`
}

// payload validates an event and encodes it. Absent values are omitted rather
// than sent as null, because a null in this body is a value the server has to
// decide about and every such decision is a place the two sides can disagree.
func (c *Client) payload(event *Event) ([]byte, error) {
	if event == nil {
		return nil, &ValidationError{
			Field:  "event",
			Reason: "Build one with NewPageview or NewEvent.",
			err:    ErrMissingName,
		}
	}

	if strings.TrimSpace(event.Name) == "" {
		return nil, &ValidationError{
			Field:  "Event.Name",
			Reason: `Use "pageview" for a pageview, or any other name for a custom event.`,
			err:    ErrMissingName,
		}
	}

	if strings.TrimSpace(event.URL) == "" {
		return nil, &ValidationError{
			Field:  "Event.URL",
			Reason: "It is the full URL of the page the event happened on, and every page-level report is derived from it.",
			err:    ErrMissingURL,
		}
	}

	if strings.TrimSpace(event.Visitor.IP) == "" {
		return nil, &ValidationError{
			Field:  "Event.Visitor.IP",
			Reason: "Without it the event arrives from your datacentre address, is classified as a bot and is dropped. Use feasible.FromRequest(r) to take it from the inbound request.",
			err:    ErrMissingClientIP,
		}
	}

	if strings.TrimSpace(event.Visitor.UserAgent) == "" {
		return nil, &ValidationError{
			Field:  "Event.Visitor.UserAgent",
			Reason: "It is what the browser, device and operating system columns are derived from, and a request without one is treated as a bot. Use feasible.FromRequest(r) to take it from the inbound request.",
			err:    ErrMissingUserAgent,
		}
	}

	domain := strings.TrimSpace(event.Domain)
	if domain == "" {
		domain = c.domain
	}

	// The revenue is normalised into a copy so that validating it cannot mutate
	// the caller's own struct, which they may well be reusing across events.
	revenue := event.Revenue
	if revenue != nil {
		code, ok := currencyCode(revenue.Currency)
		if !ok {
			return nil, &ValidationError{
				Field:  "Event.Revenue.Currency",
				Reason: fmt.Sprintf("%q is not a code such as USD or GBP, and the server ignores a revenue object it cannot read, leaving the revenue silently at zero.", revenue.Currency),
				err:    ErrInvalidCurrency,
			}
		}

		revenue = &Revenue{Amount: revenue.Amount, Currency: code}
	}

	body := wirePayload{
		Key:            newKey(),
		Name:           event.Name,
		URL:            event.URL,
		Domain:         domain,
		Referrer:       event.Referrer,
		Props:          event.Props,
		Title:          event.Title,
		Interactive:    event.Interactive,
		ScrollDepth:    event.ScrollDepth,
		EngagementTime: event.EngagementTime,
		ViewportWidth:  event.ViewportWidth,
		Revenue:        revenue,

		OverrideReferrer:    event.Attribution.Referrer,
		OverrideUTMSource:   event.Attribution.UTMSource,
		OverrideUTMMedium:   event.Attribution.UTMMedium,
		OverrideUTMCampaign: event.Attribution.UTMCampaign,
		OverrideUTMContent:  event.Attribution.UTMContent,
		OverrideUTMTerm:     event.Attribution.UTMTerm,
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("feasible: could not encode the event: %w", err)
	}

	return encoded, nil
}

// newKey mints the idempotency key an event carries on every attempt. The
// server drops a second event with the same key, which is what makes a retry
// after a lost acknowledgement harmless: the payload is encoded once per Send,
// so every retry resends the same key. It is a random UUID v4 because that is
// the only shape the server accepts in this field, and it is built by hand
// rather than imported so the package keeps its zero dependencies.
func newKey() string {
	var raw [16]byte

	if _, err := rand.Read(raw[:]); err != nil {
		// The system entropy source failing is not something a tracking call
		// can recover from, and sending no key would silently reopen the
		// double-count on retry.
		panic("feasible: crypto/rand failed: " + err.Error())
	}

	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80

	var text [36]byte
	hex.Encode(text[0:8], raw[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], raw[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], raw[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], raw[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], raw[10:16])

	return string(text[:])
}

// deliver runs the attempt loop. The retry rules are the point of this function:
// a 400 is the caller's bug and retrying it changes nothing, and a 202 carrying
// a drop reason is a decision the server made rather than a failure, so
// resending it would only duplicate a classification.
func (c *Client) deliver(ctx context.Context, event *Event, payload []byte, debug bool) (*Result, []byte, error) {
	var lastErr error

	for attempt := 1; attempt <= c.attempts; attempt++ {
		result, body, err := c.attempt(ctx, event, payload, debug, attempt)
		if err == nil {
			return result, body, nil
		}

		lastErr = err

		if !retryable(err) || attempt == c.attempts {
			return nil, nil, err
		}

		if waitErr := sleep(ctx, c.backoff(attempt)); waitErr != nil {
			return nil, nil, fmt.Errorf("feasible: giving up after %d attempt(s): %w", attempt, waitErr)
		}
	}

	return nil, nil, lastErr
}

// attempt performs one request. The response body is always drained before it
// is closed, because a body left unread is a connection that cannot go back in
// the pool and the whole point of a long-lived client is that it does.
func (c *Client) attempt(ctx context.Context, event *Event, payload []byte, debug bool, attempt int) (*Result, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("feasible: could not build the request: %w", err)
	}

	// text/plain is deliberate. It is what keeps a browser from sending a CORS
	// preflight, the endpoint accepts it, and using it everywhere means the
	// server-side path and the browser path are the same request.
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Forwarded-For", event.Visitor.IP)
	req.Header.Set("User-Agent", event.Visitor.UserAgent)

	if debug {
		req.Header.Set(HeaderDebug, "true")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Safe to retry even if the bytes did reach the server: the payload
		// carries the same idempotency key on every attempt.
		return nil, nil, &transportError{attempt: attempt, err: err}
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &Result{
			StatusCode: resp.StatusCode,
			DropReason: resp.Header.Get(HeaderDropped),
			Attempts:   attempt,
		}, body, nil
	}

	return nil, nil, &APIError{
		StatusCode: resp.StatusCode,
		Body:       strings.TrimSpace(string(body)),
		Attempts:   attempt,
	}
}

// transportError marks a failure that never reached the server, which is always
// worth another attempt. It is unexported because a caller has no reason to
// branch on it — errors.Is against the wrapped error is enough.
type transportError struct {
	attempt int
	err     error
}

// Error names the attempt count so a log line says how hard the client already
// tried before the caller saw this.
func (e *transportError) Error() string {
	return fmt.Sprintf("feasible: request failed on attempt %d: %v", e.attempt, e.err)
}

// Unwrap exposes the transport's own error, which is where the useful detail is
// — a DNS failure and a refused connection are different problems.
func (e *transportError) Unwrap() error {
	return e.err
}

// retryable decides whether another attempt could plausibly go differently. A
// 429 and a 5xx are the server asking for time; everything else at the HTTP
// level is a request that will be rejected identically forever.
func retryable(err error) bool {
	var transport *transportError
	if errors.As(err, &transport) {
		return true
	}

	var api *APIError
	if errors.As(err, &api) {
		return api.StatusCode == http.StatusTooManyRequests || api.StatusCode >= 500
	}

	return false
}

// backoff is exponential with equal jitter, capped. The jitter matters when a
// deploy restarts a fleet at once: without it every instance retries on the
// same millisecond and the server gets the same spike it was backing off from.
func (c *Client) backoff(attempt int) time.Duration {
	wait := c.baseBackoff << (attempt - 1)
	if wait <= 0 || wait > c.maxBackoff {
		wait = c.maxBackoff
	}

	half := wait / 2

	return half + time.Duration(mathrand.Int64N(int64(half)+1))
}

// sleep waits, or gives up early when the caller's context ends. A retry loop
// that ignores cancellation holds a request handler open long after whoever
// asked for it has gone.
func sleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
