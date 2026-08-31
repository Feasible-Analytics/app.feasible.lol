//
// payload_test.go
// Tests for the request contract and the limits we count rather than hide.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestParsePayloadKeys checks the single-letter keys map to the right fields.
// They match the established /api/event shape byte for byte, which is what lets
// somebody migrate by changing one hostname; renaming any of them would break
// every deployed tracker in the world.
func TestParsePayloadKeys(t *testing.T) {
	body := `{"n":"pageview","u":"https://example.com/pricing?utm_source=x",
	          "d":"example.com","r":"https://google.com/","t":"Pricing",
	          "v":3,"i":false,"sd":42,"e":9000}`

	payload, err := ParsePayload([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	if payload.Name != "pageview" || payload.Domain != "example.com" {
		t.Fatalf("name/domain = %q/%q", payload.Name, payload.Domain)
	}
	if payload.Title != "Pricing" {
		t.Fatalf("title = %q, want Pricing", payload.Title)
	}
	if payload.TrackerVersion() != 3 {
		t.Fatalf("tracker version = %d, want 3", payload.TrackerVersion())
	}
	if payload.IsInteractive() {
		t.Fatal("interactive should be false when the payload says so")
	}
	if payload.ScrollDepth() != 42 {
		t.Fatalf("scroll depth = %d, want 42", payload.ScrollDepth())
	}

	engagement, clamped := payload.EngagementTime()
	if engagement != 9000 || clamped {
		t.Fatalf("engagement = %d clamped=%v, want 9000 false", engagement, clamped)
	}
}

// TestRequiredFields checks the three fields nothing can be derived without.
// The caller is a script somebody wrote, and the only person who can fix it is
// reading the response.
func TestRequiredFields(t *testing.T) {
	cases := map[string]string{
		"no name":   `{"u":"https://example.com/","d":"example.com"}`,
		"no domain": `{"n":"pageview","u":"https://example.com/"}`,
		"no url":    `{"n":"pageview","d":"example.com"}`,
		"empty":     ``,
		"not json":  `pageview`,
	}

	for name, body := range cases {
		if _, err := ParsePayload([]byte(body)); err == nil {
			t.Errorf("%s: ParsePayload accepted it", name)
		}
	}
}

// TestInteractiveDefaultsToTrue checks the default the bounce rule turns on. An
// event that forgot the flag is far more likely to be a real interaction than
// not.
func TestInteractiveDefaultsToTrue(t *testing.T) {
	payload, err := ParsePayload([]byte(`{"n":"signup","u":"https://example.com/","d":"example.com"}`))
	if err != nil {
		t.Fatal(err)
	}

	if !payload.IsInteractive() {
		t.Fatal("interactive defaulted to false")
	}
}

// TestEngagementTimeIsClamped is the defence against a stale tracker. A
// null-arithmetic race in the incumbent's script reported Date.now() here, and
// old scripts stay in browser caches for months.
func TestEngagementTimeIsClamped(t *testing.T) {
	body := fmt.Sprintf(`{"n":"engagement","u":"https://example.com/","d":"example.com","e":%d}`, 1756512000000)

	payload, err := ParsePayload([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	engagement, clamped := payload.EngagementTime()
	if engagement != 0 {
		t.Fatalf("engagement = %d, want 0", engagement)
	}
	if !clamped {
		t.Fatal("an absurd engagement time was clamped without being counted")
	}
}

// TestScrollDepthOutOfRangeBecomesUnset checks a value outside 0-100 is not a
// measurement. The unset sentinel sits outside the range so an average of real
// scroll depths never has to exclude a NULL.
func TestScrollDepthOutOfRangeBecomesUnset(t *testing.T) {
	for _, value := range []string{"-5", "101", "999999"} {
		payload, err := ParsePayload([]byte(`{"n":"engagement","u":"https://example.com/","d":"example.com","sd":` + value + `}`))
		if err != nil {
			t.Fatal(err)
		}

		if got := payload.ScrollDepth(); got != ScrollDepthUnset {
			t.Errorf("scroll depth %s became %d, want %d", value, got, ScrollDepthUnset)
		}
	}
}

// TestPropsOverTheCapAreCountedNotHidden is the behaviour this product exists
// to fix. The incumbent drops properties past the thirtieth with no error, no
// warning and no rejection, and it is one of their most-reported mysteries.
func TestPropsOverTheCapAreCountedNotHidden(t *testing.T) {
	raw := map[string]string{}
	for i := 0; i < MaxProps+7; i++ {
		raw[fmt.Sprintf("key%02d", i)] = "value"
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}

	props, truncation, err := ParseProps(encoded)
	if err != nil {
		t.Fatal(err)
	}

	if len(props) != MaxProps {
		t.Fatalf("kept %d properties, want %d", len(props), MaxProps)
	}
	if truncation.PropsDropped != 7 {
		t.Fatalf("counted %d dropped properties, want 7", truncation.PropsDropped)
	}
	if !truncation.Any() {
		t.Fatal("the truncation was not reported at all")
	}
}

// TestWhichPropsSurviveIsDeterministic checks the cap is not at the mercy of
// Go's randomised map order. Without a sort, the same event replayed would keep
// a different thirty each time and no two runs would agree.
func TestWhichPropsSurviveIsDeterministic(t *testing.T) {
	raw := map[string]string{}
	for i := 0; i < MaxProps+10; i++ {
		raw[fmt.Sprintf("key%02d", i)] = "value"
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}

	first, _, err := ParseProps(encoded)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		again, _, err := ParseProps(encoded)
		if err != nil {
			t.Fatal(err)
		}

		if len(again) != len(first) {
			t.Fatalf("kept %d properties, want %d", len(again), len(first))
		}
		for name := range first {
			if _, ok := again[name]; !ok {
				t.Fatalf("property %q survived one parse and not another", name)
			}
		}
	}
}

// TestOverlongPropsAreTrimmedAndCounted covers the name and value limits, both
// of which are reported rather than applied quietly.
func TestOverlongPropsAreTrimmedAndCounted(t *testing.T) {
	name := strings.Repeat("n", MaxPropNameLength+50)
	value := strings.Repeat("v", MaxPropValueLength+50)

	encoded, err := json.Marshal(map[string]string{name: value})
	if err != nil {
		t.Fatal(err)
	}

	props, truncation, err := ParseProps(encoded)
	if err != nil {
		t.Fatal(err)
	}

	if truncation.PropNamesTruncated != 1 || truncation.PropValuesTruncated != 1 {
		t.Fatalf("truncation = %+v, want one of each", truncation)
	}

	for storedName, storedValue := range props {
		if len(storedName) != MaxPropNameLength {
			t.Errorf("name is %d characters, want %d", len(storedName), MaxPropNameLength)
		}
		if len(storedValue) != MaxPropValueLength {
			t.Errorf("value is %d characters, want %d", len(storedValue), MaxPropValueLength)
		}
	}
}

// TestPropsAsAString covers the tracker versions that send the object
// double-encoded. Refusing it would lose real events, and storing the literal
// text would make every property one unusable string.
func TestPropsAsAString(t *testing.T) {
	encoded, err := json.Marshal(`{"plan":"pro","seats":3}`)
	if err != nil {
		t.Fatal(err)
	}

	props, _, err := ParseProps(encoded)
	if err != nil {
		t.Fatal(err)
	}

	if props["plan"] != "pro" {
		t.Fatalf("plan = %q, want pro", props["plan"])
	}
	if props["seats"] != "3" {
		t.Fatalf("seats = %q, want 3 — an integer must not gain a decimal point", props["seats"])
	}
}

// TestPropValueTypes checks each scalar flattens the way a filter needs. "1" and
// "1.0" arriving from two tracker versions would otherwise be two rows on every
// report.
func TestPropValueTypes(t *testing.T) {
	props, _, err := ParseProps([]byte(`{"s":"text","b":true,"i":42,"f":1.5,"n":null,"o":{"a":1},"a":[1,2]}`))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"s": "text", "b": "true", "i": "42", "f": "1.5"}
	for name, value := range want {
		if props[name] != value {
			t.Errorf("prop %q = %q, want %q", name, props[name], value)
		}
	}

	// A null property is the same as an absent one, and objects and arrays are
	// not filterable values.
	for _, name := range []string{"n", "o", "a"} {
		if _, ok := props[name]; ok {
			t.Errorf("prop %q was stored and should not have been", name)
		}
	}
}

// TestUnstorablePropValuesAreCounted is the same promise as the thirty-property
// cap, in another guise: an object, an array or a null is sent, nothing is
// stored for it, and a customer looking for that filter value has to be able to
// find out why rather than watch it not appear.
func TestUnstorablePropValuesAreCounted(t *testing.T) {
	props, truncation, err := ParseProps([]byte(`{"kept":"text","obj":{"a":1},"arr":[1,2],"nil":null}`))
	if err != nil {
		t.Fatal(err)
	}

	if len(props) != 1 {
		t.Fatalf("stored %d properties, want only the scalar one", len(props))
	}
	if truncation.PropsUnsupported != 3 {
		t.Fatalf("counted %d unstorable properties, want 3 — the rest vanished silently", truncation.PropsUnsupported)
	}
	if !truncation.Any() {
		t.Fatal("Any() says nothing was cut, so the handler never records the count")
	}
}

// TestScreenSizeBuckets checks the viewport width becomes one of four buckets.
// Storing raw pixel widths would grow a dimension row per device and answer no
// question anybody asks; the boundaries are the breakpoints layouts are already
// written against.
func TestScreenSizeBuckets(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":390}`, ScreenMobile},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":575}`, ScreenMobile},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":576}`, ScreenTablet},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":991}`, ScreenTablet},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":992}`, ScreenLaptop},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":1440}`, ScreenDesktop},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":2560}`, ScreenDesktop},

		// Nothing usable is no bucket rather than a made-up one.
		{`{"n":"pageview","u":"https://example.com/","d":"example.com"}`, ""},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":0}`, ""},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":-800}`, ""},
		{`{"n":"pageview","u":"https://example.com/","d":"example.com","w":999999}`, ""},
	}

	for _, test := range cases {
		payload, err := ParsePayload([]byte(test.body))
		if err != nil {
			t.Fatal(err)
		}

		if got := payload.ScreenSize(); got != test.want {
			t.Errorf("%s: screen size = %q, want %q", test.body, got, test.want)
		}
	}
}

// TestRevenueIsMinorUnits checks money never becomes a float. A currency amount
// in a float is a rounding error waiting for a large enough report.
func TestRevenueIsMinorUnits(t *testing.T) {
	cases := []struct {
		body     string
		amount   int64
		currency string
	}{
		{`{"amount":"10.99","currency":"usd"}`, 1099, "USD"},
		{`{"amount":10.99,"currency":"EUR"}`, 1099, "EUR"},
		{`{"amount":"0.01","currency":"GBP"}`, 1, "GBP"},
		{`{"amount":1234,"currency":"JPY"}`, 123400, "JPY"},
	}

	for _, tc := range cases {
		revenue, err := ParseRevenue([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if revenue == nil {
			t.Fatalf("%s: no revenue parsed", tc.body)
		}
		if revenue.Amount != tc.amount || revenue.Currency != tc.currency {
			t.Errorf("%s: got %d %s, want %d %s", tc.body, revenue.Amount, revenue.Currency, tc.amount, tc.currency)
		}
	}
}

// TestRevenueWithoutACurrencyIsIgnored checks a half-filled revenue object does
// not produce a number nobody can reconcile.
func TestRevenueWithoutACurrencyIsIgnored(t *testing.T) {
	revenue, err := ParseRevenue([]byte(`{"amount":"10.00"}`))
	if err != nil {
		t.Fatal(err)
	}
	if revenue != nil {
		t.Fatal("an amount with no currency was stored")
	}
}

// TestAcceptableContentTypes is the compatibility rule that matters most.
// text/plain avoids a CORS preflight, which is why the official trackers use it;
// rejecting it breaks every existing integration on the internet.
func TestAcceptableContentTypes(t *testing.T) {
	accepted := []string{
		"application/json",
		"application/json; charset=utf-8",
		"text/plain",
		"text/plain;charset=UTF-8",
		"TEXT/PLAIN",
		"application/x-www-form-urlencoded",
		"",
	}

	for _, value := range accepted {
		if !acceptableContentType(value) {
			t.Errorf("content type %q was rejected", value)
		}
	}

	for _, value := range []string{"application/xml", "multipart/form-data", "image/png"} {
		if acceptableContentType(value) {
			t.Errorf("content type %q was accepted", value)
		}
	}
}
