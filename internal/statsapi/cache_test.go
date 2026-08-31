//
// cache_test.go
// The thirty-second hold on a live report's answer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package statsapi

import (
	"testing"
	"time"
)

// TestAHeldAnswerExpires is the whole contract: fresh for the window, gone
// after it. A cache that did not expire would show a dashboard the same number
// all afternoon.
func TestAHeldAnswerExpires(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	held := newCache(CacheTTL, CacheEntries)
	held.Now = func() time.Time { return now }

	key := cacheKey("example.com", []byte(`{"metrics":["visitors"]}`))
	held.put(key, []byte(`{"results":[]}`))

	if _, ok := held.get(key); !ok {
		t.Fatal("an answer put in a moment ago is not there")
	}

	now = now.Add(CacheTTL - time.Second)
	if _, ok := held.get(key); !ok {
		t.Error("an answer expired before its time was up")
	}

	now = now.Add(2 * time.Second)
	if _, ok := held.get(key); ok {
		t.Error("an answer outlived its time")
	}
}

// TestTwoSitesNeverShareAnAnswer is the failure this cache must never have.
// One site's numbers appearing on another site's dashboard is a data leak
// between customers, not a stale figure.
func TestTwoSitesNeverShareAnAnswer(t *testing.T) {
	held := newCache(CacheTTL, CacheEntries)

	body := []byte(`{"metrics":["visitors"]}`)

	held.put(cacheKey("one.example", body), []byte(`{"one":true}`))

	if _, ok := held.get(cacheKey("two.example", body)); ok {
		t.Error("the same query against a different site read the first site's answer")
	}

	// The domain is length-prefixed, so a domain and a body that concatenate to
	// the same bytes as another pair still hash differently.
	first := cacheKey("ab", []byte("cd"))
	second := cacheKey("a", []byte("bcd"))

	if first == second {
		t.Error("two different site-and-query pairs hash to one key")
	}
}

// TestADifferentQueryIsADifferentAnswer checks that the body is part of the
// key, so the eight reports on one dashboard do not overwrite each other.
func TestADifferentQueryIsADifferentAnswer(t *testing.T) {
	held := newCache(CacheTTL, CacheEntries)

	visitors := cacheKey("example.com", []byte(`{"metrics":["visitors"]}`))
	pageviews := cacheKey("example.com", []byte(`{"metrics":["pageviews"]}`))

	held.put(visitors, []byte(`{"visitors":true}`))

	if _, ok := held.get(pageviews); ok {
		t.Error("a query for pageviews read the answer to a query for visitors")
	}
}

// TestTheCacheStaysBounded checks the memory ceiling. Each entry is a rendered
// response body, so an unbounded map is a shard's heap filled with dashboards
// nobody is looking at any more.
func TestTheCacheStaysBounded(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	held := newCache(CacheTTL, 8)
	held.Now = func() time.Time { return now }

	for i := 0; i < 100; i++ {
		held.put(cacheKey("example.com", []byte{byte(i)}), []byte("{}"))
	}

	held.mu.Lock()
	size := len(held.entries)
	held.mu.Unlock()

	if size > 8 {
		t.Errorf("the cache holds %d entries with a cap of 8", size)
	}
}

// TestANilCacheNeverHits covers the handler somebody built with a struct
// literal instead of New. It has to answer every request, slowly, rather than
// panic on the first one.
func TestANilCacheNeverHits(t *testing.T) {
	var held *cache

	key := cacheKey("example.com", []byte("{}"))

	held.put(key, []byte("{}"))

	if _, ok := held.get(key); ok {
		t.Error("a cache that does not exist answered a lookup")
	}
}
