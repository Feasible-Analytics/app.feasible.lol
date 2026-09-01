//
// clientip.go
// Resolving the visitor's real IP, and saying which header it came from.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// The headers we read, in precedence order. They are constants because the
// order below affects fingerprints, geolocation and IP shields, and a wrong
// result fails silently behind a 202.
const (
	// HeaderFeasibleIP is our own override, honoured only from an address on
	// the trusted-proxy list.
	HeaderFeasibleIP = "X-Feasible-IP"

	// HeaderCFConnectingIP is an edge-proxy supplied client address.
	HeaderCFConnectingIP = "CF-Connecting-IP"

	// HeaderForwardedFor carries the chain appended by each proxy.
	HeaderForwardedFor = "X-Forwarded-For"
)

// Names for where an address came from, returned to the debug endpoint. A
// customer debugging a proxy needs to know which header we believed, and this
// is the difference between a five-minute fix and a day of guessing.
const (
	SourceFeasibleIP   = "x-feasible-ip"
	SourceCloudflare   = "cf-connecting-ip"
	SourceForwardedFor = "x-forwarded-for"
	SourceSocket       = "socket"
	SourceNone         = "none"
)

// TrustedProxies is an allow-list of addresses permitted to supply forwarded
// client-address headers. It is a type rather than a bare slice so the empty
// case has one obvious meaning: nobody is trusted, which is the safe default
// for an instance exposed straight to the internet.
type TrustedProxies struct {
	prefixes []netip.Prefix
}

// ParseTrustedProxies builds the allow-list from configuration. Both a bare
// address and a CIDR block are accepted, because an operator thinks in terms of
// "my load balancer is 10.0.0.7" as often as in terms of a subnet.
func ParseTrustedProxies(values []string) (*TrustedProxies, error) {
	list := &TrustedProxies{}

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		if strings.Contains(value, "/") {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return nil, err
			}
			list.prefixes = append(list.prefixes, prefix.Masked())
			continue
		}

		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, err
		}
		list.prefixes = append(list.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}

	return list, nil
}

// Contains reports whether an address may supply forwarded client-address
// headers. An invalid address is never trusted, so a socket peer we could not
// parse cannot accidentally unlock them.
func (t *TrustedProxies) Contains(addr netip.Addr) bool {
	if t == nil || !addr.IsValid() {
		return false
	}

	// A v4 address arriving as a v4-mapped v6 would not match a v4 prefix, and
	// that is exactly the shape a dual-stack listener produces.
	addr = addr.Unmap()

	for _, prefix := range t.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// Empty reports whether anything is trusted at all. The debug endpoint says so
// explicitly, because a forwarded header that was ignored is otherwise
// indistinguishable from a header that never arrived.
func (t *TrustedProxies) Empty() bool {
	return t == nil || len(t.prefixes) == 0
}

// ClientIP is a resolved address and the evidence behind it.
type ClientIP struct {
	Addr netip.Addr

	// Source names the header the address came from, for the debug endpoint.
	Source string
}

// String renders the address for hashing and logging. It is the canonical
// textual form, which matters because it is one of the four fingerprint inputs
// and "1.2.3.4" and "::ffff:1.2.3.4" would otherwise be two different visitors.
func (c ClientIP) String() string {
	if !c.Addr.IsValid() {
		return ""
	}

	return c.Addr.Unmap().String()
}

// ResolveClientIP works out who sent a request. The precedence is explicit and
// documented because every alternative ordering has cost somebody a day:
//
//	X-Feasible-IP → CF-Connecting-IP → first untrusted XFF hop →
//	socket peer
//
// All three headers require a trusted socket peer. X-Forwarded-For is walked
// from right to left so a trusted appending proxy cannot be tricked by a value
// a direct client placed at the left of the chain.
func ResolveClientIP(r *http.Request, trusted *TrustedProxies) ClientIP {
	peer := parseAddr(hostOnly(r.RemoteAddr))

	// Trust is checked against the socket peer, never against a forwarded
	// header: a header cannot authorise itself.
	if trusted.Contains(peer) {
		if addr := parseAddr(strings.TrimSpace(r.Header.Get(HeaderFeasibleIP))); addr.IsValid() {
			return ClientIP{Addr: addr, Source: SourceFeasibleIP}
		}

		if addr := parseAddr(strings.TrimSpace(r.Header.Get(HeaderCFConnectingIP))); addr.IsValid() {
			return ClientIP{Addr: addr, Source: SourceCloudflare}
		}

		if addr := forwardedClient(r.Header.Get(HeaderForwardedFor), trusted); addr.IsValid() {
			return ClientIP{Addr: addr, Source: SourceForwardedFor}
		}
	}

	if peer.IsValid() {
		return ClientIP{Addr: peer, Source: SourceSocket}
	}

	return ClientIP{Source: SourceNone}
}

// forwardedClient returns the nearest untrusted address in an X-Forwarded-For
// chain. A conforming proxy appends the address it received the request from,
// so reading right to left discards infrastructure hops while retaining the
// client address the outermost trusted proxy observed.
func forwardedClient(value string, trusted *TrustedProxies) netip.Addr {
	parts := strings.Split(value, ",")
	var leftmost netip.Addr

	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part == "" {
			continue
		}

		addr := parseAddr(part)
		if !addr.IsValid() {
			// An invalid hop breaks the evidence chain. Continuing farther left
			// could accept a value supplied by the client before the malformed
			// address, so fail closed and use the socket peer instead.
			return netip.Addr{}
		}

		leftmost = addr
		if !trusted.Contains(addr) {
			return addr
		}
	}

	return leftmost
}

// hostOnly strips a port from an address in either family. RemoteAddr always
// carries one, and the forwarded headers carry one often enough — some proxies
// append the source port — that both paths need it.
func hostOnly(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	// A bracketed form is always host:port or a bare bracketed literal, and
	// SplitHostPort handles both shapes correctly.
	if strings.HasPrefix(value, "[") {
		if host, _, err := net.SplitHostPort(value); err == nil {
			return host
		}
		return strings.Trim(value, "[]")
	}

	// An unbracketed value with more than one colon is a bare IPv6 literal, and
	// cutting at the last colon would turn it into a different address.
	if strings.Count(value, ":") > 1 {
		return value
	}

	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}

	return value
}

// parseAddr turns a string into an address, returning the zero value for
// anything unparseable. It strips a zone identifier because a link-local
// address arriving as "fe80::1%eth0" would otherwise fail to parse and silently
// fall through to the next header.
func parseAddr(value string) netip.Addr {
	value = hostOnly(value)
	if value == "" {
		return netip.Addr{}
	}

	if index := strings.Index(value, "%"); index > 0 {
		value = value[:index]
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}
	}

	return addr.Unmap()
}
