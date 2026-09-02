//
// clientip_test.go
// Tests for the precedence order and the trusted-proxy allow-list.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	trusted := mustTrust(t, "10.0.0.7", "10.0.0.8")

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
			name: "all forwarded headers are ignored from anyone else",
			peer: "203.0.113.99:41000",
			headers: map[string]string{
				HeaderFeasibleIP:     "203.0.113.5",
				HeaderCFConnectingIP: "198.51.100.5",
				HeaderForwardedFor:   "192.0.2.5",
			},
			wantIP:     "203.0.113.99",
			wantSource: SourceSocket,
		},
		{
			name: "Cloudflare beats the forwarded chain",
			peer: "10.0.0.7:41000",
			headers: map[string]string{
				HeaderCFConnectingIP: "198.51.100.5",
				HeaderForwardedFor:   "192.0.2.5",
			},
			wantIP:     "198.51.100.5",
			wantSource: SourceCloudflare,
		},
		{
			name:       "the chain is walked from right to left",
			peer:       "10.0.0.7:41000",
			headers:    map[string]string{HeaderForwardedFor: "198.51.100.250, 192.0.2.5, 10.0.0.8"},
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

// TestAppendingProxyRejectsASpoofedLeftmostAddress proves a direct client
// cannot choose the value returned by an appending proxy. The proxy-observed
// client address is nearest to the trusted hops and therefore wins.
func TestAppendingProxyRejectsASpoofedLeftmostAddress(t *testing.T) {
	trusted := mustTrust(t, "10.0.0.0/8")
	req := newIPRequest("10.0.0.7:41000", map[string]string{
		HeaderForwardedFor: "203.0.113.66, 198.51.100.42, 10.0.0.8",
	})

	got := ResolveClientIP(req, trusted)
	if got.String() != "198.51.100.42" {
		t.Fatalf("address = %q, want the proxy-observed client", got.String())
	}
}

// TestMalformedForwardingChainFailsClosed prevents a bad nearest hop from
// unlocking an attacker-controlled value farther left in the chain.
func TestMalformedForwardingChainFailsClosed(t *testing.T) {
	trusted := mustTrust(t, "10.0.0.0/8")
	req := newIPRequest("10.0.0.7:41000", map[string]string{
		HeaderForwardedFor: "203.0.113.66, not-an-address, 10.0.0.8",
	})

	got := ResolveClientIP(req, trusted)
	if got.String() != "10.0.0.7" || got.Source != SourceSocket {
		t.Fatalf("malformed chain resolved %q from %q, want the socket peer", got.String(), got.Source)
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
	}), mustTrust(t, "203.0.113.99"))

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

// TestKeyCannotBeChosenByTheClient is what makes a limiter keyed on it worth
// having: a header from an untrusted peer must not move the request into a
// bucket of the sender's choosing.
func TestKeyCannotBeChosenByTheClient(t *testing.T) {
	trusted := mustTrust(t, "10.0.0.7")

	cases := []struct {
		name    string
		peer    string
		headers map[string]string
		want    string
	}{
		{
			name:    "a forwarded header from a trusted proxy keys on the client",
			peer:    "10.0.0.7:41000",
			headers: map[string]string{HeaderForwardedFor: "203.0.113.5"},
			want:    "203.0.113.5",
		},
		{
			name:    "a forwarded header from anyone else keys on the peer",
			peer:    "203.0.113.99:41000",
			headers: map[string]string{HeaderForwardedFor: "203.0.113.5"},
			want:    "203.0.113.99",
		},
		{
			name: "a v4-mapped peer keys on the same string as the plain v4",
			peer: "[::ffff:203.0.113.99]:41000",
			want: "203.0.113.99",
		},
		{
			name: "a peer that is not an address still keys on something",
			peer: "unix-socket-client",
			want: "unix-socket-client",
		},
		{
			name: "no peer at all is never an empty key",
			peer: "",
			want: "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(newIPRequest(tc.peer, tc.headers), trusted); got != tc.want {
				t.Errorf("Key = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsPrivateOrLocal enumerates the ranges an outbound request must never
// reach and the settings page must warn about, in both families and in the
// v4-mapped form a dual-stack listener produces.
func TestIsPrivateOrLocal(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":          true,
		"127.8.8.8":          true,
		"::1":                true,
		"10.0.0.5":           true,
		"172.16.4.4":         true,
		"192.168.178.1":      true,
		"fd12::1":            true,
		"169.254.169.254":    true,
		"fe80::1":            true,
		"ff02::1":            true,
		"224.0.0.1":          true,
		"0.0.0.0":            true,
		"::":                 true,
		"::ffff:127.0.0.1":   true,
		"::ffff:10.0.0.5":    true,
		"::ffff:169.254.1.1": true,
		"203.0.113.5":        false,
		"8.8.8.8":            false,
		"2001:db8::1":        false,
		"::ffff:203.0.113.5": false,
	}

	for value, want := range cases {
		if got := IsPrivateOrLocal(netip.MustParseAddr(value)); got != want {
			t.Errorf("IsPrivateOrLocal(%s) = %v, want %v", value, got, want)
		}
	}

	if IsPrivateOrLocal(netip.Addr{}) {
		t.Error("the zero address reported as private; callers check validity themselves")
	}
}
