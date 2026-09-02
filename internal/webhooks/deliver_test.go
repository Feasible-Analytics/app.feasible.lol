//
// deliver_test.go
// Publishing never blocks, failure backs off, and the warning goes out first.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package webhooks

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// clockStart is the instant the fake clock in these tests begins at.
var clockStart = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// testControl builds a migrated system database with one team in it.
func testControl(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	now := clockStart.Unix()

	if _, err := db.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}

	return db
}

// recordingNotifier captures the emails that would have been sent, so a test can
// assert on the order they went out in without a mail transport.
type recordingNotifier struct {
	warned   int
	disabled int

	// warnedWhileEnabled records whether the endpoint was still enabled when the
	// warning went out. That ordering is the whole feature: a notice that
	// arrives after we stopped trying is a notice nobody can act on.
	warnedWhileEnabled bool
}

// WebhookFailing records the warning.
func (r *recordingNotifier) WebhookFailing(_ context.Context, endpoint *Endpoint, _, _ int) error {
	r.warned++
	r.warnedWhileEnabled = endpoint.Enabled

	return nil
}

// WebhookDisabled records the shutdown notice.
func (r *recordingNotifier) WebhookDisabled(_ context.Context, _ *Endpoint, _ string) error {
	r.disabled++

	return nil
}

// TestPublishNeverMakesANetworkCall is the load-bearing property of this whole
// design: whatever produced the event — an ingest worker counting a conversion,
// an import finishing — goes back to work the moment the rows are written.
func TestPublishNeverMakesANetworkCall(t *testing.T) {
	db := testControl(t)

	var reached atomic.Int64

	// The receiver blocks for far longer than any test would tolerate. If
	// Publish touched the network at all, this test would hang rather than fail,
	// which is why the receiver sleeps instead of merely counting.
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		time.Sleep(30 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	if _, err := store.Create(context.Background(), 1, nil, receiver.URL, "slow", nil); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)

	started := time.Now()

	queued, err := dispatcher.Publish(context.Background(), 1, Event{
		Type: EventGoalConverted,
		Data: map[string]any{"goal": "Signup"},
	})
	if err != nil {
		t.Fatal(err)
	}

	elapsed := time.Since(started)

	if queued != 1 {
		t.Fatalf("queued = %d, want 1", queued)
	}

	if elapsed > 2*time.Second {
		t.Fatalf("Publish took %v — it must write rows and return, never wait on a receiver", elapsed)
	}

	if reached.Load() != 0 {
		t.Fatal("Publish made a network call")
	}

	// The delivery and its job both exist, and they were written in one
	// transaction: a delivery with no job is a payload nobody will ever send.
	var deliveries, jobs int

	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE queue = ?`, Queue).Scan(&jobs); err != nil {
		t.Fatal(err)
	}

	if deliveries != 1 || jobs != 1 {
		t.Fatalf("wrote %d deliveries and %d jobs, want one of each", deliveries, jobs)
	}
}

// TestFailingEndpointBacksOffAndIsDisabledWithWarningFirst is the retry story
// end to end.
//
// It drives the worker by hand rather than starting its loop, because the
// interesting property is the schedule — each retry further out than the last —
// and a test that waited out a real backoff would take a day.
func TestFailingEndpointBacksOffAndIsDisabledWithWarningFirst(t *testing.T) {
	db := testControl(t)

	var attempts atomic.Int64
	var deliveryHeaders []string

	// The receiver records every retry header before refusing the delivery so
	// the test can prove automatic attempts retain one idempotency key.
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		deliveryHeaders = append(deliveryHeaders, r.Header.Get(DeliveryHeader))
		http.Error(w, "the database is on fire", http.StatusInternalServerError)
	}))
	defer receiver.Close()

	clock := clockStart

	store := NewStore(db)
	store.Now = func() time.Time { return clock }

	endpoint, err := store.Create(context.Background(), 1, nil, receiver.URL, "broken", nil)
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Now = func() time.Time { return clock }

	notifier := &recordingNotifier{}

	worker := NewWorker(store, time.Second)
	worker.Now = func() time.Time { return clock }
	worker.Notifier = notifier

	// One event, drained to exhaustion. It has to be exactly one: with two
	// deliveries in flight, the next job due is always whichever was published
	// most recently, and the gaps would measure the publishing rather than the
	// backoff.
	if _, err := dispatcher.Publish(context.Background(), 1, Event{
		Type: EventGoalConverted,
		Data: map[string]any{"goal": "Signup"},
	}); err != nil {
		t.Fatal(err)
	}

	gaps := drain(t, db, worker, &clock)

	if attempts.Load() < WarnAfterFailures {
		t.Fatalf("only %d attempts were made, not enough to test the thresholds", attempts.Load())
	}

	// The delivery gave up rather than retrying forever, and the log says so.
	deliveries, err := store.Deliveries(context.Background(), 1, endpoint.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(deliveries) != 1 || deliveries[0].State != StateFailed {
		t.Fatalf("log = %+v, want one failed row", deliveries)
	}

	if deliveries[0].Error == "" {
		t.Error("a failed delivery must record why")
	}
	if len(deliveryHeaders) < 2 || deliveryHeaders[0] == "" {
		t.Fatalf("delivery headers = %v, want one stable id across retries", deliveryHeaders)
	}
	for attempt, header := range deliveryHeaders[1:] {
		if header != deliveryHeaders[0] {
			t.Errorf("automatic retry %d changed delivery id from %q to %q", attempt+2, deliveryHeaders[0], header)
		}
	}

	// The gaps have to grow. A fixed retry interval against a receiver that is
	// down is just a slower flood.
	if len(gaps) < 2 {
		t.Fatalf("only %d retries were scheduled, cannot see a backoff", len(gaps))
	}

	for i := 1; i < len(gaps); i++ {
		if gaps[i] < gaps[i-1] {
			t.Fatalf("retry %d waited %v after %v — the backoff went backwards", i, gaps[i], gaps[i-1])
		}
	}

	if gaps[len(gaps)-1] <= gaps[0] {
		t.Fatalf("the backoff never grew: first gap %v, last %v", gaps[0], gaps[len(gaps)-1])
	}

	// The warning goes out while the endpoint is still enabled and events are
	// still being attempted, so the customer has the chance to fix it before
	// anything is lost. Being told a webhook has already been switched off is a
	// notice that arrives too late to act on.
	if notifier.warned == 0 {
		t.Fatal("no warning email was sent")
	}

	if !notifier.warnedWhileEnabled {
		t.Fatal("the warning went out after the endpoint was already disabled")
	}

	middle, err := store.Get(context.Background(), 1, endpoint.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !middle.Enabled {
		t.Fatal("one exhausted delivery disabled the endpoint — a single flaky event must not")
	}

	// A second event, also drained, pushes the consecutive count past the
	// disable threshold. That the threshold is more than one delivery's worth of
	// attempts is the point: an endpoint is only turned off after a sustained
	// failure, not after one bad afternoon.
	if _, err := dispatcher.Publish(context.Background(), 1, Event{Type: EventGoalConverted}); err != nil {
		t.Fatal(err)
	}

	drain(t, db, worker, &clock)

	current, err := store.Get(context.Background(), 1, endpoint.ID)
	if err != nil {
		t.Fatal(err)
	}

	if current.Enabled {
		t.Fatalf("the endpoint is still enabled after %d consecutive failures", current.ConsecutiveFailures)
	}

	if notifier.disabled == 0 {
		t.Fatal("no notice was sent when the endpoint was disabled")
	}

	if current.DisabledReason == "" {
		t.Error("a disabled endpoint must record why")
	}
}

// drain runs the queue to exhaustion against a fake clock, returning the gap
// before each retry.
//
// Moving the clock to whenever the next job is actually due is what makes this
// possible at all: the real schedule spans more than a day, and a test that
// waited it out is a test nobody runs.
func drain(t *testing.T, db *sql.DB, worker *Worker, clock *time.Time) []time.Duration {
	t.Helper()

	var gaps []time.Duration

	for round := 0; round < 100; round++ {
		worked, _ := worker.RunOnce(context.Background())
		if worked {
			continue
		}

		next, ok := nextDue(t, db)
		if !ok {
			break
		}

		gaps = append(gaps, next.Sub(*clock))
		*clock = next
	}

	return gaps
}

// nextDue reads when the queue's next job is scheduled.
func nextDue(t *testing.T, db *sql.DB) (time.Time, bool) {
	t.Helper()

	var at sql.NullInt64

	if err := db.QueryRow(
		`SELECT MIN(scheduled_at) FROM jobs WHERE state = 'available' AND queue = ?`, Queue).Scan(&at); err != nil {
		t.Fatal(err)
	}

	if !at.Valid {
		return time.Time{}, false
	}

	return time.Unix(at.Int64, 0).UTC(), true
}

// TestBackoffGrowsAndThenStops checks the schedule arithmetic on its own, which
// is the part a customer reads in the delivery log as "next attempt at".
func TestBackoffGrowsAndThenStops(t *testing.T) {
	previous := time.Duration(0)

	for attempt := 1; attempt <= 20; attempt++ {
		delay := Backoff(attempt)

		if delay < previous {
			t.Fatalf("attempt %d waits %v, less than attempt %d's %v", attempt, delay, attempt-1, previous)
		}

		if delay > backoffMax {
			t.Fatalf("attempt %d waits %v, past the %v cap", attempt, delay, backoffMax)
		}

		previous = delay
	}

	if Backoff(1) != backoffBase {
		t.Errorf("the first retry waits %v, want the base %v", Backoff(1), backoffBase)
	}

	if Backoff(20) != backoffMax {
		t.Errorf("a late retry waits %v, want the cap %v", Backoff(20), backoffMax)
	}
}

// TestSuccessfulDeliveryIsSignedAndLogged checks the happy path, including that
// what arrives is verifiable with the secret we handed out.
func TestSuccessfulDeliveryIsSignedAndLogged(t *testing.T) {
	db := testControl(t)

	var (
		gotSignature string
		gotEvent     string
		gotEventID   string
		gotBody      []byte
	)

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get(SignatureHeader)
		gotEvent = r.Header.Get(EventHeader)
		gotEventID = r.Header.Get(EventIDHeader)

		buffer := make([]byte, 4096)
		read, _ := r.Body.Read(buffer)
		gotBody = buffer[:read]

		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	clock := clockStart

	store := NewStore(db)
	store.Now = func() time.Time { return clock }

	endpoint, err := store.Create(context.Background(), 1, nil, receiver.URL, "good", []string{EventGoalConverted})
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Now = func() time.Time { return clock }

	if _, err := dispatcher.Publish(context.Background(), 1, Event{
		ID: "evt_fixed", Type: EventGoalConverted, Data: map[string]any{"goal": "Signup"},
	}); err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(store, time.Second)
	worker.Now = func() time.Time { return clock }

	if worked, err := worker.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("the worker did not deliver: worked=%v err=%v", worked, err)
	}

	if gotEvent != EventGoalConverted {
		t.Errorf("%s = %q", EventHeader, gotEvent)
	}

	if gotEventID != "evt_fixed" {
		t.Errorf("%s = %q, want the id that survives retries", EventIDHeader, gotEventID)
	}

	// The receiver's own check: the signature verifies against the secret we
	// handed out at creation, over the exact bytes that arrived.
	if err := Verify(gotSignature, gotBody, clock, DefaultReplayWindow, endpoint.Secret); err != nil {
		t.Fatalf("the delivery did not verify with the secret we issued: %v", err)
	}

	deliveries, err := store.Deliveries(context.Background(), 1, endpoint.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(deliveries) != 1 || deliveries[0].State != StateDelivered {
		t.Fatalf("log = %+v, want one delivered row", deliveries)
	}

	if deliveries[0].ResponseStatus != http.StatusNoContent {
		t.Errorf("the log recorded status %d", deliveries[0].ResponseStatus)
	}
}

// TestOnlySubscribedEndpointsAreQueued checks the two filters that decide who
// hears about an event.
func TestOnlySubscribedEndpointsAreQueued(t *testing.T) {
	db := testControl(t)

	now := clockStart.Unix()

	if _, err := db.Exec(`INSERT INTO sites (id, account_id, domain, created_at, updated_at)
		VALUES (1, 1, 'a.example', ?, ?), (2, 1, 'b.example', ?, ?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	ctx := context.Background()

	if _, err := store.Create(ctx, 1, nil, "https://all.example/hook", "everything", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create(ctx, 1, nil, "https://imports.example/hook", "imports only",
		[]string{EventImportCompleted}); err != nil {
		t.Fatal(err)
	}

	siteOne := int64(1)
	if _, err := store.Create(ctx, 1, &siteOne, "https://one.example/hook", "site one", nil); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Now = func() time.Time { return clockStart }

	siteTwo := int64(2)

	// A goal on site two: the catch-all hears it, the imports-only endpoint does
	// not, and the endpoint scoped to site one does not.
	queued, err := dispatcher.Publish(ctx, 1, Event{Type: EventGoalConverted, SiteID: &siteTwo})
	if err != nil {
		t.Fatal(err)
	}

	if queued != 1 {
		t.Fatalf("queued = %d, want only the catch-all endpoint", queued)
	}

	// An import completing: the catch-all and the imports endpoint, but still
	// not the one bound to a site the event does not name.
	queued, err = dispatcher.Publish(ctx, 1, Event{Type: EventImportCompleted})
	if err != nil {
		t.Fatal(err)
	}

	if queued != 2 {
		t.Fatalf("queued = %d, want the catch-all and the imports endpoint", queued)
	}
}

// TestDisabledEndpointIsNotDelivered checks that turning an endpoint off means
// what it says, including for deliveries already on the queue.
func TestDisabledEndpointIsNotDelivered(t *testing.T) {
	db := testControl(t)

	var reached atomic.Int64

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	ctx := context.Background()

	endpoint, err := store.Create(ctx, 1, nil, receiver.URL, "about to be off", nil)
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Now = func() time.Time { return clockStart }

	if _, err := dispatcher.Publish(ctx, 1, Event{Type: EventSiteCreated}); err != nil {
		t.Fatal(err)
	}

	off := false
	if _, err := store.Update(ctx, 1, endpoint.ID, nil, nil, nil, &off); err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(store, time.Second)
	worker.Now = func() time.Time { return clockStart }

	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if reached.Load() != 0 {
		t.Fatal("a delivery was sent to an endpoint that had been switched off")
	}
}

// TestReEnablingClearsTheFailureCount checks that an endpoint turned back on is
// not one failure away from being disabled again.
func TestReEnablingClearsTheFailureCount(t *testing.T) {
	db := testControl(t)

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	ctx := context.Background()

	endpoint, err := store.Create(ctx, 1, nil, "https://example.org/hook", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(
		`UPDATE webhook_endpoints SET consecutive_failures = ?, enabled = 0 WHERE id = ?`,
		DisableAfterFailures, endpoint.ID); err != nil {
		t.Fatal(err)
	}

	on := true

	updated, err := store.Update(ctx, 1, endpoint.ID, nil, nil, nil, &on)
	if err != nil {
		t.Fatal(err)
	}

	if updated.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive_failures = %d after re-enabling, want 0", updated.ConsecutiveFailures)
	}
}

// TestRedeliveryLeavesTheOriginalInTheLog checks the manual redeliver button.
// A new row rather than a reset of the old one keeps the log honest: the
// customer can see that the first attempt failed and that somebody pressed the
// button, which is exactly what they open the log to establish.
func TestRedeliveryLeavesTheOriginalInTheLog(t *testing.T) {
	db := testControl(t)

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	ctx := context.Background()

	endpoint, err := store.Create(ctx, 1, nil, receiver.URL, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Now = func() time.Time { return clockStart }

	if _, err := dispatcher.Publish(ctx, 1, Event{ID: "evt_original", Type: EventSiteCreated}); err != nil {
		t.Fatal(err)
	}

	first, err := store.Deliveries(ctx, 1, endpoint.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	again, err := dispatcher.Redeliver(ctx, 1, first[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	if again.ID == first[0].ID {
		t.Fatal("redelivery reused the original row")
	}

	// The event id is stable across the redelivery, so a receiver can choose to
	// collapse even an intentional manual replay when that is its desired rule.
	if again.EventID != "evt_original" {
		t.Errorf("event id = %q, want it stable across a redelivery", again.EventID)
	}

	all, err := store.Deliveries(ctx, 1, endpoint.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(all) != 2 {
		t.Fatalf("log has %d rows, want the original and the redelivery", len(all))
	}
}

// TestPlainHTTPIsRefused checks the one URL rule that is not cosmetic: a signed
// payload can name a converting visitor's page and properties, and on plain
// HTTP that is readable by anyone on the path.
func TestPlainHTTPIsRefused(t *testing.T) {
	cases := map[string]bool{
		"https://example.org/hook":   true,
		"http://localhost:9000/hook": true,
		"http://127.0.0.1:9000/hook": true,
		"http://example.org/hook":    false,
		"ftp://example.org/hook":     false,
		"example.org/hook":           false,
		"":                           false,
		"https://":                   false,
	}

	for url, allowed := range cases {
		err := ValidateURL(url)

		if allowed && err != nil {
			t.Errorf("%q was refused: %v", url, err)
		}

		if !allowed && err == nil {
			t.Errorf("%q was allowed", url)
		}
	}
}

// TestALockedAccountQueuesNothing closes the back door. A goal conversion or a
// traffic spike posted to a customer's endpoint is the same data the dashboard
// and the API have just refused, arriving on a schedule and needing no
// credential at all — so a lock that stopped at the front door would hand back
// everything it had refused.
func TestALockedAccountQueuesNothing(t *testing.T) {
	db := testControl(t)

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	if _, err := store.Create(context.Background(), 1, nil, "https://example.org/hook", "hook", nil); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Blocked = func(accountID int64) bool { return accountID == 1 }

	queued, err := dispatcher.Publish(context.Background(), 1, Event{Type: EventGoalConverted})

	// The refusal is an error rather than a quiet zero. A withheld event that
	// nothing anywhere records is exactly the failure this product exists to
	// stop making.
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if queued != 0 {
		t.Fatalf("queued = %d for a locked account", queued)
	}

	var deliveries, jobs int

	if err := db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}

	if deliveries != 0 || jobs != 0 {
		t.Fatalf("a locked account left %d deliveries and %d jobs behind", deliveries, jobs)
	}
}

// TestAnUnlockedAccountStillPublishes is the other half, and the one that would
// silently stop every customer's integration if the check were inverted.
func TestAnUnlockedAccountStillPublishes(t *testing.T) {
	db := testControl(t)

	store := NewStore(db)
	store.Now = func() time.Time { return clockStart }

	if _, err := store.Create(context.Background(), 1, nil, "https://example.org/hook", "hook", nil); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Blocked = func(int64) bool { return false }

	queued, err := dispatcher.Publish(context.Background(), 1, Event{Type: EventGoalConverted})
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued = %d, want 1", queued)
	}
}

// TestAPrivateAddressIsRefusedAtEveryStage is the SSRF case. An endpoint URL is
// a customer-supplied destination, and the delivery log shows the receiver's
// answer back to that customer — so a webhook pointed at 169.254.169.254 or at
// a service on our own network would read it and hand the body over.
func TestAPrivateAddressIsRefusedAtEveryStage(t *testing.T) {
	for _, raw := range []string{
		"https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.5/hook",
		"https://192.168.1.1/hook",
		"https://[fd00::1]/hook",
	} {
		if err := ValidateURL(raw); err == nil {
			t.Errorf("%s was accepted by ValidateURL", raw)
		}
	}

	// The form check only sees an address when the customer typed one. The
	// dialer is what stops a hostname that resolves to a private address, so
	// the delivery client is checked directly.
	store := NewStore(testControl(t))

	client := NewWorker(store, time.Second).Client

	response, err := client.Get("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("the delivery client reached the metadata endpoint")
	}

	if !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("the delivery client failed for another reason: %v", err)
	}
}

// TestADeliveryDoesNotFollowARedirect covers the second half of the same
// problem: a receiver that answers 302 pointing at loopback is a destination
// nobody validated, reached with a signed payload attached.
func TestADeliveryDoesNotFollowARedirect(t *testing.T) {
	followed := false

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer receiver.Close()

	db := testControl(t)
	clock := clockStart

	store := NewStore(db)
	store.Now = func() time.Time { return clock }

	endpoint, err := store.Create(context.Background(), 1, nil, receiver.URL, "redirecting", nil)
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store)
	dispatcher.Now = func() time.Time { return clock }

	if _, err := dispatcher.Publish(context.Background(), 1, Event{Type: EventGoalConverted}); err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(store, time.Second)
	worker.Now = func() time.Time { return clock }

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if followed {
		t.Fatal("the delivery followed a redirect to a destination nobody registered")
	}

	deliveries, err := store.Deliveries(context.Background(), 1, endpoint.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(deliveries) != 1 || deliveries[0].ResponseStatus != http.StatusFound {
		t.Fatalf("log = %+v, want the 302 itself recorded", deliveries)
	}
}
