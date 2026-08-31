//
// badinput_test.go
// The promise: a caller's mistake is a 400 with a sentence, never a 500.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"net/http"
	"testing"
)

// This is the file the whole package exists to make pass.
//
// The incumbent's breakdown endpoint returns a 500 for `page=foo`, because the
// value goes straight into an integer parse and the exception escapes. A 500 is
// indistinguishable from an outage: the caller cannot tell a typo from a broken
// server, and neither can the support desk they mail about it.
//
// Every case below is something a client can send by accident. Every one has to
// come back as a status the caller can act on and a message that names what was
// wrong.

// TestBadQueryStringIsAlwaysFourHundred drives the v1 endpoints, where the
// parameters arrive as untyped text and the danger is highest.
func TestBadQueryStringIsAlwaysFourHundred(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name string
		path string
	}{
		// The one that started this. Two spellings, because a client that
		// interpolates an empty variable sends the second.
		{"page is not a number", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&page=foo"},
		{"page is empty-ish text", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&page=%20x"},
		{"page is negative", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&page=-3"},
		{"page is a float", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&page=1.5"},
		{"limit is not a number", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&limit=all"},
		{"limit is absurd", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&limit=99999999"},
		{"limit is zero", "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&limit=0"},

		{"no property at all", "/api/v1/stats/breakdown?site_id=example.com"},
		{"a property that does not exist", "/api/v1/stats/breakdown?site_id=example.com&property=visit:planet"},
		{"a property with no key after the colon", "/api/v1/stats/breakdown?site_id=example.com&property=event:props:"},

		{"a metric that does not exist", "/api/v1/stats/aggregate?site_id=example.com&metrics=vistors"},
		{"a metric listed twice", "/api/v1/stats/aggregate?site_id=example.com&metrics=visitors,visitors"},
		{"an empty metric list", "/api/v1/stats/aggregate?site_id=example.com&metrics=,"},

		{"a period nobody has", "/api/v1/stats/aggregate?site_id=example.com&period=fortnight"},
		{"a date that is not a date", "/api/v1/stats/aggregate?site_id=example.com&period=day&date=yesterday"},
		{"a custom period with one date", "/api/v1/stats/aggregate?site_id=example.com&period=custom&date=2026-08-01"},
		{"a custom period with no dates", "/api/v1/stats/aggregate?site_id=example.com&period=custom"},
		{"a custom period that ends before it starts", "/api/v1/stats/aggregate?site_id=example.com&period=custom&date=2026-08-30,2026-08-01"},
		{"a month that does not exist", "/api/v1/stats/aggregate?site_id=example.com&period=custom&date=2026-13-01,2026-13-02"},

		{"an interval nobody has", "/api/v1/stats/timeseries?site_id=example.com&interval=fortnightly"},

		{"a comparison mode nobody has", "/api/v1/stats/aggregate?site_id=example.com&compare=last_tuesday"},
		{"with_bots is not a boolean", "/api/v1/stats/aggregate?site_id=example.com&with_bots=maybe"},

		{"a filter that is not a predicate", "/api/v1/stats/aggregate?site_id=example.com&filters=nonsense"},
		{"a filter on a dimension that does not exist", "/api/v1/stats/aggregate?site_id=example.com&filters=visit%3Aplanet%3D%3DMars"},
		{"a filter with no value", "/api/v1/stats/aggregate?site_id=example.com&filters=visit%3Asource%3D%3D"},
		{"a filter with no dimension", "/api/v1/stats/aggregate?site_id=example.com&filters=%3D%3DGoogle"},
		{"a goal filter with the wrong operator", "/api/v1/stats/aggregate?site_id=example.com&filters=event%3Agoal%21%3DSignup"},
	}

	if len(cases) < 10 {
		t.Fatalf("this suite has to carry at least ten cases, it has %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.get(t, tc.path)

			if status >= 500 {
				t.Fatalf("status = %d — a caller's mistake must never be a 500 (%s)", status, body)
			}

			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", status, body)
			}

			decoded := decode(t, body)

			message, ok := decoded["error"].(string)
			if !ok || message == "" {
				t.Fatalf("a refusal must carry a reason, got %s", body)
			}
		})
	}
}

// TestBadJSONBodyIsAlwaysFourHundred drives the endpoints that take a body,
// where the danger is a decoder panic rather than a parse error.
func TestBadJSONBodyIsAlwaysFourHundred(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"a truncated object", http.MethodPost, "/api/v2/query", `{`},
		{"not JSON at all", http.MethodPost, "/api/v2/query", `visitors please`},
		{"an empty body", http.MethodPost, "/api/v2/query", ``},
		{"an array where an object belongs", http.MethodPost, "/api/v2/query", `[1,2,3]`},
		{"no site_id", http.MethodPost, "/api/v2/query", `{"metrics":["visitors"],"date_range":"7d"}`},
		{"no metrics", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","date_range":"7d"}`},
		{"a metric that does not exist", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["vistors"],"date_range":"7d"}`},
		{"a limit that is a string", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","pagination":{"limit":"lots"}}`},
		{"a negative offset", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","pagination":{"offset":-1}}`},
		{"a timezone that is not a place", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","timezone":"Mars/Olympus"}`},
		{"a misspelled field", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","dimenions":["visit:source"]}`},
		{"a regular expression that does not compile", http.MethodPost, "/api/v2/query", `{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","filters":[["matches","event:page",["([a-z"]]]}`},

		{"a site with no domain", http.MethodPost, "/api/v1/sites", `{"domain":""}`},
		{"a URL where a domain belongs", http.MethodPost, "/api/v1/sites", `{"domain":"https://example.org/path"}`},
		{"a hostname with no dot", http.MethodPost, "/api/v1/sites", `{"domain":"localhost"}`},
		{"a timezone that is not a place, on create", http.MethodPost, "/api/v1/sites", `{"domain":"fresh.example","timezone":"Middle/Earth"}`},
		{"a goal with neither kind", http.MethodPut, "/api/v1/sites/goals", `{"site_id":"example.com","display_name":"Signup"}`},
		{"a goal with both kinds", http.MethodPut, "/api/v1/sites/goals", `{"site_id":"example.com","event_name":"Signup","page_path":"/thanks"}`},
		{"a goal whose page is not a path", http.MethodPut, "/api/v1/sites/goals", `{"site_id":"example.com","page_path":"thanks"}`},
		{"a guest with no address", http.MethodPut, "/api/v1/sites/guests", `{"site_id":"example.com","email":"nobody"}`},
		{"a guest with a role nobody has", http.MethodPut, "/api/v1/sites/guests", `{"site_id":"example.com","email":"guest@example.test","role":"emperor"}`},
		{"a member with a role nobody has", http.MethodPut, "/api/v1/teams/memberships", `{"email":"guest@example.test","role":"emperor"}`},
		{"a webhook with no URL", http.MethodPost, "/api/v1/webhooks", `{"url":""}`},
		{"a webhook over plain http", http.MethodPost, "/api/v1/webhooks", `{"url":"http://example.org/hook"}`},
		{"a webhook subscribed to nothing real", http.MethodPost, "/api/v1/webhooks", `{"url":"https://example.org/hook","event_types":["goal.exploded"]}`},
		{"a tracker endpoint that is not a URL", http.MethodPut, "/api/v1/sites/example.com/tracker", `{"api_endpoint":"example.org/api"}`},

		// An id straight out of a URL and into an integer parse is the same
		// class of bug as `page=foo`, so every path segment that carries one is
		// parsed by something that answers with a sentence.
		{"a goal id that is not a number", http.MethodDelete, "/api/v1/sites/goals/not-a-number?site_id=example.com", ``},
		{"a shared-link id that is not a number", http.MethodDelete, "/api/v1/sites/shared-links/x?site_id=example.com", ``},
		{"a webhook id that is not a number", http.MethodGet, "/api/v1/webhooks/nine", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.do(t, tc.method, tc.path, tc.body, h.Key)

			if status >= 500 {
				t.Fatalf("status = %d — a caller's mistake must never be a 500 (%s)", status, body)
			}

			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", status, body)
			}

			decoded := decode(t, body)

			if message, ok := decoded["error"].(string); !ok || message == "" {
				t.Fatalf("a refusal must carry a reason, got %s", body)
			}
		})
	}
}

// TestAbsurdInputStillNeverFiveHundreds is the fuzzier version: values nobody
// would send deliberately, aimed at every parameter at once. It asserts only
// that nothing reaches a 500, because some of these are legitimately fine.
func TestAbsurdInputStillNeverFiveHundreds(t *testing.T) {
	h := newHarness(t)

	values := []string{
		"", " ", "0", "-1", "9999999999999999999999", "null", "undefined", "NaN",
		"%00", "'; DROP TABLE events; --", "../../etc/passwd", "<script>", "🙂",
		longValue(1000),
	}

	parameters := []string{"period", "metrics", "property", "limit", "page", "interval", "filters", "date", "compare", "site_id"}

	for _, parameter := range parameters {
		for _, value := range values {
			path := "/api/v1/stats/breakdown?site_id=example.com&property=visit:source&" + parameter + "=" + urlEncode(value)

			status, body := h.get(t, path)
			if status >= 500 {
				t.Fatalf("%s=%q produced a %d (%s)", parameter, value, status, body)
			}
		}
	}
}

// longValue builds a long value, for the parameters whose failure mode is length
// rather than shape.
func longValue(length int) string {
	out := make([]byte, length)
	for i := range out {
		out[i] = 'a'
	}

	return string(out)
}
