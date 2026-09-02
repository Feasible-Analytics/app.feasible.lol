//
// throttle_test.go
// The per-address bucket: a burst, a refill, and a bounded map.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"strconv"
	"testing"
	"time"
)

// TestThrottleRefusesAfterTheBurstAndRefills checks the bucket's arithmetic
// without an HTTP server: the burst is spent, the next call is refused with a
// wait, and time brings the allowance back.
func TestThrottleRefusesAfterTheBurstAndRefills(t *testing.T) {
	clock := testNow
	limiter := &throttle{Burst: 3, PerMinute: 60, Now: func() time.Time { return clock }}

	for i := 0; i < 3; i++ {
		if ok, _ := limiter.Allow("203.0.113.5"); !ok {
			t.Fatalf("call %d of the burst was refused", i+1)
		}
	}

	ok, wait := limiter.Allow("203.0.113.5")
	if ok {
		t.Fatal("the call after the burst was allowed")
	}
	if wait < time.Second {
		t.Fatalf("wait = %s, want at least a second", wait)
	}

	// Another address has its own allowance.
	if ok, _ := limiter.Allow("198.51.100.7"); !ok {
		t.Fatal("a second address shared the first one's bucket")
	}

	clock = clock.Add(2 * time.Second)

	if ok, _ := limiter.Allow("203.0.113.5"); !ok {
		t.Fatal("two seconds at one token a second did not refill a single call")
	}
}

// TestThrottleDropsIdleBuckets checks the memory bound. A map that only ever
// grows would hold an entry for every address that ever touched the endpoint.
func TestThrottleDropsIdleBuckets(t *testing.T) {
	clock := testNow
	limiter := &throttle{Now: func() time.Time { return clock }}

	for i := 0; i < throttleSweepAt; i++ {
		limiter.Allow("10.0.0." + strconv.Itoa(i))
	}

	clock = clock.Add(throttleIdle)
	limiter.Allow("203.0.113.5")

	if held := len(limiter.buckets); held != 1 {
		t.Fatalf("holding %d buckets after every one went idle, want 1", held)
	}
}
