//
// ratelimit_test.go
// Tests for the ceiling on the one endpoint the whole internet can reach.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"net/netip"
	"testing"
	"time"
)

// TestRateLimiterIsPerAddressAndRefills covers both isolation and token refill.
func TestRateLimiterIsPerAddressAndRefills(t *testing.T) {
	now := fixtureStart
	limiter := NewRateLimiter(10, 2)
	limiter.SetClock(func() time.Time { return now })

	noisy := netip.MustParseAddr("203.0.113.1")
	quiet := netip.MustParseAddr("203.0.113.2")
	if !limiter.Allow(noisy) {
		t.Fatal("one address could not spend its first burst token")
	}
	if !limiter.Allow(noisy) {
		t.Fatal("one address could not spend its second burst token")
	}
	if limiter.Allow(noisy) {
		t.Fatal("one address exceeded its burst allowance")
	}
	if !limiter.Allow(quiet) {
		t.Fatal("a different address was throttled by its neighbour")
	}

	now = now.Add(time.Second)
	if !limiter.Allow(noisy) {
		t.Fatal("the allowance did not refill")
	}
}

// TestRateLimiterDoesNotPersistAnUnresolvedSharedBucket covers the proxy
// failure mode where every unresolved request would otherwise throttle as one.
func TestRateLimiterDoesNotPersistAnUnresolvedSharedBucket(t *testing.T) {
	limiter := NewRateLimiter(1, 1)
	for i := 0; i < 50; i++ {
		if !limiter.Allow(netip.Addr{}) {
			t.Fatal("unresolved traffic was throttled as one source")
		}
	}
	if limiter.Len() != 0 {
		t.Fatal("an unresolved address became retained limiter state")
	}
}
