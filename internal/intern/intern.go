//
// intern.go
// The dimension-string cache: value to integer id, in memory, on the hot path.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package intern turns dimension strings into the integer ids the analytics
// schema stores. Every dimension column in `events` and `sessions` is an id
// into a small `dim_*` table, which is what takes a row from roughly 300 bytes
// to 80 and makes every report's GROUP BY an integer comparison.
//
// The cost of that is a lookup on write, and this package is what stops that
// cost being a query. One account's dimension tables hold a few thousand rows
// between them, so the whole set fits in a map: warm it once at start-up and
// interning on the ingest path is a hash lookup, with a database write only for
// a value nobody has ever sent before.
package intern

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// Dimension names one interned column. The value is also the suffix of its
// table, so the two can never drift apart the way a parallel map of names to
// tables eventually would.
type Dimension string

// The dimensions, one per interned column in the analytics schema. City is
// interned like every other place name because the database we ship carries
// city names and no ids, so the alternative is a GeoNames lookup table shipped
// solely to turn an id back into the string the dashboard renders.
const (
	EventName      Dimension = "event_name"
	Hostname       Dimension = "hostname"
	Pathname       Dimension = "pathname"
	PageTitle      Dimension = "page_title"
	Referrer       Dimension = "referrer"
	Source         Dimension = "source"
	Channel        Dimension = "channel"
	UTMSource      Dimension = "utm_source"
	UTMMedium      Dimension = "utm_medium"
	UTMCampaign    Dimension = "utm_campaign"
	Country        Dimension = "country"
	Region         Dimension = "region"
	City           Dimension = "city"
	DeviceType     Dimension = "device_type"
	ScreenSize     Dimension = "screen_size"
	Browser        Dimension = "browser"
	BrowserVersion Dimension = "browser_version"
	OS             Dimension = "os"
	OSVersion      Dimension = "os_version"
	Language       Dimension = "language"
	BotReason      Dimension = "bot_reason"
)

// Dimensions is every dimension, and it is the list the cache warms and the
// list the schema is checked against. Adding a dimension means adding it here
// and in the migration; a test asserts the two agree, because a dimension
// present in one and not the other fails at the first event rather than at
// build time.
var Dimensions = []Dimension{
	EventName, Hostname, Pathname, PageTitle,
	Referrer, Source, Channel, UTMSource, UTMMedium, UTMCampaign,
	Country, Region, City,
	DeviceType, ScreenSize, Browser, BrowserVersion, OS, OSVersion, Language,
	BotReason,
}

// EmptyID is the id of the empty string in every dimension table. Pinning it to
// zero is what lets "not set" be an ordinary id: the column defaults to 0, no
// query needs a NULL branch, and no index has to carry NULLs.
const EmptyID int64 = 0

// Table returns the dimension's table name. The name is built from a constant
// rather than taken from a caller, which is what makes it safe to interpolate
// into SQL — table names cannot be bind parameters.
func (d Dimension) Table() string {
	return "dim_" + string(d)
}

// Cache is one account's interned dimension values. It is safe for concurrent
// use: reads take a shared lock and are the overwhelming majority, and a miss
// takes the exclusive lock for the one insert.
type Cache struct {
	// db is the account's writer handle. Interning has to write, and a value
	// inserted on some other connection would not be visible to a reader inside
	// the same write transaction.
	db *sql.DB

	mu     sync.RWMutex
	values map[Dimension]map[string]int64
}

// New builds an empty cache for one account. It does not touch the database, so
// a caller can construct the cache and decide separately when to pay for
// warming it.
func New(db *sql.DB) *Cache {
	values := make(map[Dimension]map[string]int64, len(Dimensions))
	for _, dimension := range Dimensions {
		values[dimension] = map[string]int64{}
	}

	return &Cache{db: db, values: values}
}

// Warm loads every dimension table into memory. It runs when an account
// database is opened rather than lazily, because the alternative is a database
// round trip for every distinct value in the first minutes after a restart —
// precisely when a busy account is least able to afford it.
func (c *Cache) Warm(ctx context.Context) error {
	loaded := make(map[Dimension]map[string]int64, len(Dimensions))

	for _, dimension := range Dimensions {
		values, err := load(ctx, c.db, dimension)
		if err != nil {
			return err
		}

		loaded[dimension] = values
	}

	// The maps are swapped in one go so a concurrent lookup sees either the old
	// contents or the new ones, never a table that is half loaded.
	c.mu.Lock()
	c.values = loaded
	c.mu.Unlock()

	return nil
}

// load reads one dimension table. It is a full scan by design: these tables are
// small, and one scan at start-up buys every later lookup for free.
func load(ctx context.Context, db *sql.DB, dimension Dimension) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, value FROM "+dimension.Table())
	if err != nil {
		return nil, fmt.Errorf("warm %s: %w", dimension.Table(), err)
	}
	defer rows.Close()

	values := map[string]int64{}

	for rows.Next() {
		var (
			id    int64
			value string
		)

		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("warm %s: %w", dimension.Table(), err)
		}

		values[value] = id
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("warm %s: %w", dimension.Table(), err)
	}

	return values, nil
}

// ID returns the id for a value, creating it if this account has never seen it.
// This is the function the ingest path calls twenty times per event, so the
// common case does no allocation, no SQL and no context work — it is a map read
// under a read lock and nothing else.
func (c *Cache) ID(ctx context.Context, dimension Dimension, value string) (int64, error) {
	// The empty string is the single most common value on the whole ingest path
	// — most events have no campaign, no region and no referrer — and it is
	// always id 0, so it never needs a lookup at all.
	if value == "" {
		return EmptyID, nil
	}

	c.mu.RLock()
	table, known := c.values[dimension]
	if known {
		if id, ok := table[value]; ok {
			c.mu.RUnlock()
			return id, nil
		}
	}
	c.mu.RUnlock()

	if !known {
		return 0, fmt.Errorf("intern: %q is not a dimension", dimension)
	}

	return c.insert(ctx, dimension, value)
}

// insert adds a value nobody has sent before. It holds the write lock across
// the database call on purpose: two goroutines racing on the same new value
// would otherwise both insert, and one of them would get the constraint error
// instead of an id.
func (c *Cache) insert(ctx context.Context, dimension Dimension, value string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Another goroutine may have inserted it while this one waited for the
	// lock, in which case there is nothing to do.
	if id, ok := c.values[dimension][value]; ok {
		return id, nil
	}

	table := dimension.Table()

	// Two statements rather than one INSERT ... RETURNING, because the row may
	// already exist in the file even though it is not in the map: another
	// process, or a restore, can have written it. DO NOTHING makes that case
	// free instead of an error.
	if _, err := c.db.ExecContext(ctx, "INSERT INTO "+table+" (value) VALUES (?) ON CONFLICT(value) DO NOTHING", value); err != nil {
		return 0, fmt.Errorf("intern %s: %w", table, err)
	}

	var id int64
	if err := c.db.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE value = ?", value).Scan(&id); err != nil {
		return 0, fmt.Errorf("intern %s: %w", table, err)
	}

	c.values[dimension][value] = id

	return id, nil
}

// Size reports how many values a dimension holds in memory. It is what a health
// check watches: a dimension growing by one row per request means the site puts
// identifiers in its URLs, which is the case path-cleaning rules exist for.
func (c *Cache) Size(dimension Dimension) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.values[dimension])
}
