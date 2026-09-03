//
// lists_test.go
// The embedded list has to be present, parseable and free of reserved space.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lists

import (
	"net/netip"
	"testing"
)

// committedRanges is roughly what a full rebuild produces. The floors below are
// set close under it rather than at some comfortable round number, because
// losing one whole cloud costs about a sixth of the list — a floor loose enough
// to survive that is a floor that never fires.
const committedRanges = 11838

// TestDatacentersIsPresent guards against the embedded file being emptied,
// truncated or replaced by a placeholder. A short list is the failure this
// package exists to fix, and it looks exactly like a week of clean traffic.
func TestDatacentersIsPresent(t *testing.T) {
	ranges := Datacenters()

	if floor := committedRanges * 9 / 10; len(ranges) < floor {
		t.Fatalf("the embedded list holds %d ranges, want at least %d — run `make lists`", len(ranges), floor)
	}
}

// TestEveryRangeParses checks that nothing in the file is unparseable. A bad
// line is skipped silently by the range set, so without this a typo would
// quietly shrink coverage with nothing to see.
func TestEveryRangeParses(t *testing.T) {
	for _, entry := range Datacenters() {
		if _, err := netip.ParsePrefix(entry); err != nil {
			t.Errorf("%q does not parse: %v", entry, err)
		}
	}
}

// TestNoReservedSpace checks the file carries no address a real visitor could
// never come from.
//
// The feeds genuinely contain these: one provider's published geofeed places
// the RFC 5737 documentation blocks in Georgia. Left in, they classify every
// test fixture written against the documentation ranges as automated traffic.
func TestNoReservedSpace(t *testing.T) {
	for _, entry := range Datacenters() {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			continue
		}

		if !routable(prefix) {
			t.Errorf("%s is reserved space and must not be in the list", prefix)
		}
	}
}

// TestBothFamiliesAreCovered checks the list did not lose one address family.
// A scraper reaching us over IPv6 is the same scraper, and a v4-only list would
// pass every other test here while missing it.
func TestBothFamiliesAreCovered(t *testing.T) {
	var v4, v6 int

	for _, entry := range Datacenters() {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			continue
		}

		if prefix.Addr().Is4() {
			v4++
		} else {
			v6++
		}
	}

	if v4 < 1000 {
		t.Errorf("only %d IPv4 ranges", v4)
	}

	if v6 < 500 {
		t.Errorf("only %d IPv6 ranges", v6)
	}
}

// TestKnownProvidersAreListed checks a handful of addresses that have belonged
// to the same provider for years.
//
// It is a coverage check rather than a correctness one: every other test here
// passes on a file containing one range, and the point of the list is breadth.
func TestKnownProvidersAreListed(t *testing.T) {
	set := newTestSet(t)

	for _, addr := range []string{
		"52.94.76.1",   // AWS us-east-1
		"34.64.32.1",   // Google Cloud
		"129.213.16.1", // Oracle Cloud
		"157.245.0.1",  // DigitalOcean
		"139.162.1.1",  // Linode
		"45.32.0.1",    // Vultr
		"47.88.0.1",    // Alibaba Cloud
		"5.9.0.1",      // Hetzner
		"51.68.0.1",    // OVH
	} {
		if !set.has(netip.MustParseAddr(addr)) {
			t.Errorf("%s is not in the list", addr)
		}
	}
}

// TestRealVisitorSpaceIsAbsent checks the list leaves alone the networks that
// carry people.
//
// Cloudflare, Fastly and Akamai are the ones that matter: WARP and iCloud
// Private Relay egress through them, so listing them would classify a large
// slice of ordinary browsing as automated and it would look like the filter
// working rather than the filter breaking.
func TestRealVisitorSpaceIsAbsent(t *testing.T) {
	set := newTestSet(t)

	for _, addr := range []string{
		"1.1.1.1",     // Cloudflare
		"104.16.0.1",  // Cloudflare
		"151.101.0.1", // Fastly
		"23.32.0.1",   // Akamai
		"8.8.8.8",     // Google public DNS, not Google Cloud
	} {
		if set.has(netip.MustParseAddr(addr)) {
			t.Errorf("%s is in the list, but that network carries real visitors", addr)
		}
	}
}

// testSet is a membership check over the embedded list, built once per test.
type testSet struct {
	prefixes []netip.Prefix
}

// newTestSet parses the whole embedded list. It is a linear scan rather than
// the production binary search, because a test that shares the lookup it is
// checking cannot catch a bug in that lookup.
func newTestSet(t *testing.T) testSet {
	t.Helper()

	entries := Datacenters()
	prefixes := make([]netip.Prefix, 0, len(entries))

	for _, entry := range entries {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			continue
		}

		prefixes = append(prefixes, prefix)
	}

	return testSet{prefixes: prefixes}
}

// has reports whether any range covers the address.
func (s testSet) has(addr netip.Addr) bool {
	for _, prefix := range s.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// TestCurrentBrowsersIsPresent guards the version floor the same way the
// address list is guarded. An empty map answers "current" to every version,
// which is the check quietly turning itself off.
func TestCurrentBrowsersIsPresent(t *testing.T) {
	current := CurrentBrowsers()

	for _, name := range []string{"Chrome", "Firefox", "Edge"} {
		major, ok := current[name]
		if !ok {
			t.Errorf("%s has no current version — run `make lists`", name)

			continue
		}

		// A browser on a four-week cadence passed 100 years ago; anything below
		// that is a parse that read the wrong field.
		if major < 100 {
			t.Errorf("%s is recorded as version %d, which is not a current release", name, major)
		}
	}
}

// TestSafariHasNoFloor checks the deliberate omission stays omitted.
//
// Safari's version follows the operating system rather than a release cadence,
// and Apple renumbered it to the year in 2025, so a supported phone can be
// several majors back and still be a person. Adding it here would turn every
// one of them into a bot.
func TestSafariHasNoFloor(t *testing.T) {
	for _, name := range []string{"Safari", "Samsung Internet", "Opera", "Internet Explorer"} {
		if major, ok := CurrentBrowsers()[name]; ok {
			t.Errorf("%s has a floor of %d, but its version cannot be judged on a cadence", name, major)
		}
	}
}
