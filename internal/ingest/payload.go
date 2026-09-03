//
// payload.go
// The POST /api/event request body, its limits, and what we refuse to guess at.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Limits on what one event may carry. Every one of them is enforced by counting
// and reporting rather than by silently discarding: the incumbent drops props
// past the thirtieth with no error, no warning and no rejection, and it is one
// of their most-reported mysteries.
const (
	// MaxURLLength caps the event URL, excluding the domain and query string.
	// It is the path that grows without bound, usually because somebody is
	// encoding state into it.
	MaxURLLength = 2000

	// MaxProps is the number of custom properties one event may carry.
	MaxProps = 30

	// MaxPropNameLength and MaxPropValueLength cap one property.
	MaxPropNameLength  = 300
	MaxPropValueLength = 2000

	// MaxBodyBytes is the largest request body we will read. It is generous
	// enough for a full props object and small enough that a malicious client
	// cannot make us buffer megabytes per connection.
	MaxBodyBytes = 64 * 1024

	// MaxEngagementTime is the ceiling for a reported engagement time, at one
	// day in milliseconds. A null-arithmetic race in the incumbent's tracker
	// reported Date.now() as an engagement time, and old scripts stay in
	// browser caches for months, so the server has to defend itself rather than
	// wait for every client to update.
	MaxEngagementTime = 24 * 60 * 60 * 1000
)

// Event names that mean something to the pipeline. Everything else is a custom
// event and is stored as-is.
const (
	EventPageview   = "pageview"
	EventEngagement = "engagement"
)

// The screen-size buckets. They are stored as text because that is what a
// dimension row is, and naming them here keeps the tracker, the pipeline and
// the dashboard from each inventing their own spelling of "Mobile".
const (
	ScreenMobile  = "Mobile"
	ScreenTablet  = "Tablet"
	ScreenLaptop  = "Laptop"
	ScreenDesktop = "Desktop"

	// MaxScreenWidth is the largest width worth believing. Anything past it is
	// a broken client rather than a display, and letting it through would put
	// a bucket on the dashboard that no visitor has.
	MaxScreenWidth = 20000
)

// ScrollDepthUnset is the stored value for "never reported". It sits outside
// the 0-100 range a real measurement can take, so an average of real scroll
// depths never has to exclude a NULL.
const ScrollDepthUnset = 255

// Payload is one inbound event exactly as the tracker sends it. The single-
// letter keys are not an aesthetic choice: they match the established
// `/api/event` shape byte for byte, which is what lets somebody migrate to us
// by changing one hostname. Renaming any of them breaks every deployed tracker
// in the world.
type Payload struct {
	Key      string          `json:"k"`  // client-generated idempotency UUID
	Name     string          `json:"n"`  // event name
	URL      string          `json:"u"`  // full URL
	Domain   string          `json:"d"`  // site identifier
	Referrer string          `json:"r"`  // referrer
	Props    json.RawMessage `json:"p"`  // custom properties
	Title    string          `json:"t"`  // page title — our addition
	Version  json.Number     `json:"v"`  // tracker version
	Revenue  json.RawMessage `json:"$"`  // revenue object
	Scroll   *json.Number    `json:"sd"` // scroll depth, 0-100
	Engage   *json.Number    `json:"e"`  // engagement time, milliseconds
	Width    *json.Number    `json:"w"`  // viewport width in CSS pixels — our addition

	// Automated reports what the page saw that a server cannot: a browser
	// making claims about itself that cannot all be true. It is a short string
	// of one-letter signals, and it is an observation rather than a verdict —
	// what it means is decided here, so the threshold can change without
	// reshipping a script that lives on other people's pages.
	Automated string `json:"a"`

	// Interactive defaults to true when absent, which is what an ordinary
	// pageview is. It is a pointer so "absent" and "explicitly false" stay
	// distinguishable — the bounce rule reads them differently.
	Interactive *bool `json:"i"`

	// The server-side override fields. Unlike the incumbent we let an Events
	// API caller set the attribution explicitly, because a delayed or offline
	// conversion has no referrer of its own and would otherwise be Direct
	// forever. They are ignored on browser traffic.
	OverrideReferrer    string `json:"referrer"`
	OverrideUTMSource   string `json:"utm_source"`
	OverrideUTMMedium   string `json:"utm_medium"`
	OverrideUTMCampaign string `json:"utm_campaign"`
	OverrideUTMContent  string `json:"utm_content"`
	OverrideUTMTerm     string `json:"utm_term"`
}

// Revenue is the money an event reports. The amount is held in minor units as
// an integer, never a float: a currency amount in a float is a rounding error
// waiting for a large enough report to become visible.
type Revenue struct {
	Amount   int64
	Currency string
}

// Truncation records what an event carried that we could not keep in full.
// Never silently truncate — every count here is surfaced on the ingestion
// health panel, because a customer whose thirty-first property vanished has no
// other way to find out.
type Truncation struct {
	// PropsDropped is how many properties past the thirtieth were sent.
	PropsDropped int

	// PropNamesTruncated and PropValuesTruncated count properties whose name or
	// value was cut to the limit.
	PropNamesTruncated  int
	PropValuesTruncated int

	// PropsUnsupported counts properties whose value was an object, an array
	// or null. Nothing is stored for them, so they are counted for the same
	// reason the thirty-first property is: a value that vanishes without a
	// number beside it is a support ticket nobody can answer.
	PropsUnsupported int

	// URLTruncated reports that the path was longer than the limit.
	URLTruncated bool

	// EngagementClamped reports an engagement time outside any sane range,
	// which in practice means a stale tracker doing arithmetic on a null.
	EngagementClamped bool
}

// Any reports whether anything was cut at all, so the hot path can skip the
// bookkeeping in the overwhelmingly common case where nothing was.
func (t Truncation) Any() bool {
	return t.PropsDropped > 0 || t.PropNamesTruncated > 0 || t.PropValuesTruncated > 0 ||
		t.PropsUnsupported > 0 || t.URLTruncated || t.EngagementClamped
}

// ParsePayload decodes a request body. It accepts anything that is valid JSON
// regardless of the declared content type, because the official trackers send
// `text/plain` to avoid a CORS preflight and rejecting that breaks every
// existing integration.
func ParsePayload(body []byte) (*Payload, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("empty request body")
	}

	var payload Payload
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, fmt.Errorf("body is not valid JSON: %w", err)
	}

	payload.Name = strings.TrimSpace(payload.Name)
	payload.Key = strings.TrimSpace(payload.Key)
	payload.Domain = strings.TrimSpace(payload.Domain)
	payload.URL = strings.TrimSpace(payload.URL)

	if payload.Name == "" {
		return nil, fmt.Errorf(`"n" (event name) is required`)
	}
	if payload.Domain == "" {
		return nil, fmt.Errorf(`"d" (domain) is required`)
	}
	if payload.Key != "" {
		parsed, err := uuid.Parse(payload.Key)
		if err != nil || parsed.Variant() != uuid.RFC4122 {
			return nil, fmt.Errorf(`"k" (idempotency key) must be an RFC 4122 UUID`)
		}
	}

	return &payload, nil
}

// IsInteractive reports whether the event counts as an interaction, defaulting
// to true. The default matters: the bounce rule turns on it, and an event that
// forgot the flag is far more likely to be a real interaction than not.
func (p *Payload) IsInteractive() bool {
	if p.Interactive == nil {
		return true
	}

	return *p.Interactive
}

// ScrollDepth returns the reported scroll depth clamped to the storable range.
// Anything outside 0-100 is not a measurement, so it becomes "never reported"
// rather than a number that would drag an average somewhere impossible.
func (p *Payload) ScrollDepth() int {
	if p.Scroll == nil {
		return ScrollDepthUnset
	}

	value, err := p.Scroll.Float64()
	if err != nil || math.IsNaN(value) || value < 0 || value > 100 {
		return ScrollDepthUnset
	}

	return int(value)
}

// ScreenSize returns the viewport bucket for the reported width, or the empty
// string when nothing usable was sent. It is bucketed here rather than stored
// as pixels because a dimension table keyed on raw widths grows a row per
// device and answers no question anybody asks: what a report is for is "how
// does this page do on a phone".
//
// The boundaries are the breakpoints layouts are already written against, so
// the buckets line up with the CSS the customer wrote.
func (p *Payload) ScreenSize() string {
	if p.Width == nil {
		return ""
	}

	value, err := p.Width.Float64()
	if err != nil || math.IsNaN(value) || value <= 0 || value > MaxScreenWidth {
		return ""
	}

	switch width := int(value); {
	case width < 576:
		return ScreenMobile
	case width < 992:
		return ScreenTablet
	case width < 1440:
		return ScreenLaptop
	default:
		return ScreenDesktop
	}
}

// EngagementTime returns the engagement time in milliseconds, and whether it
// had to be clamped. A stale tracker reporting a wall-clock timestamp here
// would put four hundred million seconds into a time-on-page average, and no
// later job could tell which rows were wrong.
func (p *Payload) EngagementTime() (int64, bool) {
	if p.Engage == nil {
		return 0, false
	}

	value, err := p.Engage.Float64()
	if err != nil || math.IsNaN(value) || value < 0 || value > MaxEngagementTime {
		return 0, p.Engage.String() != "0"
	}

	return int64(value), false
}

// TrackerVersion returns the tracker version as an integer, or zero. It is
// informational — a support answer to "which script is this site running" —
// so an unparseable value is not worth failing an event over.
func (p *Payload) TrackerVersion() int {
	if p.Version == "" {
		return 0
	}

	value, err := p.Version.Int64()
	if err != nil {
		return 0
	}

	return int(value)
}

// ParseProps decodes the custom properties and applies the limits, reporting
// exactly what it had to cut. The properties arrive either as a JSON object or
// as a string containing one, because different tracker versions send both and
// refusing either would lose real events.
func ParseProps(raw json.RawMessage) (map[string]string, Truncation, error) {
	var truncation Truncation

	if len(raw) == 0 || string(raw) == "null" {
		return nil, truncation, nil
	}

	body := raw

	// A quoted string here is a JSON document inside a JSON document. Unwrapping
	// it once is the difference between reading the props and storing the
	// literal text of the object.
	if len(body) > 0 && body[0] == '"' {
		var inner string
		if err := json.Unmarshal(body, &inner); err != nil {
			return nil, truncation, fmt.Errorf(`"p" is not valid JSON: %w`, err)
		}
		if strings.TrimSpace(inner) == "" {
			return nil, truncation, nil
		}
		body = json.RawMessage(inner)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, truncation, fmt.Errorf(`"p" is not a JSON object: %w`, err)
	}

	if len(decoded) == 0 {
		return nil, truncation, nil
	}

	// The keys are sorted so that which properties survive the cap is
	// deterministic. Map order in Go is randomised, and without this the same
	// event replayed would keep a different thirty each time.
	names := make([]string, 0, len(decoded))
	for name := range decoded {
		names = append(names, name)
	}
	sortStrings(names)

	capacity := len(names)
	if capacity > MaxProps {
		capacity = MaxProps
	}

	props := make(map[string]string, capacity)

	for _, name := range names {
		if len(props) >= MaxProps {
			truncation.PropsDropped++
			continue
		}

		value, ok := propValue(decoded[name])
		if !ok {
			// The property was sent and nothing is stored for it. Counting it
			// is the difference between a customer seeing "we could not keep
			// this" and a filter value that simply never appears.
			truncation.PropsUnsupported++
			continue
		}

		key := name
		if len(key) > MaxPropNameLength {
			key = key[:MaxPropNameLength]
			truncation.PropNamesTruncated++
		}

		if len(value) > MaxPropValueLength {
			value = value[:MaxPropValueLength]
			truncation.PropValuesTruncated++
		}

		props[key] = value
	}

	return props, truncation, nil
}

// propValue flattens one property to a string. Everything is stored as text
// because a property is a filter value on a dashboard, and a column that is
// sometimes a number and sometimes a string is a column no query can index.
func propValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		// Integers are formatted without a decimal point, because "1" and "1.0"
		// arriving from two tracker versions would otherwise be two rows.
		if typed == math.Trunc(typed) && math.Abs(typed) < 1e15 {
			return strconv.FormatInt(int64(typed), 10), true
		}
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case json.Number:
		return typed.String(), true
	case nil:
		// A null property is the same as an absent one. Storing "null" as a
		// filterable value would put it on the dashboard as if somebody meant it.
		return "", false
	}

	// Objects and arrays are not filterable and are not what this field is for.
	return "", false
}

// ParseRevenue decodes the revenue object. The amount is accepted as either a
// string or a number because both appear in the wild, and it is converted to
// minor units here so that nothing downstream ever holds money in a float.
func ParseRevenue(raw json.RawMessage) (*Revenue, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var decoded struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}

	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf(`"$" is not a revenue object: %w`, err)
	}

	currency := strings.ToUpper(strings.TrimSpace(decoded.Currency))
	if currency == "" || len(decoded.Amount) == 0 {
		return nil, nil
	}

	text := strings.Trim(string(decoded.Amount), `"`)
	amount, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return nil, fmt.Errorf(`"$".amount %q is not a number`, text)
	}

	// Rounding at the boundary is the only place it can be done once. Two
	// hundred-based currencies is not universal, but it is what every payment
	// provider reports and matching them keeps reconciliation possible.
	return &Revenue{Amount: int64(math.Round(amount * 100)), Currency: currency}, nil
}

// sortStrings sorts in place. It is here rather than a sort.Strings call so the
// hot path does not pull in the reflection-based sort for a slice that is
// almost always under ten elements.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
