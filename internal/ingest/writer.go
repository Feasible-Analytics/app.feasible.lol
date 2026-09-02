//
// writer.go
// Permanent receipts, transactional session folds, and one commit per account.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// Event receipts in recent_event_ids are permanent despite the table's name. A
// browser can replay a locally retained event at any age, so expiring a UUID
// would eventually turn a lost acknowledgement into a duplicated fact row.

const (
	// MaxRejectedHostnames is the durable cardinality cap per site and UTC day.
	MaxRejectedHostnames = 50

	// OtherRejectedHostname receives distinct hostnames past the cap.
	OtherRejectedHostname = "other"

	// RejectedHostnameRetentionDays bounds the evidence table by UTC days.
	RejectedHostnameRetentionDays = 30
)

const (
	// WriterStageAfterClaim is the first rollback boundary in a write.
	WriterStageAfterClaim = "after_claim"

	// WriterStageAfterRejection follows authoritative hostname evidence.
	WriterStageAfterRejection = "after_rejection"

	// WriterStageBeforeCommit is the final rollback-capable boundary.
	WriterStageBeforeCommit = "before_commit"
)

// UsageRecorder is told what an account actually stored, so the billable volume
// can be counted. It is an interface taking plain integers rather than the
// billing package's own type, because nothing on the ingest path may depend on
// billing — and because this is called after a commit, so an event that was
// never stored can never be billed.
type UsageRecorder interface {
	Record(accountID int64, pageviews, customEvents int64)
}

// Writer applies batches to the app shard's account databases. Everything above it
// deals in HTTP and derived events; this is the durable fact authority.
type Writer struct {
	accounts *accounts.Manager

	// Usage counts the billable volume. It is optional: a self-hosted install
	// has no billing at all, and ingestion must not depend on it existing.
	Usage UsageRecorder

	// Now is injectable so replay and retention tests do not depend on wall time.
	Now func() time.Time

	// Shield holds the country, page and hostname rules. It is evaluated here
	// rather than in the ingest tier because this is where the rule list is the
	// live table: a rule saved in the dashboard applies to the next batch,
	// without a snapshot having to cross a network first.
	Shield ShardShield

	// Counters is where a shielded event is recorded. A drop nobody can see is
	// indistinguishable from traffic that never arrived, and "my numbers went
	// down after I added a rule" has to be answerable.
	Counters *Counters

	// Observer receives only final writer outcomes. The handler records the
	// request details before buffering, but accepted and shard-side drop counts
	// belong here because this is where an event is either committed, blocked
	// by a live shield, or left waiting for a pageview that never arrives.
	Observer Observer

	// Paths applies the site's path cleaning rules before anything is interned,
	// which is what stops dim_pathname growing by a row per request on a site
	// with identifiers in its URLs.
	Paths PathCleaner

	// Failpoint injects deterministic rollback boundaries in tests. Production
	// leaves it nil.
	Failpoint func(stage string) error

	// mu guards the per-account state below. Writes to one account are
	// serialised anyway — SQLite allows one writer — so a per-account lock
	// costs nothing and makes the read-then-fold sequence atomic.
	mu    sync.Mutex
	locks map[int64]*accountLock
}

// accountLock serialises one account's writes inside a process. SQLite provides
// the corresponding arbitration between independent serving processes.
type accountLock struct {
	mu sync.Mutex
}

// NewWriter builds an account writer. Fold state is loaded from and written
// back to each account database inside the write transaction, so the writer
// holds none of it between batches.
func NewWriter(manager *accounts.Manager) *Writer {
	return &Writer{
		accounts: manager,
		locks:    map[int64]*accountLock{},
	}
}

// clock returns the writer's time source.
func (w *Writer) clock() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}

	return w.Now().UTC()
}

// fail invokes a deterministic transaction boundary when a test configured
// one.
func (w *Writer) fail(stage string) error {
	if w.Failpoint == nil {
		return nil
	}
	if err := w.Failpoint(stage); err != nil {
		return fmt.Errorf("write batch: failpoint %s: %w", stage, err)
	}

	return nil
}

// sessionIDRange is one durable reservation from the account allocator.
type sessionIDRange struct {
	next int64
	end  int64
}

// Next returns one reserved identity and fails closed if the batch exhausts
// the range it reserved.
func (r *sessionIDRange) Next() (int64, error) {
	if r.next >= r.end {
		return 0, fmt.Errorf("write batch: reserved session identity range exhausted")
	}

	id := r.next
	r.next++
	return id, nil
}

// reserveSessionIDs atomically advances the shared allocator. Gaps after a
// crash are harmless; reusing an id for a different visitor is not.
func reserveSessionIDs(ctx context.Context, db *sql.DB, count int) (*sessionIDRange, error) {
	if count < 1 {
		return &sessionIDRange{}, nil
	}

	var first int64
	if err := db.QueryRowContext(ctx, `
		UPDATE session_id_allocator
		SET next_id = next_id + ?
		WHERE singleton = 1
		RETURNING next_id - ?`, count, count).Scan(&first); err != nil {
		return nil, fmt.Errorf("write batch: reserve session identities: %w", err)
	}

	return &sessionIDRange{next: first, end: first + int64(count)}, nil
}

// lockFor returns an account's serialisation state, creating it on first use.
func (w *Writer) lockFor(accountID int64) *accountLock {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, ok := w.locks[accountID]
	if !ok {
		state = &accountLock{}
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
// synchronous=FULL it remains efficient while making the 202 durability
// boundary explicit.
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
		ids, err := w.writeAccountDurable(ctx, accountID, events)
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

// writeAccountDurable applies one account batch with SQLite as the authority
// for UUID ownership, live fold state, and pre-pageview engagement ownership.
func (w *Writer) writeAccountDurable(ctx context.Context, accountID int64, events []Event) ([]uuid.UUID, error) {
	lease, err := w.accounts.Acquire(ctx, accountID)
	if errors.Is(err, accounts.ErrDeleted) {
		// A buffered event can carry a route captured before deletion. Treat the
		// tombstone as an intentional drop so the browser drains instead of
		// retrying forever or recreating deleted account data.
		committed := make([]uuid.UUID, 0, len(events))
		for _, event := range events {
			committed = append(committed, event.UUID)
		}
		return committed, nil
	}
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the write result is more useful than an unlock error
	account := lease.Account

	state := w.lockFor(accountID)
	state.mu.Lock()
	defer state.mu.Unlock()

	identities, err := reserveSessionIDs(ctx, account.Writer(), len(events))
	if err != nil {
		return nil, err
	}

	tx, err := account.Writer().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("write batch: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	fresh, duplicates, err := w.claimEvents(ctx, tx, events)
	if err != nil {
		return nil, err
	}
	if err := w.fail(WriterStageAfterClaim); err != nil {
		return nil, err
	}

	fresh, shielded := w.applyShieldDurable(fresh)
	if err := persistHostnameRejections(ctx, tx, shielded, w.clock()); err != nil {
		return nil, err
	}
	if err := w.fail(WriterStageAfterRejection); err != nil {
		return nil, err
	}
	w.cleanPaths(fresh)

	committed := append([]uuid.UUID(nil), duplicates...)
	if len(fresh) == 0 && len(shielded) == 0 {
		return committed, nil
	}

	// Pruning happens before the fold loads anything, so it can only ever
	// remove state that predates this batch rather than state this transaction
	// is about to write.
	expired, err := w.pruneFoldState(ctx, tx, events)
	if err != nil {
		return committed, err
	}

	fold := newDurableSessionCache()
	for key, span := range durableFoldRanges(fresh) {
		if err := loadDurableFoldKey(ctx, tx, fold, accountID, key, span.first, span.last); err != nil {
			return committed, err
		}
	}

	rows := make([]eventRow, 0, len(fresh))
	batchRows := make(map[uuid.UUID]struct{}, len(fresh))
	var orphaned []uuid.UUID
	var adopted []uuid.UUID

	for i := range fresh {
		event := &fresh[i]
		session, ok, revived, err := fold.ApplyAllocated(event, identities.Next)
		if err != nil {
			return committed, err
		}
		if !ok {
			if err := persistDurableOrphan(ctx, tx, event); err != nil {
				return committed, err
			}
			orphaned = append(orphaned, event.UUID)
			continue
		}

		batchRows[event.UUID] = struct{}{}
		rows = append(rows, eventRow{event: event, sessionID: session.ID})
		for _, ping := range revived {
			rows = append(rows, eventRow{event: ping, sessionID: session.ID})
			adopted = append(adopted, ping.UUID)
		}
	}

	dirty := fold.TakeDirty(accountID)
	merges := fold.TakeMerges(accountID)
	sessions := sessionsByID(dirty)
	for _, row := range rows {
		if session, ok := sessions[row.sessionID]; ok {
			session.stamp(row.event)
		}
	}

	cacheTx := account.Intern.BeginTransaction(tx)
	defer cacheTx.Rollback()

	ids, err := internBatch(ctx, cacheTx, rows, dirty)
	if err != nil {
		return committed, err
	}
	if err := persistDurableFoldState(ctx, tx, dirty, merges, adopted); err != nil {
		return committed, err
	}
	if err := w.commitDurable(ctx, tx, rows, dirty, merges, ids); err != nil {
		return committed, err
	}
	cacheTx.Commit()

	// A ping whose pageview never came is a genuine drop, and by now its 202
	// went out over an hour ago — so this counter is the only place the
	// customer ever hears about it.
	for i := range expired {
		if w.Counters != nil {
			w.Counters.Dropped(expired[i].SiteID, ReasonNoSessionForEngage)
		}
		w.observe(&expired[i], false, ReasonNoSessionForEngage)
	}

	for _, blocked := range shielded {
		committed = append(committed, blocked.id)
		if w.Counters != nil {
			w.Counters.Dropped(blocked.siteID, blocked.reason)
		}
		w.observe(&blocked.event, false, blocked.reason)
	}
	committed = append(committed, orphaned...)
	for _, row := range rows {
		if _, belongsToBatch := batchRows[row.event.UUID]; belongsToBatch {
			committed = append(committed, row.event.UUID)
		}
		w.observe(row.event, true, row.event.BotReason)
	}

	w.recordUsage(accountID, rows)
	return committed, nil
}

// durableFoldRange is the complete timestamp window one visitor key needs in
// a batch.
type durableFoldRange struct {
	first int64
	last  int64
}

// durableFoldRanges groups current and previous-day visitor identities before
// the transaction folds anything.
func durableFoldRanges(events []Event) map[sessionKey]durableFoldRange {
	ranges := make(map[sessionKey]durableFoldRange)

	for i := range events {
		event := &events[i]
		keys := []sessionKey{{siteID: event.SiteID, userID: event.UserID}}
		if event.PreviousUserID != 0 && event.PreviousUserID != event.UserID {
			keys = append(keys, sessionKey{siteID: event.SiteID, userID: event.PreviousUserID})
		}

		for _, key := range keys {
			span, ok := ranges[key]
			if !ok {
				ranges[key] = durableFoldRange{first: event.Timestamp, last: event.Timestamp}
				continue
			}
			if event.Timestamp < span.first {
				span.first = event.Timestamp
			}
			if event.Timestamp > span.last {
				span.last = event.Timestamp
			}
			ranges[key] = span
		}
	}

	return ranges
}

// loadDurableFoldKey restores serialized fold state and durable orphan events
// into a transaction-local cache.
func loadDurableFoldKey(ctx context.Context, tx *sql.Tx, cache *SessionCache, accountID int64, key sessionKey, first, last int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT payload FROM ingest_session_state
		WHERE site_id = ? AND user_id = ?
		  AND started_at <= ? AND last_seen_at >= ?
		ORDER BY started_at`,
		key.siteID, key.userID, last+sessionTimeoutSeconds, first-sessionTimeoutSeconds)
	if err != nil {
		return fmt.Errorf("write batch: read durable sessions: %w", err)
	}

	bucket := &cache.bucket
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("write batch: read durable session: %w", err)
		}

		var session Session
		if err := json.Unmarshal(payload, &session); err != nil {
			_ = rows.Close()
			return fmt.Errorf("write batch: decode durable session: %w", err)
		}
		session.AccountID = accountID
		bucket.sessions[key] = append(bucket.sessions[key], &session)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("write batch: read durable sessions: %w", err)
	}
	if err := loadLegacyFoldKey(ctx, tx, cache, accountID, key, first, last); err != nil {
		return err
	}

	orphans, err := tx.QueryContext(ctx, `
		SELECT payload FROM ingest_orphan_engagements
		WHERE site_id = ? AND user_id = ?
		  AND timestamp BETWEEN ? AND ?
		ORDER BY timestamp`,
		key.siteID, key.userID, first-sessionTimeoutSeconds, last+sessionTimeoutSeconds)
	if err != nil {
		return fmt.Errorf("write batch: read durable orphans: %w", err)
	}
	defer func() { _ = orphans.Close() }()

	for orphans.Next() {
		var payload []byte
		if err := orphans.Scan(&payload); err != nil {
			return fmt.Errorf("write batch: read durable orphan: %w", err)
		}
		event, err := decodeDurableEvent(payload)
		if err != nil {
			return err
		}
		copied := event
		bucket.orphans[key] = append(bucket.orphans[key], &copied)
	}
	if err := orphans.Err(); err != nil {
		return fmt.Errorf("write batch: read durable orphans: %w", err)
	}

	return nil
}

// loadLegacyFoldKey hydrates session rows that have no companion state row:
// sessions written before the state table existed, or whose state was pruned
// after the retention window. The hydration is approximate — the tie-break
// keys are not recoverable from the row — and the next successful fold writes
// complete durable state again.
func loadLegacyFoldKey(ctx context.Context, tx *sql.Tx, cache *SessionCache, accountID int64, key sessionKey, first, last int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT
			s.id, s.started_at, s.last_seen_at, s.pageviews, s.events, s.is_bounce,
			COALESCE(entry_page.value, ''), COALESCE(exit_page.value, ''),
			COALESCE(entry_host.value, ''), COALESCE(exit_host.value, ''), s.entry_props,
			COALESCE(referrer.value, ''), COALESCE(source.value, ''), COALESCE(channel.value, ''),
			COALESCE(utm_source.value, ''), COALESCE(utm_medium.value, ''), COALESCE(utm_campaign.value, ''),
			COALESCE(country.value, ''), COALESCE(region.value, ''), COALESCE(city.value, ''),
			COALESCE(device.value, ''), COALESCE(screen.value, ''),
			COALESCE(browser.value, ''), COALESCE(browser_version.value, ''),
			COALESCE(os.value, ''), COALESCE(os_version.value, ''), COALESCE(language.value, ''),
			(SELECT MIN(e.timestamp) FROM events e JOIN dim_event_name n ON n.id = e.name_id
			 WHERE e.session_id = s.id AND n.value = 'pageview'),
			(SELECT MAX(e.timestamp) FROM events e JOIN dim_event_name n ON n.id = e.name_id
			 WHERE e.session_id = s.id AND n.value = 'pageview')
		FROM sessions s
		LEFT JOIN ingest_session_state state ON state.session_id = s.id
		LEFT JOIN dim_pathname entry_page ON entry_page.id = s.entry_page_id
		LEFT JOIN dim_pathname exit_page ON exit_page.id = s.exit_page_id
		LEFT JOIN dim_hostname entry_host ON entry_host.id = s.entry_hostname_id
		LEFT JOIN dim_hostname exit_host ON exit_host.id = s.exit_hostname_id
		LEFT JOIN dim_referrer referrer ON referrer.id = s.referrer_id
		LEFT JOIN dim_source source ON source.id = s.source_id
		LEFT JOIN dim_channel channel ON channel.id = s.channel_id
		LEFT JOIN dim_utm_source utm_source ON utm_source.id = s.utm_source_id
		LEFT JOIN dim_utm_medium utm_medium ON utm_medium.id = s.utm_medium_id
		LEFT JOIN dim_utm_campaign utm_campaign ON utm_campaign.id = s.utm_campaign_id
		LEFT JOIN dim_country country ON country.id = s.country_id
		LEFT JOIN dim_region region ON region.id = s.region_id
		LEFT JOIN dim_city city ON city.id = s.city_id
		LEFT JOIN dim_device_type device ON device.id = s.device_type_id
		LEFT JOIN dim_screen_size screen ON screen.id = s.screen_size_id
		LEFT JOIN dim_browser browser ON browser.id = s.browser_id
		LEFT JOIN dim_browser_version browser_version ON browser_version.id = s.browser_version_id
		LEFT JOIN dim_os os ON os.id = s.os_id
		LEFT JOIN dim_os_version os_version ON os_version.id = s.os_version_id
		LEFT JOIN dim_language language ON language.id = s.language_id
		WHERE state.session_id IS NULL AND s.site_id = ? AND s.user_id = ?
		  AND s.started_at <= ? AND s.last_seen_at >= ?
		ORDER BY s.started_at`,
		key.siteID, key.userID, last+sessionTimeoutSeconds, first-sessionTimeoutSeconds)
	if err != nil {
		return fmt.Errorf("write batch: read legacy sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	bucket := &cache.bucket

	for rows.Next() {
		var (
			session             Session
			bounce              int
			props               sql.NullString
			firstPage, lastPage sql.NullInt64
		)
		if err := rows.Scan(
			&session.ID, &session.StartedAt, &session.LastSeenAt,
			&session.Pageviews, &session.Events, &bounce,
			&session.EntryPage, &session.ExitPage, &session.EntryHostname, &session.ExitHostname, &props,
			&session.Referrer, &session.Source, &session.Channel,
			&session.UTMSource, &session.UTMMedium, &session.UTMCampaign,
			&session.Country, &session.Region, &session.City,
			&session.DeviceType, &session.ScreenSize, &session.Browser, &session.BrowserVersion,
			&session.OS, &session.OSVersion, &session.Language,
			&firstPage, &lastPage,
		); err != nil {
			return fmt.Errorf("write batch: read legacy session: %w", err)
		}

		session.AccountID = accountID
		session.SiteID = key.siteID
		session.UserID = key.userID
		session.InteractiveNonPageview = bounce == 0 && session.Pageviews < 2
		session.FirstAt = session.StartedAt
		session.EntryAt = maxInt64
		session.ExitAt = minInt64
		if firstPage.Valid {
			session.FirstAt = firstPage.Int64
			session.FirstIsPageview = true
			session.EntryAt = firstPage.Int64
		}
		if lastPage.Valid {
			session.ExitAt = lastPage.Int64
		}
		if props.Valid && props.String != "" {
			if err := json.Unmarshal([]byte(props.String), &session.EntryProps); err != nil {
				return fmt.Errorf("write batch: decode legacy session props: %w", err)
			}
		}

		bucket.sessions[key] = append(bucket.sessions[key], &session)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("write batch: read legacy sessions: %w", err)
	}

	return nil
}

// foldStateRetention is how long a visit's fold state and its unadopted pings
// outlive the visit. It is two UTC days because the fold is keyed on the daily
// visitor fingerprint and the lookup reaches exactly one salt day back: state
// older than that can never be found again, because no arriving event can
// derive the identity it is filed under. Anything shorter would throw away
// state a later event could still have joined, which is a lost visit and a
// silent one — the next event simply starts a new session.
//
// Past the window a very late event hydrates the session approximately from
// the sessions table, and a parked ping is a drop with a reason.
const foldStateRetention = 48 * time.Hour

// pruneFoldState removes fold state past the retention window for the sites in
// a batch and returns the parked pings that will now never find their pageview,
// so they can be reported after commit. Without this both tables grow for the
// life of the account, one row per visit.
//
// The cutoff trails the batch's own oldest event as well as the clock. A batch
// replayed from the outbox carries timestamps the wall clock has long passed,
// and measuring from the clock alone would delete the fold state of the very
// visits that batch is still writing — which is data loss, and silent, because
// the next event for those visitors would simply start a new session.
func (w *Writer) pruneFoldState(ctx context.Context, tx *sql.Tx, events []Event) ([]Event, error) {
	cutoff := w.clock().Unix()
	for i := range events {
		if events[i].Timestamp < cutoff {
			cutoff = events[i].Timestamp
		}
	}
	cutoff -= int64(foldStateRetention / time.Second)

	pruned := map[int64]struct{}{}

	var expired []Event
	for i := range events {
		siteID := events[i].SiteID
		if _, done := pruned[siteID]; done {
			continue
		}
		pruned[siteID] = struct{}{}

		rows, err := tx.QueryContext(ctx, `
			SELECT payload FROM ingest_orphan_engagements
			WHERE site_id = ? AND timestamp < ?`, siteID, cutoff)
		if err != nil {
			return nil, fmt.Errorf("write batch: read expired orphans: %w", err)
		}
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("write batch: read expired orphan: %w", err)
			}
			event, err := decodeDurableEvent(payload)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			expired = append(expired, event)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("write batch: read expired orphans: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			"DELETE FROM ingest_orphan_engagements WHERE site_id = ? AND timestamp < ?", siteID, cutoff); err != nil {
			return nil, fmt.Errorf("write batch: prune expired orphans: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM ingest_session_state WHERE site_id = ? AND last_seen_at < ?", siteID, cutoff); err != nil {
			return nil, fmt.Errorf("write batch: prune fold state: %w", err)
		}
	}

	return expired, nil
}

// persistDurableOrphan stores an engagement event before acknowledging it.
func persistDurableOrphan(ctx context.Context, tx *sql.Tx, event *Event) error {
	payload, err := encodeDurableEvent(event)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO ingest_orphan_engagements
			(event_uuid, site_id, user_id, timestamp, payload)
		VALUES (?, ?, ?, ?, ?)`, event.UUID[:], event.SiteID, event.UserID, event.Timestamp, payload); err != nil {
		return fmt.Errorf("write batch: store durable orphan: %w", err)
	}

	return nil
}

// encodeDurableEvent serializes an orphan for the account database.
func encodeDurableEvent(event *Event) ([]byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("write batch: encode durable orphan: %w", err)
	}

	return payload, nil
}

// decodeDurableEvent restores an orphan from the account database.
func decodeDurableEvent(payload []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return Event{}, fmt.Errorf("write batch: decode durable orphan: %w", err)
	}

	return event, nil
}

// persistDurableFoldState writes changed sessions, removes absorbed state, and
// deletes adopted orphans in the same transaction as their fact rows.
func persistDurableFoldState(ctx context.Context, tx *sql.Tx, sessions []*Session, merges []Merge, adopted []uuid.UUID) error {
	for _, session := range sessions {
		payload, err := json.Marshal(session)
		if err != nil {
			return fmt.Errorf("write batch: encode durable session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ingest_session_state
				(session_id, site_id, user_id, started_at, last_seen_at, payload)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				site_id = excluded.site_id,
				user_id = excluded.user_id,
				started_at = excluded.started_at,
				last_seen_at = excluded.last_seen_at,
				payload = excluded.payload`,
			session.ID, session.SiteID, session.UserID, session.StartedAt, session.LastSeenAt, payload); err != nil {
			return fmt.Errorf("write batch: store durable session: %w", err)
		}
	}

	for _, merge := range merges {
		if _, err := tx.ExecContext(ctx, "DELETE FROM ingest_session_state WHERE session_id = ?", merge.Absorbed); err != nil {
			return fmt.Errorf("write batch: delete absorbed durable session: %w", err)
		}
	}
	for _, id := range adopted {
		if _, err := tx.ExecContext(ctx, "DELETE FROM ingest_orphan_engagements WHERE event_uuid = ?", id[:]); err != nil {
			return fmt.Errorf("write batch: delete adopted durable orphan: %w", err)
		}
	}

	return nil
}

// recordUsage tells the billing counter what this account just stored.
//
// The rule it implements is the whole of what we bill for: a pageview or a
// custom event counts, and an engagement ping does not. An engagement ping is
// the tracker's own heartbeat — it carries time on page and scroll depth and is
// sent whether the visitor asked for it or not — so billing for it would charge
// people for a feature they cannot turn off.
func (w *Writer) recordUsage(accountID int64, rows []eventRow) {
	if w.Usage == nil || len(rows) == 0 {
		return
	}

	var pageviews, customEvents int64

	for _, row := range rows {
		switch {
		case row.event.IsPageview():
			pageviews++
		case row.event.IsEngagement():
			// Deliberately nothing.
		default:
			customEvents++
		}
	}

	w.Usage.Record(accountID, pageviews, customEvents)
}

// applyShieldDurable retains enough information to record authoritative
// hostname evidence and emit counters only after commit.
func (w *Writer) applyShieldDurable(events []Event) ([]Event, []shieldedEvent) {
	kept := make([]Event, 0, len(events))
	var blocked []shieldedEvent

	for i := range events {
		event := &events[i]
		if w.Shield == nil {
			kept = append(kept, *event)
			continue
		}

		allowed, reason := w.Shield.Allowed(event.SiteID, event.Hostname, event.Pathname, event.Country)
		if allowed {
			kept = append(kept, *event)
			continue
		}

		blocked = append(blocked, shieldedEvent{
			id: event.UUID, siteID: event.SiteID, hostname: event.Hostname, reason: reason, event: *event,
		})
	}

	return kept, blocked
}

// shieldedEvent is a claimed rejection whose counter is emitted after commit.
type shieldedEvent struct {
	id       uuid.UUID
	siteID   int64
	hostname string
	reason   string
	event    Event
}

// persistHostnameRejections writes rejection evidence inside the UUID claim
// transaction, so a replay creates both the claim and exactly one count.
func persistHostnameRejections(ctx context.Context, tx *sql.Tx, blocked []shieldedEvent, now time.Time) error {
	day := now.UTC().Unix() / 86400
	wrote := false

	for _, event := range blocked {
		if event.reason != ReasonHostnameNotAllowed {
			continue
		}

		hostname := strings.ToLower(strings.TrimSpace(event.hostname))
		hostname = strings.TrimPrefix(hostname, "www.")
		hostname = strings.TrimSuffix(hostname, ".")
		if hostname == "" || hostname == NoneHostname {
			hostname = OtherRejectedHostname
		}

		if hostname != OtherRejectedHostname {
			var exists int
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM hostname_rejections
					WHERE site_id = ? AND day = ? AND hostname = ?
				)`, event.siteID, day, hostname).Scan(&exists); err != nil {
				return fmt.Errorf("write batch: read hostname rejection: %w", err)
			}

			if exists == 0 {
				var distinct int
				if err := tx.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM hostname_rejections
					WHERE site_id = ? AND day = ? AND hostname <> ?`,
					event.siteID, day, OtherRejectedHostname).Scan(&distinct); err != nil {
					return fmt.Errorf("write batch: count hostname rejections: %w", err)
				}
				if distinct >= MaxRejectedHostnames {
					hostname = OtherRejectedHostname
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO hostname_rejections (site_id, hostname, day, events)
			VALUES (?, ?, ?, 1)
			ON CONFLICT (site_id, day, hostname)
			DO UPDATE SET events = events + 1`, event.siteID, hostname, day); err != nil {
			return fmt.Errorf("write batch: record hostname rejection: %w", err)
		}
		wrote = true
	}

	if wrote {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM hostname_rejections WHERE day < ?", day-RejectedHostnameRetentionDays); err != nil {
			return fmt.Errorf("write batch: prune hostname rejections: %w", err)
		}
	}

	return nil
}

// observe records a final outcome without recreating request-only details the
// handler has already supplied. The event timestamp keeps the outcome in the
// same health bucket as the request even when a buffered write or orphan sweep
// finishes later.
func (w *Writer) observe(event *Event, accepted bool, reason string) {
	if w.Observer == nil || event == nil {
		return
	}

	w.Observer.Observe(Observation{
		SiteID:      event.SiteID,
		AccountID:   event.AccountID,
		ReceivedAt:  event.Timestamp,
		DropReason:  reason,
		Accepted:    accepted,
		OutcomeOnly: true,
	})
}

// cleanPaths rewrites a batch's paths in place. It is a no-op for a site with
// no rules, which is the overwhelming majority, and the check is one map read
// inside the cleaner rather than a database call here.
func (w *Writer) cleanPaths(events []Event) {
	if w.Paths == nil {
		return
	}

	for i := range events {
		events[i].Pathname = w.Paths.Clean(events[i].SiteID, events[i].Pathname)
	}
}

// eventRow pairs a derived event with the session it was folded into.
type eventRow struct {
	event     *Event
	sessionID int64
}

// claimEvents atomically claims each distinct UUID before analytics data is
// inserted. SQLite arbitrates independent writers through the primary key.
func (w *Writer) claimEvents(ctx context.Context, tx *sql.Tx, events []Event) ([]Event, []uuid.UUID, error) {
	now := w.clock().Unix()
	fresh := make([]Event, 0, len(events))
	duplicates := make([]uuid.UUID, 0)
	seen := make(map[uuid.UUID]struct{}, len(events))

	for i := range events {
		id := events[i].UUID
		if _, repeated := seen[id]; repeated {
			continue
		}
		seen[id] = struct{}{}

		claimed, err := claimEventID(ctx, tx, id, now)
		if err != nil {
			return nil, nil, err
		}
		if !claimed {
			duplicates = append(duplicates, id)
			continue
		}

		fresh = append(fresh, events[i])
	}

	return fresh, duplicates, nil
}

// claimEventID inserts one permanent receipt in the fact transaction.
func claimEventID(ctx context.Context, tx *sql.Tx, id uuid.UUID, now int64) (bool, error) {
	result, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO recent_event_ids (event_uuid, received_at) VALUES (?, ?)",
		id[:], now,
	)
	if err != nil {
		return false, fmt.Errorf("write batch: claim event: %w", err)
	}

	claimed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("write batch: claim event: %w", err)
	}

	return claimed == 1, nil
}

// commitDurable writes facts and fold repairs through the transaction that
// already owns the permanent UUID receipts.
func (w *Writer) commitDurable(ctx context.Context, tx *sql.Tx, rows []eventRow, dirty []*Session, merges []Merge, ids *dimensionIDs) error {
	var sessions map[int64]*Session
	if len(merges) > 0 {
		sessions = sessionsByID(dirty)
	}

	for _, merge := range merges {
		update := "UPDATE events SET session_id = ? WHERE session_id = ?"
		args := []any{merge.Survivor, merge.Absorbed}
		if survivor, ok := sessions[merge.Survivor]; ok {
			update = "UPDATE events SET session_id = ?, " + sessionStampSet + " WHERE session_id = ?"
			args = append([]any{merge.Survivor}, append(sessionStampArgs(survivor, ids), merge.Absorbed)...)
		}

		if _, err := tx.ExecContext(ctx, update, args...); err != nil {
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
		if !session.Restamp {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE events SET "+sessionStampSet+" WHERE session_id = ?",
			append(sessionStampArgs(session, ids), session.ID)...,
		); err != nil {
			return fmt.Errorf("write batch: restamp events: %w", err)
		}
	}

	for _, row := range rows {
		if err := insertEvent(ctx, tx, row, ids); err != nil {
			return err
		}
	}

	if err := w.fail(WriterStageBeforeCommit); err != nil {
		return err
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
			country_id, region_id, city_id,
			device_type_id, screen_size_id, browser_id, browser_version_id,
			os_id, os_version_id, language_id,
			scroll_depth, engagement_time, bot_reason_id, is_imported, has_details
		) VALUES (?,?,?,?,?, ?,?,?, ?,?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?, ?,?,?,?,?)`,
		event.SiteID, event.Timestamp, ids.of(intern.EventName, event.Name), event.UserID, row.sessionID,
		ids.of(intern.Hostname, event.Hostname), ids.of(intern.Pathname, event.Pathname), ids.of(intern.PageTitle, event.PageTitle),
		ids.of(intern.Referrer, event.Referrer), ids.of(intern.Source, event.Source), ids.of(intern.Channel, event.Channel),
		ids.of(intern.UTMSource, event.UTMSource), ids.of(intern.UTMMedium, event.UTMMedium), ids.of(intern.UTMCampaign, event.UTMCampaign),
		ids.of(intern.Country, event.Country), ids.of(intern.Region, event.Region), ids.of(intern.City, event.City),
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
		INSERT INTO event_details (event_id, props, revenue_amount, revenue_currency, utm_content, utm_term)
		VALUES (?,?,?,?,?,?)`,
		eventID, props, amount, currency,
		nullIfEmpty(event.UTMContent), nullIfEmpty(event.UTMTerm),
	); err != nil {
		return fmt.Errorf("write batch: insert event details: %w", err)
	}

	return nil
}

// sessionStampSet assigns a session's acquisition, geo and device block to its
// event rows. It is written once because both repairs need it — the merge that
// repoints events at a surviving session, and the restamp after a late event
// changed where a visit came from — and two copies of a sixteen-column
// assignment list is two chances for one of them to forget a column and leave
// a breakdown that does not add up.
const sessionStampSet = `referrer_id = ?, source_id = ?, channel_id = ?, ` +
	`utm_source_id = ?, utm_medium_id = ?, utm_campaign_id = ?, ` +
	`country_id = ?, region_id = ?, city_id = ?, ` +
	`device_type_id = ?, screen_size_id = ?, browser_id = ?, browser_version_id = ?, ` +
	`os_id = ?, os_version_id = ?, language_id = ?`

// sessionStampArgs are the bound values for sessionStampSet, in its order. The
// ids come from the batch's interning, so every value one of these statements
// writes already has a dimension row.
func sessionStampArgs(session *Session, ids *dimensionIDs) []any {
	return []any{
		ids.of(intern.Referrer, session.Referrer),
		ids.of(intern.Source, session.Source),
		ids.of(intern.Channel, session.Channel),
		ids.of(intern.UTMSource, session.UTMSource),
		ids.of(intern.UTMMedium, session.UTMMedium),
		ids.of(intern.UTMCampaign, session.UTMCampaign),
		ids.of(intern.Country, session.Country),
		ids.of(intern.Region, session.Region),
		ids.of(intern.City, session.City),
		ids.of(intern.DeviceType, session.DeviceType),
		ids.of(intern.ScreenSize, session.ScreenSize),
		ids.of(intern.Browser, session.Browser),
		ids.of(intern.BrowserVersion, session.BrowserVersion),
		ids.of(intern.OS, session.OS),
		ids.of(intern.OSVersion, session.OSVersion),
		ids.of(intern.Language, session.Language),
	}
}

// sessionsByID indexes a batch's dirty sessions so an event row can find the
// visit it belongs to. The snapshots are the sessions as they stand after every
// fold in the batch, which is why they are the only correct thing to stamp an
// event from.
func sessionsByID(dirty []*Session) map[int64]*Session {
	byID := make(map[int64]*Session, len(dirty))

	for _, session := range dirty {
		byID[session.ID] = session
	}

	return byID
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
			country_id, region_id, city_id,
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
			city_id           = excluded.city_id,
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
		ids.of(intern.Country, session.Country), ids.of(intern.Region, session.Region), ids.of(intern.City, session.City),
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
