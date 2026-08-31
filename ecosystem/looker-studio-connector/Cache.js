//
// Cache.js
// The per-user response cache that keeps a dashboard refresh inside the rate limit.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Looker Studio calls getData once per chart. A ten-chart dashboard is ten
// requests for one refresh, most of them asking the same question with a
// different field list, and the Stats API counts every one of them against an
// hourly limit. The cache is what turns that into one request per distinct
// query — it is a requirement of this connector working at all, not a
// performance nicety.

// The largest string written to one cache entry.
//
// Apps Script caps an entry at 100 KB of encoded value. A JavaScript string
// costs at most three UTF-8 bytes per character within the basic plane, and
// characters above it arrive as surrogate pairs at two bytes each, so 25,000
// characters cannot exceed 75 KB however the response is spelled. Deriving the
// limit from the worst case rather than measuring bytes keeps the split cheap
// and removes the class of bug where an entry silently fails to write because
// one row happened to be in Japanese.
var CACHE_CHUNK_CHARS = 25000;

// The most chunks one answer may occupy.
//
// Beyond this the response is not cached at all. Reassembling a dozen entries
// costs more than the request it saves, every chunk is separately evictable — so
// the odds of a complete read fall with each one — and a response this large is
// a chart pulling thousands of rows that nobody is refreshing in a tight loop.
// Skipping is the honest answer; a partially cached response would be worse than
// none.
var CACHE_MAX_CHUNKS = 8;

// The marker that says an entry is a manifest rather than a payload. It cannot
// collide with a real response because every response we cache is JSON, which
// never begins with a `#`.
var CHUNK_MARKER = '#chunks:';

// cachedStatsRequest answers one Stats API call, from the cache when it can.
//
// Only a successful body is ever stored: statsRequest throws on anything that is
// not a 200, so the write below is unreachable for an error. Caching a failure
// would leave a rate limit or a typo pinned in front of somebody for five
// minutes after they fixed it.
function cachedStatsRequest(host, path, params, apiKey) {
  var cache = CacheService.getUserCache();
  var key = cacheKey(host, path, params);
  var hit = readCache(cache, key);

  if (hit !== null) {
    return hit;
  }

  var body = statsRequest(host, path, params, apiKey);

  writeCache(cache, key, body, cacheTtlSeconds(params));

  return body;
}

// readCache reassembles an entry, or reports a miss.
//
// A chunked entry whose parts have not all survived is a miss rather than a
// partial answer. Chunks expire independently and can be evicted under memory
// pressure, so half a JSON document is a real possibility and parsing one would
// fail somewhere far away from here.
function readCache(cache, key) {
  var head = cache.get(key);

  if (head === null || head === undefined) {
    return null;
  }

  if (head.indexOf(CHUNK_MARKER) !== 0) {
    return head;
  }

  var count = parseInt(head.substring(CHUNK_MARKER.length), 10);

  if (!(count > 0)) {
    return null;
  }

  var names = [];
  var i;

  for (i = 0; i < count; i++) {
    names.push(key + ':' + i);
  }

  var parts = cache.getAll(names);
  var body = '';

  for (i = 0; i < count; i++) {
    var part = parts[names[i]];

    if (part === null || part === undefined) {
      return null;
    }

    body += part;
  }

  return body;
}

// writeCache stores an answer, splitting it when it is too big for one entry.
//
// The chunks are written before the manifest that names them. The reverse order
// has a window in which the manifest exists and its chunks do not, and a reader
// landing in that window gets a miss it could have avoided — this way the
// manifest only ever appears once there is something to read.
function writeCache(cache, key, body, ttl) {
  if (body.length <= CACHE_CHUNK_CHARS) {
    cache.put(key, body, ttl);
    return true;
  }

  var count = Math.ceil(body.length / CACHE_CHUNK_CHARS);

  if (count > CACHE_MAX_CHUNKS) {
    return false;
  }

  var values = {};
  var i;

  for (i = 0; i < count; i++) {
    values[key + ':' + i] = body.substring(i * CACHE_CHUNK_CHARS, (i + 1) * CACHE_CHUNK_CHARS);
  }

  cache.putAll(values, ttl);
  cache.put(key, CHUNK_MARKER + count, ttl);

  return true;
}
