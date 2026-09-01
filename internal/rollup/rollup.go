//
// rollup.go
// Rebuilding one site's summary buckets from the raw events and sessions.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package rollup builds the pre-aggregated report tables and keeps them up to
// date. It is the write half of the seam whose read half lives in
// internal/query: that package decides when a summary may be read and turns a
// bucket back into a report, and this one turns raw rows into buckets.
//
// One rule governs everything here: a roll-up is a cache and never the truth.
// Any bucket must be rebuildable from the raw tables at any time, so every
// build is a delete-then-insert over a bucket range rather than an increment,
// and a bug in this package is a re-run rather than lost data.
package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// HourlyRetention is how long hourly buckets are kept. Nothing offers an hourly
// interval over a longer range, and hourly rows are twenty-four times as
// numerous as daily ones, so keeping them forever would trade the whole space
// saving for a graph nobody can ask for.
const HourlyRetention = 14 * 24 * time.Hour

// Chunk sizes for a rebuild. A build holds the write lock, so it is cut into
// pieces that ingestion can interleave with rather than one transaction that
// stops a busy site for a minute.
const (
	dayChunk  = 7
	hourChunk = 3
)

// Site is the little a build needs to know about a site: which rows are its,
// and which local day its buckets are cut on.
type Site struct {
	ID       int64
	Domain   string
	Timezone string
}

// Location resolves the site's timezone, falling back to UTC. A site whose
// timezone is unreadable still gets built — on UTC days, recorded as such in
// rollup_state — rather than silently having no summary at all.
func (s Site) Location() *time.Location {
	if s.Timezone == "" {
		return time.UTC
	}

	location, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.UTC
	}

	return location
}

// Zone is the timezone name recorded against the buckets. It has to match the
// name a query arrives with, because the reader compares the two strings before
// it trusts a single row.
func (s Site) Zone() string {
	if s.Timezone == "" {
		return "UTC"
	}

	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return "UTC"
	}

	return s.Timezone
}

// Builder rebuilds summary buckets for one account database.
type Builder struct {
	// DB is the account's writer handle. A build writes, and it reads the raw
	// tables through the same connection so that its temporary working tables
	// and its statements see each other.
	DB *sql.DB

	// Now is the clock a build measures "today" against, injectable because
	// every interesting property of a roll-up is about which day it is.
	Now func() time.Time
}

// New builds a builder over an account's writer handle.
func New(db *sql.DB) *Builder {
	return &Builder{DB: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the builder's clock.
func (b *Builder) now() time.Time {
	if b.Now == nil {
		return time.Now().UTC()
	}

	return b.Now()
}

// Request is one rebuild: which site, which grain, and which stretch of time.
type Request struct {
	Site  Site
	Grain query.Grain

	// From and To bound the buckets to rewrite. They are widened to whole
	// buckets, because a half-built day would answer a report with a fraction
	// of its traffic — worse than not answering it at all.
	From time.Time
	To   time.Time

	// CoverThrough is how far a reader may then trust the summary, and it is
	// separate from To because the day in progress is built but never served.
	// Today's row exists so that sealing the day at midnight is one small
	// rebuild rather than a day's worth of work; until then, today comes from
	// raw. Zero means the whole built range becomes readable.
	CoverThrough time.Time

	// FromBeginning says there is nothing before From — the build reaches back
	// to the site's first event — so the summary may be trusted for any earlier
	// date too. Without it a young site's "last 12 months" would read raw for
	// eleven empty months, which is the slow path for no data at all.
	FromBeginning bool
}

// Rebuild rewrites every bucket in a request's range and records how much of it
// a reader may trust.
//
// Everything inside the range is deleted before it is rebuilt, so a value that
// stopped appearing does not survive as a stale row, and any bucket can be
// rebuilt from raw at any time — which is what makes a bug in this package a
// re-run rather than lost data.
func (b *Builder) Rebuild(ctx context.Context, request Request) error {
	site, grain := request.Site, request.Grain
	location := site.Location()

	from := query.RollupBucketStart(request.From.In(location), grain, location)
	to := query.RollupBucketStart(request.To.In(location), grain, location)

	if !to.After(from) {
		return nil
	}

	names, err := b.eventNames(ctx)
	if err != nil {
		return err
	}

	// A site whose timezone changed has buckets cut on the wrong days. They
	// cannot be repaired in place — every row is in the wrong bucket — so they
	// are thrown away and rebuilt.
	if err := b.resetOnZoneChange(ctx, site, grain); err != nil {
		return err
	}

	for start := from; start.Before(to); start = b.chunkEnd(start, grain, to, location) {
		end := b.chunkEnd(start, grain, to, location)

		if err := b.buildChunk(ctx, site, grain, names, start, end); err != nil {
			return fmt.Errorf("rollup: build %s %s..%s: %w", grain, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
		}
	}

	covered := to
	if !request.CoverThrough.IsZero() {
		covered = query.RollupBucketStart(request.CoverThrough.In(location), grain, location)
	}

	if !covered.After(from) {
		return nil
	}

	return b.recordCoverage(ctx, site, grain, from, covered, request.FromBeginning)
}

// chunkEnd is the end of the piece of work that starts at a given bucket.
func (b *Builder) chunkEnd(start time.Time, grain query.Grain, to time.Time, location *time.Location) time.Time {
	size := dayChunk
	if grain == query.GrainHour {
		size = hourChunk * 24
	}

	end := start
	for i := 0; i < size; i++ {
		if !end.Before(to) {
			break
		}
		end = query.RollupNextBucket(end, grain, location)
	}

	if end.After(to) {
		end = to
	}

	return end
}

// eventNames reads the two interned event names every aggregate keys off. A
// name this account has never recorded resolves to -1 so it matches no row —
// id 0 is the empty string, and matching that would count every event that has
// no name at all.
func (b *Builder) eventNames(ctx context.Context) (eventNames, error) {
	names := eventNames{pageview: -1, engagement: -1}

	rows, err := b.DB.QueryContext(ctx,
		"SELECT id, value FROM dim_event_name WHERE value IN (?, ?)", ingest.EventPageview, ingest.EventEngagement)
	if err != nil {
		return names, fmt.Errorf("rollup: read event names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id    int64
			value string
		)

		if err := rows.Scan(&id, &value); err != nil {
			return names, fmt.Errorf("rollup: read event names: %w", err)
		}

		switch value {
		case ingest.EventPageview:
			names.pageview = id
		case ingest.EventEngagement:
			names.engagement = id
		}
	}

	return names, rows.Err()
}

// eventNames holds the two ids the pageview and event counts are defined by.
type eventNames struct {
	pageview   int64
	engagement int64
}

// buildChunk rewrites one slice of buckets inside a single transaction. The
// transaction is the unit that makes a roll-up safe to rebuild while the site
// is live: a reader either sees the old buckets or the new ones.
func (b *Builder) buildChunk(ctx context.Context, site Site, grain query.Grain, names eventNames, from, to time.Time) (err error) {
	// The working tables are temporary and therefore per connection, so the
	// whole chunk runs on one pinned connection rather than on whichever
	// connection the pool hands out next.
	conn, err := b.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("rollup: close build connection: %w", closeErr))
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	work := &chunk{tx: tx, site: site, grain: grain, names: names, from: from, to: to, location: site.Location()}

	if err := work.prepare(ctx); err != nil {
		return err
	}

	if err := work.clear(ctx); err != nil {
		return err
	}

	for _, dimension := range query.RollupDims() {
		if err := work.aggregate(ctx, dimension); err != nil {
			return err
		}
	}

	// The carry-over counts are what make a distinct count re-aggregate, and
	// they only ever apply to daily buckets: an hourly row is drawn as an hour
	// and never added to the hour beside it.
	if grain == query.GrainDay {
		if err := work.carryOver(ctx); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// chunk is one transaction's worth of work.
type chunk struct {
	tx       *sql.Tx
	site     Site
	grain    query.Grain
	names    eventNames
	from     time.Time
	to       time.Time
	location *time.Location
}

// window is the UTC bounds of the raw rows this chunk reads.
func (c *chunk) window() (int64, int64) {
	return c.from.Unix(), c.to.Unix()
}

// buckets is the local-seconds bucket range this chunk writes.
func (c *chunk) buckets() (int64, int64) {
	return query.RollupLocalUnix(c.from, c.location), query.RollupLocalUnix(c.to, c.location)
}

// bucketSQL renders the bucket expression for a timestamp column, over a window
// wide enough to include the bucket before the chunk — the carry-over counts
// are defined against it.
func (c *chunk) bucketSQL(column string) (string, []any) {
	return query.RollupBucketExpr(column, c.grain, c.location, c.carryFrom(), c.to)
}

// carryFrom is one bucket before the chunk starts. The first bucket in a chunk
// still needs to know what was carried into it from the bucket before, or a
// report that starts a day earlier would double-count across that boundary.
func (c *chunk) carryFrom() time.Time {
	previous := c.from.AddDate(0, 0, -1)
	if c.grain == query.GrainHour {
		previous = c.from.Add(-time.Hour)
	}

	return query.RollupBucketStart(previous, c.grain, c.location)
}

// prepare reads the chunk's raw rows once, into the working tables every
// dimension then aggregates from.
//
// The forty-odd aggregates that follow all read the same rows with a different
// GROUP BY. Reading them from the fact tables each time means forty scans of a
// table measured in hundreds of megabytes, forty evaluations of the bucket
// arithmetic and forty runs of the bot exclusion — all to answer a question
// whose answer does not change. One narrow copy makes the rest of the build a
// scan of memory.
func (c *chunk) prepare(ctx context.Context) error {
	// A pooled connection outlives the transaction that created a temporary
	// table, so the next chunk to be handed the same connection would find them
	// already there.
	for _, table := range workingTables {
		if _, err := c.tx.ExecContext(ctx, "DROP TABLE IF EXISTS temp."+table); err != nil {
			return fmt.Errorf("rollup: prepare: %w", err)
		}
	}

	eventBucket, eventBucketArgs := c.bucketSQL("e.timestamp")
	sessionBucket, sessionBucketArgs := c.bucketSQL("s.started_at")

	from := c.carryFrom().Unix()
	_, to := c.window()

	statements := []struct {
		sql  string
		args []any
	}{
		// Visits that contain automated traffic. `sessions` carries no bot flag
		// of its own and the reports exclude bots by default, so this is the
		// same set the raw path's NOT EXISTS finds — computed once, with no
		// time bound, because a visit is a bot's visit whichever day the
		// giveaway event landed on.
		{sql: `CREATE TEMP TABLE rollup_bot_session (session_id INTEGER PRIMARY KEY)`},
		{
			sql:  `INSERT INTO rollup_bot_session SELECT DISTINCT session_id FROM events WHERE site_id = ? AND bot_reason_id <> 0`,
			args: []any{c.site.ID},
		},

		// The chunk's rows, narrowed to what the aggregates read and with the
		// bucket already worked out.
		//
		// Twenty dimensions each want their own GROUP BY over the same rows,
		// and running twenty passes over the fact tables means twenty times the
		// disk, twenty times the bucket arithmetic and twenty times the bot
		// exclusion. This is the pass that makes a rebuild minutes rather than
		// an afternoon, and the chunk size is what keeps its working set small
		// enough to stay in memory.
		//
		// The window reaches one bucket further back than the chunk writes,
		// because the carry-over pass has to see what came into its first
		// bucket; the aggregates filter those rows back out.
		{
			sql: `CREATE TEMP TABLE rollup_fact_event AS
				SELECT ` + eventBucket + ` AS bucket, 0 AS zero,
				       e.user_id AS user_id, e.session_id AS session_id,
				       CASE WHEN e.name_id = ? THEN 1 ELSE 0 END AS pageview,
				       CASE WHEN e.name_id <> ? THEN 1 ELSE 0 END AS event,
				       e.` + strings.Join(eventCarryColumns(), ", e.") + `
				FROM events e
				WHERE e.site_id = ? AND e.timestamp >= ? AND e.timestamp < ?
				      AND e.bot_reason_id = 0 AND e.is_imported = 0`,
			args: append(append([]any{}, eventBucketArgs...), c.names.pageview, c.names.engagement, c.site.ID, from, to),
		},
		{
			sql: `CREATE TEMP TABLE rollup_fact_session AS
				SELECT ` + sessionBucket + ` AS bucket, 0 AS zero,
				       s.user_id AS user_id, s.is_bounce AS is_bounce,
				       s.duration AS duration, s.pageviews AS pageviews,
				       s.` + strings.Join(sessionCarryColumns(), ", s.") + `
				FROM sessions s
				WHERE s.site_id = ? AND s.started_at >= ? AND s.started_at < ?
				      AND s.is_imported = 0
				      AND s.id NOT IN (SELECT session_id FROM rollup_bot_session)`,
			args: append(append([]any{}, sessionBucketArgs...), c.site.ID, from, to),
		},
	}

	for _, statement := range statements {
		if _, err := c.tx.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("rollup: prepare: %w", err)
		}
	}

	return nil
}

// factColumn is the working table's column for one dimension. The whole-site
// row groups by a literal zero held as a real column, so that every aggregate
// is the same statement with a different name in it.
func factColumn(dimension query.RollupDim, session bool) string {
	if dimension.Total {
		return "zero"
	}

	if session {
		return dimension.SessionColumn
	}

	return dimension.EventColumn
}

// workingTables are the temporary tables a chunk builds and then reads. They are
// listed so they can be dropped before they are created, because a connection
// coming back out of the pool still carries the ones the last chunk made.
var workingTables = []string{
	"rollup_bot_session",
	"rollup_fact_event",
	"rollup_fact_session",
	"rollup_span",
	"rollup_session_span",
	"rollup_carry_user",
	"rollup_carry_visit",
	"rollup_carry_session_user",
	"rollup_carry_event",
	"rollup_carry_session_row",
}

// clear removes the buckets this chunk is about to write. A rebuild is a
// replacement rather than an increment, which is what makes "any bucket can be
// rebuilt from raw at any time" true rather than aspirational.
func (c *chunk) clear(ctx context.Context) error {
	fromBucket, toBucket := c.buckets()

	for _, table := range query.RollupTables() {
		if _, err := c.tx.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE site_id = ? AND grain = ? AND bucket >= ? AND bucket < ?",
			c.site.ID, int64(c.grain), fromBucket, toBucket,
		); err != nil {
			return fmt.Errorf("rollup: clear %s: %w", table, err)
		}
	}

	return nil
}

// aggregate writes one dimension's rows for every bucket in the chunk.
func (c *chunk) aggregate(ctx context.Context, dimension query.RollupDim) error {
	if dimension.BuildsFromEvents() {
		if err := c.aggregateEvents(ctx, dimension); err != nil {
			return err
		}
	}

	if dimension.BuildsFromSessions() {
		if err := c.aggregateSessions(ctx, dimension); err != nil {
			return err
		}
	}

	return nil
}

// aggregateEvents writes the hit-grain columns: the counts a report reads when
// it is asking about pageviews rather than about visits.
func (c *chunk) aggregateEvents(ctx context.Context, dimension query.RollupDim) error {
	fromBucket, toBucket := c.buckets()

	sqlText := `
		INSERT INTO ` + dimension.Table + ` (site_id, grain, bucket, dimension, value_id,
			pageviews, events, event_visitors, event_visits)
		SELECT ?, ?, f.bucket, ?, f.` + factColumn(dimension, false) + `,
		       SUM(f.pageview), SUM(f.event), COUNT(DISTINCT f.user_id), COUNT(DISTINCT f.session_id)
		FROM rollup_fact_event f
		WHERE f.bucket >= ? AND f.bucket < ?
		GROUP BY f.bucket, f.` + factColumn(dimension, false) + `
		ON CONFLICT(site_id, grain, dimension, bucket, value_id) DO UPDATE SET
			pageviews = excluded.pageviews,
			events = excluded.events,
			event_visitors = excluded.event_visitors,
			event_visits = excluded.event_visits`

	args := []any{c.site.ID, int64(c.grain), int64(dimension.Code), fromBucket, toBucket}

	if _, err := c.tx.ExecContext(ctx, sqlText, args...); err != nil {
		return fmt.Errorf("rollup: aggregate events into %s (%d): %w", dimension.Table, dimension.Code, err)
	}

	return nil
}

// aggregateSessions writes the visit-grain columns.
//
// A visit is placed in time by when it started, which is what the raw path
// does: a visit that began at 23:58 belongs to that day even though it ended on
// the next one, and moving it would make yesterday's total change after
// midnight.
func (c *chunk) aggregateSessions(ctx context.Context, dimension query.RollupDim) error {
	fromBucket, toBucket := c.buckets()

	sqlText := `
		INSERT INTO ` + dimension.Table + ` (site_id, grain, bucket, dimension, value_id,
			visits, visitors, bounces, visit_duration, session_pageviews)
		SELECT ?, ?, f.bucket, ?, f.` + factColumn(dimension, true) + `,
		       COUNT(*), COUNT(DISTINCT f.user_id), SUM(f.is_bounce), SUM(f.duration), SUM(f.pageviews)
		FROM rollup_fact_session f
		WHERE f.bucket >= ? AND f.bucket < ?
		GROUP BY f.bucket, f.` + factColumn(dimension, true) + `
		ON CONFLICT(site_id, grain, dimension, bucket, value_id) DO UPDATE SET
			visits = excluded.visits,
			visitors = excluded.visitors,
			bounces = excluded.bounces,
			visit_duration = excluded.visit_duration,
			session_pageviews = excluded.session_pageviews`

	args := []any{c.site.ID, int64(c.grain), int64(dimension.Code), fromBucket, toBucket}

	if _, err := c.tx.ExecContext(ctx, sqlText, args...); err != nil {
		return fmt.Errorf("rollup: aggregate sessions into %s (%d): %w", dimension.Table, dimension.Code, err)
	}

	return nil
}

// eventCarryColumns lists the events columns the working tables have to carry.
func eventCarryColumns() []string {
	return distinctColumns(func(d query.RollupDim) string { return d.EventColumn })
}

// sessionCarryColumns lists the sessions columns the working tables have to
// carry.
func sessionCarryColumns() []string {
	return distinctColumns(func(d query.RollupDim) string { return d.SessionColumn })
}

// distinctColumns collects the distinct column names one side of the registry
// groups by, in registry order so the generated SQL is stable. Reading them off
// the registry is what makes a new dimension start being summarised without a
// second list to remember.
func distinctColumns(pick func(query.RollupDim) string) []string {
	seen := map[string]bool{}
	var columns []string

	for _, d := range query.RollupDims() {
		name := pick(d)
		if name == "" || seen[name] {
			continue
		}

		seen[name] = true
		columns = append(columns, name)
	}

	return columns
}

// carryOver fills in the columns that make a distinct count re-aggregate.
//
// A visitor id is derived from a salt that rotates at UTC midnight, so on any
// site whose day does not start there one id can be present in two adjacent
// local days; a visit can straddle a boundary for the simpler reason that
// somebody kept clicking. Either way the entity is real once and counted twice
// when the buckets are added, and `_carried` records exactly how many such
// entities each bucket inherited from the one before it.
//
// Only entities that actually appear in two buckets can carry anything, and
// there are very few of them, so the whole pass runs against a working table of
// their rows rather than against the fact tables.
func (c *chunk) carryOver(ctx context.Context) error {
	if err := c.prepareCarry(ctx); err != nil {
		return err
	}

	previous := query.RollupPreviousBucketSQL("a.bucket", c.grain)

	for _, dimension := range query.RollupDims() {
		if dimension.BuildsFromEvents() {
			column := factColumn(dimension, false)

			if err := c.carryColumn(ctx, dimension, "rollup_carry_event", column, "user_id", "event_visitors_carried", previous); err != nil {
				return err
			}

			if err := c.carryColumn(ctx, dimension, "rollup_carry_event", column, "session_id", "event_visits_carried", previous); err != nil {
				return err
			}
		}

		if dimension.BuildsFromSessions() {
			if err := c.carryColumn(ctx, dimension, "rollup_carry_session_row", factColumn(dimension, true), "user_id", "visitors_carried", previous); err != nil {
				return err
			}
		}
	}

	return nil
}

// prepareCarry builds the working tables the carry-over pass reads: the rows
// belonging to the handful of visitors and visits that appear in two buckets.
func (c *chunk) prepareCarry(ctx context.Context) error {
	eventColumns := eventCarryColumns()
	sessionColumns := sessionCarryColumns()

	statements := []struct {
		sql  string
		args []any
	}{
		// The distinct (bucket, visitor, visit) triples in the window. Every
		// question the carry-over pass asks about which entities span a
		// boundary is answered from this one table.
		{sql: `CREATE TEMP TABLE rollup_span AS
			SELECT DISTINCT bucket, user_id, session_id FROM rollup_fact_event`},
		{sql: `CREATE TEMP TABLE rollup_session_span AS
			SELECT DISTINCT bucket, user_id FROM rollup_fact_session`},

		{sql: `CREATE TEMP TABLE rollup_carry_user AS
			SELECT user_id FROM (SELECT DISTINCT bucket, user_id FROM rollup_span)
			GROUP BY user_id HAVING COUNT(*) > 1`},
		{sql: `CREATE TEMP TABLE rollup_carry_visit AS
			SELECT session_id FROM (SELECT DISTINCT bucket, session_id FROM rollup_span)
			GROUP BY session_id HAVING COUNT(*) > 1`},
		{sql: `CREATE TEMP TABLE rollup_carry_session_user AS
			SELECT user_id FROM rollup_session_span GROUP BY user_id HAVING COUNT(*) > 1`},

		// Only the rows of the few entities that really do appear in two
		// buckets. Everything after this point reads a table of thousands
		// rather than one of millions.
		{sql: `CREATE TEMP TABLE rollup_carry_event AS
			SELECT bucket, zero, user_id, session_id, ` + strings.Join(eventColumns, ", ") + `
			FROM rollup_fact_event
			WHERE user_id IN (SELECT user_id FROM rollup_carry_user)
			   OR session_id IN (SELECT session_id FROM rollup_carry_visit)`},
		{sql: `CREATE TEMP TABLE rollup_carry_session_row AS
			SELECT bucket, zero, user_id, ` + strings.Join(sessionColumns, ", ") + `
			FROM rollup_fact_session
			WHERE user_id IN (SELECT user_id FROM rollup_carry_session_user)`},

		{sql: `CREATE INDEX rollup_carry_event_key ON rollup_carry_event (bucket, user_id, session_id)`},
		{sql: `CREATE INDEX rollup_carry_session_row_key ON rollup_carry_session_row (bucket, user_id)`},
	}

	for _, statement := range statements {
		if _, err := c.tx.ExecContext(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("rollup: prepare carry-over: %w", err)
		}
	}

	return nil
}

// carryColumn writes one carry-over count: how many of a bucket's distinct
// entities were already present, under the same dimension value, in the bucket
// before it.
func (c *chunk) carryColumn(ctx context.Context, dimension query.RollupDim, source, column, entity, target, previous string) error {
	fromBucket, toBucket := c.buckets()

	sqlText := `
		UPDATE ` + dimension.Table + ` AS r
		SET ` + target + ` = c.carried
		FROM (
			SELECT a.bucket AS bucket, a.v AS v, COUNT(*) AS carried
			FROM (SELECT DISTINCT bucket, ` + column + ` AS v, ` + entity + ` AS entity FROM ` + source + `) a
			JOIN (SELECT DISTINCT bucket, ` + column + ` AS v, ` + entity + ` AS entity FROM ` + source + `) b
			  ON b.v = a.v AND b.entity = a.entity AND b.bucket = ` + previous + `
			GROUP BY a.bucket, a.v
		) c
		WHERE r.site_id = ? AND r.grain = ? AND r.dimension = ?
		      AND r.bucket >= ? AND r.bucket < ?
		      AND r.bucket = c.bucket AND r.value_id = c.v`

	args := []any{c.site.ID, int64(c.grain), int64(dimension.Code), fromBucket, toBucket}

	if _, err := c.tx.ExecContext(ctx, sqlText, args...); err != nil {
		return fmt.Errorf("rollup: carry-over %s.%s (%d): %w", dimension.Table, target, dimension.Code, err)
	}

	return nil
}

// resetOnZoneChange throws away a site's summary when its timezone has changed.
// Every bucket is cut on a local day, so a new timezone means every row is in
// the wrong bucket, and there is no repair short of rebuilding.
func (b *Builder) resetOnZoneChange(ctx context.Context, site Site, grain query.Grain) error {
	var stored string

	err := b.DB.QueryRowContext(ctx,
		"SELECT timezone FROM rollup_state WHERE site_id = ? AND grain = ?", site.ID, int64(grain)).Scan(&stored)
	if err == sql.ErrNoRows || stored == site.Zone() {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rollup: read state: %w", err)
	}

	return b.Reset(ctx, site.ID, grain)
}

// Reset deletes every summary row for one site and grain. It is what the
// rebuild command runs when it is told to start over, and what a timezone
// change forces.
func (b *Builder) Reset(ctx context.Context, siteID int64, grain query.Grain) error {
	for _, table := range query.RollupTables() {
		if _, err := b.DB.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE site_id = ? AND grain = ?", siteID, int64(grain)); err != nil {
			return fmt.Errorf("rollup: reset %s: %w", table, err)
		}
	}

	if _, err := b.DB.ExecContext(ctx,
		"DELETE FROM rollup_state WHERE site_id = ? AND grain = ?", siteID, int64(grain)); err != nil {
		return fmt.Errorf("rollup: reset state: %w", err)
	}

	return nil
}

// recordCoverage widens the window a reader is allowed to trust.
//
// The window is extended only when the new range touches the old one. A build
// that leaves a hole — days 1 to 5 and then days 20 to 25 — must not claim days
// 1 to 25, because a report over the gap would read zero and present it as a
// week with no traffic.
func (b *Builder) recordCoverage(ctx context.Context, site Site, grain query.Grain, from, to time.Time, fromBeginning bool) error {
	location := site.Location()

	builtFrom := query.RollupLocalUnix(from, location)
	builtThrough := query.RollupLocalUnix(to, location)

	// A build that reaches the site's first event covers everything before it
	// too, because everything before it is empty and summing no buckets gives
	// the zero those days really hold.
	if fromBeginning {
		builtFrom = 0
	}

	var (
		zone            string
		coveredFrom     int64
		coveredThrough  int64
		hasExistingRows = true
	)

	err := b.DB.QueryRowContext(ctx,
		"SELECT timezone, covered_from, covered_through FROM rollup_state WHERE site_id = ? AND grain = ?",
		site.ID, int64(grain)).Scan(&zone, &coveredFrom, &coveredThrough)

	switch {
	case err == sql.ErrNoRows:
		hasExistingRows = false
	case err != nil:
		return fmt.Errorf("rollup: read state: %w", err)
	}

	if hasExistingRows && zone == site.Zone() && builtFrom <= coveredThrough && builtThrough >= coveredFrom {
		if coveredFrom < builtFrom {
			builtFrom = coveredFrom
		}
		if coveredThrough > builtThrough {
			builtThrough = coveredThrough
		}
	}

	_, err = b.DB.ExecContext(ctx, `
		INSERT INTO rollup_state (site_id, grain, timezone, covered_from, covered_through, built_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(site_id, grain) DO UPDATE SET
			timezone = excluded.timezone,
			covered_from = excluded.covered_from,
			covered_through = excluded.covered_through,
			built_at = excluded.built_at`,
		site.ID, int64(grain), site.Zone(), builtFrom, builtThrough, b.now().Unix())
	if err != nil {
		return fmt.Errorf("rollup: record state: %w", err)
	}

	return nil
}

// Prune drops hourly buckets that have aged out and moves the covered window up
// behind them. Deleting the rows without moving the window would leave a reader
// trusting buckets that are no longer there and reporting a fortnight-old
// morning as having had no traffic.
func (b *Builder) Prune(ctx context.Context, site Site) error {
	location := site.Location()

	cutoff := query.RollupBucketStart(b.now().Add(-HourlyRetention).In(location), query.GrainHour, location)
	local := query.RollupLocalUnix(cutoff, location)

	for _, table := range query.RollupTables() {
		if _, err := b.DB.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE site_id = ? AND grain = ? AND bucket < ?",
			site.ID, int64(query.GrainHour), local); err != nil {
			return fmt.Errorf("rollup: prune %s: %w", table, err)
		}
	}

	if _, err := b.DB.ExecContext(ctx, `
		UPDATE rollup_state SET covered_from = ?
		WHERE site_id = ? AND grain = ? AND covered_from < ?`,
		local, site.ID, int64(query.GrainHour), local); err != nil {
		return fmt.Errorf("rollup: prune state: %w", err)
	}

	return nil
}

// Coverage reports what is built for one site and grain, for the status command
// and for tests. The serving path reads the same table through the query
// package's own cached reader.
func (b *Builder) Coverage(ctx context.Context, siteID int64, grain query.Grain) (query.RollupCoverage, bool, error) {
	var coverage query.RollupCoverage

	err := b.DB.QueryRowContext(ctx,
		"SELECT timezone, covered_from, covered_through FROM rollup_state WHERE site_id = ? AND grain = ?",
		siteID, int64(grain)).Scan(&coverage.Timezone, &coverage.From, &coverage.Through)

	if err == sql.ErrNoRows {
		return coverage, false, nil
	}
	if err != nil {
		return coverage, false, fmt.Errorf("rollup: read state: %w", err)
	}

	return coverage, true, nil
}
