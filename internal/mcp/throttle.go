//
// throttle.go
// A per-address token bucket for the endpoints that take no credential.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"sync"
	"time"
)

// The OAuth endpoints answer before anybody has proved anything, so the
// per-key limiter cannot protect them: registration is open by design, the
// consent form tries a key against the database on every submit, and the token
// endpoint is where a stolen code or refresh token would be tried. The only
// thing to key a limit on is the client address.
//
// A real client registers once, authorises once and refreshes hourly, so the
// ceiling is far above legitimate use even for an office behind one address.
const (
	oauthBurst     = 30
	oauthPerMinute = 30
)

// throttleIdle is how long a bucket has to sit full before it is dropped. It
// is the refill time of a whole burst, so dropping one loses nothing: a fresh
// bucket is full anyway.
const throttleIdle = time.Duration(float64(time.Minute) * oauthBurst / oauthPerMinute)

// throttleSweepAt is how many addresses the map may hold before a call pays
// for a sweep. It bounds memory without a goroutine of its own.
const throttleSweepAt = 4096

// throttle counts calls per client address. It is a token bucket rather than
// the fixed window the API-key limiter uses because the failure it guards
// against is a burst from one address rather than sustained use, and a bucket
// answers that with a small memory cost per address.
type throttle struct {
	// Burst is how many calls an idle address may make at once, and PerMinute
	// how fast that allowance comes back.
	Burst     float64
	PerMinute float64

	// Now is the clock, injectable so a test can refill a bucket without
	// waiting for it.
	Now func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// tokenBucket is one address's allowance and when it was last topped up.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// now reads the clock.
func (t *throttle) now() time.Time {
	if t.Now == nil {
		return time.Now().UTC()
	}

	return t.Now()
}

// Allow spends one call for an address and reports whether it fits. It also
// says how long the caller should wait when it does not, so the refusal can
// carry a Retry-After rather than leaving the client to guess.
func (t *throttle) Allow(address string) (bool, time.Duration) {
	burst, perMinute := t.Burst, t.PerMinute
	if burst <= 0 {
		burst = oauthBurst
	}
	if perMinute <= 0 {
		perMinute = oauthPerMinute
	}

	perSecond := perMinute / 60
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.buckets == nil {
		t.buckets = map[string]*tokenBucket{}
	}

	if len(t.buckets) >= throttleSweepAt {
		t.sweep(now)
	}

	bucket, ok := t.buckets[address]
	if !ok {
		bucket = &tokenBucket{tokens: burst, last: now}
		t.buckets[address] = bucket
	}

	bucket.tokens = min(burst, bucket.tokens+now.Sub(bucket.last).Seconds()*perSecond)
	bucket.last = now

	if bucket.tokens < 1 {
		wait := time.Duration((1 - bucket.tokens) / perSecond * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}

		return false, wait
	}

	bucket.tokens--

	return true, 0
}

// sweep drops every bucket that has been idle long enough to be full again.
// It runs under the lock its caller holds.
func (t *throttle) sweep(now time.Time) {
	for address, bucket := range t.buckets {
		if now.Sub(bucket.last) >= throttleIdle {
			delete(t.buckets, address)
		}
	}
}
