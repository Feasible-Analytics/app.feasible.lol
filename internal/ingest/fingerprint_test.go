//
// fingerprint_test.go
// Regression tests for the one thing in this project that can never be fixed later.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import "testing"

// testSalt is a fixed sixteen-byte salt. The expected hashes below were
// computed with an independent SipHash-2-4 implementation rather than by
// recording what this code produced, because a golden file generated from the
// code under test proves only that the code has not changed — not that it was
// ever right.
var testSalt = []byte("feasible-salt-16")

// testUserAgent is one ordinary desktop Chrome header, long enough to exercise
// the multi-block path through the hash.
const testUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// TestFingerprintExactOutput pins the hash to known values. Every number in the
// product is derived from this function, and a change to it cannot be detected
// after the fact or recomputed from stored data — there is no salt to redo it
// with, by design. If this test fails, the change is wrong.
func TestFingerprintExactOutput(t *testing.T) {
	cases := []struct {
		name       string
		userAgent  string
		clientIP   string
		siteDomain string
		rootDomain string
		want       int64
	}{
		{
			name:       "ordinary visitor",
			userAgent:  testUserAgent,
			clientIP:   "203.0.113.7",
			siteDomain: "example.com",
			rootDomain: "example.com",
			want:       -2548444057373803192,
		},
		{
			name:       "different address is a different visitor",
			userAgent:  testUserAgent,
			clientIP:   "198.51.100.9",
			siteDomain: "example.com",
			rootDomain: "example.com",
			want:       4023434735161076289,
		},
		{
			name:       "absent user agent hashes as the empty string",
			userAgent:  "",
			clientIP:   "203.0.113.7",
			siteDomain: "example.com",
			rootDomain: "example.com",
			want:       -130830567970946830,
		},
		{
			name:       "the (none) hostname is a literal input",
			userAgent:  testUserAgent,
			clientIP:   "203.0.113.7",
			siteDomain: "example.com",
			rootDomain: NoneHostname,
			want:       2654490734416135523,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Fingerprint(testSalt, tc.userAgent, tc.clientIP, tc.siteDomain, tc.rootDomain)
			if got != tc.want {
				t.Fatalf("Fingerprint = %d, want %d — this value can never be recomputed, so a change here is a permanent break", got, tc.want)
			}
		})
	}
}

// TestFingerprintHasNoSeparators is the other half of the formula. The message
// is a bare concatenation, and adding a delimiter of any kind changes every hash
// ever produced — so this asserts the separated form is a different value, which
// is the only way to catch somebody "tidying up" the concatenation.
func TestFingerprintHasNoSeparators(t *testing.T) {
	bare := Fingerprint(testSalt, testUserAgent, "203.0.113.7", "example.com", "example.com")

	// The same four inputs joined with pipes, hashed as one string.
	separated := Fingerprint(testSalt, testUserAgent+"|203.0.113.7|example.com|", "", "", "example.com")

	if bare == separated {
		t.Fatal("a separated message hashed to the same value, which is impossible unless the concatenation changed")
	}
	if separated != -1789244638188400262 {
		t.Fatalf("separated form = %d, want -1789244638188400262", separated)
	}
}

// TestSubdomainsShareAVisitor is the property the fourth input exists for.
// app.example.com and example.com must produce the same visitor id: subdomains
// share visitors by design, and treating them as separate people would split
// every visit that crosses from a marketing site to an app.
func TestSubdomainsShareAVisitor(t *testing.T) {
	root := RootDomain("example.com")
	sub := RootDomain("app.example.com")

	if root != sub {
		t.Fatalf("RootDomain disagreed: %q vs %q", root, sub)
	}

	// The site domain is the same for both — it is how the site is registered —
	// so with equal registrable domains the two hashes must match exactly.
	onRoot := Fingerprint(testSalt, testUserAgent, "203.0.113.7", "example.com", root)
	onSub := Fingerprint(testSalt, testUserAgent, "203.0.113.7", "example.com", sub)

	if onRoot != onSub {
		t.Fatalf("example.com hashed to %d and app.example.com to %d; subdomains must share a visitor", onRoot, onSub)
	}
}

// TestFingerprintChangesWithTheSalt is what makes the identifier unreconstructable
// after 48 hours. The same visitor under a different salt has to be a different
// number, or rotating the salt would achieve nothing.
func TestFingerprintChangesWithTheSalt(t *testing.T) {
	today := Fingerprint(testSalt, testUserAgent, "203.0.113.7", "example.com", "example.com")
	yesterday := Fingerprint([]byte("yesterday-salt16"), testUserAgent, "203.0.113.7", "example.com", "example.com")

	if today == yesterday {
		t.Fatal("two different salts produced the same fingerprint")
	}
}

// TestRootDomain covers the edge cases the formula names explicitly. Each one is
// a hash input, so a change to any of them is a change to the fingerprint.
func TestRootDomain(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"example.com", "example.com"},
		{"app.example.com", "example.com"},
		{"deep.nested.app.example.com", "example.com"},
		{"example.co.uk", "example.co.uk"},
		{"blog.example.co.uk", "example.co.uk"},

		// An IPv4 literal is left as-is. The public-suffix list would otherwise
		// hand back the last octet, turning every visitor on a bare-IP install
		// into a different person whenever their address changed.
		{"192.168.1.10", "192.168.1.10"},
		{"203.0.113.7", "203.0.113.7"},

		// The (none) sentinel and an unresolvable hostname fall through
		// unchanged rather than becoming an empty string.
		{NoneHostname, NoneHostname},
		{"localhost", "localhost"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := RootDomain(tc.host); got != tc.want {
			t.Errorf("RootDomain(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

// TestSaltKeySplit checks the salt reaches SipHash as its two little-endian key
// words. A byte-order mistake here would still produce stable-looking hashes,
// which is exactly why it needs asserting rather than eyeballing.
func TestSaltKeySplit(t *testing.T) {
	salt := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}

	k0, k1 := saltKey(salt)

	if k0 != 0x0807060504030201 {
		t.Errorf("k0 = %#x, want 0x0807060504030201", k0)
	}
	if k1 != 0x100f0e0d0c0b0a09 {
		t.Errorf("k1 = %#x, want 0x100f0e0d0c0b0a09", k1)
	}
}

// BenchmarkFingerprint keeps the hot path honest. The whole request has a
// sub-millisecond CPU budget and this runs once per event.
func BenchmarkFingerprint(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Fingerprint(testSalt, testUserAgent, "203.0.113.7", "example.com", "example.com")
	}
}
