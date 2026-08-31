//
// writer.go
// The shard side: dedupe, fold into sessions, and write one transaction per account.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// DedupeRetention is how long a written event id is remembered. Twenty-four
// hours because the realistic redelivery window is seconds to minutes; the
// bound is not about correctness, it is what keeps the index small enough for
// the lookup to stay cheap on the write path.
const DedupeRetention = 24 * time.Hour

// prunePeriod is how often the dedupe table is trimmed. Once a minute is often
// enough that the table never grows past a day's traffic and rare enough that
// the DELETE never shares a transaction with a burst of events.
const prunePeriod = time.Minute

// Writer applies batches to the account databases. It is the shard: everything
// above it deals in HTTP and derived events, and it is the only thing that
// knows a row from a column.
type Writer struct {
	accounts *accounts.Manager
	sessions *SessionCache

	// Now is injectable so a replay test can control the dedupe window and the
	// prune cutoff rather than depending on when the suite runs.
	Now func() time.Time

	// mu guards the per-account state below. Writes to one account are
	// serialised anyway — SQLite allows one writer — so a per-account lock
	// costs nothing and makes the read-then-fold sequence atomic.
	mu    sync.Mutex
	locks map[int64]*accountLock
}

// accountLock is one account's write serialisation and the bookkeeping that has
// to survive a failed transaction.
type accountLock struct {
	mu sync.Mutex

	// seeded records that the session id allocator has read this database's
	// high water mark. Without it two processes — or one process after a
	// restart — would hand out ids that already exist.
	seeded bool

	// pending holds events that were folded into the session cache but whose
	// transaction did not commit. A retry must not fold them a second time, and
	// the database's own dedupe table cannot help because it rolled back with
	// everything else.
	pending map[uuid.UUID]int64

	lastPruned time.Time
}

// NewWriter builds a shard writer over the account manager and a session cache.
func NewWriter(manager *accounts.Manager, sessions *SessionCache) *Writer {
	return &Writer{
		accounts: manager,
		sessions: sessions,
		locks:    map[int64]*accountLock{},
	}
}

// Sessions exposes the cache so that shutdown can persist it and a health check
// can size it.
func (w *Writer) Sessions() *SessionCache {
	return w.sessions
}

// clock returns the writer's time source.
func (w *Writer) clock() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}

	return w.Now().UTC()
}

// lockFor returns an account's serialisation state, creating it on first use.
func (w *Writer) lockFor(accountID int64) *accountLock {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, ok := w.locks[accountID]
	if !ok {
		state = &accountLock{pending: map[uuid.UUID]int64{}}
		w.locks[accountID] = state
	}

	return state
}

// Write applies a batch and returns the ids that are durably committed.
//
// The batch is grouped by account and written as one transaction per account.
// At a thousand events a second spread across accounts that turns a thousand
// individual writes into perhaps fifty to two hundred transactions: un-batched,
// SQLite caps out in the low hundreds of writes per second, and batched under
// synchronous=NORMAL it runs in the tens of thousands of rows per second.
func (w *Writer) Write(ctx context.Context, batch []Event) ([]uuid.UUID, error) {
	if len(batch) == 0 {
		return nil, nil
	}

	byAccount := map[int64][]Event{}
	for i := range batch {
		byAccount[batch[i].AccountID] = append(byAccount[batch[i].AccountID], batch[i])
	}

	var (
		committed []uuid.UUID
		firstErr  error
	)

	for accountID, events := range byAccount {
		ids, err := w.writeAccount(ctx, accountID, events)
		committed = append(committed, ids...)

		// One account failing must not stop the others. Their events are
		// unrelated and are already in memory; abandoning them would turn one
		// full disk into data loss across every customer on the box.
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return committed, firstErr
}

// writeAccount applies one account's events in a single transaction. The
// sequence is deliberate: everything that has to talk to the database on its
// own connection happens before the transaction opens, because the account
// writer is a pool of exactly one connection and a query issued while a
// transaction holds it would wait for a connection that only the transaction
// can release.
func (w *Writer) writeAccount(ctx context.Context, accountID int64, events []Event) ([]uuid.UUID, error) {
	account, err := w.accounts.Open(ctx, accountID)
	if err != nil {
		return nil, err
	}

	state := w.lockFor(accountID)
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.seeded {
		highest, err := highestSessionID(ctx, account.Writer())
		if err != nil {
			return nil, err
		}
		w.sessions.SeedIDs(accountID, highest)
		state.seeded = true
	}

	// The dedupe check runs before the fold, not inside the transaction with
	// it. Holding the account lock is what makes that safe: nothing else can
	// insert an id between the check and the write.
	fresh, duplicates, err := w.partition(ctx, account.Writer(), state, events)
	if err != nil {
		return nil, err
	}

	// A duplicate is committed as far as the sender is concerned. Telling it
	// otherwise would make a lost acknowledgement retry forever.
	committed := make([]uuid.UUID, 0, len(events))
	committed = append(committed, duplicates...)

	if len(fresh) == 0 {
		return committed, nil
	}

	rows := make([]eventRow, 0, len(fresh))

	for i := range fresh {
		event := &fresh[i]

		sessionID, known := state.pending[event.UUID]
		if known {
			rows = append(rows, eventRow{event: event, sessionID: sessionID})
			continue
		}

		session, ok, revived := w.sessions.Apply(event)
		if !ok {
			// An engagement ping with no visit to attach to yet. It is parked
			// rather than lost, and comes back below when its pageview arrives.
			continue
		}

		state.pending[event.UUID] = session.ID
		rows = append(rows, eventRow{event: event, sessionID: session.ID})

		// A ping that arrived before its own pageview was never written. Now
		// that the visit exists, its row is written with everything else — this
		// is where time-on-page and scroll depth stop depending on which order
		// a retry delivered the batch in.
		for _, ping := range revived {
			state.pending[ping.UUID] = session.ID
			rows = append(rows, eventRow{event: ping, sessionID: session.ID})
		}
	}

	dirty := w.sessions.TakeDirty(accountID)
	merges := w.sessions.TakeMerges(accountID)

	// Interning may insert a dimension row, so it has to finish before the
	// transaction takes the single write connection.
	ids, err := internBatch(ctx, account.Intern, rows, dirty)
	if err != nil {
		return nil, err
	}

	if err := w.commit(ctx, account.Writer(), state, rows, dirty, merges, ids); err != nil {
		// The fold already happened in memory. Putting the sessions back in the
		// dirty set and keeping the pending ids is what makes the retry write
		// the same rows rather than folding them a second time.
		w.sessions.Redirty(dirty)
		return committed, err
	}

	for i := range rows {
		delete(state.pending, rows[i].event.UUID)
		committed = append(committed, rows[i].event.UUID)
	}

	return committed, nil
}

// eventRow pairs a derived event with the session it was folded into.
type eventRow struct {
	event     *Event
	sessionID int64
}

// partition splits a batch into events nobody has written and events somebody
// has. It consults the in-memory pending set first because a batch being
// retried after a failed commit has ids the database never saw.
func (w *Writer) partition(ctx context.Context, db *sql.DB, state *accountLock, events []Event) ([]Event, []uuid.UUID, error) {
	seen, err := knownEventIDs(ctx, db, events)
	if err != nil {
		return nil, nil, err
	}

	fresh := make([]Event, 0, len(events))
	var duplicates []uuid.UUID

	// A batch can carry the same id twice on its own, which is exactly what a
	// sender that retried into the middle of a live batch produces.
	inBatch := make(map[uuid.UUID]struct{}, len(events))

	for i := range events {
		id := events[i].UUID

		if _, ok := seen[id]; ok {
			duplicates = append(duplicates, id)
			continue
		}
		if _, ok := inBatch[id]; ok {
			duplicates = append(duplicates, id)
			continue
		}

		inBatch[id] = struct{}{}
		fresh = append(fresh, events[i])
	}

	return fresh, duplicates, nil
}

// knownEventIDs asks the dedupe table which of these ids it already holds. It is
// one query with a bound parameter per id rather than one query per event,
// because a batch is up to a few hundred events and a round trip each would
// undo the point of batching.
func knownEventIDs(ctx context.Context, db *sql.DB, events []Event) (map[uuid.UUID]struct{}, error) {
	if len(events) == 0 {
		return nil, nil
	}

	query := "SELECT event_uuid FROM recent_event_ids WHERE event_uuid IN (?"
	args := make([]any, 0, len(events))
	args = append(args, events[0].UUID[:])

	for i := 1; i < len(events); i++ {
		query += ",?"
		args = append(args, events[i].UUID[:])
	}
	query += ")"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("dedupe lookup: %w", err)
	}
	defer rows.Close()

	seen := map[uuid.UUID]struct{}{}

	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("dedupe lookup: %w", err)
		}

		id, err := uuid.FromBytes(raw)
		if err != nil {
			continue
		}
		seen[id] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dedupe lookup: %w", err)
	}

	return seen, nil
}

// commit writes everything for one account in a single transaction. Either the
// whole batch lands or none of it does, which is what lets the caller retry the
// batch unchanged.
func (w *Writer) commit(ctx context.Context, db *sql.DB, state *accountLock, rows []eventRow, dirty []*Session, merges []Merge, ids *dimensionIDs) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("write batch: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	now := w.clock()

	// Merges come first so that events written by an earlier batch are pointed
	// at the surviving session before this batch's rows are counted against it.
	for _, merge := range merges {
		if _, err := tx.ExecContext(ctx, "UPDATE events SET session_id = ? WHERE session_id = ?", merge.Survivor, merge.Absorbed); err != nil {
			return fmt.Errorf("write batch: merge sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", merge.Absorbed); err != nil {
			return fmt.Errorf("write batch: merge sessions: %w", err)
		}
	}

	for _, session := range dirty {
		if err := upsertSession(ctx, tx, session, ids); err != nil {
			return err
		}
	}

	for _, row := range rows {
		if err := insertEvent(ctx, tx, row, ids); err != nil {
			return err
		}

		// INSERT OR IGNORE rather than a plain INSERT: two senders retrying the
		// same event into two processes is a race the database should absorb,
		// not a constraint error somebody has to handle.
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)",
			row.event.UUID[:], now.Unix(),
		); err != nil {
			return fmt.Errorf("write batch: dedupe: %w", err)
		}
	}

	// Pruning rides along on a batch that is already writing, so it never takes
	// the write lock on its own.
	if now.Sub(state.lastPruned) >= prunePeriod {
		cutoff := now.Add(-DedupeRetention).Unix()
		if _, err := tx.ExecContext(ctx, "DELETE FROM recent_event_ids WHERE received_at < ?", cutoff); err != nil {
			return fmt.Errorf("write batch: prune dedupe: %w", err)
		}
		state.lastPruned = now
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("write batch: commit: %w", err)
	}

	return nil
}

// insertEvent writes one row to the hot table and, when there is something to
// write, one to the cold one. The split is why this is two statements: SQLite
// reads the whole row off disk even for a three-column query, so a props blob
// in the hot table would be dragged through every scan that never looks at it.
func insertEvent(ctx context.Context, tx *sql.Tx, row eventRow, ids *dimensionIDs) error {
	event := row.event

	result, err := tx.ExecContext(ctx, `
		INSERT INTO events (
			site_id, timestamp, name_id, user_id, session_id,
			hostname_id, pathname_id, page_title_id,
			referrer_id, source_id, channel_id, utm_source_id, utm_medium_id, utm_campaign_id,
			country_id, region_id, city_geoname_id,
			device_type_id, screen_size_id, browser_id, browser_version_id,
			os_id, os_version_id, language_id,
			scroll_depth, engagement_time, bot_reason_id, is_imported, has_details
		) VALUES (?,?,?,?,?, ?,?,?, ?,?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?, ?,?,?,?,?)`,
		event.SiteID, event.Timestamp, ids.of(intern.EventName, event.Name), event.UserID, row.sessionID,
		ids.of(intern.Hostname, event.Hostname), ids.of(intern.Pathname, event.Pathname), ids.of(intern.PageTitle, event.PageTitle),
		ids.of(intern.Referrer, event.Referrer), ids.of(intern.Source, event.Source), ids.of(intern.Channel, event.Channel),
		ids.of(intern.UTMSource, event.UTMSource), ids.of(intern.UTMMedium, event.UTMMedium), ids.of(intern.UTMCampaign, event.UTMCampaign),
		ids.of(intern.Country, event.Country), ids.of(intern.Region, event.Region), event.CityGeonameID,
		ids.of(intern.DeviceType, event.DeviceType), ids.of(intern.ScreenSize, event.ScreenSize),
		ids.of(intern.Browser, event.Browser), ids.of(intern.BrowserVersion, event.BrowserVersion),
		ids.of(intern.OS, event.OS), ids.of(intern.OSVersion, event.OSVersion), ids.of(intern.Language, event.Language),
		event.ScrollDepth, event.EngagementTime, ids.of(intern.BotReason, event.BotReason), 0, boolToInt(event.HasDetails()),
	)
	if err != nil {
		return fmt.Errorf("write batch: insert event: %w", err)
	}

	if !event.HasDetails() {
		return nil
	}

	eventID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("write batch: insert event: %w", err)
	}

	var props any
	if len(event.Props) > 0 {
		encoded, err := json.Marshal(event.Props)
		if err != nil {
			return fmt.Errorf("write batch: encode props: %w", err)
		}
		props = string(encoded)
	}

	var amount, currency any
	if event.Revenue != nil {
		amount, currency = event.Revenue.Amount, event.Revenue.Currency
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_details (event_id, props, revenue_amount, revenue_currency, utm_content, utm_term, full_url)
		VALUES (?,?,?,?,?,?,?)`,
		eventID, props, amount, currency,
		nullIfEmpty(event.UTMContent), nullIfEmpty(event.UTMTerm), nullIfEmpty(event.FullURL),
	); err != nil {
		return fmt.Errorf("write batch: insert event details: %w", err)
	}

	return nil
}

// upsertSession writes a session row, updating it in place when it already
// exists. One row per session updated in place is the whole reason this schema
// does not need a sign column and a collapsing merge, and it is why every
// average in the query layer is a plain AVG.
func upsertSession(ctx context.Context, tx *sql.Tx, session *Session, ids *dimensionIDs) error {
	var props any
	if len(session.EntryProps) > 0 {
		encoded, err := json.Marshal(session.EntryProps)
		if err != nil {
			return fmt.Errorf("write batch: encode entry props: %w", err)
		}
		props = string(encoded)
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
			pageviews, events, entry_page_id, exit_page_id, entry_hostname_id, exit_hostname_id,
			entry_props,
			referrer_id, source_id, channel_id, utm_source_id, utm_medium_id, utm_campaign_id,
			country_id, region_id, city_geoname_id,
			device_type_id, screen_size_id, browser_id, browser_version_id,
			os_id, os_version_id, language_id, is_imported
		) VALUES (?,?,?,?,?,?,?, ?,?,?,?,?,?, ?, ?,?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?,?)
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
			city_geoname_id   = excluded.city_geoname_id,
			device_type_id    = excluded.device_type_id,
			screen_size_id    = excluded.screen_size_id,
			browser_id        = excluded.browser_id,
			browser_version_id = excluded.browser_version_id,
			os_id             = excluded.os_id,
			os_version_id     = excluded.os_version_id,
			language_id       = excluded.language_id`,
		session.ID, session.SiteID, session.UserID, session.StartedAt, session.LastSeenAt,
		session.Duration(), boolToInt(session.IsBounce()),
		session.Pageviews, session.Events,
		ids.of(intern.Pathname, session.EntryPage), ids.of(intern.Pathname, session.ExitPage),
		ids.of(intern.Hostname, session.EntryHostname), ids.of(intern.Hostname, session.ExitHostname),
		props,
		ids.of(intern.Referrer, session.Referrer), ids.of(intern.Source, session.Source), ids.of(intern.Channel, session.Channel),
		ids.of(intern.UTMSource, session.UTMSource), ids.of(intern.UTMMedium, session.UTMMedium), ids.of(intern.UTMCampaign, session.UTMCampaign),
		ids.of(intern.Country, session.Country), ids.of(intern.Region, session.Region), session.CityGeonameID,
		ids.of(intern.DeviceType, session.DeviceType), ids.of(intern.ScreenSize, session.ScreenSize),
		ids.of(intern.Browser, session.Browser), ids.of(intern.BrowserVersion, session.BrowserVersion),
		ids.of(intern.OS, session.OS), ids.of(intern.OSVersion, session.OSVersion), ids.of(intern.Language, session.Language),
		0,
	)
	if err != nil {
		return fmt.Errorf("write batch: upsert session: %w", err)
	}

	return nil
}

// highestSessionID reads an account's session id high water mark. It runs once
// per account per process, when the first batch for that account arrives.
func highestSessionID(ctx context.Context, db *sql.DB) (int64, error) {
	var highest sql.NullInt64

	if err := db.QueryRowContext(ctx, "SELECT MAX(id) FROM sessions").Scan(&highest); err != nil {
		return 0, fmt.Errorf("read session high water mark: %w", err)
	}

	return highest.Int64, nil
}

// boolToInt renders a Go bool as the integer SQLite stores. It exists so the
// conversion reads the same at every call site rather than being an inline
// ternary somebody eventually gets backwards.
func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

// nullIfEmpty stores NULL rather than an empty string in the cold table. The
// detail columns are sparse by nature, and a NULL costs a byte where an empty
// string costs a row header entry on every row that does not use the column.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}
