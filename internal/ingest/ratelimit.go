//
// ratelimit.go
// A ceiling per source address on the one endpoint the whole internet can reach.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// The default ceiling per source address is intentionally generous. Real
// traffic arrives from many addresses, while the common abuse case does not.
const (
	DefaultEventRate  = 100
	DefaultEventBurst = 500
)

// The limiter's own bounds prevent its address-keyed map becoming a leak.
const (
	rateLimitSweep   = time.Minute
	rateLimitIdle    = 5 * time.Minute
	rateLimitEntries = 200_000
)

// RateLimiter is an in-memory token bucket per source address. It never writes
// an address to disk, preserving the ingest boundary that discards raw IPs.
type RateLimiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[netip.Addr]*bucket
}

// bucket is one address's current allowance.
type bucket struct {
	tokens float64
	seen   time.Time
}

// NewRateLimiter builds a limiter at a sustained rate per second with a burst
// allowance. A non-positive rate returns nil, which means no limit.
func NewRateLimiter(rate, burst int) *RateLimiter {
	if rate <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = rate
	}

	return &RateLimiter{
		rate:    float64(rate),
		burst:   float64(burst),
		now:     time.Now,
		buckets: map[netip.Addr]*bucket{},
	}
}

// SetClock replaces the limiter's clock so refills can be tested without
// sleeping.
func (l *RateLimiter) SetClock(now func() time.Time) {
	l.now = now
}

// Allow reports whether an address may send one more event. A nil limiter and
// an unresolvable address both allow, avoiding one shared bucket for bad proxy
// configuration.
func (l *RateLimiter) Allow(addr netip.Addr) bool {
	if l == nil || !addr.IsValid() {
		return true
	}

	addr = addr.Unmap()
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, known := l.buckets[addr]
	if !known {
		if len(l.buckets) >= rateLimitEntries {
			l.evict(now)
		}

		entry = &bucket{tokens: l.burst, seen: now}
		l.buckets[addr] = entry
	}

	entry.tokens += now.Sub(entry.seen).Seconds() * l.rate
	if entry.tokens > l.burst {
		entry.tokens = l.burst
	}
	entry.seen = now

	if entry.tokens < 1 {
		return false
	}

	entry.tokens--
	return true
}

// evict drops idle buckets, then clears the map if hostile churn still leaves
// it at the hard cap. It runs with the lock held.
func (l *RateLimiter) evict(now time.Time) {
	cutoff := now.Add(-rateLimitIdle)
	for addr, entry := range l.buckets {
		if entry.seen.Before(cutoff) {
			delete(l.buckets, addr)
		}
	}

	if len(l.buckets) >= rateLimitEntries {
		l.buckets = map[netip.Addr]*bucket{}
	}
}

// Run sweeps idle buckets until the context is cancelled.
func (l *RateLimiter) Run(ctx context.Context) {
	if l == nil {
		return
	}

	ticker := time.NewTicker(rateLimitSweep)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			l.evict(l.now())
			l.mu.Unlock()
		}
	}
}

// Len reports how many addresses the limiter is tracking.
func (l *RateLimiter) Len() int {
	if l == nil {
		return 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.buckets)
}
