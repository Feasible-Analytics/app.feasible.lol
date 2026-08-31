//
// bench.go
// The measured ceiling: how fast we can write, and how fast we can read it back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package bench measures the two numbers every capacity claim in this system
// rests on: how many events a second one process can write, and how long a
// report takes to read back. Both were estimates until something ran them, and
// an estimate is a poor thing to design a storage layer around.
//
// It is a package rather than a pile of benchmark functions so that the load
// generator and the query set are ordinary code: the same shapes can be run
// from a benchmark, from a test that guards a budget, or one day from a command
// on the hardware we actually deploy to. RESULTS.md beside this file records
// what the numbers were when they were last taken, so a change that halves them
// is visible rather than merely felt.
package bench

import (
	"math"
	"sort"
	"time"
)

// Latencies is a distribution, reported the way an operator reads one. The
// median says what a normal moment feels like and the 99th says what the worst
// moment feels like; a mean hides both.
type Latencies struct {
	Count int
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	Max   time.Duration
}

// summarise turns raw samples into the distribution. It sorts a copy, because a
// caller collecting samples in arrival order usually still wants them that way.
func summarise(samples []time.Duration) Latencies {
	if len(samples) == 0 {
		return Latencies{}
	}

	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	return Latencies{
		Count: len(sorted),
		P50:   percentile(sorted, 0.50),
		P95:   percentile(sorted, 0.95),
		P99:   percentile(sorted, 0.99),
		Max:   sorted[len(sorted)-1],
	}
}

// percentile reads one point out of an already-sorted sample. It uses the
// nearest-rank method rather than interpolating: a p99 that is a value which
// actually happened is easier to reason about than one that never did.
func percentile(sorted []time.Duration, fraction float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	rank := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}

	return sorted[rank]
}
