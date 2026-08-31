//
// cache.go
// Holding a live report's answer for half a minute.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package statsapi

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

// CacheTTL is how long a report whose range reaches today is held.
//
// The dashboard refreshes itself and people leave it open on a second monitor,
// so the same eight queries arrive again and again with nothing between them
// but a few new events. Thirty seconds is short enough that a number on screen
// is never visibly stale and long enough that a forgotten tab costs one round
// of queries a minute instead of one every few seconds.
//
// Only ranges that include now are cached. A finished period cannot change, and
// it is already answered from the summary tables in single-digit milliseconds,
// so caching it would spend memory to save nothing.
const CacheTTL = 30 * time.Second

// CacheEntries bounds how many answers are held. Each one is a rendered
// response body, so the cap is a memory ceiling: a shard serving hundreds of
// accounts must not be able to fill its heap with dashboards nobody is looking
// at any more.
const CacheEntries = 512

// cache is a small time-to-live map of rendered responses.
//
// It is deliberately not an LRU. The entries expire on their own in half a
// minute, so the only thing the size cap has to prevent is unbounded growth
// between expiries — and dropping everything expired, then the whole map if it
// is still too big, is a page of code less than a linked list and behaves the
// same for this workload.
type cache struct {
	ttl time.Duration
	max int

	// Now is injectable because the entire behaviour of a cache is about what
	// happens as time passes, and a test that waited thirty seconds is a test
	// nobody runs.
	Now func() time.Time

	mu      sync.Mutex
	entries map[[32]byte]entry
}

// entry is one held response and when it stops being fresh.
type entry struct {
	body      []byte
	expiresAt time.Time
}

// newCache builds an empty cache.
func newCache(ttl time.Duration, max int) *cache {
	return &cache{ttl: ttl, max: max, entries: map[[32]byte]entry{}}
}

// now reads the cache's clock.
func (c *cache) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}

	return c.Now()
}

// key identifies one answer: the site it is about and the exact request that
// asked for it.
//
// The request body is hashed verbatim rather than after normalisation, so two
// spellings of the same query are two entries. That is the safe direction to be
// wrong in: a miss costs a query, where a collision would serve one site's
// numbers to another.
func cacheKey(domain string, body []byte) [32]byte {
	hash := sha256.New()

	// The domain is length-prefixed so that a domain ending in the first bytes
	// of a body cannot hash the same as a shorter domain and a longer body.
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(domain)))

	hash.Write(length[:])
	hash.Write([]byte(domain))
	hash.Write(body)

	var key [32]byte
	copy(key[:], hash.Sum(nil))

	return key
}

// get returns a held answer while it is still fresh. A nil cache is a handler
// that was built by hand rather than through New, and it simply never hits.
func (c *cache) get(key [32]byte) ([]byte, bool) {
	if c == nil {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	held, ok := c.entries[key]
	if !ok || !c.now().Before(held.expiresAt) {
		return nil, false
	}

	return held.body, true
}

// put holds an answer for the cache's lifetime.
func (c *cache) put(key [32]byte, body []byte) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.max {
		c.evict()
	}

	c.entries[key] = entry{body: body, expiresAt: c.now().Add(c.ttl)}
}

// evict drops what has expired, and everything if that was not enough. Emptying
// the map is acceptable because every entry is worth one query to rebuild and
// the cap is only ever reached by a burst nobody is watching the results of.
func (c *cache) evict() {
	now := c.now()

	for key, held := range c.entries {
		if !now.Before(held.expiresAt) {
			delete(c.entries, key)
		}
	}

	if len(c.entries) >= c.max {
		c.entries = map[[32]byte]entry{}
	}
}
