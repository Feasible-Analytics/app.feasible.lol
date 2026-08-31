//
// fingerprint.go
// The cookieless visitor identifier: SipHash-2-4 over a bare concatenation.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"strings"

	"github.com/dchest/siphash"
	"golang.org/x/net/publicsuffix"
)

// NoneHostname is what a URL with no usable host becomes. It is a literal
// string rather than an empty one because it is an input to the fingerprint,
// and an empty string there would collide with a genuinely absent hostname.
const NoneHostname = "(none)"

// Fingerprint computes the visitor id.
//
//	user_id = SipHash-2-4(key = daily_salt,
//	                      msg = user_agent || client_ip || site_domain || root_domain)
//
// Three things about this function are load-bearing and none of them can be
// changed later. Get any of them wrong and every number in the product is
// subtly wrong forever, with no way to recompute from stored data.
//
//   - The message is a bare concatenation with no separators. Adding a
//     delimiter — even one byte — changes every hash ever produced.
//   - The salt is the SipHash key, not a prefix of the message.
//   - root_domain is the registrable domain, which is why app.example.com and
//     example.com produce the same visitor id. Subdomains share visitors by
//     design; it is not an accident to be fixed.
func Fingerprint(salt []byte, userAgent, clientIP, siteDomain, rootDomain string) int64 {
	// A strings.Builder rather than concatenation because this runs once per
	// event and the four parts are known, so one allocation is all it needs.
	var msg strings.Builder
	msg.Grow(len(userAgent) + len(clientIP) + len(siteDomain) + len(rootDomain))
	msg.WriteString(userAgent)
	msg.WriteString(clientIP)
	msg.WriteString(siteDomain)
	msg.WriteString(rootDomain)

	// SipHash takes a 128-bit key as two 64-bit halves. The salt is exactly 16
	// bytes, so the halves are its little-endian words.
	k0, k1 := saltKey(salt)

	// The hash is a uint64 and the column is a signed INTEGER. Reinterpreting
	// the bits rather than clamping or masking keeps all 64 bits of the hash,
	// which is what keeps collisions at the rate the hash promises.
	return int64(siphash.Hash(k0, k1, []byte(msg.String())))
}

// saltKey splits a 16-byte salt into SipHash's two key words. A short salt is
// zero-padded rather than rejected, because the only caller that can produce
// one is a test, and a panic on the ingest path over a configuration mistake
// would take the whole front door down.
func saltKey(salt []byte) (uint64, uint64) {
	var key [16]byte
	copy(key[:], salt)

	var k0, k1 uint64
	for i := 0; i < 8; i++ {
		k0 |= uint64(key[i]) << (8 * i)
		k1 |= uint64(key[8+i]) << (8 * i)
	}

	return k0, k1
}

// RootDomain returns the registrable domain of a hostname — the part somebody
// actually bought. It is one of the four fingerprint inputs, so its edge cases
// are part of the formula rather than a detail:
//
//   - An IPv4 literal is returned as-is. The public-suffix list has no answer
//     for one, and inventing a fallback would split every visitor behind a
//     bare-IP install.
//   - The string "(none)" is returned as-is, for the same reason.
//   - Anything the list cannot resolve falls back to the hostname itself.
func RootDomain(hostname string) string {
	if hostname == "" || hostname == NoneHostname {
		return hostname
	}

	// An IPv4 literal is all digits and dots. Testing the shape rather than
	// parsing it keeps this cheap, and a hostname of that shape is not a
	// registrable domain either way.
	if isIPv4Literal(hostname) {
		return hostname
	}

	root, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil || root == "" {
		return hostname
	}

	return root
}

// isIPv4Literal reports whether a hostname is four dot-separated numbers. It
// exists because the public-suffix list would otherwise hand back the last
// octet as the "domain", turning every visitor on a bare-IP install into a
// different person the moment their address changed.
func isIPv4Literal(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}

	for _, part := range parts {
		if part == "" || len(part) > 3 {
			return false
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}

	return true
}
