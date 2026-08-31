//
// ratelimit.go
// The hourly request ceiling, counted in memory rather than in the database.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package apikeys

import (
	"sync"
	"time"
)

// DefaultHourlyLimit is what a key with no limit of its own gets.
//
// Ten thousand an hour is roughly three requests a second sustained, which is
// past the point where any dashboard, export or scheduled report is the problem.
// The incumbent ships six hundred, hard-coded even in the build people run on
// their own hardware, and the workaround people actually use is an UPDATE
// against their database — a number nobody can change is not a limit, it is a
// bug with a support queue attached.
const DefaultHourlyLimit = 10000

// window is the period the count is measured over. A fixed window rather than a
// sliding one is the honest trade here: it costs one integer and one timestamp
// per key, where a sliding log costs a slice per key that grows with the limit,
// and the failure mode — twice the limit across a window boundary — is a burst
// this system can absorb.
const window = time.Hour

// Limiter counts requests per key. It is in memory rather than in control.db
// because the alternative is a write on the busiest read path in the product,
// taking the one lock the whole deployment shares.
//
// The consequence is that the limit is per process. With more than one app
// process a customer gets the limit times the number of processes, which is a
// deliberate trade: this is a fairness and runaway-script guard, not a billing
// meter, and a shared counter would mean a network round trip in front of every
// query to enforce a number nobody is trying to cheat.
type Limiter struct {
	// Default is the ceiling for a key that does not name its own.
	Default int

	// Now is the clock, injectable so a test can cross a window boundary
	// without waiting an hour.
	Now func() time.Time

	mu      sync.Mutex
	buckets map[int64]*bucket
}

// bucket is one key's count and the window it belongs to.
type bucket struct {
	count      int
	windowEnds time.Time
}

// Decision is the outcome of one rate-limit check. It carries the numbers the
// response headers report, because a client that is being throttled needs to
// know the ceiling and when it resets — being told only "no" leaves retrying
// blindly as the only option, which makes the problem worse.
type Decision struct {
	Allowed   bool
	Limit     int
	Remaining int
	ResetsAt  time.Time
}

// RetryAfter is how long a refused caller should wait, rounded up to the next
// whole second so a client that retries exactly on it is inside the new window.
func (d Decision) RetryAfter() time.Duration {
	remaining := time.Until(d.ResetsAt)
	if remaining < time.Second {
		return time.Second
	}

	return remaining.Round(time.Second)
}

// NewLimiter builds a limiter with a default ceiling. A zero or negative
// default falls back to the shipped one rather than to "no requests allowed",
// because a misconfigured limit must not lock every customer out of the API.
func NewLimiter(defaultLimit int) *Limiter {
	if defaultLimit <= 0 {
		defaultLimit = DefaultHourlyLimit
	}

	return &Limiter{
		Default: defaultLimit,
		Now:     func() time.Time { return time.Now().UTC() },
		buckets: map[int64]*bucket{},
	}
}

// now reads the limiter's clock.
func (l *Limiter) now() time.Time {
	if l.Now == nil {
		return time.Now().UTC()
	}

	return l.Now()
}

// Allow counts one request against a key and reports whether it may proceed.
func (l *Limiter) Allow(key *Key) Decision {
	limit := l.Default
	if key != nil && key.HourlyLimit > 0 {
		limit = key.HourlyLimit
	}

	var id int64
	if key != nil {
		id = key.ID
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.buckets == nil {
		l.buckets = map[int64]*bucket{}
	}

	current, ok := l.buckets[id]
	if !ok || !now.Before(current.windowEnds) {
		current = &bucket{windowEnds: now.Add(window)}
		l.buckets[id] = current
	}

	if current.count >= limit {
		return Decision{Allowed: false, Limit: limit, Remaining: 0, ResetsAt: current.windowEnds}
	}

	current.count++

	return Decision{
		Allowed:   true,
		Limit:     limit,
		Remaining: limit - current.count,
		ResetsAt:  current.windowEnds,
	}
}

// Sweep drops the buckets whose window has passed. Without it a process that
// has served a million one-off keys holds a million counters forever; with it
// the map is bounded by the keys actually in use this hour.
func (l *Limiter) Sweep() {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	for id, current := range l.buckets {
		if !now.Before(current.windowEnds) {
			delete(l.buckets, id)
		}
	}
}
