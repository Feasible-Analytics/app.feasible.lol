//
// clientip.go
// Resolving the client's real IP, and saying which header it came from.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package clientip is the one place a request's client address is worked out.
// The ingest pipeline, the settings page, rate limiters and audit logs all
// call it, so a proxy header is trusted under exactly the same rules
// everywhere: an address resolved differently in one place is a shield rule
// that does not match the traffic it was built from, or a rate limit an
// attacker can key with a header of their choosing.
package clientip

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

	// HeaderSenderForwardedFor carries the X-Forwarded-For chain exactly as
	// the sender supplied it, copied there by a trusted edge that overwrites
	// X-Forwarded-For with the address it observed. Only ResolveVisitorIP
	// reads it, so a sender's own claim never reaches a login or API limit.
	HeaderSenderForwardedFor = "X-Feasible-Sender-Forwarded-For"
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

	// SenderSilent is true when ResolveVisitorIP looked for the visitor a
	// sender stated and found none, so Addr is the sender a trusted edge
	// observed. A browser and a relay that forwards nothing both look like
	// this, and the caller tells them apart by what kind of address it is.
	SenderSilent bool
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

// ResolveVisitorIP works out whose visit an event describes. It is for the
// event endpoint only, where the sender is often a customer's own proxy or
// server relaying a visitor, and the visitor is who must be located, hashed
// and rate limited.
//
// With trustSender off it is ResolveClientIP. With it on, the sender is
// believed one hop further: the chain a trusted edge copied into
// X-Feasible-Sender-Forwarded-For names the visitor, provided it holds a
// public address. A sender can only misstate visits to the site it is
// already able to send any event for, which is why this is safe for events
// and for nothing else.
//
// X-Feasible-IP and CF-Connecting-IP keep their precedence, and the address
// the edge observed is the answer whenever the sender stated nothing usable,
// marked SenderSilent.
func ResolveVisitorIP(r *http.Request, trusted *TrustedProxies, trustSender bool) ClientIP {
	client := ResolveClientIP(r, trusted)
	if !trustSender || client.Source == SourceFeasibleIP || client.Source == SourceCloudflare {
		return client
	}

	// The copy is only as good as whoever wrote it, so it is read from a
	// trusted edge and never from a directly connected sender.
	if !trusted.Contains(parseAddr(hostOnly(r.RemoteAddr))) {
		return client
	}

	if addr := statedVisitor(r.Header.Get(HeaderSenderForwardedFor), trusted); addr.IsValid() {
		return ClientIP{Addr: addr, Source: SourceForwardedFor}
	}

	client.SenderSilent = true

	return client
}

// statedVisitor returns the nearest public address in the chain a sender
// supplied, reading right to left. Private and trusted hops are skipped
// because they are the sender's own infrastructure, and a forward proxy on an
// office network states a private address that would otherwise merge everyone
// behind every such proxy. A malformed hop ends the walk with no answer.
func statedVisitor(value string, trusted *TrustedProxies) netip.Addr {
	parts := strings.Split(value, ",")

	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part == "" {
			continue
		}

		addr := parseAddr(part)
		if !addr.IsValid() {
			return netip.Addr{}
		}

		if trusted.Contains(addr) || IsPrivateOrLocal(addr) {
			continue
		}

		return addr
	}

	return netip.Addr{}
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

// Key returns a stable string for keying per-client rate limits: the resolved
// client address when one can be determined through trusted proxies, else the
// socket peer host. Never empty.
//
// It goes through ResolveClientIP rather than reading X-Forwarded-For directly
// so a client cannot pick its own bucket by sending a header, which is the
// flaw that turns a limiter into a no-op.
func Key(r *http.Request, trusted *TrustedProxies) string {
	if key := ResolveClientIP(r, trusted).String(); key != "" {
		return key
	}

	if host := hostOnly(r.RemoteAddr); host != "" {
		return host
	}

	return "unknown"
}

// IsPrivateOrLocal reports whether an address is not routable on the public
// internet: loopback, RFC 1918 and ULA ranges, link-local, unspecified and
// multicast, in either family. A v4-mapped v6 address is unmapped first, so
// ::ffff:127.0.0.1 is loopback here rather than an ordinary global address.
func IsPrivateOrLocal(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}

	addr = addr.Unmap()

	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() ||
		addr.IsUnspecified() ||
		addr.IsMulticast()
}
