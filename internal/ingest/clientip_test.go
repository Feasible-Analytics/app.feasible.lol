//
// clientip_test.go
// Tests for the precedence order and the trusted-proxy allow-list.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newIPRequest builds a request with a socket peer and a set of headers, which
// is the only shape this resolution ever sees.
func newIPRequest(peer string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/event", nil)
	req.RemoteAddr = peer

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	return req
}

// mustTrust builds an allow-list or fails the test. A malformed entry here would
// silently trust nobody, which is the failure this helper makes loud.
func mustTrust(t testing.TB, entries ...string) *TrustedProxies {
	t.Helper()

	list, err := ParseTrustedProxies(entries)
	if err != nil {
		t.Fatal(err)
	}

	return list
}

// TestClientIPPrecedence walks the documented order. Every wrong ordering here
// has cost somebody a day and none of them raise an error anywhere, which is
// why the order is asserted rather than assumed.
func TestClientIPPrecedence(t *testing.T) {
	trusted := mustTrust(t, "10.0.0.7")

	cases := []struct {
		name       string
		peer       string
		headers    map[string]string
		wantIP     string
		wantSource string
	}{
		{
			name: "the override wins from a trusted proxy",
			peer: "10.0.0.7:41000",
			headers: map[string]string{
				HeaderFeasibleIP:     "203.0.113.5",
				HeaderCFConnectingIP: "198.51.100.5",
				HeaderForwardedFor:   "192.0.2.5",
			},
			wantIP:     "203.0.113.5",
			wantSource: SourceFeasibleIP,
		},
		{
			// The incumbent trusts this header unconditionally. On a
			// directly-exposed instance that lets anyone forge their own
			// geolocation and split their own fingerprint at will.
			name: "the override is ignored from anyone else",
			peer: "203.0.113.99:41000",
			headers: map[string]string{
				HeaderFeasibleIP:   "203.0.113.5",
				HeaderForwardedFor: "192.0.2.5",
			},
			wantIP:     "192.0.2.5",
			wantSource: SourceForwardedFor,
		},
		{
			name: "Cloudflare beats the forwarded chain",
			peer: "203.0.113.99:41000",
			headers: map[string]string{
				HeaderCFConnectingIP: "198.51.100.5",
				HeaderForwardedFor:   "192.0.2.5",
			},
			wantIP:     "198.51.100.5",
			wantSource: SourceCloudflare,
		},
		{
			// Taking the last entry — which several frameworks do — reports the
			// nearest proxy as the visitor and collapses everyone into one.
			name:       "the first forwarded entry is the client",
			peer:       "203.0.113.99:41000",
			headers:    map[string]string{HeaderForwardedFor: "192.0.2.5, 10.0.0.7, 10.0.0.8"},
			wantIP:     "192.0.2.5",
			wantSource: SourceForwardedFor,
		},
		{
			name:       "the socket peer is the last resort",
			peer:       "203.0.113.99:41000",
			headers:    nil,
			wantIP:     "203.0.113.99",
			wantSource: SourceSocket,
		},
		{
			name:       "an unparseable header falls through",
			peer:       "203.0.113.99:41000",
			headers:    map[string]string{HeaderForwardedFor: "not-an-address"},
			wantIP:     "203.0.113.99",
			wantSource: SourceSocket,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveClientIP(newIPRequest(tc.peer, tc.headers), trusted)

			if got.String() != tc.wantIP {
				t.Errorf("address = %q, want %q", got.String(), tc.wantIP)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// TestEmptyAllowListTrustsNobody is the safe default. An instance exposed
// straight to the internet with no configuration must not honour the override.
func TestEmptyAllowListTrustsNobody(t *testing.T) {
	empty := mustTrust(t)

	if !empty.Empty() {
		t.Fatal("an empty allow-list does not report itself as empty")
	}

	got := ResolveClientIP(newIPRequest("10.0.0.7:41000", map[string]string{
		HeaderFeasibleIP: "203.0.113.5",
	}), empty)

	if got.String() != "10.0.0.7" {
		t.Fatalf("address = %q, want the socket peer", got.String())
	}
}

// TestTrustedProxyCIDR checks a subnet entry works, because an operator thinks
// in terms of "my load balancers are 10.0.0.0/8" as often as a single address.
func TestTrustedProxyCIDR(t *testing.T) {
	trusted := mustTrust(t, "10.0.0.0/8", "2001:db8::/32")

	got := ResolveClientIP(newIPRequest("10.4.5.6:41000", map[string]string{
		HeaderFeasibleIP: "203.0.113.5",
	}), trusted)

	if got.Source != SourceFeasibleIP {
		t.Fatalf("a proxy inside the trusted subnet was not trusted (source %q)", got.Source)
	}

	// And one outside it is not.
	outside := ResolveClientIP(newIPRequest("11.4.5.6:41000", map[string]string{
		HeaderFeasibleIP: "203.0.113.5",
	}), trusted)

	if outside.Source == SourceFeasibleIP {
		t.Fatal("a proxy outside the trusted subnet was trusted")
	}
}

// TestPortsAndBracketsAreStripped covers both address families. A port left on
// the end makes the address unparseable, which silently falls through to the
// next header and reports the wrong visitor.
func TestPortsAndBracketsAreStripped(t *testing.T) {
	cases := []struct {
		peer string
		want string
	}{
		{"203.0.113.99:41000", "203.0.113.99"},
		{"[2001:db8::1]:41000", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"203.0.113.99", "203.0.113.99"},

		// A v4-mapped v6 address has to normalise, or the same visitor arriving
		// over a dual-stack listener would be two people.
		{"[::ffff:203.0.113.99]:41000", "203.0.113.99"},
	}

	for _, tc := range cases {
		got := ResolveClientIP(newIPRequest(tc.peer, nil), mustTrust(t))
		if got.String() != tc.want {
			t.Errorf("peer %q resolved to %q, want %q", tc.peer, got.String(), tc.want)
		}
	}
}

// TestZoneIdentifierIsStripped checks a link-local address parses. Left in, the
// zone makes ParseAddr fail and the resolution falls through to a header that
// may not be there.
func TestZoneIdentifierIsStripped(t *testing.T) {
	got := ResolveClientIP(newIPRequest("203.0.113.99:41000", map[string]string{
		HeaderForwardedFor: "fe80::1%eth0",
	}), mustTrust(t))

	if got.String() != "fe80::1" {
		t.Fatalf("address = %q, want fe80::1", got.String())
	}
}

// TestNoAddressAtAll checks the case a unix socket or a stripped RemoteAddr
// produces. Reporting "none" is what lets the debug endpoint say so rather than
// showing a blank field somebody has to guess about.
func TestNoAddressAtAll(t *testing.T) {
	got := ResolveClientIP(newIPRequest("", nil), mustTrust(t))

	if got.Source != SourceNone {
		t.Fatalf("source = %q, want %q", got.Source, SourceNone)
	}
	if got.String() != "" {
		t.Fatalf("address = %q, want empty", got.String())
	}
}

// TestBadTrustedProxyEntryIsRejected checks a typo in configuration fails at
// start-up rather than becoming an allow-list entry that matches nothing.
func TestBadTrustedProxyEntryIsRejected(t *testing.T) {
	for _, entry := range []string{"not-an-address", "10.0.0.0/99", "10.0.0.0/"} {
		if _, err := ParseTrustedProxies([]string{entry}); err == nil {
			t.Errorf("ParseTrustedProxies accepted %q", entry)
		}
	}
}
