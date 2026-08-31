//
// weights.go
// Power-law weights and the sampler that turns a random number into a value.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package seed generates realistic fake traffic. It exists because a dashboard
// cannot be built, and a performance claim cannot be measured, against an empty
// database — and because uniformly random data would be wrong twice over: too
// many distinct page paths and the dimension tables explode so everything looks
// slow, too few and the whole working set fits in cache so everything looks
// fast. Neither says anything about production.
//
// So every dimension here is sampled from a power law, and the events are
// derived by calling the same functions the ingest path calls. Only the network
// is skipped.
package seed

import (
	"math"
	"sort"
)

// chooser samples an index from a fixed weight distribution. It is built once
// per dimension and then sampled millions of times, so the cost that matters is
// the sample: a cumulative table and a binary search rather than a walk over
// the weights.
type chooser struct {
	// cumulative holds the running total normalised to 1, so a sample is a
	// search for a uniform value rather than a division per lookup.
	cumulative []float64
}

// newChooser builds a sampler over a set of weights. The weights need not sum
// to anything in particular; they are normalised here so a caller can write
// them as relative sizes and never think about it again.
func newChooser(weights []float64) *chooser {
	cumulative := make([]float64, len(weights))

	total := 0.0
	for i, weight := range weights {
		if weight < 0 {
			weight = 0
		}
		total += weight
		cumulative[i] = total
	}

	// A distribution with no weight at all would divide by zero below. It can
	// only come from an empty catalogue, so it degrades to "always the first
	// entry" rather than failing a seed run over a data file.
	if total <= 0 {
		for i := range cumulative {
			cumulative[i] = 1
		}
		return &chooser{cumulative: cumulative}
	}

	for i := range cumulative {
		cumulative[i] /= total
	}

	return &chooser{cumulative: cumulative}
}

// pick returns the index a uniform value in [0,1) falls in. Taking the random
// number as an argument rather than holding a generator is what keeps a run
// reproducible: every draw comes from one seeded stream in a known order.
func (c *chooser) pick(u float64) int {
	if len(c.cumulative) == 0 {
		return 0
	}

	index := sort.SearchFloat64s(c.cumulative, u)
	if index >= len(c.cumulative) {
		index = len(c.cumulative) - 1
	}

	return index
}

// len reports how many values the distribution covers.
func (c *chooser) len() int {
	return len(c.cumulative)
}

// share reports what fraction of traffic the first n values take. It is how the
// distribution targets in the specification — top 10 pages are half the
// traffic, top 5 sources are seventy per cent — are asserted rather than
// assumed.
func (c *chooser) share(n int) float64 {
	if len(c.cumulative) == 0 || n <= 0 {
		return 0
	}
	if n > len(c.cumulative) {
		n = len(c.cumulative)
	}

	return c.cumulative[n-1]
}

// zipf builds the weights of a Zipf distribution: the rank-r value gets
// 1/r^exponent. Real traffic is Zipf-shaped in every dimension we report on,
// and the exponent is the one knob that sets how concentrated the head is —
// which is the whole point of generating data this way rather than uniformly.
func zipf(n int, exponent float64) []float64 {
	weights := make([]float64, n)

	for i := range weights {
		weights[i] = 1 / math.Pow(float64(i+1), exponent)
	}

	return weights
}

// zipfTail builds a Zipf distribution over 1..n whose first entry is pinned to
// a fixed share. It is what generates session lengths: roughly sixty per cent
// of visits are a single pageview, and the rest fall away as a power law with a
// long tail out past twenty.
func zipfTail(n int, headShare, exponent float64) []float64 {
	weights := zipf(n, exponent)

	// The head is set first and the tail is scaled to whatever is left, so the
	// two numbers a reader cares about — "sixty per cent bounce out" and "the
	// tail reaches thirty" — are both stated directly.
	tail := 0.0
	for _, weight := range weights[1:] {
		tail += weight
	}

	if tail <= 0 {
		return weights
	}

	scale := (1 - headShare) / tail
	weights[0] = headShare

	for i := 1; i < len(weights); i++ {
		weights[i] *= scale
	}

	return weights
}
