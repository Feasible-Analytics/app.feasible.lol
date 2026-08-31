//
// geo.go
// Turning a client IP into a country, region and city, behind a swappable interface.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package geo resolves an IP address to a place. It exists as its own package
// with an interface in front of it because the data source is a licensing
// decision as much as a technical one, and that decision has already changed
// once: we ship DB-IP Lite (CC-BY-4.0), whose only obligation is an attribution
// link, rather than the free MaxMind data, which forbids third-party disclosure
// and costs more to license than the paid database it is a subset of.
//
// Two rules govern everything here. A missing database degrades to "unknown"
// and never fails — an optional data file must not stop the app booting. And a
// lookup never makes a network call: a round trip per pageview would put a
// third-party dependency in the one path that must never lose an event.
package geo

import "net/netip"

// AnonymousVPNCountry is the bucket commercial VPN exits land in. Their traffic
// arrives from datacentre ranges, so geolocating it gives the datacentre's
// country rather than the visitor's, and dropping it throws away real people —
// the incumbent lost months of genuine Mullvad and Proton users that way.
// Naming the bucket keeps the visitor counted and tells the truth about what we
// know.
const AnonymousVPNCountry = "Anonymous VPN Service"

// Location is everything we keep about where a visitor is. It is deliberately
// coarse: a country and two levels of subdivision are what a dashboard shows,
// and anything finer would be a privacy liability with no reporting value.
type Location struct {
	// Country is the ISO-3166-1 alpha-2 code, or AnonymousVPNCountry, or the
	// empty string when we do not know.
	Country string

	// Subdivision1 is the first-level region. It is an ISO-3166-2 code
	// ("US-CA") when the database carries one and the English region name
	// ("England") when it does not, because DB-IP Lite names its subdivisions
	// and never codes them. Empty when the database is country-level or has no
	// answer.
	Subdivision1 string

	// Subdivision2 is the second-level region, which most countries do not have
	// at all. It follows the same code-then-name rule as Subdivision1.
	Subdivision2 string

	// City is the English city name. A name rather than a GeoNames id because
	// DB-IP Lite ships no ids at all, and because the name is what a dashboard
	// renders — storing the id would mean shipping a GeoNames lookup table
	// purely to turn it back into this string.
	City string
}

// IsZero reports whether nothing at all was resolved. Callers use it to decide
// whether an event is worth a "geolocation returned nothing" health warning,
// which is how a missing database becomes visible instead of silently turning
// every map grey.
func (l Location) IsZero() bool {
	return l.Country == "" && l.Subdivision1 == "" && l.Subdivision2 == "" && l.City == ""
}

// Locator answers "where is this address". It is an interface so that swapping
// DB-IP Lite for a paid GeoIP2 City database later is a file swap and a
// constructor change rather than a rewrite of the ingest path.
type Locator interface {
	// Lookup never returns an error. A geolocation failure must not fail an
	// event, so an unknown answer and a broken database are the same thing to
	// the caller: a zero Location.
	Lookup(addr netip.Addr) Location

	// Close releases whatever the implementation holds open, which for the mmdb
	// reader is a memory mapping.
	Close() error
}

// Unknown is the Locator used when no database is available. It is a real type
// rather than a nil check at every call site, because a nil Locator is how an
// optional data file turns into a panic on the hot path.
type Unknown struct{}

// Lookup always returns nothing, which is the honest answer when there is no
// database to ask.
func (Unknown) Lookup(netip.Addr) Location { return Location{} }

// Close does nothing, so shutdown can treat every Locator the same way.
func (Unknown) Close() error { return nil }
