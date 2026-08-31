//
// regexp.go
// The regular-expression matcher SQLite does not ship with.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"sync"

	"modernc.org/sqlite"
)

// MatchFunction is the SQL function the `matches` operator compiles to. SQLite
// has no built-in regular expression support at all — REGEXP is a syntax with
// no implementation behind it — so the alternative to registering one is
// pulling every candidate dimension value into Go and matching there, which
// turns a filter into a full table read.
//
// It is registered against the driver rather than a connection, so every
// connection this process opens afterwards has it. That is also why it is
// registered from init: the driver only hands the function to connections
// opened after registration, and connections are opened lazily at the first
// query, which is always after every package's init has run.
const MatchFunction = "feasible_match"

// matchArgs is how many arguments the function takes: pattern, value and
// whether the match is case sensitive.
const matchArgs = 3

// maxCachedPatterns bounds the compiled-pattern cache. Patterns come from
// validated queries, so the cache is small in practice; the bound exists so a
// script issuing a new pattern per request cannot grow it for the life of the
// process.
const maxCachedPatterns = 512

var (
	registerOnce sync.Once
	registerErr  error

	patternMu    sync.Mutex
	patternCache = map[string]*regexp.Regexp{}
)

// init registers the matcher exactly once. A registration failure is kept
// rather than panicking, so that a process which never issues a regex filter
// still starts, and one that does gets a clear error on the query instead of a
// crash at boot.
func init() {
	registerOnce.Do(func() {
		registerErr = sqlite.RegisterDeterministicScalarFunction(MatchFunction, matchArgs, matchScalar)
	})
}

// matchScalar is the function body SQLite calls per row. It answers 1 or 0
// rather than a boolean because SQLite has no boolean type, and a NULL here
// would make a negated filter silently drop rows instead of keeping them.
func matchScalar(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != matchArgs {
		return nil, fmt.Errorf("%s takes %d arguments", MatchFunction, matchArgs)
	}

	pattern, ok := text(args[0])
	if !ok {
		return int64(0), nil
	}

	value, ok := text(args[1])
	if !ok {
		// A NULL value matches nothing, which keeps `matches` and `matches_not`
		// exact complements of each other.
		return int64(0), nil
	}

	compiled, err := compilePattern(pattern, truthy(args[2]))
	if err != nil {
		return nil, err
	}

	if compiled.MatchString(value) {
		return int64(1), nil
	}

	return int64(0), nil
}

// compilePattern returns a compiled pattern, caching it. The cache matters
// because SQLite calls this once per candidate row, and compiling a regular
// expression per row would cost more than the scan it is filtering.
func compilePattern(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	key := "s:" + pattern
	source := pattern

	if !caseSensitive {
		key = "i:" + pattern
		source = "(?i)" + pattern
	}

	patternMu.Lock()
	defer patternMu.Unlock()

	if compiled, ok := patternCache[key]; ok {
		return compiled, nil
	}

	compiled, err := regexp.Compile(source)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression %q: %w", pattern, err)
	}

	// Dropping the whole cache rather than evicting one entry keeps this to a
	// map and a mutex. The cost is recompiling a handful of live patterns on a
	// boundary nobody reaches in practice.
	if len(patternCache) >= maxCachedPatterns {
		patternCache = map[string]*regexp.Regexp{}
	}

	patternCache[key] = compiled

	return compiled, nil
}

// text reads a driver value as a string, accepting both of the forms SQLite
// hands back for a text column.
func text(value driver.Value) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	default:
		return "", false
	}
}

// truthy reads the case-sensitivity flag, which arrives as an integer.
func truthy(value driver.Value) bool {
	switch typed := value.(type) {
	case int64:
		return typed != 0
	case bool:
		return typed
	default:
		return false
	}
}

// matcherError reports a failed registration. The filter compiler asks before
// it emits a call to the function, so an unusable matcher is a clear error on
// the one query that needed it rather than a syntax error from SQLite.
func matcherError() error {
	if registerErr == nil {
		return nil
	}

	return fmt.Errorf("regular expression matching is unavailable: %w", registerErr)
}
