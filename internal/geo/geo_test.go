//
// geo_test.go
// Tests for the Location value and the no-op Locator every call site can hold.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package geo

import (
	"net/netip"
	"testing"
)

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

// TestIsZeroCoversEveryField is what makes "geolocation returned nothing" a
// health warning rather than a silent grey map. A field left out of IsZero
// would report a located visitor as unlocated, or the reverse.
func TestIsZeroCoversEveryField(t *testing.T) {
	for _, location := range []Location{
		{Country: "GB"},
		{Subdivision1: "England"},
		{Subdivision2: "Greater London"},
		{City: "London"},
	} {
		if location.IsZero() {
			t.Errorf("%+v reports itself as empty", location)
		}
	}

	if !(Location{}).IsZero() {
		t.Error("an empty Location does not report itself as empty")
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
