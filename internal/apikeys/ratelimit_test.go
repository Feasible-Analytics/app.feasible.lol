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
