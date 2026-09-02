//
// writer_test.go
// Tests for dedupe, per-account transactions, session merges and pruning.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// TestWriterDropsAStaleRouteAfterCrossProcessDeletion proves an already-open
// shard handle cannot write or recreate an account after another manager has
// established the durable tombstone.
func TestWriterDropsAStaleRouteAfterCrossProcessDeletion(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)
	deleter := accounts.NewManager(dataDir)
	t.Cleanup(func() { checkClose(t, "writer account manager", manager.CloseAll) })
	t.Cleanup(func() { checkClose(t, "deletion account manager", deleter.CloseAll) })

	writer := NewWriter(manager)
	first := writerEvent(1, EventPageview, fixtureStart.Unix(), "/before")
	if _, err := writer.Write(ctx, []Event{first}); err != nil {
		t.Fatal(err)
	}
	if err := deleter.Block(1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(accounts.Dir(dataDir, 1)); err != nil {
		t.Fatal(err)
	}

	stale := writerEvent(1, EventPageview, fixtureStart.Add(time.Minute).Unix(), "/after")
	committed, err := writer.Write(ctx, []Event{stale})
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 1 || committed[0] != stale.UUID {
		t.Fatalf("stale event acknowledgement = %v", committed)
	}
	if _, err := os.Stat(accounts.Dir(dataDir, 1)); !os.IsNotExist(err) {
		t.Fatalf("stale route recreated the deleted directory: %v", err)
	}
}

// TestWriterWriteAndBeginDeletionShareOneAccountFence stops an accepted write
// at its final rollback boundary, starts deletion after the durable tombstone
// is visible, and proves deletion cannot close the handle until that write has
// committed and released its lease. A later queued event is then acknowledged
// as an intentional tombstone drop without recreating the removed shard.
func TestWriterWriteAndBeginDeletionShareOneAccountFence(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)
	t.Cleanup(func() { checkClose(t, "overlap account manager", manager.CloseAll) })

	writer := NewWriter(manager)
	entered := make(chan struct{})
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	var once sync.Once
	writer.Failpoint = func(stage string) error {
		if stage == WriterStageBeforeCommit {
			once.Do(func() { close(entered) })
			<-resume
		}
		return nil
	}

	event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/ordered-before-deletion")
	type writeResult struct {
		ids []uuid.UUID
		err error
	}
	written := make(chan writeResult, 1)
	go func() {
		ids, err := writer.Write(ctx, []Event{event})
		written <- writeResult{ids: ids, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("accepted writer never reached the pre-commit boundary")
	}

	deleted := make(chan *accounts.DeletionGuard, 1)
	deleteErrors := make(chan error, 1)
	go func() {
		guard, err := manager.BeginDeletion(1)
		if err != nil {
			deleteErrors <- err
			return
		}
		deleted <- guard
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(accounts.TombstonePath(dataDir, 1)); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("deletion never published its tombstone")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case guard := <-deleted:
		_ = guard.Release()
		t.Fatal("deletion acquired its exclusive fence before the accepted write committed")
	case err := <-deleteErrors:
		t.Fatal(err)
	case <-time.After(150 * time.Millisecond):
	}

	close(resume)
	var result writeResult
	select {
	case result = <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("accepted writer did not finish after its commit boundary was released")
	}
	if result.err != nil {
		t.Fatalf("accepted writer crossed deletion with error: %v", result.err)
	}
	if len(result.ids) != 1 || result.ids[0] != event.UUID {
		t.Fatalf("accepted writer settled %v, want %s", result.ids, event.UUID)
	}

	var guard *accounts.DeletionGuard
	select {
	case guard = <-deleted:
	case err := <-deleteErrors:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("deletion did not acquire its fence after the accepted write released")
	}
	account, err := guard.OpenAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if account == nil {
		t.Fatal("deletion could not inspect the shard committed immediately before its fence")
	}
	for table, query := range map[string]string{
		"event":   "SELECT COUNT(*) FROM events",
		"receipt": "SELECT COUNT(*) FROM recent_event_ids",
	} {
		var count int
		if err := account.Reader().QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("committed %s count = %d, want 1", table, count)
		}
	}
	if err := guard.CloseAccount(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(accounts.Dir(dataDir, 1)); err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}

	after := writerEvent(1, EventPageview, fixtureStart.Add(time.Minute).Unix(), "/rejected-after-deletion")
	settled, err := writer.Write(ctx, []Event{after})
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 1 || settled[0] != after.UUID {
		t.Fatalf("post-deletion event acknowledgement = %v, want %s", settled, after.UUID)
	}
	if _, err := os.Stat(accounts.Dir(dataDir, 1)); !os.IsNotExist(err) {
		t.Fatalf("post-deletion write recreated the account directory: %v", err)
	}
}

// rejectHostnameShield rejects every event as a hostname policy failure.
type rejectHostnameShield struct{}

// Allowed returns the explicit hostname rejection used by the transaction tests.
func (rejectHostnameShield) Allowed(int64, string, string, string) (bool, string) {
	return false, ReasonHostnameNotAllowed
}

// recordingAllowShield simulates a hostname becoming allowed after the public
// request tier produced a stale rejection advisory.
type recordingAllowShield struct{}

// Allowed permits the now-live hostname rule.
func (*recordingAllowShield) Allowed(int64, string, string, string) (bool, string) {
	return true, ""
}

// newWriter builds a writer over a temporary data directory. Account databases
// are created on first use, so nothing has to be set up beforehand.
func newWriter(t testing.TB) (*Writer, *accounts.Manager) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { checkClose(t, "account manager", manager.CloseAll) })

	writer := NewWriter(manager)
	writer.Now = func() time.Time { return fixtureStart }

	return writer, manager
}

// checkClose runs one test cleanup and reports a failure against the test that
// owns the resource instead of silently discarding it.
func checkClose(t testing.TB, name string, close func() error) {
	t.Helper()
	if err := close(); err != nil {
		t.Errorf("close %s: %v", name, err)
	}
}

// countRows is the "how many are actually on disk" check every test here makes.
func countRows(t testing.TB, manager *accounts.Manager, accountID int64, query string) int64 {
	t.Helper()

	account, err := manager.Open(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := account.Reader().QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count
}

// writerEvent builds an event ready to be written.
func writerEvent(accountID int64, name string, timestamp int64, path string) Event {
	e := event(name, timestamp, path)
	e.AccountID = accountID
	e.SiteID = accountID
	e.UserID = testUser + accountID
	e.Browser = "Chrome"
	e.Country = "US"
	e.Region = "US-NY"
	e.City = "Syracuse"
	e.Source = "Google"
	e.Channel = "Organic Search"

	return e
}

// rejectingShardShield blocks one path so a test can distinguish a final
// shard-side drop from an event the writer actually commits.
type rejectingShardShield struct{}

// Allowed rejects only /blocked with the same closed reason production uses.
func (rejectingShardShield) Allowed(_ int64, _ string, pathname, _ string) (bool, string) {
	return pathname != "/blocked", ReasonShieldPage
}

// TestCityIsInternedLikeEveryOtherPlace pins the column the geolocation fix
// depends on. The database we ship carries city names and no ids, so a city
// that did not reach dim_city would be a permanently empty column on every
// event — which is the state this replaced.
func TestCityIsInternedLikeEveryOtherPlace(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	if _, err := writer.Write(ctx, []Event{writerEvent(1, EventPageview, fixtureStart.Unix(), "/")}); err != nil {
		t.Fatal(err)
	}

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Read the name back through the dimension table on both fact tables, which
	// is exactly what a report does.
	for _, query := range []string{
		"SELECT c.value FROM events e JOIN dim_city c ON c.id = e.city_id",
		"SELECT c.value FROM sessions s JOIN dim_city c ON c.id = s.city_id",
	} {
		var city string
		if err := account.Reader().QueryRow(query).Scan(&city); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if city != "Syracuse" {
			t.Errorf("%s gave %q, want Syracuse", query, city)
		}
	}
}

// TestAnEventWithNoCityStoresTheEmptyID checks the other half of interning: a
// visitor the database cannot place has to land on id 0 rather than on a NULL
// that every GROUP BY would then have to handle specially.
func TestAnEventWithNoCityStoresTheEmptyID(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	unplaced := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")
	unplaced.Region = ""
	unplaced.City = ""

	if _, err := writer.Write(ctx, []Event{unplaced}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT city_id FROM events"); got != 0 {
		t.Fatalf("an unplaced visitor stored city_id %d, want 0", got)
	}
}

// TestWriteIsIdempotent is the point of the dedupe table. The classic case is a
// shard that commits and then loses the acknowledgement: the sender retries and
// the event would otherwise be written twice, which is a wrong number with no
// obvious cause.
func TestWriteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	batch := []Event{
		writerEvent(1, EventPageview, fixtureStart.Unix(), "/"),
		writerEvent(1, EventPageview, fixtureStart.Unix()+30, "/pricing"),
	}

	first, err := writer.Write(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("committed %d events, want 2", len(first))
	}

	// The same batch again, exactly as a redelivery would arrive.
	second, err := writer.Write(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("a redelivery reported %d committed, want 2 — an unacknowledged retry would loop forever", len(second))
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("events table holds %d rows after a redelivery, want 2", got)
	}
	if got := countRows(t, manager, 1, "SELECT pageviews FROM sessions"); got != 2 {
		t.Fatalf("session pageviews = %d, want 2 — the fold counted a duplicate", got)
	}
}

// TestIndependentWritersClaimOneUUIDAtomically exercises separate process-local
// locks over the same account file. SQLite must settle every retry while only
// one process writes the fact.
func TestIndependentWritersClaimOneUUIDAtomically(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	firstManager := accounts.NewManager(dir)
	secondManager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = firstManager.CloseAll() })
	t.Cleanup(func() { _ = secondManager.CloseAll() })

	if _, err := firstManager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := secondManager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}

	first := NewWriter(firstManager)
	second := NewWriter(secondManager)
	first.Now = func() time.Time { return fixtureStart }
	second.Now = first.Now
	event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/atomic")
	writers := []*Writer{first, second}

	var wait sync.WaitGroup
	errors := make(chan error, 32)
	for i := 0; i < cap(errors); i++ {
		wait.Add(1)
		go func(writer *Writer) {
			defer wait.Done()
			settled, err := writer.Write(ctx, []Event{event})
			if err == nil && len(settled) != 1 {
				err = fmt.Errorf("settled %d UUIDs, want 1", len(settled))
			}
			errors <- err
		}(writers[i%len(writers)])
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got := countRows(t, firstManager, 1, "SELECT COUNT(*) FROM events"); got != 1 {
		t.Fatalf("independent writers stored %d event rows, want 1", got)
	}
	if got := countRows(t, firstManager, 1, "SELECT pageviews FROM sessions"); got != 1 {
		t.Fatalf("independent writers counted %d pageviews, want 1", got)
	}
}

// TestIndependentWritersReserveDistinctSessionOwnership races separate SQLite
// connections creating visits for different people. Session IDs must never be
// reused for different visitor ownership.
func TestIndependentWritersReserveDistinctSessionOwnership(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	firstManager := accounts.NewManager(dir)
	secondManager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = firstManager.CloseAll() })
	t.Cleanup(func() { _ = secondManager.CloseAll() })

	if _, err := firstManager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := secondManager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	first := NewWriter(firstManager)
	second := NewWriter(secondManager)
	first.Now = func() time.Time { return fixtureStart }
	second.Now = first.Now

	one := writerEvent(1, EventPageview, fixtureStart.Unix(), "/one")
	one.UUID = uuid.New()
	one.UserID = 101
	two := writerEvent(1, EventPageview, fixtureStart.Unix(), "/two")
	two.UUID = uuid.New()
	two.UserID = 202

	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, item := range []struct {
		writer *Writer
		event  Event
	}{{first, one}, {second, two}} {
		go func(item struct {
			writer *Writer
			event  Event
		}) {
			<-start
			_, err := item.writer.Write(ctx, []Event{item.event})
			errors <- err
		}(item)
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}

	if got := countRows(t, firstManager, 1, "SELECT COUNT(DISTINCT id) FROM sessions"); got != 2 {
		t.Fatalf("independent writers created %d session identities, want 2", got)
	}
	if got := countRows(t, firstManager, 1, `
		SELECT COUNT(*) FROM events e
		JOIN sessions s ON s.id = e.session_id
		WHERE e.site_id = s.site_id AND e.user_id = s.user_id`); got != 2 {
		t.Fatalf("%d events link to the correctly owned session, want 2", got)
	}
}

// TestIndependentWritersShareOneVisitorSession races different UUIDs for one
// visitor. Durable fold state must keep both pageviews in one visit.
func TestIndependentWritersShareOneVisitorSession(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	firstManager := accounts.NewManager(dir)
	secondManager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = firstManager.CloseAll() })
	t.Cleanup(func() { _ = secondManager.CloseAll() })

	if _, err := firstManager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := secondManager.Open(ctx, 1); err != nil {
		t.Fatal(err)
	}
	first := NewWriter(firstManager)
	second := NewWriter(secondManager)
	first.Now = func() time.Time { return fixtureStart }
	second.Now = first.Now

	one := writerEvent(1, EventPageview, fixtureStart.Unix(), "/one")
	two := writerEvent(1, EventPageview, fixtureStart.Unix()+10, "/two")
	two.UUID = uuid.New()
	start := make(chan struct{})
	errors := make(chan error, 2)
	for _, item := range []struct {
		writer *Writer
		event  Event
	}{{first, one}, {second, two}} {
		go func(item struct {
			writer *Writer
			event  Event
		}) {
			<-start
			_, err := item.writer.Write(ctx, []Event{item.event})
			errors <- err
		}(item)
	}
	close(start)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}

	if got := countRows(t, firstManager, 1, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("overlapping writers created %d sessions for one visitor, want 1", got)
	}
	if got := countRows(t, firstManager, 1, "SELECT pageviews FROM sessions"); got != 2 {
		t.Fatalf("shared visitor session has %d pageviews, want 2", got)
	}
}

// TestHostnameRejectionClaimAndFactShareEveryKillBoundary proves the UUID
// receipt and hostname evidence always commit or roll back together.
func TestHostnameRejectionClaimAndFactShareEveryKillBoundary(t *testing.T) {
	for _, stage := range []string{WriterStageAfterClaim, WriterStageAfterRejection, WriterStageBeforeCommit} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			writer, manager := newWriter(t)
			writer.Shield = rejectHostnameShield{}
			writer.Counters = NewCounters()
			writer.Failpoint = func(current string) error {
				if current == stage {
					return errors.New("simulated process kill")
				}
				return nil
			}

			event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/rejected")
			event.Hostname = "preview.example.net"
			if _, err := writer.Write(ctx, []Event{event}); err == nil {
				t.Fatal("kill boundary write committed")
			}
			for table, query := range map[string]string{
				"receipt": "SELECT COUNT(*) FROM recent_event_ids", "rejection": "SELECT COUNT(*) FROM hostname_rejections",
				"event": "SELECT COUNT(*) FROM events",
			} {
				if got := countRows(t, manager, 1, query); got != 0 {
					t.Fatalf("%s survived rollback with %d rows", table, got)
				}
			}

			writer.Failpoint = nil
			for range 2 {
				settled, err := writer.Write(ctx, []Event{event})
				if err != nil {
					t.Fatal(err)
				}
				if len(settled) != 1 {
					t.Fatalf("replay settled %d UUIDs, want 1", len(settled))
				}
			}
			if got := countRows(t, manager, 1, "SELECT events FROM hostname_rejections"); got != 1 {
				t.Fatalf("rejection count = %d, want 1", got)
			}
		})
	}
}

// TestHostnameRejectionTransactionEnforcesTheDurableCap proves cardinality is
// bounded in SQLite while preserving the exact rejected event total.
func TestHostnameRejectionTransactionEnforcesTheDurableCap(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	writer.Shield = rejectHostnameShield{}
	batch := make([]Event, 0, MaxRejectedHostnames+5)
	for i := 0; i < MaxRejectedHostnames+5; i++ {
		event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/rejected")
		event.UUID = uuid.New()
		event.Hostname = fmt.Sprintf("preview-%02d.example.net", i)
		batch = append(batch, event)
	}

	if _, err := writer.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM hostname_rejections"); got != MaxRejectedHostnames+1 {
		t.Fatalf("rejection table has %d rows, want %d", got, MaxRejectedHostnames+1)
	}
	if got := countRows(t, manager, 1, "SELECT SUM(events) FROM hostname_rejections"); got != int64(len(batch)) {
		t.Fatalf("rejection table counted %d events, want %d", got, len(batch))
	}
}

// TestMissingHostnameUsesTheAggregateRejectionBucket keeps malformed or absent
// page URLs visible without offering a one-click rule for a hostname that can
// never be valid.
func TestMissingHostnameUsesTheAggregateRejectionBucket(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	writer.Shield = rejectHostnameShield{}
	event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")
	event.Hostname = NoneHostname

	if _, err := writer.Write(ctx, []Event{event}); err != nil {
		t.Fatal(err)
	}

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var hostname string
	if err := account.Reader().QueryRowContext(ctx,
		"SELECT hostname FROM hostname_rejections").Scan(&hostname); err != nil {
		t.Fatal(err)
	}
	if hostname != OtherRejectedHostname {
		t.Fatalf("missing URL recorded as %q, want aggregate %q", hostname, OtherRejectedHostname)
	}
}

// TestShardAllowsAHostnameNewlyValidAfterIngest proves a stale public advisory
// cannot override the writer's live hostname policy.
func TestShardAllowsAHostnameNewlyValidAfterIngest(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	writer.Shield = &recordingAllowShield{}
	writer.Counters = NewCounters()
	// A hostname the public tier refuses. The handler forwards it anyway, and
	// the live shield below is the only thing entitled to decide.
	event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/rejected")
	event.Hostname = "preview.example.net"

	for range 2 {
		committed, err := writer.Write(ctx, []Event{event})
		if err != nil {
			t.Fatal(err)
		}
		if len(committed) != 1 {
			t.Fatalf("settled %d events, want 1", len(committed))
		}
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 1 {
		t.Fatalf("newly valid hostname stored %d events, want 1", got)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM hostname_rejections"); got != 0 {
		t.Fatalf("newly valid hostname produced %d durable rejections", got)
	}
}

// TestWriterReportsOnlyFinalOutcomes checks the recorder-facing contract at
// the point that knows the truth: stored rows are accepted, classified rows
// are accepted with a reason, and a live shard shield is a drop.
func TestWriterReportsOnlyFinalOutcomes(t *testing.T) {
	writer, manager := newWriter(t)
	writer.Shield = rejectingShardShield{}

	var outcomes []Observation
	writer.Observer = ObserverFunc(func(observation Observation) {
		outcomes = append(outcomes, observation)
	})

	plain := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")
	classified := writerEvent(1, EventPageview, fixtureStart.Unix()+1, "/bot")
	classified.BotReason = ReasonBot
	blocked := writerEvent(1, EventPageview, fixtureStart.Unix()+2, "/blocked")

	committed, err := writer.Write(context.Background(), []Event{plain, classified, blocked})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(committed) != 3 {
		t.Fatalf("acknowledged %d outcomes, want 3", len(committed))
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("stored %d events, want 2", got)
	}

	accepted := 0
	classifiedAccepted := 0
	shielded := 0

	for _, outcome := range outcomes {
		if !outcome.OutcomeOnly || outcome.Pending {
			t.Fatalf("writer emitted a non-final observation: %+v", outcome)
		}

		switch {
		case outcome.Accepted && outcome.DropReason == "":
			accepted++
		case outcome.Accepted && outcome.DropReason == ReasonBot:
			classifiedAccepted++
		case !outcome.Accepted && outcome.DropReason == ReasonShieldPage:
			shielded++
		}
	}

	if accepted != 1 || classifiedAccepted != 1 || shielded != 1 {
		t.Fatalf("final outcomes plain=%d classified=%d shielded=%d", accepted, classifiedAccepted, shielded)
	}
}

// TestBatchPastTheBindLimitStillDrains is the backed-up buffer.
//
// A batch is a few hundred events while everything is healthy, but the buffer
// keeps accepting while a flush runs slow, so the batch that arrives after a
// stall is tens of thousands. SQLite refuses a statement with more than about
// thirty-two thousand bound parameters, and a write that binds one per event
// then fails on the one batch that most needs to be written, is requeued
// unchanged, and fails identically forever. A buffer that can never drain is
// worse than a slow one.
func TestBatchPastTheBindLimitStillDrains(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	// Past SQLite's bind limit, which is where a per-event parameter fails.
	const events = 40_000

	// One visitor inside one session window, so the batch exercises the write
	// path's size rather than forty thousand separate visits.
	batch := make([]Event, 0, events)
	for i := range events {
		e := writerEvent(1, EventPageview, fixtureStart.Unix()+int64(i%600), "/")
		e.UUID = uuid.New()
		batch = append(batch, e)
	}

	committed, err := writer.Write(ctx, batch)
	if err != nil {
		t.Fatalf("writing %d events: %v", events, err)
	}
	if len(committed) != events {
		t.Fatalf("settled %d of %d events", len(committed), events)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != events {
		t.Fatalf("stored %d events, want %d", got, events)
	}
}

// TestDuplicateWithinOneBatch covers a sender that retried into the middle of a
// live batch, which produces the same id twice in one call.
func TestDuplicateWithinOneBatch(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	one := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")

	if _, err := writer.Write(ctx, []Event{one, one, one}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 1 {
		t.Fatalf("events table holds %d rows, want 1", got)
	}
}

// TestOneTransactionPerAccount checks a batch spanning accounts lands in the
// right files. Getting this wrong would write one customer's traffic into
// another's database, which no later job could untangle.
func TestOneTransactionPerAccount(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	batch := []Event{
		writerEvent(1, EventPageview, fixtureStart.Unix(), "/"),
		writerEvent(2, EventPageview, fixtureStart.Unix(), "/"),
		writerEvent(1, EventPageview, fixtureStart.Unix()+30, "/pricing"),
		writerEvent(3, EventPageview, fixtureStart.Unix(), "/"),
	}

	if _, err := writer.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}

	for accountID, want := range map[int64]int64{1: 2, 2: 1, 3: 1} {
		if got := countRows(t, manager, accountID, "SELECT COUNT(*) FROM events"); got != want {
			t.Errorf("account %d holds %d events, want %d", accountID, got, want)
		}
	}
}

// TestDetailsOnlyWhenThereIsSomethingToStore checks the hot and cold tables stay
// split. SQLite reads the whole row off disk even for a three-column query, so a
// props blob in the hot table would be dragged through every scan.
func TestDetailsOnlyWhenThereIsSomethingToStore(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	plain := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")

	withProps := writerEvent(1, "signup", fixtureStart.Unix()+10, "/")
	withProps.Props = map[string]string{"plan": "pro"}

	if _, err := writer.Write(ctx, []Event{plain, withProps}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM event_details"); got != 1 {
		t.Fatalf("event_details holds %d rows, want 1", got)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events WHERE has_details = 1"); got != 1 {
		t.Fatalf("%d events claim details, want 1", got)
	}
}

// TestMergedSessionIsRepairedOnDisk checks the out-of-order repair reaches the
// database: events written under the absorbed session are repointed and its row
// is deleted, or a visit would be counted twice.
func TestMergedSessionIsRepairedOnDisk(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	// Two visits far enough apart to be separate — more than thirty minutes —
	// but close enough that one event in the gap is within thirty minutes of
	// both. They are written in separate batches so both rows exist on disk
	// before the bridge arrives.
	if _, err := writer.Write(ctx, []Event{writerEvent(1, EventPageview, base+3000, "/checkout")}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(ctx, []Event{writerEvent(1, EventPageview, base, "/")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM sessions"); got != 2 {
		t.Fatalf("expected two sessions before the bridge, got %d", got)
	}

	if _, err := writer.Write(ctx, []Event{writerEvent(1, EventPageview, base+1500, "/pricing")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("sessions table holds %d rows after the bridge, want 1", got)
	}
	if got := countRows(t, manager, 1, "SELECT pageviews FROM sessions"); got != 3 {
		t.Fatalf("merged session has %d pageviews, want 3", got)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(DISTINCT session_id) FROM events"); got != 1 {
		t.Fatalf("events point at %d sessions, want 1 — the merge did not repoint them", got)
	}
}

// TestALateEventThatStartsTheVisitRestampsWhatIsOnDisk covers the out-of-order
// half of the denormalisation. Every event row holds a copy of its session's
// acquisition, so an event that arrives late and turns out to be where the
// visit actually began leaves rows behind that say something else — and one
// visitor is then two rows on a source breakdown.
func TestALateEventThatStartsTheVisitRestampsWhatIsOnDisk(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	// The second page of the visit lands first and is written on its own, which
	// is what a retry of the first one leaves behind.
	second := writerEvent(1, EventPageview, base+60, "/pricing")
	second.Source, second.Channel = "", "Direct"

	if _, err := writer.Write(ctx, []Event{second}); err != nil {
		t.Fatal(err)
	}

	// Now the page they arrived on.
	landing := writerEvent(1, EventPageview, base, "/")
	landing.Source, landing.Channel = "Hacker News", "Organic Social"

	if _, err := writer.Write(ctx, []Event{landing}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(DISTINCT source_id) FROM events"); got != 1 {
		t.Fatalf("the visit's events carry %d sources, want 1", got)
	}

	if got := countRows(t, manager, 1,
		"SELECT COUNT(*) FROM events e JOIN dim_source s ON s.id = e.source_id WHERE s.value = 'Hacker News'",
	); got != 2 {
		t.Fatalf("%d events carry the visit's source, want 2", got)
	}
}

// TestMergedEventsTakeTheSurvivorsAcquisition is the merge half of it. Events
// written under the absorbed session were stamped with its acquisition, and
// repointing them without restamping them leaves one visit reported under two
// sources.
func TestMergedEventsTakeTheSurvivorsAcquisition(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	// Two visits more than thirty minutes apart, so they are separate until an
	// event in the gap proves they were always one.
	later := writerEvent(1, EventPageview, base+3000, "/checkout")
	later.Source, later.Channel = "", "Direct"

	if _, err := writer.Write(ctx, []Event{later}); err != nil {
		t.Fatal(err)
	}

	landing := writerEvent(1, EventPageview, base, "/")
	landing.Source, landing.Channel = "Hacker News", "Organic Social"

	if _, err := writer.Write(ctx, []Event{landing}); err != nil {
		t.Fatal(err)
	}

	bridge := writerEvent(1, EventPageview, base+1500, "/pricing")
	bridge.Source, bridge.Channel = "", "Direct"

	if _, err := writer.Write(ctx, []Event{bridge}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("sessions table holds %d rows after the bridge, want 1", got)
	}

	if got := countRows(t, manager, 1,
		"SELECT COUNT(*) FROM events e JOIN dim_source s ON s.id = e.source_id WHERE s.value = 'Hacker News'",
	); got != 3 {
		t.Fatalf("%d events carry the surviving visit's source, want 3", got)
	}
}

// TestDedupeReceiptSurvivesReplayPastTwentyFourHours proves a browser-retained
// UUID never becomes a new fact merely because its acknowledgement was lost
// for longer than the old receipt window.
func TestDedupeReceiptSurvivesReplayPastTwentyFourHours(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	clock := fixtureStart
	writer.Now = func() time.Time { return clock }
	event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")

	if _, err := writer.Write(ctx, []Event{event}); err != nil {
		t.Fatal(err)
	}

	clock = fixtureStart.Add(8 * 24 * time.Hour)
	if _, err := writer.Write(ctx, []Event{event}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 1 {
		t.Fatalf("late replay stored %d event rows, want 1", got)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM recent_event_ids"); got != 1 {
		t.Fatalf("permanent receipt table holds %d rows, want 1", got)
	}
}

// TestAVisitContinuesAcrossARestart is the reason fold state is durable rather
// than held in memory. A process that restarts mid-visit must fold the next
// event into the same session, or every restart splits a visit in two and
// nothing afterwards can tell the halves were one.
func TestAVisitContinuesAcrossARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := fixtureStart.Unix()

	firstManager := accounts.NewManager(dir)
	first := NewWriter(firstManager)
	first.Now = func() time.Time { return fixtureStart }

	if _, err := first.Write(ctx, []Event{writerEvent(1, EventPageview, base, "/")}); err != nil {
		t.Fatal(err)
	}
	if err := firstManager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	// A second process over the same files with an empty fold — exactly what a
	// restart looks like — and an event still inside the visit's window.
	secondManager := accounts.NewManager(dir)
	t.Cleanup(func() { checkClose(t, "restarted account manager", secondManager.CloseAll) })
	second := NewWriter(secondManager)
	second.Now = first.Now

	if _, err := second.Write(ctx, []Event{writerEvent(1, EventPageview, base+60, "/pricing")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, secondManager, 1, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("the restart left %d sessions, want the one visit", got)
	}
	if got := countRows(t, secondManager, 1, "SELECT pageviews FROM sessions"); got != 2 {
		t.Fatalf("the continued visit holds %d pageviews, want 2", got)
	}
	if got := countRows(t, secondManager, 1, "SELECT started_at FROM sessions"); got != base {
		t.Fatalf("the continued visit starts at %d, want %d — the restart lost where it began", got, base)
	}
}

// TestExpiredOrphanIsReported checks the one drop nobody can be told about at
// request time is still told about. A ping whose pageview never came is a
// genuine drop, and by the time that is known its 202 went out an hour ago —
// so the counter is the only place the customer ever hears about it.
func TestExpiredOrphanIsReported(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	writer.Counters = NewCounters()

	var outcomes []Observation
	writer.Observer = ObserverFunc(func(observation Observation) {
		outcomes = append(outcomes, observation)
	})

	ping := writerEvent(1, EventEngagement, fixtureStart.Unix(), "/")
	if _, err := writer.Write(ctx, []Event{ping}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM ingest_orphan_engagements"); got != 1 {
		t.Fatalf("the parked ping left %d rows, want 1", got)
	}

	// A later batch past the retention window, by which point the pageview is
	// never arriving.
	writer.Now = func() time.Time { return fixtureStart.Add(foldStateRetention + time.Minute) }
	later := writerEvent(1, EventPageview, writer.clock().Unix(), "/")
	if _, err := writer.Write(ctx, []Event{later}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM ingest_orphan_engagements"); got != 0 {
		t.Fatalf("the expired ping left %d rows, want 0", got)
	}

	reported := int64(0)
	for _, count := range writer.Counters.Snapshot().Dropped {
		if count.Reason == ReasonNoSessionForEngage {
			reported += count.Count
			if count.SiteID != 1 {
				t.Fatalf("the drop was reported against site %d, want 1 — a count nobody can attribute is not visibility",
					count.SiteID)
			}
		}
	}
	if reported != 1 {
		t.Fatalf("counted %d expired pings, want 1", reported)
	}

	observed := 0
	for _, outcome := range outcomes {
		if !outcome.Accepted && outcome.DropReason == ReasonNoSessionForEngage {
			observed++
		}
	}
	if observed != 1 {
		t.Fatalf("the health panel saw %d expired pings, want 1", observed)
	}
}

// TestSessionIDsSurviveARestart checks the durable allocator stays above the
// identities already committed by a previous process.
func TestSessionIDsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first := accounts.NewManager(dir)
	writerOne := NewWriter(first)
	writerOne.Now = func() time.Time { return fixtureStart }

	if _, err := writerOne.Write(ctx, []Event{writerEvent(1, EventPageview, fixtureStart.Unix(), "/")}); err != nil {
		t.Fatal(err)
	}
	if err := first.CloseAll(); err != nil {
		t.Fatal(err)
	}

	// A second process over the same files, with an empty cache — exactly what
	// a restart looks like.
	second := accounts.NewManager(dir)
	t.Cleanup(func() { checkClose(t, "second account manager", second.CloseAll) })

	writerTwo := NewWriter(second)
	writerTwo.Now = func() time.Time { return fixtureStart }

	later := writerEvent(1, EventPageview, fixtureStart.Unix()+100000, "/after-restart")
	if _, err := writerTwo.Write(ctx, []Event{later}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, second, 1, "SELECT COUNT(*) FROM sessions"); got != 2 {
		t.Fatalf("sessions table holds %d rows after a restart, want 2", got)
	}
}

// TestRevivedPingSurvivesAFailedCommit is the one case where an event can be
// lost with nothing left to retry it. Adopting a parked ping takes it out of the
// orphan map, and the ping is not in the batch the sender will redeliver — so a
// transaction that rolls back after the adoption leaves its row unwritten and
// nothing anywhere able to write it.
func TestRevivedPingSurvivesAFailedCommit(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	// The ping arrives before its own pageview and is parked, not written.
	ping := writerEvent(1, EventEngagement, base, "/")
	if _, err := writer.Write(ctx, []Event{ping}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 0 {
		t.Fatalf("wrote %d rows for a parked ping, want 0", got)
	}

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// A trigger is the cheapest deterministic "the transaction fails on the way
	// in" — the fold has already happened by the time the insert runs.
	if _, err := account.Writer().ExecContext(ctx,
		`CREATE TRIGGER refuse_events BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
	); err != nil {
		t.Fatal(err)
	}

	view := writerEvent(1, EventPageview, base+10, "/")
	if _, err := writer.Write(ctx, []Event{view}); err == nil {
		t.Fatal("the write succeeded while every insert was being refused")
	}

	if _, err := account.Writer().ExecContext(ctx, "DROP TRIGGER refuse_events"); err != nil {
		t.Fatal(err)
	}

	// The sender retries exactly what it sent: the pageview, and nothing about
	// the ping, which it has long since had a 202 for.
	if _, err := writer.Write(ctx, []Event{view}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("events table holds %d rows, want 2 — the adopted ping's row was lost by the rollback", got)
	}

	// And it landed on the visit it belongs to rather than one of its own.
	if got := countRows(t, manager, 1, "SELECT COUNT(DISTINCT session_id) FROM events"); got != 1 {
		t.Fatalf("the two rows point at %d sessions, want 1", got)
	}
	if got := countRows(t, manager, 1, "SELECT started_at FROM sessions"); got != base {
		t.Fatalf("session started at %d, want %d — the ping did not reach the fold", got, base)
	}
}

// TestRestartedWriterAdoptsAnotherWritersOrphan proves a pre-pageview ping is
// durable shared state rather than ownership trapped in one process.
func TestRestartedWriterAdoptsAnotherWritersOrphan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	firstManager := accounts.NewManager(dir)
	first := NewWriter(firstManager)
	first.Now = func() time.Time { return fixtureStart }
	base := fixtureStart.Unix()

	ping := writerEvent(1, EventEngagement, base, "/")
	settled, err := first.Write(ctx, []Event{ping})
	if err != nil || len(settled) != 1 {
		t.Fatalf("durable orphan settled %d events: %v", len(settled), err)
	}
	if err := firstManager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	secondManager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = secondManager.CloseAll() })
	second := NewWriter(secondManager)
	second.Now = first.Now
	view := writerEvent(1, EventPageview, base+10, "/")
	if _, err := second.Write(ctx, []Event{view}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, secondManager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("restarted writer stored %d rows, want pageview plus adopted ping", got)
	}
	if got := countRows(t, secondManager, 1, "SELECT COUNT(*) FROM ingest_orphan_engagements"); got != 0 {
		t.Fatalf("restarted writer left %d adopted orphan rows", got)
	}
	if got := countRows(t, secondManager, 1, "SELECT COUNT(DISTINCT session_id) FROM events"); got != 1 {
		t.Fatalf("adopted events use %d sessions, want 1", got)
	}
}

// TestDurableFoldAdoptsMoreThanTheLocalOrphanCap proves the old memory-safety
// ceiling cannot acknowledge and strand a durable engagement event.
func TestDurableFoldAdoptsMoreThanTheLocalOrphanCap(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	base := fixtureStart.Unix()
	batch := make([]Event, 0, 102)

	for i := 0; i < 101; i++ {
		ping := writerEvent(1, EventEngagement, base+int64(i%10), "/")
		ping.UUID = uuid.New()
		batch = append(batch, ping)
	}
	view := writerEvent(1, EventPageview, base+10, "/")
	view.UUID = uuid.New()
	batch = append(batch, view)

	settled, err := writer.Write(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != len(batch) {
		t.Fatalf("settled %d events, want %d", len(settled), len(batch))
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != int64(len(batch)) {
		t.Fatalf("durable fold wrote %d rows, want %d", got, len(batch))
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM ingest_orphan_engagements"); got != 0 {
		t.Fatalf("durable fold stranded %d adoptable pings", got)
	}
}

// TestPingRevivedInItsClaimTransactionStaysDeduplicated covers an engagement
// parked and adopted in one batch. Its receipt must survive with its fact row.
func TestPingRevivedInItsClaimTransactionStaysDeduplicated(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	base := fixtureStart.Unix()

	ping := writerEvent(1, EventEngagement, base, "/")
	view := writerEvent(1, EventPageview, base+10, "/")
	if _, err := writer.Write(ctx, []Event{ping, view}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("initial batch wrote %d rows, want 2", got)
	}

	committed, err := writer.Write(ctx, []Event{ping})
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 1 {
		t.Fatalf("redelivery settled %d events, want 1", len(committed))
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("redelivered ping produced %d rows, want 2", got)
	}
}

// TestSessionRowIsUpdatedInPlace checks the whole reason this schema needs no
// sign column: a visit is one row, changed as it goes.
func TestSessionRowIsUpdatedInPlace(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	for i, path := range []string{"/", "/pricing", "/signup"} {
		one := writerEvent(1, EventPageview, base+int64(i)*30, path)
		if _, err := writer.Write(ctx, []Event{one}); err != nil {
			t.Fatal(err)
		}
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Fatalf("sessions table holds %d rows, want 1", got)
	}
	if got := countRows(t, manager, 1, "SELECT duration FROM sessions"); got != 60 {
		t.Fatalf("duration = %d, want 60", got)
	}
	if got := countRows(t, manager, 1, "SELECT is_bounce FROM sessions"); got != 0 {
		t.Fatal("a three-page visit is still marked as a bounce")
	}
}
