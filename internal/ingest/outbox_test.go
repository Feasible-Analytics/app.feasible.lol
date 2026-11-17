//
// outbox_test.go
// Durability and exact-ack coverage for the ingester-owned SQLite outbox.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// testContext returns the non-cancelled context used by synchronous outbox
// operations; Go 1.23 does not yet expose testing.T.Context.
func testContext() context.Context { return context.Background() }

// queued reads the active queue depth and fails the test if it cannot be read,
// so an unreadable database can never be mistaken for a drained queue.
func queued(t *testing.T, outbox *Outbox) int {
	t.Helper()

	count, err := outbox.Len()
	if err != nil {
		t.Fatal(err)
	}

	return count
}

// parked reads the dead-letter depth, failing the test on an unreadable one.
func parked(t *testing.T, outbox *Outbox) int {
	t.Helper()

	count, err := outbox.Parked()
	if err != nil {
		t.Fatal(err)
	}

	return count
}

// closeTestOutbox registers a checked close for one temporary outbox.
func closeTestOutbox(t *testing.T, outbox *Outbox) {
	t.Helper()
	t.Cleanup(func() {
		if err := outbox.Close(); err != nil {
			t.Error(err)
		}
	})
}

// outboxDestination is a controllable app shard that records deliveries and
// acknowledges either all events or only the first one.
type outboxDestination struct {
	down       atomic.Bool
	partial    atomic.Bool
	mu         sync.Mutex
	deliveries []Event
}

// TestOutboxTransportReleasesWaiterBeforeAppCommit verifies the hosted 202
// boundary: the in-memory batch waits for buffer.db, not for a reachable app.
func TestOutboxTransportReleasesWaiterBeforeAppCommit(t *testing.T) {
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	buffer := NewBuffer(outbox, 1, time.Hour)
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"}
	if err := buffer.AddAndWait(testContext(), event); err != nil {
		t.Fatal(err)
	}
	if queued(t, outbox) != 1 {
		t.Fatalf("durable boundary retained %d rows, want 1", queued(t, outbox))
	}
}

// ServeHTTP publishes one route and accepts private ingest batches.
func (d *outboxDestination) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case InternalDomainsPath:
		_ = json.NewEncoder(w).Encode(DomainsResponse{Shard: 1, Sites: []RoutedSite{{
			Site: sites.Site{ID: 1, AccountID: 10, Domain: "known.example"},
		}}})
	case InternalIngestPath:
		if d.down.Load() {
			http.Error(w, "app down", http.StatusServiceUnavailable)
			return
		}
		var batch IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.deliveries = append(d.deliveries, batch.Events...)
		d.mu.Unlock()
		committed := make([]uuid.UUID, 0, len(batch.Events))
		for _, event := range batch.Events {
			committed = append(committed, event.UUID)
			if d.partial.Load() {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(IngestResponse{Committed: committed})
	default:
		http.NotFound(w, r)
	}
}

// TestOutboxSurvivesAppFailureAndIngesterRestart verifies the central
// availability promise: a local commit earns 202 ownership, app downtime does
// not delete it, and a restarted ingester drains it after the app returns.
func TestOutboxSurvivesAppFailureAndIngesterRestart(t *testing.T) {
	destination := &outboxDestination{}
	destination.down.Store(true)
	server := httptest.NewServer(destination)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "buffer.db")
	signer := &InternalSigner{Key: "secret"}

	outbox, err := OpenOutbox(testContext(), path, []string{server.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example", Name: EventPageview}
	committed, err := outbox.Send(testContext(), 0, []Event{event})
	if err != nil || len(committed) != 1 || queued(t, outbox) != 1 {
		t.Fatalf("append committed=%v len=%d err=%v", committed, queued(t, outbox), err)
	}
	if err := outbox.deliver(testContext(), 0); err == nil || queued(t, outbox) != 1 {
		t.Fatalf("failed delivery err=%v len=%d", err, queued(t, outbox))
	}
	if err := outbox.Close(); err != nil {
		t.Fatal(err)
	}

	destination.down.Store(false)
	restarted, err := OpenOutbox(testContext(), path, []string{server.URL}, signer)
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, restarted)
	if queued(t, restarted) != 1 {
		t.Fatalf("restart retained %d events, want 1", queued(t, restarted))
	}
	if err := restarted.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := restarted.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	if queued(t, restarted) != 0 {
		t.Fatalf("acknowledged event remained in outbox: %d", queued(t, restarted))
	}
}

// TestOutboxDeletesOnlyExactAcknowledgments verifies that a partial app commit
// cannot lose an unacknowledged neighbor from the same delivery batch.
func TestOutboxDeletesOnlyExactAcknowledgments(t *testing.T) {
	destination := &outboxDestination{}
	destination.partial.Store(true)
	server := httptest.NewServer(destination)
	defer server.Close()
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{server.URL},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	if err := outbox.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"},
		{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"},
	}
	if _, err := outbox.Send(testContext(), 0, events); err != nil {
		t.Fatal(err)
	}
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatalf("a shard that answered was reported as failing: %v", err)
	}
	if queued(t, outbox) != 1 {
		t.Fatalf("partial acknowledgment left %d rows, want 1", queued(t, outbox))
	}
	var attempts int
	if err := outbox.DB.QueryRow("SELECT attempts FROM outbox").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("the unacknowledged event has %d attempts, want 1 so it is retried", attempts)
	}
}

// TestOutboxPersistsNoRawAddress verifies the privacy boundary against the
// serialized object that actually lands in buffer.db.
func TestOutboxPersistsNoRawAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "buffer.db")
	outbox, err := OpenOutbox(testContext(), path, []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example", Country: "US"}
	if _, err := outbox.Send(testContext(), 0, []Event{event, event}); err != nil {
		t.Fatal(err)
	}
	if queued(t, outbox) != 1 {
		t.Fatalf("duplicate UUID created %d rows, want 1", queued(t, outbox))
	}
	var payload []byte
	if err := outbox.DB.QueryRow("SELECT payload FROM outbox WHERE event_uuid = ?", event.UUID.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"IP", "ClientIP", "UserAgent", "RawUserAgent"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("raw identity field %q reached buffer.db", forbidden)
		}
	}
	if err := outbox.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOutboxReplaysParkedRows verifies operator review has a supported path
// back to delivery rather than requiring edits to the SQLite file.
func TestOutboxReplaysParkedRows(t *testing.T) {
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"}
	if _, err := outbox.Send(testContext(), 0, []Event{event}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := outbox.DB.QueryRow("SELECT id FROM outbox WHERE event_uuid = ?", event.UUID.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := outbox.parkIDs(testContext(), []int64{id}, "review me"); err != nil {
		t.Fatal(err)
	}
	if queued(t, outbox) != 0 || parked(t, outbox) != 1 {
		t.Fatalf("before replay active=%d parked=%d", queued(t, outbox), parked(t, outbox))
	}
	count, err := outbox.ReplayParked(testContext())
	if err != nil || count != 1 || queued(t, outbox) != 1 || parked(t, outbox) != 0 {
		t.Fatalf("replay count=%d active=%d parked=%d err=%v", count, queued(t, outbox), parked(t, outbox), err)
	}
}

// TestParkingIsAnnounced checks the dead letter is not silent. A parked event
// has been acknowledged to the browser and will never be delivered until a
// person runs the replay command, so nothing may reach that table quietly.
func TestParkingIsAnnounced(t *testing.T) {
	var lines bytes.Buffer

	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	outbox.Log = logger.New(logger.Options{Level: "error", Output: &lines})

	event := Event{UUID: uuid.New(), Shard: 0, AccountID: 10, SiteID: 1, Domain: "known.example"}
	if _, err := outbox.Send(testContext(), 0, []Event{event}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := outbox.DB.QueryRow("SELECT id FROM outbox WHERE event_uuid = ?", event.UUID.String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := outbox.parkIDs(testContext(), []int64{id}, "the destination refused it twenty times"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(lines.String(), "ingest events parked for operator review") {
		t.Fatalf("parking an event logged nothing: %q", lines.String())
	}
	if !strings.Contains(lines.String(), "refused it twenty times") {
		t.Fatalf("the park was logged without the reason: %q", lines.String())
	}
}

// TestStuckDeliveryIsReported is the store-and-forward blind spot. The client
// already has its 202, so an ingester that can reach no shard at all accepts
// events forever; without a log line nothing anywhere says the queue has
// stopped moving.
func TestStuckDeliveryIsReported(t *testing.T) {
	var lines bytes.Buffer

	// A destination that never answered the routing handshake, which is what a
	// wrong URL or a mismatched signing key looks like from here.
	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{"http://127.0.0.1:1"},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	outbox.Log = logger.New(logger.Options{Level: "error", Output: &lines})

	// Long enough for the run to reach outboxErrorAfter with room to spare, and
	// far short of the repeat.
	ctx, cancel := context.WithTimeout(testContext(), (outboxErrorAfter+5)*outboxPoll)
	defer cancel()
	outbox.Run(ctx)

	logged := strings.Count(lines.String(), "outbox delivery failed")
	if logged == 0 {
		t.Fatalf("a destination that never validated was never reported as an error: %q", lines.String())
	}

	// Several ticks failed, and a stuck destination must not turn the log into
	// a flood at the poll interval.
	if logged != 1 {
		t.Fatalf("one run of failures produced %d log lines, want 1", logged)
	}
}

// TestDeliveryRecoveryIsReported checks the other half: a queue that starts
// moving again says so, so nobody is left reading an hour-old error and
// wondering whether it is still true.
func TestDeliveryRecoveryIsReported(t *testing.T) {
	var lines bytes.Buffer

	outbox := &Outbox{Log: logger.New(logger.Options{Level: "info", Output: &lines})}
	state := &deliveryState{}

	outbox.report(state, errors.New("connection refused"), "outbox delivery", "shard", 1)
	for range outboxFailureReportEvery - 1 {
		outbox.report(state, errors.New("connection refused"), "outbox delivery", "shard", 1)
	}
	outbox.report(state, nil, "outbox delivery", "shard", 1)

	if got := strings.Count(lines.String(), "outbox delivery failed"); got != 3 {
		t.Fatalf("%d failures were reported across %d ticks, want the first, the one that became an error, and one repeat",
			got, outboxFailureReportEvery)
	}
	if !strings.Contains(lines.String(), "outbox delivery recovered") {
		t.Fatalf("a queue that started moving again said nothing: %q", lines.String())
	}

	// The run of failures is over, so the next one is news again.
	outbox.report(state, errors.New("connection refused"), "outbox delivery", "shard", 1)
	if got := strings.Count(lines.String(), "outbox delivery failed"); got != 4 {
		t.Fatalf("a fresh failure after recovery was reported %d times, want 4 in total", got)
	}
}

// TestAFailureThatClearsIsOnlyAWarning checks one failed try that the next tick
// clears is a warning, and a run of outboxErrorAfter failures is one error.
func TestAFailureThatClearsIsOnlyAWarning(t *testing.T) {
	var lines bytes.Buffer

	outbox := &Outbox{Log: logger.New(logger.Options{Level: "info", Output: &lines})}
	state := &deliveryState{}

	outbox.report(state, errors.New("database is locked"), "outbox delivery", "shard", 1)
	outbox.report(state, nil, "outbox delivery", "shard", 1)

	if strings.Contains(lines.String(), "level=ERROR") {
		t.Fatalf("one failure that cleared on the next tick was logged as an error: %q", lines.String())
	}
	if !strings.Contains(lines.String(), "level=WARN") || !strings.Contains(lines.String(), "database is locked") {
		t.Fatalf("the failure was not reported as a warning with its reason: %q", lines.String())
	}

	lines.Reset()
	for range outboxErrorAfter {
		outbox.report(state, errors.New("database is locked"), "outbox delivery", "shard", 1)
	}

	if got := strings.Count(lines.String(), "level=ERROR"); got != 1 ||
		!strings.Contains(lines.String(), fmt.Sprintf("consecutive_failures=%d ", outboxErrorAfter)) {
		t.Fatalf("a run of %d failures produced %d errors, want one naming the run: %q",
			outboxErrorAfter, got, lines.String())
	}
}

// accountsDestination is an app shard that owns two accounts and can refuse
// every write for either of them, the way a shard answers when one account's
// database cannot take writes.
type accountsDestination struct {
	mu      sync.Mutex
	failing map[int64]bool
	down    bool
	batches [][]Event
}

// setFailing marks an account's writes as failing or working.
func (d *accountsDestination) setFailing(account int64, failing bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failing == nil {
		d.failing = map[int64]bool{}
	}
	d.failing[account] = failing
}

// delivered counts the events of one account the shard committed.
func (d *accountsDestination) delivered(account int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, batch := range d.batches {
		for _, event := range batch {
			if event.AccountID == account && !d.failing[account] {
				count++
			}
		}
	}

	return count
}

// lastBatch returns how many events of each account the latest request carried.
func (d *accountsDestination) lastBatch() map[int64]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	counts := map[int64]int{}
	if len(d.batches) == 0 {
		return counts
	}
	for _, event := range d.batches[len(d.batches)-1] {
		counts[event.AccountID]++
	}

	return counts
}

// ServeHTTP publishes both accounts' sites and commits every event except
// those of a failing account, answering 503 with the committed list as the
// real shard does.
func (d *accountsDestination) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case InternalDomainsPath:
		_ = json.NewEncoder(w).Encode(DomainsResponse{Shard: 1, Sites: []RoutedSite{
			{Site: sites.Site{ID: 1, AccountID: 10, Domain: "a.example"}},
			{Site: sites.Site{ID: 2, AccountID: 20, Domain: "b.example"}},
			{Site: sites.Site{ID: 3, AccountID: 30, Domain: "c.example"}},
		}})
	case InternalIngestPath:
		var batch IngestBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		if d.down {
			d.mu.Unlock()
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		d.batches = append(d.batches, batch.Events)
		response := IngestResponse{}
		var failures []error
		for _, event := range batch.Events {
			if !d.failing[event.AccountID] {
				response.Committed = append(response.Committed, event.UUID)
			} else if _, seen := response.Failed[event.AccountID]; !seen {
				failure := &AccountError{AccountID: event.AccountID,
					Err: fmt.Errorf("account %d cannot take writes", event.AccountID)}
				failures = append(failures, failure)
				response.Failed = accountFailures(errors.Join(failures...))
			}
		}
		if len(failures) > 0 {
			response.Error = errors.Join(failures...).Error()
		}
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if response.Error != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(response)
	default:
		http.NotFound(w, r)
	}
}

// accountsOutbox opens an outbox delivering to an accountsDestination, with a
// clock the test moves by hand.
func accountsOutbox(t *testing.T, destination *accountsDestination, log *bytes.Buffer) (*Outbox, *time.Time) {
	t.Helper()

	server := httptest.NewServer(destination)
	t.Cleanup(server.Close)

	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{server.URL},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	if err := outbox.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	if log != nil {
		outbox.Log = logger.New(logger.Options{Level: "info", Output: log})
	}

	now := time.Now().UTC()
	outbox.Now = func() time.Time { return now }

	return outbox, &now
}

// queueEvents appends count events for one account's site.
func queueEvents(t *testing.T, outbox *Outbox, account int64, count int) {
	t.Helper()

	domain := fmt.Sprintf("site%d.example", account)
	if known, ok := map[int64]string{10: "a.example", 20: "b.example", 30: "c.example"}[account]; ok {
		domain = known
	}
	events := make([]Event, 0, count)
	for range count {
		events = append(events, Event{UUID: uuid.New(), Shard: 0, AccountID: account, SiteID: account / 10,
			Domain: domain, Name: EventPageview})
	}
	if _, err := outbox.Send(testContext(), 0, events); err != nil {
		t.Fatal(err)
	}
}

// queuedFor counts one account's rows still waiting in the outbox.
func queuedFor(t *testing.T, outbox *Outbox, account int64) int {
	t.Helper()

	var count int
	if err := outbox.DB.QueryRow("SELECT COUNT(*) FROM outbox WHERE account_id = ?", account).Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count
}

// TestOneFailingAccountDoesNotHoldUpTheOthers is the outage this queue must
// survive: one account's database stops taking writes with a deep backlog
// queued ahead of everyone else. Every other account's events must still go
// through within a few ticks, and the broken account's events must wait in
// the queue rather than be parked.
func TestOneFailingAccountDoesNotHoldUpTheOthers(t *testing.T) {
	destination := &accountsDestination{}
	destination.setFailing(10, true)
	outbox, _ := accountsOutbox(t, destination, nil)

	queueEvents(t, outbox, 10, 10000)
	queueEvents(t, outbox, 20, 100)

	for range 3 {
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatalf("a shard that answered was reported as failing: %v", err)
		}
	}

	if got := destination.delivered(20); got != 100 {
		t.Fatalf("the healthy account delivered %d of 100 events in three ticks", got)
	}
	if got := queuedFor(t, outbox, 20); got != 0 {
		t.Fatalf("%d of the healthy account's events are still queued", got)
	}
	if got := queuedFor(t, outbox, 10); got != 10000 {
		t.Fatalf("the failing account has %d events queued, want all 10000", got)
	}
	if got := parked(t, outbox); got != 0 {
		t.Fatalf("%d events were parked because an account was failing", got)
	}
}

// TestAFailingAccountIsHeldAndDrainsOnceItRecovers checks the broken account
// is tried less and less often rather than in every batch, is reported once
// when it breaks and once when it recovers, and then drains its whole backlog.
func TestAFailingAccountIsHeldAndDrainsOnceItRecovers(t *testing.T) {
	var lines bytes.Buffer
	destination := &accountsDestination{}
	destination.setFailing(10, true)
	outbox, now := accountsOutbox(t, destination, &lines)

	queueEvents(t, outbox, 10, 1200)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	requests := len(destination.batches)

	// Held: the next tick sends nothing for it, although most of its rows were
	// never tried and are due.
	queueEvents(t, outbox, 20, 5)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	if got := destination.lastBatch(); got[10] != 0 || got[20] != 5 {
		t.Fatalf("while held the batch carried %v, want only the other account's 5 events", got)
	}

	// Each failed try lengthens the hold, up to the cap.
	for range 20 {
		*now = now.Add(outboxAccountHoldMax)
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(destination.batches) - requests; got != 21 {
		t.Fatalf("%d requests went out, want one per tick", got)
	}
	hold := outbox.holds[accountKey{shard: 0, account: 10}]
	if hold == nil || hold.until.Sub(*now) != outboxAccountHoldMax {
		t.Fatalf("after 21 failures the hold is %v, want %v", hold, outboxAccountHoldMax)
	}

	destination.setFailing(10, false)
	for range 10 {
		*now = now.Add(outboxAccountHoldMax)
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := queuedFor(t, outbox, 10); got != 0 {
		t.Fatalf("%d of the recovered account's events are still queued", got)
	}
	if got := parked(t, outbox); got != 0 {
		t.Fatalf("%d events were parked", got)
	}

	// A warning when it breaks, an error once it keeps failing, and one repeat.
	failed := strings.Count(lines.String(), "outbox account delivery failed")
	if failed != 3 || !strings.Contains(lines.String(), "account_id=10") ||
		!strings.Contains(lines.String(), "account 10 cannot take writes") {
		t.Fatalf("21 failed tries were reported %d times, want three, naming the account and the reason: %q",
			failed, lines.String())
	}
	if warned := strings.Count(lines.String(), "level=WARN"); warned != 1 {
		t.Fatalf("%d failures were warnings, want only the first: %q", warned, lines.String())
	}
	if errored := strings.Count(lines.String(), "level=ERROR"); errored != 2 ||
		!strings.Contains(lines.String(), fmt.Sprintf("consecutive_failures=%d ", outboxErrorAfter)) {
		t.Fatalf("%d failures were errors, want the run reaching %d and one repeat: %q",
			errored, outboxErrorAfter, lines.String())
	}
	if got := strings.Count(lines.String(), "outbox account delivery recovered"); got != 1 {
		t.Fatalf("the recovery was reported %d times, want once: %q", got, lines.String())
	}
}

// TestABatchIsSharedBetweenAccounts checks one busy account cannot fill a batch
// while other accounts wait, and that an account alone in the queue still gets
// the whole batch.
func TestABatchIsSharedBetweenAccounts(t *testing.T) {
	destination := &accountsDestination{}
	outbox, now := accountsOutbox(t, destination, nil)

	queueEvents(t, outbox, 10, 1000)
	queueEvents(t, outbox, 20, 100)
	queueEvents(t, outbox, 30, 5)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	want := map[int64]int{10: outboxBatchSize - 105, 20: 100, 30: 5}
	if got := destination.lastBatch(); got[10] != want[10] || got[20] != want[20] || got[30] != want[30] {
		t.Fatalf("the first batch carried %v, want %v", got, want)
	}

	// A queue far behind sends catch-up batches, and one account alone in it
	// fills them.
	queueEvents(t, outbox, 10, outboxCatchUpBatch)
	*now = now.Add(2 * outboxCatchUpAfter)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	if got := destination.lastBatch(); got[10] != outboxCatchUpBatch {
		t.Fatalf("a lone account sent %v, want a full batch of %d", got, outboxCatchUpBatch)
	}
}

// holdFor reads how long an account's current hold has left.
func holdFor(outbox *Outbox, account int64, now time.Time) time.Duration {
	outbox.holdsMu.Lock()
	defer outbox.holdsMu.Unlock()

	hold := outbox.holds[accountKey{shard: 0, account: account}]
	if hold == nil {
		return 0
	}

	return hold.until.Sub(now)
}

// TestAHoldDoublesWithEachFailedTry checks a broken account is tried less and
// less often, up to the cap, rather than at a fixed rate.
func TestAHoldDoublesWithEachFailedTry(t *testing.T) {
	destination := &accountsDestination{}
	destination.setFailing(10, true)
	outbox, now := accountsOutbox(t, destination, nil)
	queueEvents(t, outbox, 10, 50)

	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, seconds := range want {
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatal(err)
		}
		hold := holdFor(outbox, 10, *now)
		if hold != seconds*time.Second {
			t.Fatalf("after failure %d the hold is %v, want %v", i+1, hold, seconds*time.Second)
		}

		// Just before the hold runs out the account is still left alone.
		*now = now.Add(hold - time.Second)
		requests := len(destination.batches)
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatal(err)
		}
		if len(destination.batches) != requests {
			t.Fatalf("the account was tried %v into a %v hold", hold-time.Second, hold)
		}
		*now = now.Add(time.Second)
	}
}

// TestMoreAccountsThanABatchTakeTurns checks that when more accounts are
// waiting than a batch has room for, each gets one row before any gets a
// second, the oldest first, and the account left out is not left out for long.
func TestMoreAccountsThanABatchTakeTurns(t *testing.T) {
	destination := &accountsDestination{}
	outbox, _ := accountsOutbox(t, destination, nil)

	const last = (outboxBatchSize + 1) * 1000
	for account := int64(1); account <= outboxBatchSize+1; account++ {
		queueEvents(t, outbox, account*1000, 2)
	}

	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}
	first := destination.lastBatch()
	if len(first) != outboxBatchSize {
		t.Fatalf("the first batch carried %d accounts, want %d with one row each", len(first), outboxBatchSize)
	}
	if _, ok := first[last]; ok {
		t.Fatal("the newest account went ahead of an older one")
	}

	for range 2 {
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := destination.delivered(last); got != 2 {
		t.Fatalf("the account left out had %d of its 2 rows delivered after three batches", got)
	}
}

// TestAShardThatWritesNothingIsReportedAsFailing checks the shard-level report
// still sees a shard that answers but cannot write any account, as when its
// disk is full, and does not mistake one broken account for that.
func TestAShardThatWritesNothingIsReportedAsFailing(t *testing.T) {
	destination := &accountsDestination{}
	destination.setFailing(10, true)
	outbox, _ := accountsOutbox(t, destination, nil)

	queueEvents(t, outbox, 10, 5)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatalf("one broken account was reported as a broken shard: %v", err)
	}

	destination.setFailing(20, true)
	queueEvents(t, outbox, 20, 5)
	queueEvents(t, outbox, 30, 5)
	destination.setFailing(30, true)
	if err := outbox.deliver(testContext(), 0); err == nil {
		t.Fatal("a shard that wrote none of the accounts in the batch was reported as working")
	}
}

// TestEachHeldAccountIsReportedWithItsOwnReason checks two accounts failing in
// the same batch are each logged with their own cause.
func TestEachHeldAccountIsReportedWithItsOwnReason(t *testing.T) {
	var lines bytes.Buffer
	destination := &accountsDestination{}
	destination.setFailing(10, true)
	destination.setFailing(20, true)
	outbox, _ := accountsOutbox(t, destination, &lines)

	queueEvents(t, outbox, 10, 1)
	queueEvents(t, outbox, 20, 1)
	queueEvents(t, outbox, 30, 1)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}

	for _, line := range strings.Split(strings.TrimSpace(lines.String()), "\n") {
		for _, account := range []int64{10, 20} {
			if !strings.Contains(line, fmt.Sprintf("account_id=%d ", account)) {
				continue
			}
			other := 30 - account
			if !strings.Contains(line, fmt.Sprintf("account %d cannot", account)) ||
				strings.Contains(line, fmt.Sprintf("account %d cannot", other)) {
				t.Fatalf("account %d was reported with the wrong reason: %s", account, line)
			}
		}
	}
	var lastError string
	if err := outbox.DB.QueryRow("SELECT last_error FROM outbox WHERE account_id = 10").Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError != "account 10 cannot take writes" {
		t.Fatalf("account 10's row kept the reason %q", lastError)
	}
}

// TestAnOldHoldDoesNotSilenceANewFailure checks a hold left behind by an
// earlier failure, whose rows left another way, does not swallow the first
// report of a new one.
func TestAnOldHoldDoesNotSilenceANewFailure(t *testing.T) {
	var lines bytes.Buffer
	destination := &accountsDestination{}
	destination.setFailing(10, true)
	outbox, now := accountsOutbox(t, destination, &lines)

	queueEvents(t, outbox, 10, 1)
	for range 3 {
		*now = now.Add(time.Minute)
		if err := outbox.deliver(testContext(), 0); err != nil {
			t.Fatal(err)
		}
	}

	// The rows leave without the shard answering for them, and much later the
	// account fails again.
	if _, err := outbox.DB.Exec("DELETE FROM outbox"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	queueEvents(t, outbox, 10, 1)
	if err := outbox.deliver(testContext(), 0); err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(lines.String(), "consecutive_failures=1 "); got != 2 {
		t.Fatalf("the new failure was not reported as a first failure: %q", lines.String())
	}
	if hold := holdFor(outbox, 10, *now); hold != time.Second {
		t.Fatalf("the new failure started with a %v hold, want 1s", hold)
	}
}

// TestIdleTicksDoNotReportRecovery checks a shard that is down is not
// reported as recovered on the ticks in between, when its rows are waiting
// out their backoff and nothing is sent.
func TestIdleTicksDoNotReportRecovery(t *testing.T) {
	var lines bytes.Buffer
	destination := &accountsDestination{down: true}
	server := httptest.NewServer(destination)
	defer server.Close()

	outbox, err := OpenOutbox(testContext(), filepath.Join(t.TempDir(), "buffer.db"), []string{server.URL},
		&InternalSigner{Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	closeTestOutbox(t, outbox)
	if err := outbox.Router.RefreshAll(testContext()); err != nil {
		t.Fatal(err)
	}
	outbox.Log = logger.New(logger.Options{Level: "info", Output: &lines})
	queueEvents(t, outbox, 10, 1)

	ctx, cancel := context.WithTimeout(testContext(), 6*outboxPoll)
	defer cancel()
	outbox.Run(ctx)

	if !strings.Contains(lines.String(), "outbox delivery failed") {
		t.Fatalf("a shard answering 502 was never reported: %q", lines.String())
	}
	if strings.Contains(lines.String(), "outbox delivery recovered") {
		t.Fatalf("a shard that never accepted a batch was reported as recovered: %q", lines.String())
	}
}
