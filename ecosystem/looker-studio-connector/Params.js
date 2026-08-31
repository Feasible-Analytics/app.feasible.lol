//
// Params.js
// Turning a Looker Studio request into Stats API parameters, filters and cache keys.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// This is the only file in the connector that touches no Google runtime — no
// UrlFetchApp, no CacheService, no DataStudioApp. That is deliberate: the
// request building, the filter translation and the cache key are the parts with
// real logic in them and the parts nothing else can check, so they live where a
// plain Node test can run them. Everything Google-shaped is a thin wrapper
// around these functions.

// The hosted service. A self-hosted instance overrides it in the connector's
// configuration, and the placeholder in the host field shows this value so an
// empty box still means something sensible rather than a broken URL.
var DEFAULT_HOST = 'https://app.feasible.lol';

// How long an answer stays in the per-user cache.
//
// Looker Studio calls getData once per chart, so a ten-chart dashboard is ten
// requests against an hourly rate limit for one refresh — and a person clicking
// between date ranges refreshes it repeatedly. Five minutes is long enough that
// a dashboard refresh costs one request per distinct query rather than one per
// chart, and short enough that nobody is looking at yesterday's numbers.
var CACHE_TTL_SECONDS = 300;

// A realtime window is the last half hour and moves every minute. Caching it
// for five minutes would show a number that is wrong in the one place people
// look at it precisely because it is live.
var REALTIME_TTL_SECONDS = 30;

// The cache key prefix. It carries a format version so that a change to what we
// store cannot read a stale entry written by the previous shape — the entries
// simply stop matching and expire on their own.
var CACHE_PREFIX = 'fs1:';

// A breakdown asks for the API's maximum page size in one call. Looker Studio
// wants a whole table, and paging inside a single getData would multiply the
// rate-limit cost of every chart by the number of pages.
var BREAKDOWN_LIMIT = 1000;

// The row count a schema-detection request asks for. Looker sets
// sampleExtraction when it only wants to see the shape of the data, and pulling
// a thousand rows to answer that is a request nobody reads.
var SAMPLE_LIMIT = 10;

// The Looker Studio filter operators that map onto a Stats API predicate, keyed
// by operator and inclusion.
//
// Everything absent from this table is left to Looker Studio: REGEXP_EXACT_MATCH
// and REGEXP_PARTIAL_MATCH (the API has contains, not regular expressions),
// NUMERIC_GREATER_THAN and the rest of the numeric family (they apply to
// metrics, and the API filters dimensions), IS_NULL, and BETWEEN. A filter left
// out here costs rows over the wire, never correctness — getData reports
// filtersApplied only when every predicate was pushed down, so Looker Studio
// applies the whole set itself whenever one was not.
var FILTER_OPERATORS = {
  'EQUALS:INCLUDE': '==',
  'EQUALS:EXCLUDE': '!=',
  'IN:INCLUDE': '==',
  'IN:EXCLUDE': '!=',
  'CONTAINS:INCLUDE': '~',
  'CONTAINS:EXCLUDE': '!~'
};

// The characters a filter value has to be escaped for. The API splits predicates
// on `;` and values on `|`, and finds the operator by scanning for the first
// unescaped `=`, `!` or `~` — so a page path containing any of them would
// otherwise be read as a second predicate or as an operator, and the filter
// would quietly mean something else.
var FILTER_ESCAPES = '\\;|=!~';

// normaliseHost trims a host and drops trailing slashes so that a value typed
// with one and a path that starts with one cannot build a double slash. The
// double slash reaches the server as a different path and answers 404.
function normaliseHost(value) {
  var host = String(value === undefined || value === null ? '' : value).trim();

  while (host.length > 0 && host.charAt(host.length - 1) === '/') {
    host = host.substring(0, host.length - 1);
  }

  return host === '' ? DEFAULT_HOST : host;
}

// parseCredential reads what the person typed into the API key box.
//
// Looker Studio's KEY authentication gives a connector exactly one text field,
// but a self-hosted instance needs a host as well as a key — and the host cannot
// come from the configuration screen, because authentication happens first. So
// the field takes either a key on its own, or a host and a key separated by
// whitespace. An API key never contains a space, which is what makes the split
// unambiguous.
function parseCredential(raw) {
  var value = String(raw === undefined || raw === null ? '' : raw).trim();

  if (value === '') {
    return {host: '', key: '', error: 'Paste your Feasible API key. Self-hosting? Type your host, a space, then the key.'};
  }

  var parts = value.split(/\s+/);

  if (parts.length === 1) {
    return {host: '', key: parts[0], error: ''};
  }

  if (parts.length === 2 && parts[0].indexOf('http') === 0) {
    return {host: normaliseHost(parts[0]), key: parts[1], error: ''};
  }

  return {
    host: '',
    key: '',
    error: 'That does not look like an API key. Paste the key on its own, or your host, a space, then the key.'
  };
}

// planQuery decides which of the three Stats API endpoints answers a request.
//
// The endpoints are shaped by what a report needs rather than by what a query
// language allows: totals with no grouping, one row per day, or one row per
// value of a single dimension. A chart that asks for a date and a second
// dimension has no single call behind it, and saying so is better than
// answering with one of the two silently dropped — a chart that looks right and
// is not is the expensive kind of wrong.
function planQuery(dimensionNames) {
  var names = dimensionNames || [];
  var hasDate = false;
  var others = [];
  var i;

  for (i = 0; i < names.length; i++) {
    if (names[i] === 'date') {
      hasDate = true;
    } else {
      others.push(names[i]);
    }
  }

  if (others.length > 1) {
    return {
      endpoint: '',
      dimensionField: '',
      error: 'This chart groups by ' + others.join(' and ') +
        '. Feasible breaks down one dimension at a time, so please remove all but one.'
    };
  }

  if (hasDate && others.length === 1) {
    return {
      endpoint: '',
      dimensionField: '',
      error: 'This chart groups by date and by ' + others[0] +
        '. Feasible answers one or the other, so please remove the date or remove ' + others[0] + '.'
    };
  }

  if (hasDate) {
    return {endpoint: 'timeseries', dimensionField: 'date', error: ''};
  }

  if (others.length === 1) {
    return {endpoint: 'breakdown', dimensionField: others[0], error: ''};
  }

  return {endpoint: 'aggregate', dimensionField: '', error: ''};
}

// endpointPath is the URL each plan calls.
function endpointPath(endpoint) {
  return '/api/v1/stats/' + endpoint;
}

// buildStatsParams assembles the query string for one Stats API call.
//
// Every endpoint takes the same core parameters, so they are built once here
// rather than three times at the call sites: a metric list that differs between
// the request and the cache key is a cache that answers the wrong question.
function buildStatsParams(options) {
  var metrics = options.metrics && options.metrics.length > 0 ? options.metrics : ['visitors'];

  var params = {
    site_id: options.siteId,
    metrics: metrics.join(','),
    period: options.period || 'custom'
  };

  // period=custom is the only period Looker Studio ever asks for, because its
  // date picker always resolves to two absolute dates before the connector is
  // called.
  if (params.period === 'custom') {
    params.date = options.startDate + ',' + options.endDate;
  }

  if (options.endpoint === 'timeseries') {
    // One row per day. The API would otherwise pick a bucket width from the
    // period, and an hourly bucket over a month is 720 rows Looker has to fold
    // back into days anyway.
    params.interval = 'date';
  }

  if (options.endpoint === 'breakdown') {
    params.property = options.property;
    params.limit = options.sample ? SAMPLE_LIMIT : BREAKDOWN_LIMIT;
  }

  if (options.filters) {
    params.filters = options.filters;
  }

  return params;
}

// lookerDateRange turns Looker Studio's date range into the API's `date`
// parameter halves, refusing anything that is not two ISO dates.
//
// The refusal matters because a malformed range would otherwise be sent as the
// literal string `undefined,undefined`, and the API's 400 would name a parameter
// the person never typed.
function lookerDateRange(dateRange) {
  var range = dateRange || {};
  var pattern = /^\d{4}-\d{2}-\d{2}$/;

  if (!pattern.test(String(range.startDate)) || !pattern.test(String(range.endDate))) {
    return {startDate: '', endDate: '', error: 'This report has no date range. Set one on the chart or on the page.'};
  }

  return {startDate: range.startDate, endDate: range.endDate, error: ''};
}

// escapeFilterValue protects the characters the API's filter grammar reads as
// syntax. The API strips the backslashes again, so escaping a character that
// did not strictly need it is harmless — which is why the set is the whole
// grammar rather than a guess at which ones matter in this position.
function escapeFilterValue(value) {
  var raw = String(value === undefined || value === null ? '' : value);
  var out = '';
  var i;

  for (i = 0; i < raw.length; i++) {
    if (FILTER_ESCAPES.indexOf(raw.charAt(i)) >= 0) {
      out += '\\';
    }

    out += raw.charAt(i);
  }

  return out;
}

// translateFilterGroup turns one of Looker Studio's OR groups into a single
// predicate, or explains why it cannot.
//
// The API expresses OR as several values on one dimension with one operator, so
// a group that mixes fields or operators has no single predicate — and there is
// no way to express it as several predicates either, because those are ANDed.
function translateFilterGroup(group, lookup) {
  if (!group || group.length === 0) {
    return {predicate: '', reason: 'an empty filter group'};
  }

  var first = group[0];
  var operator = FILTER_OPERATORS[first.operator + ':' + first.type];

  if (!operator) {
    return {predicate: '', reason: first.operator + ' on ' + first.fieldName};
  }

  var dimension = lookup(first.fieldName);

  if (!dimension) {
    return {predicate: '', reason: 'a filter on ' + first.fieldName};
  }

  var values = [];
  var i;
  var j;

  for (i = 0; i < group.length; i++) {
    if (group[i].fieldName !== first.fieldName ||
      group[i].operator !== first.operator ||
      group[i].type !== first.type) {
      return {predicate: '', reason: 'a mixed OR group on ' + first.fieldName};
    }

    var members = group[i].values || [];

    for (j = 0; j < members.length; j++) {
      values.push(escapeFilterValue(members[j]));
    }
  }

  if (values.length === 0) {
    return {predicate: '', reason: 'a filter on ' + first.fieldName + ' with no values'};
  }

  return {predicate: dimension + operator + values.join('|'), reason: ''};
}

// translateFilters pushes as much of Looker Studio's filtering down to the API
// as the API can express.
//
// `applied` is the load-bearing part of the answer. Reporting that the connector
// applied the filters when one of them was skipped would leave Looker Studio
// trusting rows that were never filtered, so it is true only when every group
// went down the wire.
function translateFilters(groups, lookup) {
  var pushed = [];
  var skipped = [];
  var i;

  if (!groups || groups.length === 0) {
    return {filters: '', skipped: skipped, applied: false};
  }

  for (i = 0; i < groups.length; i++) {
    var clause = translateFilterGroup(groups[i], lookup);

    if (clause.predicate) {
      pushed.push(clause.predicate);
    } else {
      skipped.push(clause.reason);
    }
  }

  return {
    filters: pushed.join(';'),
    skipped: skipped,
    applied: skipped.length === 0
  };
}

// cacheKeyInput is the exact identity of one API call, as a string.
//
// The parameters are sorted so that two calls that differ only in the order
// their parameters were built land on the same entry. The API key is not in
// here and must never be: the cache is the per-user cache, already scoped to one
// person, and putting a credential in a key only spreads it somewhere it can be
// read back.
function cacheKeyInput(host, endpoint, params) {
  var names = [];
  var parts = [host, endpoint];
  var name;
  var i;

  for (name in params) {
    if (Object.prototype.hasOwnProperty.call(params, name)) {
      names.push(name);
    }
  }

  names.sort();

  for (i = 0; i < names.length; i++) {
    parts.push(names[i] + '=' + params[names[i]]);
  }

  return parts.join('\n');
}

// fnv1a is one 32-bit pass of FNV-1a over a string.
function fnv1a(value, seed) {
  var hash = seed >>> 0;
  var hex;
  var i;

  for (i = 0; i < value.length; i++) {
    hash = (hash ^ value.charCodeAt(i)) >>> 0;
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }

  hex = hash.toString(16);

  while (hex.length < 8) {
    hex = '0' + hex;
  }

  return hex;
}

// hashString is two FNV-1a passes with different seeds, giving 64 bits.
//
// A cache key is a lookup, not a security boundary, so a fast non-cryptographic
// hash is the right tool — and writing it in plain arithmetic rather than
// calling Utilities.computeDigest is what keeps this file runnable, and
// therefore testable, outside Apps Script.
function hashString(value) {
  return fnv1a(value, 0x811c9dc5) + fnv1a(value, 0x9dc5811c);
}

// cacheKey names the entry one API call is stored under. It is hashed rather
// than spelled out because Apps Script caps a key at 250 characters and a
// filter string alone can be longer than that.
function cacheKey(host, endpoint, params) {
  return CACHE_PREFIX + hashString(cacheKeyInput(host, endpoint, params));
}

// cacheTtlSeconds is how long an answer may be reused.
//
// The realtime branch exists because a realtime period describes the last half
// hour and moves continuously; anything else describes a closed window whose
// numbers only change as new events land. They cannot share a lifetime.
function cacheTtlSeconds(params) {
  return params && params.period === 'realtime' ? REALTIME_TTL_SECONDS : CACHE_TTL_SECONDS;
}

// lookerDate turns the API's `YYYY-MM-DD` into the `YYYYMMDD` a YEAR_MONTH_DAY
// field has to be given. Handing Looker Studio the dashes instead produces a
// dimension it reads as text, which sorts alphabetically and draws a time series
// with no gaps where days are missing.
function lookerDate(value) {
  return String(value === undefined || value === null ? '' : value).substring(0, 10).replace(/-/g, '');
}

// Apps Script has no module system: every file shares one global scope and
// `module` is undefined there. The guard is what lets this same source be
// required by the Node test suite, which is the only place any of it can be
// exercised without a Google runtime.
if (typeof module !== 'undefined') {
  module.exports = {
    DEFAULT_HOST: DEFAULT_HOST,
    CACHE_TTL_SECONDS: CACHE_TTL_SECONDS,
    REALTIME_TTL_SECONDS: REALTIME_TTL_SECONDS,
    CACHE_PREFIX: CACHE_PREFIX,
    BREAKDOWN_LIMIT: BREAKDOWN_LIMIT,
    SAMPLE_LIMIT: SAMPLE_LIMIT,
    normaliseHost: normaliseHost,
    parseCredential: parseCredential,
    planQuery: planQuery,
    endpointPath: endpointPath,
    buildStatsParams: buildStatsParams,
    lookerDateRange: lookerDateRange,
    escapeFilterValue: escapeFilterValue,
    translateFilterGroup: translateFilterGroup,
    translateFilters: translateFilters,
    cacheKeyInput: cacheKeyInput,
    hashString: hashString,
    cacheKey: cacheKey,
    cacheTtlSeconds: cacheTtlSeconds,
    lookerDate: lookerDate
  };
}
