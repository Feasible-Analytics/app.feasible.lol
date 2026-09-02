//
// ratelimit.go
// The attempt limiter in front of login, reset and two-factor verification.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
)

// Limits for each protected action. They are separate because the actions have
// very different shapes: someone genuinely mistypes a password a few times in a
// row, whereas five password reset emails in ten minutes is never a person
// trying to get in, it is a person being mailbombed.
const (
	// LoginAttempts and LoginWindow bound password guessing. Ten in fifteen
	// minutes is above what a real person needs and far below what an attacker
	// needs.
	LoginAttempts = 10
	LoginWindow   = 15 * time.Minute

	// ResetAttempts and ResetWindowLimit bound reset emails, per address and
	// per source. The cost of getting this wrong is not a break-in, it is
	// somebody's inbox being used as a weapon against them.
	ResetAttempts    = 5
	ResetWindowLimit = 30 * time.Minute

	// TwoFactorAttempts and TwoFactorWindow bound TOTP guessing. A TOTP code is
	// six digits and lives for thirty seconds, so an unlimited endpoint is a
	// one-in-a-million guess repeated as fast as the network allows. Five
	// attempts a minute makes that arithmetic hopeless.
	TwoFactorAttempts = 5
	TwoFactorWindow   = time.Minute

	// VerifyInstallAttempts and VerifyInstallWindow bound the installation
	// check per site. Each check connects to wherever the domain points, and
	// a loop of them from a signed-in account is a port scanner carrying our
	// address. Ten in ten minutes is plenty for somebody redeploying.
	VerifyInstallAttempts = 10
	VerifyInstallWindow   = 10 * time.Minute
)

// sweepInterval is how often dead buckets are discarded. Without it the map is
// an unbounded record of every address anybody has ever tried to sign in as,
// which is both a memory leak and a list worth stealing.
const sweepInterval = 10 * time.Minute

// Limiter is a fixed-window attempt counter, keyed by a caller-chosen string.
//
// It is in-process and deliberately so. The alternative is a table in
// system.db, which puts a write on the shared writer connection for every
// failed password in the system — and the thing being defended is an online
// guessing attack, which is per-process by definition because every process
// sees the requests it is serving. A restart resets the counters; an attacker
// who can restart the server has already won.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	now       func() time.Time
	lastSweep time.Time
}

// bucket is one key's window: when it opened and how many attempts landed in
// it.
type bucket struct {
	count    int
	openedAt time.Time
}

// NewLimiter builds an empty limiter.
func NewLimiter() *Limiter {
	return &Limiter{buckets: map[string]*bucket{}, now: time.Now}
}

// SetClock replaces the clock, so a test can walk past a window without
// sleeping through it.
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.now = now
}

// Allow records an attempt and reports whether it is within the limit. It
// counts the attempt even when it refuses, so that hammering a locked key keeps
// it locked rather than letting the window roll forward under load.
func (l *Limiter) Allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.openedAt) >= window {
		l.buckets[key] = &bucket{count: 1, openedAt: now}
		return true
	}

	b.count++

	return b.count <= limit
}

// Reset clears a key. A successful sign-in calls it so that somebody who
// mistyped their password four times is not left one attempt from a lockout for
// the next quarter of an hour.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.buckets, key)
}

// sweep drops buckets nothing could still be inside. It runs opportunistically
// under the lock the caller already holds rather than on a goroutine, because a
// background sweeper for a map that is only touched during sign-in attempts is
// a goroutine that spends its life asleep.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}

	l.lastSweep = now

	// The longest window any caller uses, so a bucket is only dropped once no
	// limit could still be counting it.
	const longest = 30 * time.Minute

	for key, b := range l.buckets {
		if now.Sub(b.openedAt) > longest {
			delete(l.buckets, key)
		}
	}
}

// ClientKey builds a rate-limit key from the request source and a label.
//
// The source is resolved under the same trusted-proxy rules as everything else
// in this binary. Behind our own reverse proxy every connection arrives from
// the proxy's address, so keying on the socket alone puts every visitor in one
// bucket and ten bad passwords from anybody lock sign-in for everybody. A
// forwarded header is honoured only from a peer on the trusted list, because a
// header anyone can set is a limit anyone can escape by writing a different
// number in it.
func ClientKey(r *http.Request, trusted *clientip.TrustedProxies, label string) string {
	return label + "|" + clientip.Key(r, trusted)
}

// SubjectKey builds a rate-limit key from what is being attacked rather than
// where from. Limiting by address alone lets a botnet spread one account's
// guesses across a thousand sources; limiting by subject alone lets one source
// walk through every account. Both keys are checked, which is why there are
// two functions.
func SubjectKey(subject, label string) string {
	return label + "|" + strings.ToLower(strings.TrimSpace(subject))
}
