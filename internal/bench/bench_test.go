//
// bench_test.go
// Tests for the distribution summary the benchmarks report.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package bench

import (
	"testing"
	"time"
)

// TestSummariseReadsTheDistribution checks the percentiles are values that
// actually happened, and that the samples are not reordered underneath the
// caller.
func TestSummariseReadsTheDistribution(t *testing.T) {
	samples := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}

	got := summarise(samples)

	if got.Count != 100 {
		t.Errorf("count = %d, want 100", got.Count)
	}
	if got.P50 != 50*time.Millisecond {
		t.Errorf("p50 = %v, want 50ms", got.P50)
	}
	if got.P99 != 99*time.Millisecond {
		t.Errorf("p99 = %v, want 99ms", got.P99)
	}
	if got.Max != 100*time.Millisecond {
		t.Errorf("max = %v, want 100ms", got.Max)
	}

	if samples[0] != 100*time.Millisecond {
		t.Error("summarise sorted the caller's slice")
	}
}

// TestSummariseHandlesAnEmptyRun checks a run that recorded nothing reports
// zeroes rather than panicking. A benchmark that crashed while reporting would
// hide whatever it was measuring.
func TestSummariseHandlesAnEmptyRun(t *testing.T) {
	if got := summarise(nil); got != (Latencies{}) {
		t.Fatalf("got %+v, want the zero value", got)
	}
}
