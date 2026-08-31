//
// cache.go
// An LRU in front of the parser, because the same few user agents repeat forever.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package useragent

import (
	"container/list"
	"sync"
	"time"
)

// DefaultCapacity is how many distinct user agents the cache holds. A hundred
// thousand entries covers the real distribution several times over — a busy site
// sees a few thousand distinct headers a day — while capping the memory a
// header-randomising bot can make us spend.
const DefaultCapacity = 100_000

// DefaultTTL expires an entry an hour after it was parsed. Nothing about a
// parse result goes stale, so the TTL is not about correctness: it is what
// stops a long-running process holding a hundred thousand strings that stopped
// being asked for weeks ago.
const DefaultTTL = 60 * time.Minute

// entry is one cached parse, with enough to expire it and to find its place in
// the recency list without a scan.
type entry struct {
	ua       string
	result   Result
	parsedAt time.Time
	element  *list.Element
}

// Cache is a bounded, expiring cache in front of Parse. Parsing is the single
// most expensive thing the ingest path does per event, and it is also the most
// repetitive — the incumbent measured this exact cache as their biggest ingest
// win, which is reason enough to have it from the first version rather than
// after the first slow day.
type Cache struct {
	capacity int
	ttl      time.Duration

	// now is injectable so the expiry test does not have to sleep for an hour.
	now func() time.Time

	// mu guards everything below. A plain mutex rather than an RWMutex on
	// purpose: every read moves an element to the front of the recency list, so
	// a "read" is a write and a shared lock would buy nothing.
	mu      sync.Mutex
	entries map[string]*entry
	recency *list.List

	hits   uint64
	misses uint64
}

// NewCache builds a cache with the given bounds. Zero or negative values fall
// back to the defaults so a caller can ask for "the normal one" by passing
// nothing meaningful.
func NewCache(capacity int, ttl time.Duration) *Cache {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	return &Cache{
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]*entry),
		recency:  list.New(),
	}
}

// Parse returns the parsed user agent, parsing it only if this is the first
// time or the entry has expired. The signature matches the package function so
// a call site can be handed either one.
func (c *Cache) Parse(ua string) Result {
	// An empty header parses to nothing and would otherwise take a cache slot
	// that every stripped-user-agent request would then contend on.
	if ua == "" {
		return Result{}
	}

	now := c.now()

	c.mu.Lock()
	if found, ok := c.entries[ua]; ok {
		if now.Sub(found.parsedAt) < c.ttl {
			c.recency.MoveToFront(found.element)
			c.hits++
			result := found.result
			c.mu.Unlock()
			return result
		}

		// Expired. Drop it now so the parse below inserts a fresh entry rather
		// than leaving a stale one to be found by the next caller.
		c.remove(found)
	}
	c.misses++
	c.mu.Unlock()

	// Parsing happens outside the lock. It is pure and cheap enough that two
	// goroutines racing on the same new header and both parsing it costs less
	// than holding a global lock across every parse in the process.
	result := Parse(ua)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Another goroutine may have inserted it while this one was parsing, in
	// which case its answer is just as good and keeping it avoids two entries.
	if found, ok := c.entries[ua]; ok {
		found.result = result
		found.parsedAt = now
		c.recency.MoveToFront(found.element)
		return result
	}

	added := &entry{ua: ua, result: result, parsedAt: now}
	added.element = c.recency.PushFront(added)
	c.entries[ua] = added

	for len(c.entries) > c.capacity {
		oldest := c.recency.Back()
		if oldest == nil {
			break
		}
		c.remove(oldest.Value.(*entry))
	}

	return result
}

// remove drops one entry from both the map and the recency list. Both have to
// move together, and doing it in one place is what keeps them from drifting
// into a leak that only shows up after a week of uptime.
func (c *Cache) remove(e *entry) {
	c.recency.Remove(e.element)
	delete(c.entries, e.ua)
}

// Stats reports hits, misses and current size. A hit rate that collapses means
// something is randomising user agents, which is worth seeing on the ingestion
// health panel before it is worth seeing in a CPU profile.
func (c *Cache) Stats() (hits, misses uint64, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.hits, c.misses, len(c.entries)
}
