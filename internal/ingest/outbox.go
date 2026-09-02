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
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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

// Outbox owns every event after the public 202 and before the app shard's
// commit acknowledgment.
type Outbox struct {
	DB     *sql.DB
	Router *RemoteRouter
	Shards []string
	Client *http.Client
	Signer *InternalSigner
	Now    func() time.Time

	closeOnce sync.Once
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
		db.Close()
		return nil, err
	}
	router, err := NewRemoteRouter(ctx, db, shards, signer)
	if err != nil {
		db.Close()
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
	defer tx.Rollback()

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
func (o *Outbox) Len() int {
	var count int
	_ = o.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count)

	return count
}

// Parked reports rows removed from automatic delivery for operator review.
func (o *Outbox) Parked() int {
	var count int
	_ = o.DB.QueryRow("SELECT COUNT(*) FROM outbox_parked").Scan(&count)

	return count
}

// ReplayParked returns every operator-reviewed row to automatic delivery. UUID
// uniqueness keeps this safe if an app committed before a bad response caused
// the row to be parked.
func (o *Outbox) ReplayParked(ctx context.Context) (int64, error) {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("outbox replay: begin: %w", err)
	}
	defer tx.Rollback()
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

// OldestAge reports how long the oldest undelivered event has been retained.
func (o *Outbox) OldestAge() time.Duration {
	var created sql.NullInt64
	if err := o.DB.QueryRow("SELECT MIN(created_at) FROM outbox").Scan(&created); err != nil || !created.Valid {
		return 0
	}

	return o.clock().Sub(time.Unix(created.Int64, 0))
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
		CREATE INDEX IF NOT EXISTS outbox_send ON outbox(shard_id, next_try_at, id);
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = o.deliver(ctx, shard)
		}
	}
}

// runUnrouted revisits events accepted only because a shard was missing from
// the merged map.
func (o *Outbox) runUnrouted(ctx context.Context) {
	ticker := time.NewTicker(outboxPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = o.resolveUnrouted(ctx)
		}
	}
}

// deliver sends one due batch and applies per-UUID acknowledgments.
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
	defer response.Body.Close()

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
	for _, row := range rows {
		if _, ok := committed[row.Event.UUID]; ok {
			continue
		}
		if _, ok := notMine[row.Event.Domain]; ok {
			continue
		}
		remaining = append(remaining, row)
	}
	if len(remaining) > 0 {
		reason := result.Error
		if reason == "" {
			reason = response.Status
		}
		return o.retryRows(ctx, remaining, reason, response.StatusCode >= 400 && response.StatusCode < 500)
	}

	return nil
}

// selectRows reads one fair, bounded batch for a destination.
func (o *Outbox) selectRows(ctx context.Context, shard int) ([]outboxRow, error) {
	limit := outboxBatchSize
	if o.OldestAge() > outboxCatchUpAfter {
		limit = outboxCatchUpBatch
	}
	rows, err := o.DB.QueryContext(ctx, `
		SELECT id, attempts, payload FROM outbox
		WHERE shard_id = ? AND next_try_at <= ? ORDER BY id LIMIT ?`, shard, o.clock().Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("outbox select shard %d: %w", shard, err)
	}
	defer rows.Close()
	var selected []outboxRow
	for rows.Next() {
		var row outboxRow
		var payload []byte
		if err := rows.Scan(&row.ID, &row.Attempts, &payload); err != nil {
			return nil, fmt.Errorf("outbox scan shard %d: %w", shard, err)
		}
		if err := json.Unmarshal(payload, &row.Event); err != nil {
			if parkErr := o.parkIDs(ctx, []int64{row.ID}, "decode event: "+err.Error()); parkErr != nil {
				return nil, parkErr
			}
			continue
		}
		selected = append(selected, row)
	}

	return selected, rows.Err()
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

// retryRows schedules transient failures and parks repeated permanent failures.
func (o *Outbox) retryRows(ctx context.Context, rows []outboxRow, reason string, permanent bool) error {
	for _, row := range rows {
		attempts := row.Attempts + 1
		if permanent && attempts >= outboxMaxAttempts {
			if err := o.parkIDs(ctx, []int64{row.ID}, reason); err != nil {
				return err
			}
			continue
		}
		delay := time.Second << min(attempts-1, 5)
		if _, err := o.DB.ExecContext(ctx,
			"UPDATE outbox SET attempts = ?, next_try_at = ?, last_error = ? WHERE id = ?",
			attempts, o.clock().Add(delay).Unix(), reason, row.ID); err != nil {
			return fmt.Errorf("outbox retry %d: %w", row.ID, err)
		}
	}

	return fmt.Errorf("outbox delivery deferred: %s", reason)
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
func (o *Outbox) parkIDs(ctx context.Context, ids []int64, reason string) error {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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

	return tx.Commit()
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
