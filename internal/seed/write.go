//
// write.go
// The bulk write path: fold into sessions in memory, then one transaction per batch.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// batchRows is how many events are collected before a transaction is opened.
// Un-batched, SQLite caps out in the low hundreds of writes a second; twenty
// thousand rows a transaction is where the commit cost per row disappears and
// the memory held between commits is still a few megabytes.
const batchRows = 20_000

// insertChunk is how many rows go into one INSERT statement. There is a real
// optimum here rather than "as many as possible", and it is worth knowing why:
// the pure-Go SQLite driver re-parses a statement on every execution, so a
// prepared statement saves nothing and the parse cost grows with the number of
// bound parameters in the text. Batching amortises the fixed per-statement
// costs, and past a dozen rows the parser starts costing more than they do.
// Measured on a million-pageview run, a dozen is about fifteen per cent faster
// than one row a statement and about twenty per cent faster than sixty-four.
const insertChunk = 12

// eventColumns is the hot table in bind order. It is deliberately the same set
// the ingest writer uses, and a test asserts it still covers every column in
// the table: the whole point of a seed is that its rows are indistinguishable
// from real ones, and a column this forgot would be a column every report
// silently reads as zero.
//
// The id is bound rather than left to SQLite, which the ingest writer does not
// do. A multi-row insert has one last-inserted id between all of its rows and
// the cold table needs one per row it belongs to, so the generator allocates
// them — exactly as it already allocates session ids.
const eventColumns = `id, site_id, timestamp, name_id, user_id, session_id,
	hostname_id, pathname_id, page_title_id,
	referrer_id, source_id, channel_id, utm_source_id, utm_medium_id, utm_campaign_id,
	country_id, region_id, city_id,
	device_type_id, screen_size_id, browser_id, browser_version_id,
	os_id, os_version_id, language_id,
	scroll_depth, engagement_time, bot_reason_id, is_imported, has_details`

// eventColumnCount is how many values one event row binds.
const eventColumnCount = 30

// insertDetailsSQL writes the cold row, and only when there is something to put
// in it. It stays one row at a time because only a small share of events carry
// properties, revenue or a long-tail UTM field.
const insertDetailsSQL = `
	INSERT INTO event_details (event_id, props, revenue_amount, revenue_currency, utm_content, utm_term, full_url)
	VALUES (?,?,?,?,?,?,?)`

// sessionColumns is the session row in bind order.
const sessionColumns = `id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
	pageviews, events, entry_page_id, exit_page_id, entry_hostname_id, exit_hostname_id,
	entry_props,
	referrer_id, source_id, channel_id, utm_source_id, utm_medium_id, utm_campaign_id,
	country_id, region_id, city_id,
	device_type_id, screen_size_id, browser_id, browser_version_id,
	os_id, os_version_id, language_id, is_imported`

// sessionColumnCount is how many values one session row binds.
const sessionColumnCount = 31

// sessionConflict updates a visit in place when it is still going. One row per
// session updated in place is why every average in the query layer is a plain
// AVG rather than a ratio of signed sums.
const sessionConflict = `
	ON CONFLICT(id) DO UPDATE SET
		last_seen_at      = excluded.last_seen_at,
		started_at        = excluded.started_at,
		duration          = excluded.duration,
		is_bounce         = excluded.is_bounce,
		pageviews         = excluded.pageviews,
		events            = excluded.events,
		entry_page_id     = excluded.entry_page_id,
		exit_page_id      = excluded.exit_page_id,
		entry_hostname_id = excluded.entry_hostname_id,
		exit_hostname_id  = excluded.exit_hostname_id,
		entry_props       = excluded.entry_props,
		referrer_id       = excluded.referrer_id,
		source_id         = excluded.source_id,
		channel_id        = excluded.channel_id,
		utm_source_id     = excluded.utm_source_id,
		utm_medium_id     = excluded.utm_medium_id,
		utm_campaign_id   = excluded.utm_campaign_id,
		country_id        = excluded.country_id,
		region_id         = excluded.region_id,
		city_id           = excluded.city_id,
		device_type_id    = excluded.device_type_id,
		screen_size_id    = excluded.screen_size_id,
		browser_id        = excluded.browser_id,
		browser_version_id = excluded.browser_version_id,
		os_id             = excluded.os_id,
		os_version_id     = excluded.os_version_id,
		language_id       = excluded.language_id`

// multiRowSQL builds an insert for a fixed number of rows. The statement text
// depends only on the row count, so one batch needs two of them at most: one
// for the full chunks and one for the remainder.
func multiRowSQL(table, columns string, values, rows int, suffix string) string {
	var query strings.Builder

	query.WriteString("INSERT INTO ")
	query.WriteString(table)
	query.WriteString(" (")
	query.WriteString(columns)
	query.WriteString(") VALUES ")

	for row := 0; row < rows; row++ {
		if row > 0 {
			query.WriteString(",")
		}

		query.WriteString("(")
		for i := 0; i < values; i++ {
			if i > 0 {
				query.WriteString(",")
			}
			query.WriteString("?")
		}
		query.WriteString(")")
	}

	query.WriteString(suffix)

	return query.String()
}

// dimensionKey is one dimension and one value within it.
type dimensionKey struct {
	dimension intern.Dimension
	value     string
}

// eventRow pairs a derived event with the session it was folded into and the id
// it will be written under.
type eventRow struct {
	event     *ingest.Event
	sessionID int64
	id        int64
}

// batchWriter accumulates one account's events and writes them in transactions.
//
// It exists rather than calling the ingest writer because the two jobs are not
// the same: the ingest writer earns its dedupe table, its pending map and its
// deferred rows from retries and lost acknowledgements, none of which a
// generator can produce, and all of which cost a row's worth of work again. What
// it does share is everything that decides what a row contains — the derive
// pipeline, the session cache and the interning cache are the real ones.
type batchWriter struct {
	account  *accounts.Account
	sessions *ingest.SessionCache

	// ids caches the dimension ids for the whole run. A seeded catalogue is a
	// few thousand values reused millions of times, so after the first minute
	// every lookup is a map hit and no event costs a query.
	ids map[dimensionKey]int64

	rows []eventRow

	// nextEventID is this account's row id allocator, seeded from the high
	// water mark in the file so that seeding on top of existing data carries on
	// rather than collides.
	nextEventID int64

	// args is reused across statements. At sixty-four rows of thirty values it
	// would otherwise be a two-thousand element allocation per statement.
	args []any
}

// newBatchWriter builds the writer for one open account database.
func newBatchWriter(account *accounts.Account, sessions *ingest.SessionCache) *batchWriter {
	return &batchWriter{
		account:  account,
		sessions: sessions,
		ids:      map[dimensionKey]int64{},
		rows:     make([]eventRow, 0, batchRows),
		args:     make([]any, 0, insertChunk*eventColumnCount),
	}
}

// add folds one derived event into its session and queues its row. It returns
// whether the event produced a row: an engagement ping whose pageview has not
// arrived yet is parked in the session cache instead, and comes back when the
// pageview does.
func (w *batchWriter) add(event *ingest.Event) bool {
	session, ok, revived := w.sessions.Apply(event)
	if !ok {
		return false
	}

	w.rows = append(w.rows, eventRow{event: event, sessionID: session.ID})

	// A ping that arrived before its own pageview was never written. Now that
	// the visit exists, its row goes in with everything else.
	for _, ping := range revived {
		w.rows = append(w.rows, eventRow{event: ping, sessionID: session.ID})
	}

	return true
}

// full reports whether the batch is big enough to be worth a transaction.
func (w *batchWriter) full() bool {
	return len(w.rows) >= batchRows
}

// flush writes everything queued for this account. The order is forced by the
// account handle holding exactly one write connection: anything that might
// insert a dimension row has to finish before the transaction opens, or it
// would wait for a connection only the transaction can release.
func (w *batchWriter) flush(ctx context.Context) error {
	dirty := w.sessions.TakeDirty(w.account.ID)
	merges := w.sessions.TakeMerges(w.account.ID)

	if len(w.rows) == 0 && len(dirty) == 0 && len(merges) == 0 {
		return nil
	}

	if err := w.internBatch(ctx, dirty); err != nil {
		return err
	}

	if err := w.commit(ctx, dirty, merges); err != nil {
		return err
	}

	w.rows = w.rows[:0]

	return nil
}

// internBatch resolves every dimension string this batch will write. Both the
// events and the dirty sessions are walked, because a session's attribution is
// frozen at its first event and that event may have been written by an earlier
// batch — its strings are in memory but its ids are not.
func (w *batchWriter) internBatch(ctx context.Context, dirty []*ingest.Session) error {
	add := func(dimension intern.Dimension, value string) error {
		if value == "" {
			return nil
		}

		key := dimensionKey{dimension, value}
		if _, ok := w.ids[key]; ok {
			return nil
		}

		id, err := w.account.Intern.ID(ctx, dimension, value)
		if err != nil {
			return fmt.Errorf("seed intern: %w", err)
		}
		w.ids[key] = id

		return nil
	}

	for _, row := range w.rows {
		event := row.event

		for _, pair := range []struct {
			dimension intern.Dimension
			value     string
		}{
			{intern.EventName, event.Name},
			{intern.Hostname, event.Hostname},
			{intern.Pathname, event.Pathname},
			{intern.PageTitle, event.PageTitle},
			{intern.Referrer, event.Referrer},
			{intern.Source, event.Source},
			{intern.Channel, event.Channel},
			{intern.UTMSource, event.UTMSource},
			{intern.UTMMedium, event.UTMMedium},
			{intern.UTMCampaign, event.UTMCampaign},
			{intern.Country, event.Country},
			{intern.Region, event.Region},
			{intern.City, event.City},
			{intern.DeviceType, event.DeviceType},
			{intern.ScreenSize, event.ScreenSize},
			{intern.Browser, event.Browser},
			{intern.BrowserVersion, event.BrowserVersion},
			{intern.OS, event.OS},
			{intern.OSVersion, event.OSVersion},
			{intern.Language, event.Language},
			{intern.BotReason, event.BotReason},
		} {
			if err := add(pair.dimension, pair.value); err != nil {
				return err
			}
		}
	}

	for _, session := range dirty {
		for _, pair := range []struct {
			dimension intern.Dimension
			value     string
		}{
			{intern.Pathname, session.EntryPage},
			{intern.Pathname, session.ExitPage},
			{intern.Hostname, session.EntryHostname},
			{intern.Hostname, session.ExitHostname},
			{intern.Referrer, session.Referrer},
			{intern.Source, session.Source},
			{intern.Channel, session.Channel},
			{intern.UTMSource, session.UTMSource},
			{intern.UTMMedium, session.UTMMedium},
			{intern.UTMCampaign, session.UTMCampaign},
			{intern.Country, session.Country},
			{intern.Region, session.Region},
			{intern.City, session.City},
			{intern.DeviceType, session.DeviceType},
			{intern.ScreenSize, session.ScreenSize},
			{intern.Browser, session.Browser},
			{intern.BrowserVersion, session.BrowserVersion},
			{intern.OS, session.OS},
			{intern.OSVersion, session.OSVersion},
			{intern.Language, session.Language},
		} {
			if err := add(pair.dimension, pair.value); err != nil {
				return err
			}
		}
	}

	return nil
}

// of returns the id for a dimension value. A value that was not interned cannot
// happen — internBatch walks exactly the fields the inserts read — so a miss
// falls back to the empty-string id rather than failing a run over it.
func (w *batchWriter) of(dimension intern.Dimension, value string) int64 {
	if value == "" {
		return intern.EmptyID
	}

	if id, ok := w.ids[dimensionKey{dimension, value}]; ok {
		return id
	}

	return intern.EmptyID
}

// commit writes one batch in a single transaction, sixty-four rows to a
// statement. Statement overhead — the driver round trip, the connection lock,
// the VDBE program — is fixed per statement rather than per row, and at a
// million rows it is most of the run if each row pays it.
func (w *batchWriter) commit(ctx context.Context, dirty []*ingest.Session, merges []ingest.Merge) error {
	tx, err := w.account.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed write: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	// Merges come first, so events written by an earlier batch are pointed at
	// the surviving session before this batch's rows are counted against it.
	for _, merge := range merges {
		if _, err := tx.ExecContext(ctx, "UPDATE events SET session_id = ? WHERE session_id = ?", merge.Survivor, merge.Absorbed); err != nil {
			return fmt.Errorf("seed write: merge sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", merge.Absorbed); err != nil {
			return fmt.Errorf("seed write: merge sessions: %w", err)
		}
	}

	if err := w.writeSessions(ctx, tx, dirty); err != nil {
		return err
	}

	if err := w.writeEvents(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("seed write: commit: %w", err)
	}

	return nil
}

// writeSessions upserts the visits this batch touched.
func (w *batchWriter) writeSessions(ctx context.Context, tx *sql.Tx, dirty []*ingest.Session) error {
	statements := map[int]*sql.Stmt{}

	defer func() {
		for _, stmt := range statements {
			stmt.Close() //nolint:errcheck // closing a statement inside a finished transaction has nothing to report
		}
	}()

	for start := 0; start < len(dirty); start += insertChunk {
		end := min(start+insertChunk, len(dirty))
		chunk := dirty[start:end]

		stmt, err := prepared(ctx, tx, statements, "sessions", sessionColumns, sessionColumnCount, len(chunk), sessionConflict)
		if err != nil {
			return err
		}

		w.args = w.args[:0]

		for _, session := range chunk {
			var props any
			if len(session.EntryProps) > 0 {
				encoded, err := json.Marshal(session.EntryProps)
				if err != nil {
					return fmt.Errorf("seed write: encode entry props: %w", err)
				}
				props = string(encoded)
			}

			w.args = append(w.args,
				session.ID, session.SiteID, session.UserID, session.StartedAt, session.LastSeenAt,
				session.Duration(), boolToInt(session.IsBounce()),
				session.Pageviews, session.Events,
				w.of(intern.Pathname, session.EntryPage), w.of(intern.Pathname, session.ExitPage),
				w.of(intern.Hostname, session.EntryHostname), w.of(intern.Hostname, session.ExitHostname),
				props,
				w.of(intern.Referrer, session.Referrer), w.of(intern.Source, session.Source), w.of(intern.Channel, session.Channel),
				w.of(intern.UTMSource, session.UTMSource), w.of(intern.UTMMedium, session.UTMMedium), w.of(intern.UTMCampaign, session.UTMCampaign),
				w.of(intern.Country, session.Country), w.of(intern.Region, session.Region), w.of(intern.City, session.City),
				w.of(intern.DeviceType, session.DeviceType), w.of(intern.ScreenSize, session.ScreenSize),
				w.of(intern.Browser, session.Browser), w.of(intern.BrowserVersion, session.BrowserVersion),
				w.of(intern.OS, session.OS), w.of(intern.OSVersion, session.OSVersion), w.of(intern.Language, session.Language),
				0,
			)
		}

		if _, err := stmt.ExecContext(ctx, w.args...); err != nil {
			return fmt.Errorf("seed write: upsert sessions: %w", err)
		}
	}

	return nil
}

// writeEvents writes the hot rows and, for the few that have anything to put in
// it, the cold ones. The split is why they are two statements: SQLite reads the
// whole row off disk even for a three-column query, so a props blob in the hot
// table would be dragged through every scan that never looks at it.
func (w *batchWriter) writeEvents(ctx context.Context, tx *sql.Tx) error {
	statements := map[int]*sql.Stmt{}

	defer func() {
		for _, stmt := range statements {
			stmt.Close() //nolint:errcheck // closing a statement inside a finished transaction has nothing to report
		}
	}()

	var details *sql.Stmt

	for start := 0; start < len(w.rows); start += insertChunk {
		end := min(start+insertChunk, len(w.rows))
		chunk := w.rows[start:end]

		stmt, err := prepared(ctx, tx, statements, "events", eventColumns, eventColumnCount, len(chunk), "")
		if err != nil {
			return err
		}

		w.args = w.args[:0]

		for i := range chunk {
			chunk[i].id = w.nextEventID
			w.nextEventID++

			event := chunk[i].event

			w.args = append(w.args,
				chunk[i].id, event.SiteID, event.Timestamp, w.of(intern.EventName, event.Name), event.UserID, chunk[i].sessionID,
				w.of(intern.Hostname, event.Hostname), w.of(intern.Pathname, event.Pathname), w.of(intern.PageTitle, event.PageTitle),
				w.of(intern.Referrer, event.Referrer), w.of(intern.Source, event.Source), w.of(intern.Channel, event.Channel),
				w.of(intern.UTMSource, event.UTMSource), w.of(intern.UTMMedium, event.UTMMedium), w.of(intern.UTMCampaign, event.UTMCampaign),
				w.of(intern.Country, event.Country), w.of(intern.Region, event.Region), w.of(intern.City, event.City),
				w.of(intern.DeviceType, event.DeviceType), w.of(intern.ScreenSize, event.ScreenSize),
				w.of(intern.Browser, event.Browser), w.of(intern.BrowserVersion, event.BrowserVersion),
				w.of(intern.OS, event.OS), w.of(intern.OSVersion, event.OSVersion), w.of(intern.Language, event.Language),
				event.ScrollDepth, event.EngagementTime, w.of(intern.BotReason, event.BotReason), 0, boolToInt(event.HasDetails()),
			)
		}

		if _, err := stmt.ExecContext(ctx, w.args...); err != nil {
			return fmt.Errorf("seed write: insert events: %w", err)
		}

		for _, row := range chunk {
			if !row.event.HasDetails() {
				continue
			}

			if details == nil {
				details, err = tx.PrepareContext(ctx, insertDetailsSQL)
				if err != nil {
					return fmt.Errorf("seed write: prepare event details: %w", err)
				}
				defer details.Close() //nolint:errcheck // same
			}

			if err := insertDetails(ctx, details, row); err != nil {
				return err
			}
		}
	}

	return nil
}

// insertDetails writes one cold row.
func insertDetails(ctx context.Context, stmt *sql.Stmt, row eventRow) error {
	event := row.event

	var props any
	if len(event.Props) > 0 {
		encoded, err := json.Marshal(event.Props)
		if err != nil {
			return fmt.Errorf("seed write: encode props: %w", err)
		}
		props = string(encoded)
	}

	var amount, currency any
	if event.Revenue != nil {
		amount, currency = event.Revenue.Amount, event.Revenue.Currency
	}

	if _, err := stmt.ExecContext(ctx,
		row.id, props, amount, currency,
		nullIfEmpty(event.UTMContent), nullIfEmpty(event.UTMTerm), nullIfEmpty(event.FullURL),
	); err != nil {
		return fmt.Errorf("seed write: insert event details: %w", err)
	}

	return nil
}

// prepared returns the statement for a row count, preparing it the first time
// it is asked for. A batch uses two at most — the full chunk size and whatever
// is left over — so the map never holds more than that.
func prepared(ctx context.Context, tx *sql.Tx, cache map[int]*sql.Stmt, table, columns string, values, rows int, suffix string) (*sql.Stmt, error) {
	if stmt, ok := cache[rows]; ok {
		return stmt, nil
	}

	stmt, err := tx.PrepareContext(ctx, multiRowSQL(table, columns, values, rows, suffix))
	if err != nil {
		return nil, fmt.Errorf("seed write: prepare %s insert: %w", table, err)
	}

	cache[rows] = stmt

	return stmt, nil
}

// seedIDs reads an account's row id high water marks into the allocators.
// Seeding into a database that already holds data has to carry on from where it
// left off, or the first new row would collide with an existing one.
func (w *batchWriter) seedIDs(ctx context.Context) error {
	var sessions, events sql.NullInt64

	if err := w.account.Writer().QueryRowContext(ctx, "SELECT MAX(id) FROM sessions").Scan(&sessions); err != nil {
		return fmt.Errorf("seed session ids: %w", err)
	}

	if err := w.account.Writer().QueryRowContext(ctx, "SELECT MAX(id) FROM events").Scan(&events); err != nil {
		return fmt.Errorf("seed event ids: %w", err)
	}

	w.sessions.SeedIDs(w.account.ID, sessions.Int64)
	w.nextEventID = events.Int64 + 1

	// Memory-mapped I/O is a read optimisation, and on a write this size it is
	// a cost: the mapping is torn down and rebuilt as the file grows, once per
	// few megabytes, for the whole run. The pragma is per connection and this
	// pool holds exactly one, so it applies to the load and to nothing else —
	// the reader the dashboard and the shape report use keeps its mapping.
	if _, err := w.account.Writer().ExecContext(ctx, "PRAGMA mmap_size = 0"); err != nil {
		return fmt.Errorf("seed: disable mmap for the load: %w", err)
	}

	return nil
}

// nullIfEmpty stores NULL rather than an empty string in the cold table, which
// is what the ingest writer does and what the queries against it expect.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}
