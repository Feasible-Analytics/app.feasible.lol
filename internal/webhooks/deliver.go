//
// deliver.go
// Publishing through the job queue, retrying with backoff, and giving up loudly.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package webhooks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Queue is the job queue name deliveries are enqueued on. It is its own queue
// so that a customer endpoint being slow cannot starve imports or roll-ups: the
// queues run independently, and this one is the only one that waits on somebody
// else's server.
const Queue = "webhooks"

// JobKind is the job the worker claims.
const JobKind = "webhook.deliver"

// MaxAttempts is how many times one event is offered to one endpoint before the
// delivery is marked failed. Twelve attempts on the schedule below spans more
// than a day, which is long enough for a receiver to be redeployed and short
// enough that a payload does not arrive so late it is useless.
const MaxAttempts = 12

// The backoff schedule. It starts fast, because most failures are a deploy that
// took thirty seconds, and tops out at six hours, because past that the retry is
// no longer about a blip and the customer needs an email rather than another
// POST.
const (
	backoffBase   = 30 * time.Second
	backoffFactor = 3
	backoffMax    = 6 * time.Hour
)

// DefaultTimeout bounds one delivery attempt. Ten seconds is generous for a
// receiver that only has to acknowledge, and it is the number that stops one
// hung endpoint from holding a worker forever.
const DefaultTimeout = 10 * time.Second

// maxResponseBody is how much of a receiver's answer is kept in the log. Two
// kilobytes is enough to see an error message and not enough for a receiver
// that answers errors with a full HTML page to fill our disk one retry at a
// time.
const maxResponseBody = 2048

// Notifier is how the customer hears about an endpoint that is failing. It is
// an interface rather than a mail package so that this package can be tested
// without a mail transport, and so that the warning can also become a dashboard
// banner without touching the delivery logic.
type Notifier interface {
	// WebhookFailing is the warning, sent while the endpoint is still enabled.
	// It carries the threshold so the message can say how long is left, which is
	// the difference between a notice somebody acts on and one they file.
	WebhookFailing(ctx context.Context, endpoint *Endpoint, consecutiveFailures, disableAfter int) error

	// WebhookDisabled is sent once we have stopped trying.
	WebhookDisabled(ctx context.Context, endpoint *Endpoint, reason string) error
}

// Event is one thing that happened, ready to be delivered.
type Event struct {
	// ID is stable across every retry and every manual redelivery, and is what
	// a receiver keys its own idempotency on.
	ID string

	Type string

	// SiteID scopes the event. A webhook registered against one site only hears
	// about that site; one registered against the team hears about all of them.
	SiteID *int64

	// Data is the event-specific body. It is `any` because each type has its
	// own shape and forcing them into one struct would mean a payload full of
	// fields that are null for every type but one.
	Data any
}

// ErrLocked is what Publish answers for an account whose dashboard is locked.
// It is an error rather than a silent zero because a withheld event is exactly
// the kind of thing that must never vanish without a line somewhere saying so.
var ErrLocked = errors.New("webhooks: this account is locked, so nothing was queued")

// Dispatcher turns an event into rows. It never makes a network call: the whole
// point of this design is that whatever produced the event — an ingest worker
// counting a conversion, an import finishing — goes back to work the moment the
// rows are written.
type Dispatcher struct {
	store *Store
	Now   func() time.Time

	// Blocked reports whether an account's data may not leave the building. A
	// goal conversion or a traffic spike posted to a customer's endpoint is the
	// dashboard by another route, so a lock that ignored this queue would hand
	// back the numbers it had just refused. It is a function rather than a
	// dependency on the billing packages so that delivering a webhook does not
	// require billing to exist, and nil means nothing is ever blocked.
	Blocked func(accountID int64) bool
}

// NewDispatcher builds a dispatcher over the endpoint store.
func NewDispatcher(store *Store) *Dispatcher {
	return &Dispatcher{store: store, Now: store.Now}
}

// now reads the dispatcher's clock.
func (d *Dispatcher) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}

	return d.Now()
}

// envelope is the JSON a receiver sees. It is the same shape for every event
// type, with the type-specific part under `data`, so a receiver writes one
// parser and switches on one field.
type envelope struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CreatedAt int64  `json:"created_at"`
	SiteID    *int64 `json:"site_id,omitempty"`
	Data      any    `json:"data"`
}

// Publish writes one delivery row and one job row per subscribed endpoint, in a
// single transaction, and returns.
//
// The transaction is what makes this safe to call from anywhere: either the
// delivery and its job both exist or neither does. A delivery row with no job is
// a payload nobody will ever send, and a job with no delivery row is a worker
// that wakes up to find nothing to do.
func (d *Dispatcher) Publish(ctx context.Context, teamID int64, event Event) (int, error) {
	if !ValidEventType(event.Type) {
		return 0, fmt.Errorf("webhooks: publish: unknown event type %q", event.Type)
	}

	if d.Blocked != nil && d.Blocked(teamID) {
		return 0, ErrLocked
	}

	if event.ID == "" {
		event.ID = uuid.NewString()
	}

	endpoints, err := d.store.List(ctx, teamID)
	if err != nil {
		return 0, err
	}

	now := d.now()

	body, err := json.Marshal(envelope{
		ID: event.ID, Type: event.Type, CreatedAt: now.Unix(), SiteID: event.SiteID, Data: event.Data,
	})
	if err != nil {
		return 0, fmt.Errorf("webhooks: publish: %w", err)
	}

	tx, err := d.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("webhooks: publish: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	queued := 0

	for _, endpoint := range endpoints {
		if !endpoint.Enabled || !endpoint.Wants(event.Type) {
			continue
		}

		// A site-scoped endpoint hears only about its own site. An event with
		// no site — a usage limit, say — reaches only the team-wide endpoints,
		// because there is no site it could be said to belong to.
		if endpoint.SiteID != nil && (event.SiteID == nil || *endpoint.SiteID != *event.SiteID) {
			continue
		}

		if err := enqueue(ctx, tx, endpoint.ID, event, string(body), now); err != nil {
			return 0, err
		}

		queued++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("webhooks: publish: %w", err)
	}

	return queued, nil
}

// enqueue writes one delivery and its job inside the caller's transaction.
func enqueue(ctx context.Context, tx *sql.Tx, endpointID int64, event Event, body string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (endpoint_id, event_id, event_type, payload, state, max_attempts, created_at, next_attempt_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)`,
		endpointID, event.ID, event.Type, body, MaxAttempts, now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("webhooks: publish: %w", err)
	}

	deliveryID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("webhooks: publish: %w", err)
	}

	args, err := json.Marshal(map[string]int64{"delivery_id": deliveryID})
	if err != nil {
		return fmt.Errorf("webhooks: publish: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (queue, kind, args, state, max_attempts, scheduled_at)
		VALUES (?, ?, ?, 'available', ?, ?)`,
		Queue, JobKind, string(args), MaxAttempts, now.Unix()); err != nil {
		return fmt.Errorf("webhooks: publish: %w", err)
	}

	return nil
}

// Redeliver queues an existing delivery again, as a fresh row with the same
// event id. A new row rather than a reset of the old one keeps the log honest:
// the customer can see that the first attempt failed and that somebody pressed
// the button, which is exactly what they are trying to establish when they open
// the log.
func (d *Dispatcher) Redeliver(ctx context.Context, teamID, deliveryID int64) (*Delivery, error) {
	original, err := d.store.Delivery(ctx, teamID, deliveryID)
	if err != nil {
		return nil, err
	}

	now := d.now()

	tx, err := d.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("webhooks: redeliver: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	event := Event{ID: original.EventID, Type: original.EventType}

	if err := enqueue(ctx, tx, original.EndpointID, event, original.Payload, now); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("webhooks: redeliver: %w", err)
	}

	var latest int64
	if err := d.store.DB().QueryRowContext(ctx,
		`SELECT MAX(id) FROM webhook_deliveries WHERE endpoint_id = ?`, original.EndpointID).Scan(&latest); err != nil {
		return nil, fmt.Errorf("webhooks: redeliver: %w", err)
	}

	return d.store.Delivery(ctx, teamID, latest)
}

// Worker drains the delivery queue. It is a struct with an explicit RunOnce so
// that the whole retry and disable path can be driven from a test without a
// goroutine, a ticker or a sleep.
type Worker struct {
	store *Store

	// Client is the HTTP client deliveries go out on. It comes from
	// outbound.Policy, so the endpoint URL is resolved and checked again at
	// connect time and a redirect is never followed: an endpoint that resolved
	// to a public address when it was saved and to 169.254.169.254 when it is
	// used, or one that answers 302 pointing at loopback, is a way to make this
	// process read something on its own network and put the answer in a
	// delivery log the customer can open.
	Client *http.Client

	// Notifier is told when an endpoint is failing and when it is disabled.
	// A nil notifier means nobody is told at all, which is only ever right in a
	// test: the whole point of warning before disabling is that somebody hears
	// about it.
	Notifier Notifier

	// Log records what happened, so that a delivery nobody is watching is still
	// visible afterwards.
	Log func(message string, args ...any)

	Now func() time.Time
}

// NewWorker builds a worker with the default timeout, delivering through the
// store's outbound policy.
func NewWorker(store *Store, timeout time.Duration) *Worker {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	return &Worker{
		store:  store,
		Client: store.Policy.NewClient(timeout),
		Now:    store.Now,
	}
}

// now reads the worker's clock.
func (w *Worker) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}

	return w.Now()
}

// log records an event if a logger was supplied.
func (w *Worker) log(message string, args ...any) {
	if w.Log != nil {
		w.Log(message, args...)
	}
}

// Run drains the queue until the context is cancelled. The interval is how long
// it waits when there was nothing to do; when there is work it loops without
// pausing, so a backlog drains at whatever rate the receivers can take.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		worked, err := w.RunOnce(ctx)
		if err != nil {
			w.log("webhook delivery failed", "error", err)
		}

		wait := interval
		if worked {
			wait = 0
		}

		timer.Reset(wait)
	}
}

// RunOnce claims at most one job and attempts its delivery. It reports whether
// there was anything to do, so the caller can back off when the queue is empty
// rather than spinning on it.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	jobID, deliveryID, err := w.claim(ctx)
	if err != nil || jobID == 0 {
		return false, err
	}

	attemptErr := w.attempt(ctx, deliveryID)

	// The job is always completed, whatever happened. Retries are scheduled as
	// a *new* job by the attempt itself, because the delivery row is the source
	// of truth for how many tries are left — leaving the job available would
	// give us two schedules for the same delivery and no way to reconcile them.
	if _, err := w.store.DB().ExecContext(ctx,
		`UPDATE jobs SET state = 'completed', completed_at = ?, last_error = ? WHERE id = ?`,
		w.now().Unix(), errorText(attemptErr), jobID); err != nil {
		return true, fmt.Errorf("webhooks: complete job: %w", err)
	}

	return true, attemptErr
}

// errorText renders an error for the job log, or the empty string for success.
func errorText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// claim takes the oldest due job off the queue. The select and the update run in
// one transaction so two workers cannot claim the same row — which matters even
// though every queue runs at concurrency one, because a deploy briefly runs two
// processes at once.
func (w *Worker) claim(ctx context.Context) (int64, int64, error) {
	tx, err := w.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("webhooks: claim: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	var (
		jobID int64
		args  string
	)

	err = tx.QueryRowContext(ctx, `
		SELECT id, args FROM jobs
		WHERE state = 'available' AND queue = ? AND scheduled_at <= ?
		ORDER BY scheduled_at, id LIMIT 1`, Queue, w.now().Unix()).Scan(&jobID, &args)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("webhooks: claim: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'executing', attempt = attempt + 1, attempted_at = ? WHERE id = ?`,
		w.now().Unix(), jobID); err != nil {
		return 0, 0, fmt.Errorf("webhooks: claim: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("webhooks: claim: %w", err)
	}

	var payload struct {
		DeliveryID int64 `json:"delivery_id"`
	}

	if err := json.Unmarshal([]byte(args), &payload); err != nil {
		return jobID, 0, fmt.Errorf("webhooks: claim: job %d has unreadable args: %w", jobID, err)
	}

	return jobID, payload.DeliveryID, nil
}

// attempt makes one delivery and records the outcome.
func (w *Worker) attempt(ctx context.Context, deliveryID int64) error {
	row := w.store.DB().QueryRowContext(ctx,
		`SELECT `+deliveryColumns+` FROM webhook_deliveries WHERE id = ?`, deliveryID)

	delivery, err := scanDelivery(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("webhooks: attempt: %w", err)
	}

	if delivery.State == StateDelivered {
		return nil
	}

	endpointRow := w.store.DB().QueryRowContext(ctx,
		`SELECT `+endpointColumns+` FROM webhook_endpoints WHERE id = ?`, delivery.EndpointID)

	endpoint, secret, _, _, err := scanEndpoint(endpointRow.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("webhooks: attempt: %w", err)
	}

	// An endpoint disabled between the enqueue and the attempt is not tried.
	// Delivering to an endpoint somebody has just turned off is the one thing
	// they explicitly asked us not to do.
	if !endpoint.Enabled {
		return w.finish(ctx, delivery, StateFailed, 0, "", "endpoint is disabled", 0)
	}

	status, body, elapsed, sendErr := w.send(ctx, endpoint.URL, secret, delivery)

	if sendErr == nil && status >= 200 && status < 300 {
		if err := w.finish(ctx, delivery, StateDelivered, status, body, "", elapsed); err != nil {
			return err
		}

		return w.recordSuccess(ctx, endpoint)
	}

	reason := errorText(sendErr)
	if reason == "" {
		reason = fmt.Sprintf("endpoint answered %d", status)
	}

	if err := w.recordFailure(ctx, endpoint); err != nil {
		return err
	}

	return w.reschedule(ctx, delivery, status, body, reason, elapsed)
}

// send performs one HTTP POST and returns what came back.
func (w *Worker) send(ctx context.Context, url, secret string, delivery *Delivery) (status int, body string, elapsed int, err error) {
	payload := []byte(delivery.Payload)
	now := w.now()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, "", 0, fmt.Errorf("build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "feasible-webhooks/1")
	request.Header.Set(SignatureHeader, Sign(secret, payload, now))
	request.Header.Set(EventHeader, delivery.EventType)
	request.Header.Set(EventIDHeader, delivery.EventID)
	request.Header.Set(DeliveryHeader, fmt.Sprint(delivery.ID))

	started := time.Now()

	response, err := w.Client.Do(request)
	elapsed = int(time.Since(started).Milliseconds())

	if err != nil {
		return 0, "", elapsed, err
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close webhook response: %w", closeErr))
		}
	}()

	answer, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))

	return response.StatusCode, string(answer), elapsed, nil
}

// finish writes a terminal outcome for one delivery.
func (w *Worker) finish(ctx context.Context, delivery *Delivery, state string, status int, body, failure string, elapsed int) error {
	now := w.now().Unix()

	var delivered any
	if state == StateDelivered {
		delivered = now
	}

	var responseStatus any
	if status > 0 {
		responseStatus = status
	}

	if _, err := w.store.DB().ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET state = ?, attempt = attempt + 1, response_status = ?, response_body = ?, error = ?,
		    duration_ms = ?, attempted_at = ?, next_attempt_at = NULL, delivered_at = ?
		WHERE id = ?`,
		state, responseStatus, body, failure, elapsed, now, delivered, delivery.ID); err != nil {
		return fmt.Errorf("webhooks: record delivery: %w", err)
	}

	return nil
}

// reschedule records a failed attempt and queues the next one, unless the
// delivery is out of attempts.
func (w *Worker) reschedule(ctx context.Context, delivery *Delivery, status int, body, failure string, elapsed int) error {
	attempt := delivery.Attempt + 1

	if attempt >= delivery.MaxAttempts {
		w.log("webhook delivery gave up", "delivery", delivery.ID, "endpoint", delivery.EndpointID, "reason", failure)

		return w.finish(ctx, delivery, StateFailed, status, body, failure, elapsed)
	}

	next := w.now().Add(Backoff(attempt))
	now := w.now().Unix()

	var responseStatus any
	if status > 0 {
		responseStatus = status
	}

	tx, err := w.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("webhooks: reschedule: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET attempt = ?, response_status = ?, response_body = ?, error = ?, duration_ms = ?,
		    attempted_at = ?, next_attempt_at = ?
		WHERE id = ?`,
		attempt, responseStatus, body, failure, elapsed, now, next.Unix(), delivery.ID); err != nil {
		return fmt.Errorf("webhooks: reschedule: %w", err)
	}

	args, err := json.Marshal(map[string]int64{"delivery_id": delivery.ID})
	if err != nil {
		return fmt.Errorf("webhooks: reschedule: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (queue, kind, args, state, attempt, max_attempts, scheduled_at)
		VALUES (?, ?, ?, 'available', ?, ?, ?)`,
		Queue, JobKind, string(args), attempt, delivery.MaxAttempts, next.Unix()); err != nil {
		return fmt.Errorf("webhooks: reschedule: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("webhooks: reschedule: %w", err)
	}

	return nil
}

// Backoff is how long to wait before attempt number n, counting from one. It is
// exported because the delay is a customer-visible promise — the delivery log
// shows when the next attempt is due — and a function is the only honest way to
// document a schedule that changes shape at the cap.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	delay := backoffBase
	for i := 1; i < attempt; i++ {
		delay *= backoffFactor

		if delay >= backoffMax {
			return backoffMax
		}
	}

	return delay
}

// recordSuccess clears the failure counter. It is reset rather than decremented
// because the counter answers "is this endpoint broken right now", and one
// success is enough to say no.
func (w *Worker) recordSuccess(ctx context.Context, endpoint *Endpoint) error {
	if endpoint.ConsecutiveFailures == 0 {
		return nil
	}

	if _, err := w.store.DB().ExecContext(ctx,
		`UPDATE webhook_endpoints SET consecutive_failures = 0, warned_at = NULL, updated_at = ? WHERE id = ?`,
		w.now().Unix(), endpoint.ID); err != nil {
		return fmt.Errorf("webhooks: record success: %w", err)
	}

	return nil
}

// recordFailure counts a failure, warns the customer on the way past the
// warning threshold and disables the endpoint at the disable threshold.
//
// The order is the whole point of the feature: the email goes out while the
// endpoint is still enabled and events are still being attempted, so the
// customer has the chance to fix it before anything is lost. Telling somebody
// their webhook has been switched off is a notice they can do nothing with.
func (w *Worker) recordFailure(ctx context.Context, endpoint *Endpoint) error {
	failures := endpoint.ConsecutiveFailures + 1
	now := w.now().Unix()

	if _, err := w.store.DB().ExecContext(ctx,
		`UPDATE webhook_endpoints SET consecutive_failures = ?, updated_at = ? WHERE id = ?`,
		failures, now, endpoint.ID); err != nil {
		return fmt.Errorf("webhooks: record failure: %w", err)
	}

	endpoint.ConsecutiveFailures = failures

	if failures == WarnAfterFailures {
		w.log("webhook endpoint failing", "endpoint", endpoint.ID, "failures", failures)

		if w.Notifier != nil {
			if err := w.Notifier.WebhookFailing(ctx, endpoint, failures, DisableAfterFailures); err != nil {
				w.log("webhook warning email failed", "endpoint", endpoint.ID, "error", err)
			}
		}

		if _, err := w.store.DB().ExecContext(ctx,
			`UPDATE webhook_endpoints SET warned_at = ? WHERE id = ?`, now, endpoint.ID); err != nil {
			return fmt.Errorf("webhooks: record warning: %w", err)
		}
	}

	if failures >= DisableAfterFailures {
		reason := fmt.Sprintf("disabled automatically after %d consecutive failed deliveries", failures)

		if _, err := w.store.DB().ExecContext(ctx,
			`UPDATE webhook_endpoints SET enabled = 0, disabled_at = ?, disabled_reason = ?, updated_at = ? WHERE id = ?`,
			now, reason, now, endpoint.ID); err != nil {
			return fmt.Errorf("webhooks: disable: %w", err)
		}

		endpoint.Enabled = false
		endpoint.DisabledReason = reason

		w.log("webhook endpoint disabled", "endpoint", endpoint.ID, "failures", failures)

		if w.Notifier != nil {
			if err := w.Notifier.WebhookDisabled(ctx, endpoint, reason); err != nil {
				w.log("webhook disabled email failed", "endpoint", endpoint.ID, "error", err)
			}
		}
	}

	return nil
}
