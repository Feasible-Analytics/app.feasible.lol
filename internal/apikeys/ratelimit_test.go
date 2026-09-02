//
// ratelimit_test.go
// The hourly ceiling: the number it ships with, and the memory it bounds.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package apikeys

import (
	"testing"
	"time"
)

// TestDefaultLimitIsTheShippedOne pins the number the whole feature is about.
// The incumbent ships six hundred an hour, hard-coded even for self-hosters.
func TestDefaultLimitIsTheShippedOne(t *testing.T) {
	if DefaultHourlyLimit != 10000 {
		t.Fatalf("DefaultHourlyLimit = %d, want 10000", DefaultHourlyLimit)
	}

	// A misconfigured limit must not lock every customer out of the API, so a
	// zero or negative default falls back rather than refusing everything.
	for _, configured := range []int{0, -1} {
		limiter := NewLimiter(configured)

		if decision := limiter.Allow(&Key{ID: 1}); !decision.Allowed || decision.Limit != DefaultHourlyLimit {
			t.Fatalf("a limiter configured with %d refused a request", configured)
		}
	}
}

// TestSweepDropsExpiredBuckets checks the bound on the limiter's memory. Without
// it, a process that has served a million one-off keys holds a million counters
// forever.
func TestSweepDropsExpiredBuckets(t *testing.T) {
	clock := now

	limiter := NewLimiter(10)
	limiter.Now = func() time.Time { return clock }

	for id := int64(1); id <= 50; id++ {
		limiter.Allow(&Key{ID: id})
	}

	if len(limiter.buckets) != 50 {
		t.Fatalf("holding %d buckets, want 50", len(limiter.buckets))
	}

	clock = clock.Add(2 * time.Hour)
	limiter.Sweep()

	if len(limiter.buckets) != 0 {
		t.Fatalf("holding %d buckets after the window passed, want none", len(limiter.buckets))
	}
}

// TestAllowNChargesTheWholeCost checks that an expensive call spends its whole
// cost against the window and is refused whole rather than partly. A cost that
// could be paid in instalments would let a caller at the edge of the limit run
// the first half of an investigation and be cut off inside it.
func TestAllowNChargesTheWholeCost(t *testing.T) {
	limiter := NewLimiter(10)
	key := &Key{ID: 1}

	if decision := limiter.AllowN(key, 7); !decision.Allowed || decision.Remaining != 3 {
		t.Fatalf("first charge = %+v, want allowed with 3 remaining", decision)
	}

	if decision := limiter.AllowN(key, 4); decision.Allowed {
		t.Fatalf("a cost of 4 against 3 remaining was allowed: %+v", decision)
	}

	if decision := limiter.AllowN(key, 3); !decision.Allowed || decision.Remaining != 0 {
		t.Fatalf("a cost that exactly fits was refused: %+v", decision)
	}

	if decision := limiter.AllowN(key, 0); decision.Allowed {
		t.Fatal("a zero cost was free against an exhausted window")
	}
}
