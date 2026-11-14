//
// outbox.go
// The ingester-owned durable SQLite queue and per-shard delivery workers.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

const (
	outboxBatchSize       = 500
	outboxCatchUpBatch    = 2000
	outboxCatchUpAfter    = 5 * time.Minute
	outboxPoll            = 250 * time.Millisecond
	outboxMaxAttempts     = 20
	outboxUnroutedMaxRows = 100000
	outboxUnroutedMaxByte = 50 * 1024 * 1024
)

// outboxFailureReportEvery is how many consecutive failing ticks pass between
// repeat reports of the same broken destination. The first failure is reported
// immediately; at the poll interval this is then roughly one line a minute,
// which is often enough that a stuck queue is obvious in a log tail and rare
// enough that a night-long outage does not bury everything else.
const outboxFailureReportEvery = 240

// outboxAccountHoldMax is the longest an account whose writes keep failing
// waits between tries. Each try costs the shard's lock wait on top of the
// batch it rides in, so a broken account is tried rarely, but often enough
// that its backlog starts draining within minutes of it recovering.
const outboxAccountHoldMax = 5 * time.Minute

// outboxAccountReportEvery is how many consecutive failed tries pass between
// repeat reports of one broken account: about an hour once its hold is at the
// cap.
const outboxAccountReportEvery = 12

// Outbox owns every event after the public 202 and before the app shard's
// commit acknowledgment.
type Outbox struct {
	DB     *sql.DB
	Router *RemoteRouter
	Shards []string
	Client *http.Client
	Signer *InternalSigner
	Now    func() time.Time

	// Log is how a stuck queue becomes visible. Store and forward hides failure
	// by design — the client already has its 202 — so without this an ingester
	// that can reach no shard at all accepts events forever and says nothing.
	Log *logger.Logger

	// holds are the accounts whose writes the shard refused, keyed by shard and
	// account. They live in memory: after a restart a broken account costs one
	// more try before it is held again.
	holdsMu sync.Mutex
	holds   map[accountKey]*accountHold

	closeOnce sync.Once
}

// accountKey names one account on one destination shard.
type accountKey struct {
	shard   int
	account int64
}

// accountHold is one account's run of failed tries and when it may be tried
// again.
type accountHold struct {
	failures int
	until    time.Time
}

// deliveryState tracks one loop's run of consecutive failures, so a broken
// destination is reported when it breaks and when it recovers rather than on
// every tick.
type deliveryState struct {
	failures int
}

// OpenOutbox opens the ingester's private database and creates its queue
// tables. It does not use application migrations because this database belongs
// only to the ingest role and must be recoverable without system.db.
func OpenOutbox(ctx context.Context, path string, shards []string, signer *InternalSigner) (*Outbox, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	outbox := &Outbox{DB: db, Shards: shards, Signer: signer}
	if err := outbox.createSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	router, err := NewRemoteRouter(ctx, db, shards, signer)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	outbox.Router = router

	return outbox, nil
}

// Send durably appends a derived batch. Returning every UUID means the public
// handler may issue 202; it does not claim the destination app has received it.
func (o *Outbox) Send(ctx context.Context, _ int, batch []Event) ([]uuid.UUID, error) {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("outbox: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var unroutedRows, unroutedBytes int64
	for _, event := range batch {
		if event.Shard >= 0 {
			continue
		}
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*), COALESCE(SUM(LENGTH(payload)), 0) FROM outbox WHERE shard_id = -1").Scan(&unroutedRows, &unroutedBytes); err != nil {
			return nil, fmt.Errorf("outbox: measure unrouted queue: %w", err)
		}
		break
	}

	committed := make([]uuid.UUID, 0, len(batch))
	for _, event := range batch {
		payload, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("outbox: encode %s: %w", event.UUID, err)
		}
		if event.Shard < 0 {
			if unroutedRows >= outboxUnroutedMaxRows || unroutedBytes+int64(len(payload)) > outboxUnroutedMaxByte {
				return nil, fmt.Errorf("outbox: unrouted capacity reached; retry after every app shard is reachable")
			}
			unroutedRows++
			unroutedBytes += int64(len(payload))
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox (shard_id, account_id, event_uuid, domain, payload, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(event_uuid) DO NOTHING`,
			event.Shard, event.AccountID, event.UUID.String(), event.Domain, payload, o.clock().Unix()); err != nil {
			return committed, fmt.Errorf("outbox: append %s: %w", event.UUID, err)
		}
		committed = append(committed, event.UUID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("outbox: commit: %w", err)
	}

	return committed, nil
}

// Run starts one independent sender per shard plus the resolver for domains
// held while the merged routing map is incomplete.
func (o *Outbox) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for shard := range o.Shards {
		shard := shard
		workers.Add(1)
		go func() {
			defer workers.Done()
			o.runShard(ctx, shard)
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		o.runUnrouted(ctx)
	}()
	workers.Wait()
}

// Len reports all active rows, including those waiting for route resolution.
// A failed read is an error rather than a zero: an unreadable queue and a
// drained one are the same number, and only one of them is good news.
func (o *Outbox) Len() (int, error) {
	var count int
	if err := o.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count); err != nil {
		return 0, fmt.Errorf("outbox: count queued events: %w", err)
	}

	return count, nil
}

// Parked reports rows removed from automatic delivery for operator review. It
// reports a failed read for the same reason Len does, and more sharply: this
// number answers "is there anything waiting for me", and answering zero when
// the truth is unknown is how a dead letter queue goes unread.
func (o *Outbox) Parked() (int, error) {
	var count int
	if err := o.DB.QueryRow("SELECT COUNT(*) FROM outbox_parked").Scan(&count); err != nil {
		return 0, fmt.Errorf("outbox: count parked events: %w", err)
	}

	return count, nil
}

// ReplayParked returns every operator-reviewed row to automatic delivery. UUID
// uniqueness keeps this safe if an app committed before a bad response caused
// the row to be parked.
func (o *Outbox) ReplayParked(ctx context.Context) (int64, error) {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("outbox replay: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO outbox
		(shard_id, account_id, event_uuid, domain, payload, created_at, attempts, next_try_at, last_error)
		SELECT shard_id, account_id, event_uuid, domain, payload, created_at, 0, 0, ''
		FROM outbox_parked`)
	if err != nil {
		return 0, fmt.Errorf("outbox replay: restore rows: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox replay: count rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM outbox_parked"); err != nil {
		return 0, fmt.Errorf("outbox replay: clear parked rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("outbox replay: commit: %w", err)
	}

	return count, nil
}

// OldestAge reports how long the oldest undelivered event has been retained. A
// NULL is a genuinely empty queue and reports zero; a failed read reports the
// error, because zero here means "nothing is waiting" and that is the one
// answer an unreadable queue must never give.
func (o *Outbox) OldestAge() (time.Duration, error) {
	var created sql.NullInt64
	if err := o.DB.QueryRow("SELECT MIN(created_at) FROM outbox").Scan(&created); err != nil {
		return 0, fmt.Errorf("outbox: read oldest queued event: %w", err)
	}
	if !created.Valid {
		return 0, nil
	}

	return o.clock().Sub(time.Unix(created.Int64, 0)), nil
}

// Close releases the private database after delivery workers have stopped.
func (o *Outbox) Close() error {
	var err error
	o.closeOnce.Do(func() { err = o.DB.Close() })

	return err
}

// createSchema installs the active and parked queues.
func (o *Outbox) createSchema(ctx context.Context) error {
	_, err := o.DB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS outbox (
			id INTEGER PRIMARY KEY,
			shard_id INTEGER NOT NULL,
			account_id INTEGER NOT NULL,
			event_uuid TEXT NOT NULL UNIQUE,
			domain TEXT NOT NULL,
			payload BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_try_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT ''
		);
		DROP INDEX IF EXISTS outbox_send;
		CREATE INDEX IF NOT EXISTS outbox_account ON outbox(shard_id, account_id, id, next_try_at);
		CREATE TABLE IF NOT EXISTS outbox_parked (
			id INTEGER PRIMARY KEY,
			shard_id INTEGER NOT NULL,
			account_id INTEGER NOT NULL,
			event_uuid TEXT NOT NULL UNIQUE,
			domain TEXT NOT NULL,
			payload BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			attempts INTEGER NOT NULL,
			last_error TEXT NOT NULL,
			parked_at INTEGER NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("outbox schema: %w", err)
	}

	return nil
}

// outboxRow is one selected delivery row.
type outboxRow struct {
	ID       int64
	Attempts int
	Event    Event
}

// runShard keeps one destination independent from every other destination.
func (o *Outbox) runShard(ctx context.Context, shard int) {
	ticker := time.NewTicker(outboxPoll)
	defer ticker.Stop()
	state := &deliveryState{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.report(state, o.deliver(ctx, shard), "outbox delivery", "shard", shard+1,
				"destination", o.Shards[shard])
		}
	}
}

// runUnrouted revisits events accepted only because a shard was missing from
// the merged map.
func (o *Outbox) runUnrouted(ctx context.Context) {
	ticker := time.NewTicker(outboxPoll)
	defer ticker.Stop()
	state := &deliveryState{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.report(state, o.resolveUnrouted(ctx), "outbox route resolution")
		}
	}
}

// report turns one tick's outcome into something a person can see. It reports
// the tick a loop starts failing and the tick it recovers, and repeats while it
// is still broken, because a queue that has been stuck since last night must
// not look the same in a log as one that is fine.
func (o *Outbox) report(state *deliveryState, err error, what string, attrs ...any) {
	if err == nil {
		if state.failures > 0 && o.Log != nil {
			o.Log.Info(what+" recovered", append(attrs, "failed_attempts", state.failures)...)
		}
		state.failures = 0

		return
	}

	state.failures++
	if o.Log == nil {
		return
	}
	if state.failures == 1 || state.failures%outboxFailureReportEvery == 0 {
		o.Log.Error(what+" failed", append(attrs, "consecutive_failures", state.failures, "error", err)...)
	}
}

// deliver sends one due batch and applies per-UUID acknowledgments.
//
// An error means the shard could not be reached or answered nonsense. When the
// shard answers and some accounts did not commit, that is not a shard failure:
// the other accounts went through, so those accounts are held and reported on
// their own instead.
func (o *Outbox) deliver(ctx context.Context, shard int) error {
	if !o.Router.DestinationReady(shard) {
		return fmt.Errorf("outbox destination %d has not validated its app shard identity", shard+1)
	}
	rows, err := o.selectRows(ctx, shard)
	if err != nil || len(rows) == 0 {
		return err
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, row.Event)
	}
	body, err := json.Marshal(IngestBatch{Events: events})
	if err != nil {
		return o.parkRows(ctx, rows, "encode batch: "+err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(o.Shards[shard], "/")+InternalIngestPath, bytes.NewReader(body))
	if err != nil {
		return o.retryRows(ctx, rows, err.Error(), false)
	}
	request.Header.Set("Content-Type", "application/json")
	if err := o.Signer.Sign(request, body); err != nil {
		return o.retryRows(ctx, rows, err.Error(), false)
	}
	response, err := o.client().Do(request)
	if err != nil {
		return o.retryRows(ctx, rows, err.Error(), false)
	}
	defer func() { _ = response.Body.Close() }()

	var result IngestResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return o.retryRows(ctx, rows, "decode response: "+err.Error(), response.StatusCode >= 400 && response.StatusCode < 500)
	}
	if err := o.deleteCommitted(ctx, rows, result.Committed); err != nil {
		return err
	}
	if len(result.NotMine) > 0 {
		if err := o.handleNotMine(ctx, rows, result.NotMine); err != nil {
			return err
		}
	}

	committed := map[uuid.UUID]struct{}{}
	for _, id := range result.Committed {
		committed[id] = struct{}{}
	}
	notMine := map[string]struct{}{}
	for _, domain := range result.NotMine {
		notMine[domain] = struct{}{}
	}
	var remaining []outboxRow
	answered := map[int64]bool{}
	failed := map[int64]bool{}
	for _, row := range rows {
		if _, ok := notMine[row.Event.Domain]; ok {
			continue
		}
		answered[row.Event.AccountID] = true
		if _, ok := committed[row.Event.UUID]; ok {
			continue
		}
		failed[row.Event.AccountID] = true
		remaining = append(remaining, row)
	}
	reason := result.Error
	if reason == "" {
		reason = response.Status
	}
	if len(remaining) > 0 {
		if err := o.scheduleRetry(ctx, remaining, reason, response.StatusCode >= 400 && response.StatusCode < 500); err != nil {
			return err
		}
	}
	for account := range answered {
		if failed[account] {
			o.holdAccount(shard, account, reason)
		} else {
			o.releaseAccount(shard, account)
		}
	}

	return nil
}

// selectRows reads one bounded batch for a destination.
//
// The batch takes one row from each account with rows due, then a second from
// each, and so on, oldest first. An account with a deep backlog therefore fills
// only what the others leave, and an account alone in the queue fills the whole
// batch. Held accounts are skipped until their hold runs out.
func (o *Outbox) selectRows(ctx context.Context, shard int) ([]outboxRow, error) {
	age, err := o.OldestAge()
	if err != nil {
		return nil, err
	}
	limit := outboxBatchSize
	if age > outboxCatchUpAfter {
		limit = outboxCatchUpBatch
	}
	now := o.clock()

	accounts, err := o.queuedAccounts(ctx, shard)
	if err != nil {
		return nil, err
	}

	type turn struct {
		turn int
		id   int64
	}

	var turns []turn
	for _, account := range accounts {
		if o.held(shard, account, now) {
			continue
		}
		ids, err := o.dueIDs(ctx, shard, account, now, limit)
		if err != nil {
			return nil, err
		}
		for i, id := range ids {
			turns = append(turns, turn{turn: i, id: id})
		}
	}
	slices.SortFunc(turns, func(a, b turn) int {
		return cmp.Or(cmp.Compare(a.turn, b.turn), cmp.Compare(a.id, b.id))
	})

	ids := make([]int64, 0, min(len(turns), limit))
	for _, t := range turns[:min(len(turns), limit)] {
		ids = append(ids, t.id)
	}
	slices.Sort(ids)

	return o.loadRows(ctx, shard, ids)
}

// queuedAccounts lists the accounts with any row queued for a destination. It
// steps through the index one account at a time, so its cost follows the
// number of accounts rather than the depth of the queue.
func (o *Outbox) queuedAccounts(ctx context.Context, shard int) ([]int64, error) {
	var accounts []int64
	after := int64(math.MinInt64)
	for {
		var account int64
		err := o.DB.QueryRowContext(ctx,
			"SELECT account_id FROM outbox WHERE shard_id = ? AND account_id > ? ORDER BY account_id LIMIT 1",
			shard, after).Scan(&account)
		if errors.Is(err, sql.ErrNoRows) {
			return accounts, nil
		}
		if err != nil {
			return nil, fmt.Errorf("outbox list accounts shard %d: %w", shard, err)
		}
		accounts = append(accounts, account)
		after = account
	}
}

// dueIDs reads up to limit of one account's due rows, oldest first.
func (o *Outbox) dueIDs(ctx context.Context, shard int, account int64, now time.Time, limit int) ([]int64, error) {
	rows, err := o.DB.QueryContext(ctx, `
		SELECT id FROM outbox
		WHERE shard_id = ? AND account_id = ? AND next_try_at <= ? ORDER BY id LIMIT ?`,
		shard, account, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("outbox select shard %d account %d: %w", shard, account, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("outbox scan shard %d account %d: %w", shard, account, err)
		}
		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// loadRows reads and decodes the chosen rows. A row that no longer decodes is
// parked rather than retried, because it will never decode.
func (o *Outbox) loadRows(ctx context.Context, shard int, ids []int64) ([]outboxRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("outbox select shard %d: %w", shard, err)
	}
	rows, err := o.DB.QueryContext(ctx, `
		SELECT id, attempts, payload FROM outbox
		WHERE id IN (SELECT value FROM json_each(?)) ORDER BY id`, string(list))
	if err != nil {
		return nil, fmt.Errorf("outbox select shard %d: %w", shard, err)
	}
	defer func() { _ = rows.Close() }()

	var selected []outboxRow
	undecodable := map[int64]string{}
	for rows.Next() {
		var row outboxRow
		var payload []byte
		if err := rows.Scan(&row.ID, &row.Attempts, &payload); err != nil {
			return nil, fmt.Errorf("outbox scan shard %d: %w", shard, err)
		}
		if err := json.Unmarshal(payload, &row.Event); err != nil {
			undecodable[row.ID] = "decode event: " + err.Error()
			continue
		}
		selected = append(selected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox select shard %d: %w", shard, err)
	}
	for id, reason := range undecodable {
		if err := o.parkIDs(ctx, []int64{id}, reason); err != nil {
			return nil, err
		}
	}

	return selected, nil
}

// held reports whether an account is waiting out a hold on a destination.
func (o *Outbox) held(shard int, account int64, now time.Time) bool {
	o.holdsMu.Lock()
	defer o.holdsMu.Unlock()

	hold, ok := o.holds[accountKey{shard: shard, account: account}]

	return ok && now.Before(hold.until)
}

// holdAccount keeps an account the shard could not write out of the next
// batches. The hold doubles with each failed try up to outboxAccountHoldMax,
// so one broken account stops riding in every batch the others need.
func (o *Outbox) holdAccount(shard int, account int64, reason string) {
	o.holdsMu.Lock()
	if o.holds == nil {
		o.holds = map[accountKey]*accountHold{}
	}
	key := accountKey{shard: shard, account: account}
	hold := o.holds[key]
	if hold == nil {
		hold = &accountHold{}
		o.holds[key] = hold
	}
	hold.failures++
	wait := min(time.Second<<min(hold.failures-1, 10), outboxAccountHoldMax)
	hold.until = o.clock().Add(wait)
	failures := hold.failures
	o.holdsMu.Unlock()

	if o.Log != nil && (failures == 1 || failures%outboxAccountReportEvery == 0) {
		o.Log.Error("outbox account delivery failed",
			"shard", shard+1, "destination", o.Shards[shard], "account_id", account,
			"consecutive_failures", failures, "held_for", wait.String(), "error", reason)
	}
}

// releaseAccount clears an account's hold once the shard commits its rows,
// and says so if it had been failing.
func (o *Outbox) releaseAccount(shard int, account int64) {
	key := accountKey{shard: shard, account: account}

	o.holdsMu.Lock()
	hold, ok := o.holds[key]
	delete(o.holds, key)
	o.holdsMu.Unlock()

	if ok && o.Log != nil {
		o.Log.Info("outbox account delivery recovered",
			"shard", shard+1, "destination", o.Shards[shard], "account_id", account,
			"failed_attempts", hold.failures)
	}
}

// deleteCommitted removes only UUIDs from the batch this destination received.
// A malformed response can therefore never acknowledge another shard's row.
func (o *Outbox) deleteCommitted(ctx context.Context, rows []outboxRow, ids []uuid.UUID) error {
	selected := make(map[uuid.UUID]int64, len(rows))
	for _, row := range rows {
		selected[row.Event.UUID] = row.ID
	}
	for _, id := range ids {
		rowID, ok := selected[id]
		if !ok {
			continue
		}
		if _, err := o.DB.ExecContext(ctx, "DELETE FROM outbox WHERE id = ? AND event_uuid = ?", rowID, id.String()); err != nil {
			return fmt.Errorf("outbox delete %s: %w", id, err)
		}
	}

	return nil
}

// retryRows schedules a batch the shard never answered for, and reports the
// delivery as deferred.
func (o *Outbox) retryRows(ctx context.Context, rows []outboxRow, reason string, permanent bool) error {
	if err := o.scheduleRetry(ctx, rows, reason, permanent); err != nil {
		return err
	}

	return fmt.Errorf("outbox delivery deferred: %s", reason)
}

// scheduleRetry backs off transient failures and parks repeated permanent
// failures.
func (o *Outbox) scheduleRetry(ctx context.Context, rows []outboxRow, reason string, permanent bool) error {
	// Exhausted rows are parked together so a whole batch giving up is one
	// entry in the log rather than five hundred.
	var exhausted []int64

	for _, row := range rows {
		attempts := row.Attempts + 1
		if permanent && attempts >= outboxMaxAttempts {
			exhausted = append(exhausted, row.ID)
			continue
		}
		delay := time.Second << min(attempts-1, 5)
		if _, err := o.DB.ExecContext(ctx,
			"UPDATE outbox SET attempts = ?, next_try_at = ?, last_error = ? WHERE id = ?",
			attempts, o.clock().Add(delay).Unix(), reason, row.ID); err != nil {
			return fmt.Errorf("outbox retry %d: %w", row.ID, err)
		}
	}

	if len(exhausted) > 0 {
		return o.parkIDs(ctx, exhausted, reason)
	}

	return nil
}

// parkRows moves an already-decoded group out of the active path.
func (o *Outbox) parkRows(ctx context.Context, rows []outboxRow, reason string) error {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	return o.parkIDs(ctx, ids, reason)
}

// parkIDs atomically copies rows to the operator-visible dead letter and
// removes them from automatic delivery.
//
// Every park is logged, because parking is the point at which accepted events
// stop being delivered and start waiting for a person. Nothing else in the
// system will mention them again until somebody runs the replay command.
func (o *Outbox) parkIDs(ctx context.Context, ids []int64, reason string) error {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO outbox_parked
			(shard_id, account_id, event_uuid, domain, payload, created_at, attempts, last_error, parked_at)
			SELECT shard_id, account_id, event_uuid, domain, payload, created_at, attempts, ?, ?
			FROM outbox WHERE id = ?`, reason, o.clock().Unix(), id); err != nil {
			return fmt.Errorf("outbox park %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE id = ?", id); err != nil {
			return fmt.Errorf("outbox remove parked %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if o.Log != nil {
		o.Log.Error("ingest events parked for operator review",
			"events", len(ids), "reason", reason,
			"recover_with", "feasible ingest -replay-parked")
	}

	return nil
}

// handleNotMine reroutes against the merged map and returns undecidable rows
// to the unrouted queue while any shard is silent.
func (o *Outbox) handleNotMine(ctx context.Context, rows []outboxRow, domains []string) error {
	wanted := map[string]struct{}{}
	for _, domain := range domains {
		wanted[domain] = struct{}{}
	}
	for _, row := range rows {
		if _, ok := wanted[row.Event.Domain]; !ok {
			continue
		}
		event := row.Event
		resolved, absent := o.Router.ResolveEvent(&event)
		switch {
		case resolved:
			payload, err := json.Marshal(event)
			if err != nil {
				return err
			}
			_, err = o.DB.ExecContext(ctx,
				"UPDATE outbox SET shard_id = ?, account_id = ?, payload = ?, attempts = 0, next_try_at = 0, last_error = '' WHERE id = ?",
				event.Shard, event.AccountID, payload, row.ID)
			if err != nil {
				return fmt.Errorf("outbox reroute %d: %w", row.ID, err)
			}
		case absent:
			if _, err := o.DB.ExecContext(ctx, "DELETE FROM outbox WHERE id = ?", row.ID); err != nil {
				return fmt.Errorf("outbox discard unclaimed %d: %w", row.ID, err)
			}
		default:
			if _, err := o.DB.ExecContext(ctx,
				"UPDATE outbox SET shard_id = -1, account_id = 0, attempts = 0, next_try_at = 0, last_error = ? WHERE id = ?",
				"owner cannot be resolved while routing map is incomplete", row.ID); err != nil {
				return fmt.Errorf("outbox hold unresolved %d: %w", row.ID, err)
			}
		}
	}

	return nil
}

// resolveUnrouted attaches newly discovered ownership or drops a domain only
// after every configured shard has checked in.
func (o *Outbox) resolveUnrouted(ctx context.Context) error {
	rows, err := o.selectRows(ctx, -1)
	if err != nil {
		return err
	}
	for _, row := range rows {
		event := row.Event
		resolved, absent := o.Router.ResolveEvent(&event)
		if absent {
			_, err = o.DB.ExecContext(ctx, "DELETE FROM outbox WHERE id = ?", row.ID)
			if err != nil {
				return err
			}
			continue
		}
		if !resolved {
			continue
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := o.DB.ExecContext(ctx,
			"UPDATE outbox SET shard_id = ?, account_id = ?, payload = ? WHERE id = ?",
			event.Shard, event.AccountID, payload, row.ID); err != nil {
			return err
		}
	}

	return nil
}

// client returns the configured transport or a bounded default.
func (o *Outbox) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}

	return &http.Client{Timeout: 30 * time.Second}
}

// clock returns the injected UTC clock or wall time.
func (o *Outbox) clock() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}

	return time.Now().UTC()
}
