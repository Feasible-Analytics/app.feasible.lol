//
// geo_test.go
// Tests for degrading to unknown rather than failing when there is no database.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package geo

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// TestNoDatabaseDegradesToUnknown is the rule an optional data file lives by. A
// grey country map is a far smaller problem than a process that will not start,
// and a self-hoster who has not downloaded the city database still has a working
// install.
func TestNoDatabaseDegradesToUnknown(t *testing.T) {
	locator, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("a missing database was reported as an error: %v", err)
	}
	defer locator.Close()

	got := locator.Lookup(netip.MustParseAddr("203.0.113.7"))
	if !got.IsZero() {
		t.Fatalf("Lookup returned %+v with no database, want nothing", got)
	}
}

// TestUnknownLocatorIsSafe checks the no-op implementation is a real type rather
// than something call sites have to nil-check. A nil Locator on the hot path is
// how an optional data file turns into a panic.
func TestUnknownLocatorIsSafe(t *testing.T) {
	var locator Locator = Unknown{}

	if got := locator.Lookup(netip.MustParseAddr("2001:db8::1")); !got.IsZero() {
		t.Fatalf("Unknown returned %+v", got)
	}
	if err := locator.Close(); err != nil {
		t.Fatal(err)
	}

	// An invalid address must not panic either, because a request with no
	// resolvable client address still has to be geolocated.
	if got := locator.Lookup(netip.Addr{}); !got.IsZero() {
		t.Fatalf("Unknown returned %+v for an invalid address", got)
	}
}

// TestCorruptDatabaseIsReported checks the distinction that matters
// operationally: a file nobody downloaded is normal, and a file that exists but
// cannot be read is a real problem somebody has to be told about.
func TestCorruptDatabaseIsReported(t *testing.T) {
	dir := t.TempDir()
	geoDir := filepath.Join(dir, DataDirName)

	if err := os.MkdirAll(geoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(geoDir, CountryFileName), []byte("not an mmdb"), 0o644); err != nil {
		t.Fatal(err)
	}

	locator, err := Open(dir)
	if err == nil {
		t.Fatal("a corrupt database was accepted")
	}

	// Even on failure the caller gets something usable, so a bad file cannot
	// leave the pipeline holding a nil Locator.
	if locator == nil {
		t.Fatal("Open returned a nil Locator alongside its error")
	}
	if got := locator.Lookup(netip.MustParseAddr("203.0.113.7")); !got.IsZero() {
		t.Fatal("the fallback Locator returned a location")
	}
}

// TestPrivateAddressesAreNotLookedUp checks the shortcut. A loopback or private
// address is in no geolocation database, and asking is a wasted binary search on
// every request from a developer's laptop or a misconfigured proxy.
func TestPrivateAddressesAreNotLookedUp(t *testing.T) {
	locator := NewMMDB(nil, nil)

	for _, value := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "::1"} {
		if got := locator.Lookup(netip.MustParseAddr(value)); !got.IsZero() {
			t.Errorf("%s returned %+v", value, got)
		}
	}
}

// TestQualifySubdivision checks the country prefix. The databases store "CA" for
// both California and Cataluña, so without it two unrelated regions collapse
// into one row on every report that groups by region.
func TestQualifySubdivision(t *testing.T) {
	if got := qualify("US", "CA"); got != "US-CA" {
		t.Errorf("qualify = %q, want US-CA", got)
	}
	if got := qualify("ES", "CA"); got != "ES-CA" {
		t.Errorf("qualify = %q, want ES-CA", got)
	}
	if got := qualify("", "CA"); got != "" {
		t.Errorf("qualify with no country = %q, want empty", got)
	}
	if got := qualify("US", ""); got != "" {
		t.Errorf("qualify with no subdivision = %q, want empty", got)
	}
}

// TestAnonymousVPNCountryIsNamed checks the bucket exists as a constant rather
// than a string written at each call site. Dropping VPN traffic throws away real
// people, and the incumbent lost months of genuine users that way.
func TestAnonymousVPNCountryIsNamed(t *testing.T) {
	if AnonymousVPNCountry == "" {
		t.Fatal("the VPN bucket has no name")
	}

	location := Location{Country: AnonymousVPNCountry}
	if location.IsZero() {
		t.Fatal("a VPN-bucketed location reports itself as empty")
	}
}
