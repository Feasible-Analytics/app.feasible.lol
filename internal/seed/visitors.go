//
// visitors.go
// Synthetic people: one address, one browser, one place, stable across visits.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"fmt"
	"math"
	"net/netip"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/geo"
)

// The synthetic address space. Every seeded visitor gets an address in
// 100.64.0.0/10, the range reserved for carrier-grade NAT: it is routable
// nowhere, it cannot be confused with a real visitor's address if one of these
// ever reaches a log, and it is large enough that four million of them exist.
//
// The address is a fingerprint input and a geolocation input and then it is
// discarded, exactly as it is in production. Nothing here reaches disk.
const (
	cgnatFirstOctet = 100
	cgnatBaseSecond = 64
	cgnatSpread     = 63 // 100.64.x.x through 100.126.x.x

	// vpnSecondOctet is the slice of the range treated as commercial VPN exits.
	// It is registered with the bot filter as a datacentre range, so traffic
	// from it is classified and bucketed as "Anonymous VPN Service" by the same
	// code that does it in production — which is the only way to know that
	// bucket renders at all.
	vpnSecondOctet = 127
	vpnRange       = "100.127.0.0/16"

	// vpnShare is how much of the traffic arrives through one. It is small and
	// it is not zero: the incumbent lost months of real Mullvad and Proton
	// users by dropping this traffic instead of naming it.
	vpnShare = 0.02
)

// visitorSkew turns a uniform draw into a visitor index. Above one it
// concentrates on the low indices, which is what makes some people return
// often and most come once — a uniform draw would give every visitor the same
// number of visits and the returning-visitor report nothing to show.
//
// It is deliberately mild. Pushed harder, the busiest visitor picks up dozens
// of visits a day, two of them land within the session timeout of each other,
// and the fold correctly merges them into one visit of several hundred
// pageviews — which is not a bug in the fold and is not a visit any real site
// has.
const visitorSkew = 1.35

// visitor is one synthetic person. Every field is a pure function of the
// visitor index, which is what makes a returning visitor return as the same
// person: change the browser between visits and the fingerprint changes, and
// the visit becomes somebody else's.
type visitor struct {
	IP       string
	UA       string
	Width    int
	Language string
}

// visitorFor builds the person at an index. It takes the agent and language
// samplers rather than drawing from the run's random stream, because a
// visitor's browser has to be the same on Tuesday as it was on Monday and a
// stream position is not.
func visitorFor(index uint32, agents, langs *chooser) visitor {
	seed := mix(uint64(index))

	vpn := uniform(mix(seed^0x5bf0)) < vpnShare

	entry := agentCatalog[agents.pick(uniform(mix(seed^0x1d3f)))]

	// The build number varies within a browser. It does not change the stored
	// browser version, which is trimmed to the major, but it does change the
	// fingerprint — which is how one office or one household produces several
	// visitors from one address, exactly as it does in reality.
	build := int(mix(seed^0x77a1) % 64)

	return visitor{
		IP:       visitorIP(index, vpn),
		UA:       fmt.Sprintf("%s Build/%d", entry.UA, build),
		Width:    viewport(entry.Width, uniform(mix(seed^0x2c9e))),
		Language: languages[langs.pick(uniform(mix(seed^0x40b7)))],
	}
}

// visitorIP lays a visitor index out across the synthetic range. The VPN slice
// is carved out of the same block so that one constant registered with the bot
// filter covers it, rather than a second range somebody has to keep in step.
func visitorIP(index uint32, vpn bool) string {
	if vpn {
		return fmt.Sprintf("%d.%d.%d.%d", cgnatFirstOctet, vpnSecondOctet, (index>>8)&0xFF, index&0xFF)
	}

	second := cgnatBaseSecond + int((index>>16)%cgnatSpread)

	return fmt.Sprintf("%d.%d.%d.%d", cgnatFirstOctet, second, (index>>8)&0xFF, index&0xFF)
}

// viewport jitters a device's nominal width. A laptop with a half-width window
// reports as a tablet, and a seed where every device sat exactly on its nominal
// width would make the screen-size buckets a copy of the device-type report.
func viewport(nominal int, u float64) int {
	switch {
	case u < 0.72:
		return nominal
	case u < 0.90:
		return nominal * 4 / 5
	case u < 0.97:
		return nominal * 2 / 3
	default:
		return nominal * 11 / 10
	}
}

// pickVisitor turns a uniform draw into a visitor index within a pool. The pool
// is sized from the traffic a site gets, so a busy site has more distinct
// people rather than the same few visiting more often.
func pickVisitor(u float64, pool int) uint32 {
	if pool < 1 {
		pool = 1
	}

	index := int(float64(pool) * math.Pow(u, visitorSkew))
	if index >= pool {
		index = pool - 1
	}

	return uint32(index)
}

// locator answers "where is this address" from the country distribution instead
// of from the geolocation database. The lookup is not what a seeded dataset
// exists to test, the mmdb file is optional, and skipping it is measurably
// faster on a run that does it a million times.
//
// It is a pure function of the address, so a returning visitor is always in the
// same city — which is what makes the location reports agree with the visitor
// reports.
type locator struct {
	places  []place
	chooser *chooser
}

// newLocator builds the geolocation stand-in over the place catalogue.
func newLocator() *locator {
	places, weights := placeCatalog()

	return &locator{places: places, chooser: newChooser(weights)}
}

// Lookup returns the place an address belongs to. Hashing the address rather
// than encoding a place into it is what keeps the address space free for
// visitor identity: any address at all resolves, and the distribution over many
// addresses is the country distribution.
func (l *locator) Lookup(addr netip.Addr) geo.Location {
	if !addr.IsValid() {
		return geo.Location{}
	}

	raw := addr.As16()
	var packed uint64
	for _, b := range raw[8:] {
		packed = packed<<8 | uint64(b)
	}

	found := l.places[l.chooser.pick(uniform(mix(packed)))]

	return geo.Location{Country: found.Country, Subdivision1: found.Region, City: found.City}
}

// Close satisfies the Locator interface. There is nothing to release: that is
// the point of not opening a database.
func (l *locator) Close() error { return nil }

// mix is a 64-bit integer hash. It exists so that everything derived from a
// visitor index — the address, the browser, the language, the place — is
// independent of everything else derived from it, without four separate random
// streams that would have to stay in step across runs.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb

	return x ^ (x >> 31)
}

// uniform turns a hash into a value in [0,1). The top 53 bits are used because
// that is the mantissa of a float64, so every representable value is reachable
// and none is twice as likely as its neighbour.
func uniform(x uint64) float64 {
	return float64(x>>11) / float64(uint64(1)<<53)
}
