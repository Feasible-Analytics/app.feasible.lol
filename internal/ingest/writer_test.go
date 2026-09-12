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
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	sqlite "modernc.org/sqlite"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
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
	writer.Failpoint = func(_ int64, stage string) error {
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
			writer.Failpoint = func(_ int64, current string) error {
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
		t.Fatalf("the receipt table holds %d rows, want 1", got)
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

// TestThePruneSeeksTheRowsItDeletes is what stops the cost of writing a batch
// growing with the site's own traffic.
//
// It plans the statements the writer issues rather than a copy of them: a copy
// passes while the real query scans every session the site has had, which is
// exactly the regression a plan assertion exists to catch.
//
// The whole detail line is asserted, not just the index name. Both indexes
// begin with site_id, so "uses the expiry index" is satisfied by a plan that
// seeks the site and then reads all of it — and the time column appearing as a
// bound is the difference.
func TestThePruneSeeksTheRowsItDeletes(t *testing.T) {
	db := planDatabase(t)
	ctx := context.Background()

	for name, want := range map[string]struct {
		query string
		plan  string
	}{
		"reading expired orphans": {
			selectExpiredOrphans,
			"SEARCH ingest_orphan_engagements USING INDEX ingest_orphan_engagements_expiry " +
				"(site_id=? AND timestamp<?)",
		},
		"deleting expired orphans": {
			deleteExpiredOrphans,
			"SEARCH ingest_orphan_engagements USING INDEX ingest_orphan_engagements_expiry " +
				"(site_id=? AND timestamp<?)",
		},
		"deleting expired session state": {
			deleteExpiredSessionState,
			"SEARCH ingest_session_state USING INDEX ingest_session_state_expiry " +
				"(site_id=? AND last_seen_at<?)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if plan := queryPlan(t, ctx, db, want.query); !strings.Contains(plan, want.plan) {
				t.Errorf("plan is\n%s\nwant it to contain\n%s", plan, want.plan)
			}
		})
	}
}

// TestTheVisitorReadsKeepTheirOwnIndex is the other half. The read path asks
// about a chunk of visitors, and the indexes added for the prune must not have
// made SQLite prefer one of them for that.
//
// The statements are rendered with one visitor in the list, which is the shape
// the planner is being asked about; a longer list changes how many times the
// index is seeked, not which index.
func TestTheVisitorReadsKeepTheirOwnIndex(t *testing.T) {
	db := planDatabase(t)
	ctx := context.Background()

	for name, want := range map[string]struct {
		query string
		plan  string
	}{
		"loading a chunk's session state": {
			fmt.Sprintf(selectVisitorSessionState, placeholders(1)),
			"SEARCH ingest_session_state USING INDEX ingest_session_state_visitor " +
				"(site_id=? AND user_id=? AND last_seen_at>?)",
		},
		"adopting a chunk's parked pings": {
			fmt.Sprintf(selectVisitorOrphans, placeholders(1)),
			"SEARCH ingest_orphan_engagements USING INDEX ingest_orphan_engagements_visitor " +
				"(site_id=? AND user_id=? AND timestamp>? AND timestamp<?)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if plan := queryPlan(t, ctx, db, want.query); !strings.Contains(plan, want.plan) {
				t.Errorf("plan is\n%s\nwant it to contain\n%s", plan, want.plan)
			}
		})
	}
}

// TestThePruneDeletesOnlyWhatHasExpired runs the real prune over a site with a
// long history and checks it took the old rows and left the rest.
//
// The plan tests above say the prune seeks; this says the seek is over the right
// rows. An index that made the query fast by matching nothing would satisfy one
// of them and not the other.
func TestThePruneDeletesOnlyWhatHasExpired(t *testing.T) {
	ctx := context.Background()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { checkClose(t, "fold state prune account manager", manager.CloseAll) })

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	db := account.Writer()

	const (
		expired = 200
		live    = 5_000
	)

	now := fixtureStart.Unix()
	stale := now - int64(foldStateRetention/time.Second) - 1

	seedFoldState(t, ctx, db, expired, stale)
	seedFoldState(t, ctx, db, live, now)

	// Another site's rows, all of them expired. The prune is asked about site 1
	// only, and a WHERE clause that lost its site_id would take these too.
	seedOtherSite(t, ctx, db, 2, expired, stale)

	writer := NewWriter(manager)
	writer.Now = func() time.Time { return fixtureStart }

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.pruneFoldState(ctx, tx, []Event{
		writerEvent(1, EventPageview, now, "/now"),
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for table, want := range map[string]int{
		"ingest_session_state":      live,
		"ingest_orphan_engagements": live,
	} {
		var left int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE site_id = 1").Scan(&left); err != nil {
			t.Fatal(err)
		}

		if left != want {
			t.Errorf("%s kept %d rows for the pruned site, want the %d live ones", table, left, want)
		}
	}

	for table, want := range map[string]int{
		"ingest_session_state":      expired,
		"ingest_orphan_engagements": expired,
	} {
		var left int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE site_id = 2").Scan(&left); err != nil {
			t.Fatal(err)
		}

		if left != want {
			t.Errorf("%s kept %d rows for the untouched site, want all %d", table, left, want)
		}
	}
}

// planDatabase is one migrated account database, opened the way the writer
// opens it so the plans are the plans production gets.
func planDatabase(t *testing.T) *sql.DB {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { checkClose(t, "query plan account manager", manager.CloseAll) })

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	return account.Writer()
}

// TestThePruneReadsTheExpiredRowsAndNoOthers is the cost property: the prune's
// work follows what it deletes, not what the site has.
//
// It runs the prune's own predicate twice over one database — once as the
// writer issues it, once pinned to the index that existed before — and compares
// them. Both passes see the same b-tree at the same size with the same pages
// cached, so the only difference between them is how many rows each has to look
// at, which is the thing being claimed. Timing two databases of different sizes
// would not show that: deleting from a bigger tree costs more page reads even
// when the plan is perfect.
func TestThePruneReadsTheExpiredRowsAndNoOthers(t *testing.T) {
	ctx := context.Background()
	db := planDatabase(t)

	const (
		expired = 500
		live    = 20_000
	)

	now := fixtureStart.Unix()
	cutoff := now - int64(foldStateRetention/time.Second)

	seedFoldState(t, ctx, db, expired, cutoff-1)
	seedFoldState(t, ctx, db, live, now)

	// Counting rather than deleting: a delete would empty the table on the
	// first pass and leave the second measuring nothing.
	const counted = "SELECT COUNT(*) FROM ingest_session_state %s WHERE site_id = ? AND last_seen_at < ?"

	seeking := fmt.Sprintf(counted, "")
	scanning := fmt.Sprintf(counted, "INDEXED BY ingest_session_state_visitor")

	if plan := queryPlan(t, ctx, db, seeking); !strings.Contains(plan, "ingest_session_state_expiry") {
		t.Fatalf("the control is not measuring what it thinks:\n%s", plan)
	}

	// Both statements once first, so neither pays for the other's cold pages.
	countExpired(t, ctx, db, seeking, expired)
	countExpired(t, ctx, db, scanning, expired)

	started := time.Now()
	countExpired(t, ctx, db, seeking, expired)
	seek := time.Since(started)

	started = time.Now()
	countExpired(t, ctx, db, scanning, expired)
	scan := time.Since(started)

	// The seek reads 500 index entries and the scan reads 20,500, so the gap is
	// forty-fold. Five is the floor a loaded machine still clears.
	if ratio := float64(scan) / float64(seek); ratio < 5 {
		t.Errorf("seeking the expired rows was only %.1fx faster than reading the whole site "+
			"(%v against %v), so the prune is not skipping the history", ratio, seek, scan)
	}
}

// countExpired runs one form of the prune's predicate and checks it found the
// rows the prune would delete.
func countExpired(t *testing.T, ctx context.Context, db *sql.DB, query string, want int) {
	t.Helper()

	var found int
	if err := db.QueryRowContext(ctx, query, 1,
		fixtureStart.Unix()-int64(foldStateRetention/time.Second)).Scan(&found); err != nil {
		t.Fatal(err)
	}

	if found != want {
		t.Fatalf("%q matched %d rows, want %d", query, found, want)
	}
}

// seedFoldState fills site 1 with session state and parked engagement, all
// stamped at one time.
func seedFoldState(t *testing.T, ctx context.Context, db *sql.DB, rows int, at int64) {
	t.Helper()

	seedOtherSite(t, ctx, db, 1, rows, at)
}

// seedOtherSite is the same for any site, so a test can prove the prune stays
// inside the one it was asked about.
func seedOtherSite(t *testing.T, ctx context.Context, db *sql.DB, siteID int64, rows int, at int64) {
	t.Helper()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	// The visitor ids continue past whatever is already there, so two calls
	// build one long history rather than colliding on the primary key.
	var next int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(user_id), 0) + 1 FROM ingest_session_state").Scan(&next); err != nil {
		t.Fatal(err)
	}

	sessions, err := tx.PrepareContext(ctx, `
		INSERT INTO ingest_session_state (site_id, user_id, started_at, last_seen_at, payload)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = sessions.Close() }()

	orphans, err := tx.PrepareContext(ctx, `
		INSERT INTO ingest_orphan_engagements (event_uuid, site_id, user_id, timestamp, payload)
		VALUES (randomblob(16), ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = orphans.Close() }()

	// The prune decodes every row it removes, so the payloads have to be the
	// real encoding rather than a placeholder blob.
	session, err := json.Marshal(Session{SiteID: siteID, StartedAt: at, LastSeenAt: at})
	if err != nil {
		t.Fatal(err)
	}

	orphan, err := json.Marshal(writerEvent(siteID, EventEngagement, at, "/parked"))
	if err != nil {
		t.Fatal(err)
	}

	for i := range rows {
		user := next + int64(i)

		if _, err := sessions.ExecContext(ctx, siteID, user, at, at, session); err != nil {
			t.Fatal(err)
		}

		if _, err := orphans.ExecContext(ctx, siteID, user, at, orphan); err != nil {
			t.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// queryPlan asks SQLite how it would run one statement. The placeholders are
// filled with zeroes: the plan depends on the shape of the query, not on the
// values.
func queryPlan(t *testing.T, ctx context.Context, db *sql.DB, query string) string {
	t.Helper()

	args := make([]any, strings.Count(query, "?"))
	for i := range args {
		args[i] = 0
	}

	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}

	defer func() { _ = rows.Close() }()

	var plan strings.Builder

	for rows.Next() {
		var id, parent, notused int
		var detail string

		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}

		fmt.Fprintln(&plan, detail)
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return plan.String()
}

// countingDriver wraps the real driver and counts the queries a connection is
// asked to run, which is the only way to assert "this does not grow with the
// visitor count" rather than to assume it.
type countingDriver struct {
	inner driver.Driver
	count *atomic.Int64
}

// Open hands back a connection that reports every query through the counter.
func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}

	return countingConn{Conn: conn, count: d.count}, nil
}

// countingConn is one such connection. It embeds the real one so that every
// interface the pool checks for — transactions, prepared statements, context
// support — is answered by the driver rather than reimplemented here.
type countingConn struct {
	driver.Conn
	count *atomic.Int64
}

// QueryContext counts one read and passes it on.
func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.count.Add(1)

	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// ExecContext passes a write through uncounted: this test is about reads.
func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

// BeginTx keeps the transaction the pool asks for rather than the legacy Begin.
func (c countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

// countedDatabases numbers the driver registrations this package makes.
var countedDatabases atomic.Int64

// countedDatabase is a migrated account database whose reads are counted.
func countedDatabase(t *testing.T) (*sql.DB, *atomic.Int64) {
	t.Helper()

	count := &atomic.Int64{}

	// A driver name can only be registered once in a process, and -count=2 runs
	// the same test twice.
	name := fmt.Sprintf("counting-%s-%d", t.Name(), countedDatabases.Add(1))

	sql.Register(name, countingDriver{inner: &sqlite.Driver{}, count: count})

	db, err := sql.Open(name, store.DSN(filepath.Join(t.TempDir(), "account.db"), store.TxLockImmediate))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.Account()); err != nil {
		t.Fatal(err)
	}

	return db, count
}

// TestTheFoldIsReadPerChunkNotPerVisitor is the cost property. The round trips
// a batch makes to load its fold state must follow the chunk count, which is
// bounded, rather than the visitor count, which is what grows when a customer's
// traffic grows.
func TestTheFoldIsReadPerChunkNotPerVisitor(t *testing.T) {
	ctx := context.Background()
	db, count := countedDatabase(t)

	// Three reads per chunk: the serialized state, the sessions with no state
	// row, and the parked engagement.
	const perChunk = 3

	for _, visitors := range []int{1, foldChunk, foldChunk * 3} {
		ranges := map[sessionKey]durableFoldRange{}
		for i := range visitors {
			ranges[sessionKey{siteID: 1, userID: int64(i) + 1}] = durableFoldRange{first: 1000, last: 2000}
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}

		count.Store(0)

		if err := loadDurableFold(ctx, tx, newDurableSessionCache(), 1, ranges); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}

		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}

		chunks := (visitors + foldChunk - 1) / foldChunk

		if got := count.Load(); got != int64(chunks*perChunk) {
			t.Errorf("%d visitors cost %d reads, want %d — %d chunk(s) of %d",
				visitors, got, chunks*perChunk, chunks, perChunk)
		}
	}
}

// TestEveryVisitorInABatchIsFound is the other half: a batch with more visitors
// than one chunk holds must see every one of them.
//
// Reading in chunks is where a visitor goes missing silently — the fold simply
// starts them a new session, and the customer sees one visit become two with
// nothing anywhere saying why. The read count is asserted separately; this is
// about the answer being complete.
func TestEveryVisitorInABatchIsFound(t *testing.T) {
	ctx := context.Background()
	db := planDatabase(t)

	const visitors = foldChunk*2 + 1

	seedVisitorSessions(t, ctx, db, visitors)

	ranges := map[sessionKey]durableFoldRange{}
	for i := range visitors {
		ranges[sessionKey{siteID: 1, userID: int64(i) + 1}] = durableFoldRange{first: 1000, last: 1000}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	cache := newDurableSessionCache()
	if err := loadDurableFold(ctx, tx, cache, 1, ranges); err != nil {
		t.Fatal(err)
	}

	if got := len(cache.bucket.sessions); got != visitors {
		t.Errorf("%d visitors came back, want all %d", got, visitors)
	}

	for key := range ranges {
		if len(cache.bucket.sessions[key]) != 1 {
			t.Errorf("visitor %d has %d sessions, want one", key.userID, len(cache.bucket.sessions[key]))
		}
	}
}

// seedVisitorSessions gives each of the first n visitors one stored session.
func seedVisitorSessions(t *testing.T, ctx context.Context, db *sql.DB, visitors int) {
	t.Helper()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO ingest_session_state (site_id, user_id, started_at, last_seen_at, payload)
		VALUES (1, ?, 1000, 1000, ?)`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = insert.Close() }()

	for i := range visitors {
		user := int64(i) + 1

		payload, err := json.Marshal(Session{ID: user, SiteID: 1, UserID: user, StartedAt: 1000, LastSeenAt: 1000})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := insert.ExecContext(ctx, user, payload); err != nil {
			t.Fatal(err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAChunkDoesNotHandAVisitorSomebodyElsesWindow is the risk this change
// introduces and the reason the Go filter exists.
//
// One statement now covers a chunk of visitors, so it has to be bounded by the
// widest window any of them needs. A visitor whose own window is far from that
// edge would otherwise be handed a session they never had — folded into a visit
// that never happened, with nothing anywhere saying so.
func TestAChunkDoesNotHandAVisitorSomebodyElsesWindow(t *testing.T) {
	ctx := context.Background()
	db := planDatabase(t)

	const (
		early = 1_000_000
		late  = 2_000_000
	)

	// Visitor 1 has an old visit of their own that the chunk's window reaches
	// only because visitor 2 is in the same chunk. Nothing in the statement can
	// exclude it; only the per-visitor check can.
	writeStoredSession(t, ctx, db, 1, early)
	writeStoredSession(t, ctx, db, 1, late)
	writeStoredSession(t, ctx, db, 2, late)

	writeParkedPing(t, ctx, db, 1, early)
	writeParkedPing(t, ctx, db, 1, late)
	writeParkedPing(t, ctx, db, 2, late)

	// And the same again for a session written before the state table existed,
	// which is hydrated by its own read.
	writeLegacySession(t, ctx, db, 1, early)
	writeLegacySession(t, ctx, db, 1, late)

	ranges := map[sessionKey]durableFoldRange{
		{siteID: 1, userID: 1}: {first: early, last: early},
		{siteID: 1, userID: 2}: {first: late, last: late},
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	cache := newDurableSessionCache()
	if err := loadDurableFold(ctx, tx, cache, 1, ranges); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[sessionKey]int64{
		{siteID: 1, userID: 1}: early,
		{siteID: 1, userID: 2}: late,
	} {
		sessions := cache.bucket.sessions[key]

		// Visitor 1 has a legacy row of their own in the same window as their
		// state row, so both reads contribute one.
		wantSessions := 1
		if key.userID == 1 {
			wantSessions = 2
		}

		if len(sessions) != wantSessions {
			t.Fatalf("visitor %d got %d sessions, want %d — only their own",
				key.userID, len(sessions), wantSessions)
		}

		for _, session := range sessions {
			if session.StartedAt != want {
				t.Errorf("visitor %d got a session at %d, want theirs at %d",
					key.userID, session.StartedAt, want)
			}
		}

		orphans := cache.bucket.orphans[key]
		if len(orphans) != 1 {
			t.Fatalf("visitor %d got %d parked pings, want only their own", key.userID, len(orphans))
		}

		if orphans[0].Timestamp != want {
			t.Errorf("visitor %d got the ping at %d, want theirs at %d",
				key.userID, orphans[0].Timestamp, want)
		}
	}
}

// TestTheFoldWindowIncludesItsOwnEdge pins the one comparison this change moved
// out of SQL and into Go.
//
// A session starting exactly one session-timeout after the batch's last event
// is inside the window, and one a second past that is not. Both copies of the
// predicate — the chunk's bound and the per-row check — have to agree on that
// forever, and a boundary nothing asserts is a boundary that drifts.
func TestTheFoldWindowIncludesItsOwnEdge(t *testing.T) {
	ctx := context.Background()
	db := planDatabase(t)

	const at = 1_000_000

	writeStoredSession(t, ctx, db, 1, at+sessionTimeoutSeconds)
	writeStoredSession(t, ctx, db, 2, at+sessionTimeoutSeconds+1)

	ranges := map[sessionKey]durableFoldRange{
		{siteID: 1, userID: 1}: {first: at, last: at},
		{siteID: 1, userID: 2}: {first: at, last: at},
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	cache := newDurableSessionCache()
	if err := loadDurableFold(ctx, tx, cache, 1, ranges); err != nil {
		t.Fatal(err)
	}

	if got := len(cache.bucket.sessions[sessionKey{siteID: 1, userID: 1}]); got != 1 {
		t.Errorf("a session starting exactly on the edge came back %d times, want once", got)
	}

	if got := len(cache.bucket.sessions[sessionKey{siteID: 1, userID: 2}]); got != 0 {
		t.Errorf("a session starting one second past the edge came back %d times, want none", got)
	}
}

// TestTheFoldKeepsEachSitesVisitorsApart is about the grouping a chunked read
// needs and a per-visitor one did not.
//
// One account holds many sites, and a visitor id is a hash that says nothing
// about which. Group them wrongly and a site's visitors never find their own
// fold state: every visit silently splits in two, and nothing anywhere says so.
func TestTheFoldKeepsEachSitesVisitorsApart(t *testing.T) {
	ctx := context.Background()
	db := planDatabase(t)

	const at = 1_000_000

	// The same visitor id on two sites, which is what a hash collision across
	// sites looks like, plus one visitor only the second site has.
	writeStoredSessionOn(t, ctx, db, 1, 7, at)
	writeStoredSessionOn(t, ctx, db, 2, 7, at)
	writeStoredSessionOn(t, ctx, db, 2, 8, at)

	ranges := map[sessionKey]durableFoldRange{
		{siteID: 1, userID: 7}: {first: at, last: at},
		{siteID: 2, userID: 7}: {first: at, last: at},
		{siteID: 2, userID: 8}: {first: at, last: at},
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	cache := newDurableSessionCache()
	if err := loadDurableFold(ctx, tx, cache, 1, ranges); err != nil {
		t.Fatal(err)
	}

	for key := range ranges {
		sessions := cache.bucket.sessions[key]

		if len(sessions) != 1 {
			t.Fatalf("site %d visitor %d got %d sessions, want their own one",
				key.siteID, key.userID, len(sessions))
		}

		if sessions[0].SiteID != key.siteID {
			t.Errorf("site %d visitor %d was handed site %d's session",
				key.siteID, key.userID, sessions[0].SiteID)
		}
	}
}

// writeStoredSession gives one visitor on site 1 one session at one moment.
func writeStoredSession(t *testing.T, ctx context.Context, db *sql.DB, userID, at int64) {
	t.Helper()

	writeStoredSessionOn(t, ctx, db, 1, userID, at)
}

// writeStoredSessionOn is the same for any site, so a test can prove one site's
// visitors are never handed another's.
func writeStoredSessionOn(t *testing.T, ctx context.Context, db *sql.DB, siteID, userID, at int64) {
	t.Helper()

	payload, err := json.Marshal(Session{
		ID: at*100 + siteID*10 + userID, SiteID: siteID, UserID: userID, StartedAt: at, LastSeenAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO ingest_session_state (site_id, user_id, started_at, last_seen_at, payload)
		VALUES (?, ?, ?, ?, ?)`, siteID, userID, at, at, payload); err != nil {
		t.Fatal(err)
	}
}

// writeParkedPing gives one visitor one engagement ping waiting for a pageview.
func writeParkedPing(t *testing.T, ctx context.Context, db *sql.DB, userID, at int64) {
	t.Helper()

	event := writerEvent(1, EventEngagement, at, "/parked")
	event.UserID = userID

	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO ingest_orphan_engagements (event_uuid, site_id, user_id, timestamp, payload)
		VALUES (randomblob(16), 1, ?, ?, ?)`, userID, at, payload); err != nil {
		t.Fatal(err)
	}
}

// writeLegacySession gives one visitor a session row with no companion state,
// which is what a visit written before that table existed looks like.
func writeLegacySession(t *testing.T, ctx context.Context, db *sql.DB, userID, at int64) {
	t.Helper()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, pageviews, events)
		VALUES (?, 1, ?, ?, ?, 1, 1)`, at*10+userID, userID, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestTheAccountPoolHoldsExactlyItsBound is the point of the pool, and its
// limit, in one assertion.
//
// Different accounts are different files with different locks whose commits
// each wait for a disk sync, so those waits should overlap. But a batch can
// span hundreds of accounts, and each one in flight is an open database, a
// transaction and a handful of descriptors — so the overlap has to stop
// somewhere.
func TestTheAccountPoolHoldsExactlyItsBound(t *testing.T) {
	for _, bound := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("%d at once", bound), func(t *testing.T) {
			ctx := context.Background()
			writer, _ := newWriter(t)
			writer.Concurrency = bound

			const accounts = 8

			// Creating and migrating eight databases costs far more than the
			// window below, so it happens first. Otherwise the test measures
			// SQLite start-up rather than the pool.
			for account := int64(1); account <= accounts; account++ {
				warm := writerEvent(account, EventPageview, fixtureStart.Unix(), "/warm")
				if _, err := writer.Write(ctx, []Event{warm}); err != nil {
					t.Fatal(err)
				}
			}

			var (
				mu      sync.Mutex
				inside  int
				highest int
				open    sync.Once
			)

			// Everyone waits until the pool is full, so the accounts a pool is
			// willing to run at once are inside together and counted. The extra
			// hold after the release catches a pool that is wider than it should
			// be: those arrivals land while the first ones are still inside.
			full := make(chan struct{})

			giveUp := make(chan struct{})
			time.AfterFunc(20*time.Second, func() { close(giveUp) })

			writer.Failpoint = func(_ int64, stage string) error {
				if stage != WriterStageBeforeCommit {
					return nil
				}

				mu.Lock()
				inside++
				highest = max(highest, inside)
				reached := inside
				mu.Unlock()

				if reached >= bound {
					open.Do(func() { close(full) })
				}

				select {
				case <-full:
				case <-giveUp:
					// One deadline for the whole batch, not one per account: a
					// serial regression would otherwise wait out eight of them
					// and report as a test timeout rather than as itself.
				}

				time.Sleep(50 * time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()

				return nil
			}

			batch := make([]Event, 0, accounts)
			for account := int64(1); account <= accounts; account++ {
				batch = append(batch, writerEvent(account, EventPageview, fixtureStart.Unix(), "/measured"))
			}

			if _, err := writer.Write(ctx, batch); err != nil {
				t.Fatal(err)
			}

			if highest != bound {
				t.Errorf("%d accounts were in flight at once, want exactly %d", highest, bound)
			}
		})
	}
}

// TestConcurrentBatchesForOneAccountStillFoldSerially is the invariant the pool
// must not touch.
//
// Two batches for the same visitor, written at the same time, have to produce
// the one visit they would have produced one after another. Session
// accumulation is one of the two things this project says must be byte-exact:
// get it wrong and every number drifts with no way back.
func TestConcurrentBatchesForOneAccountStillFoldSerially(t *testing.T) {
	ctx := context.Background()

	const pageviews = 20

	serial := writeVisitorPageviews(t, ctx, pageviews, false)
	concurrent := writeVisitorPageviews(t, ctx, pageviews, true)

	if concurrent.sessions != serial.sessions {
		t.Errorf("concurrent writing produced %d sessions, want the %d serial writing does",
			concurrent.sessions, serial.sessions)
	}

	if concurrent.pageviews != serial.pageviews {
		t.Errorf("concurrent writing folded %d pageviews into the visit, want the %d serial writing does",
			concurrent.pageviews, serial.pageviews)
	}

	if concurrent.pageviews != pageviews {
		t.Errorf("the visit holds %d pageviews, want all %d", concurrent.pageviews, pageviews)
	}
}

// TestASessionIDIsNeverHandedOutTwice is what stops two accounts, or two
// batches, writing over each other's visits.
//
// The allocator is durable and per account, so this should already hold — but
// "should" is the reason to assert it once the caller became concurrent.
func TestASessionIDIsNeverHandedOutTwice(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	const (
		accounts = 4
		visitors = 25
	)

	var wg sync.WaitGroup

	for account := int64(1); account <= accounts; account++ {
		for visitor := range visitors {
			wg.Add(1)

			go func() {
				defer wg.Done()

				// The uuid is derived from the path, so each visitor needs one
				// of their own or the batch deduplicates itself.
				event := writerEvent(account, EventPageview, fixtureStart.Unix(),
					fmt.Sprintf("/visitor-%d", visitor))
				event.UserID = testUser + int64(visitor)

				if _, err := writer.Write(ctx, []Event{event}); err != nil {
					t.Error(err)
				}
			}()
		}
	}

	wg.Wait()

	for account := int64(1); account <= accounts; account++ {
		rows := countRows(t, manager, account, "SELECT COUNT(*) FROM sessions")
		distinct := countRows(t, manager, account, "SELECT COUNT(DISTINCT id) FROM sessions")

		if rows != distinct {
			t.Errorf("account %d has %d sessions under %d ids, so an id was reused", account, rows, distinct)
		}

		if rows != visitors {
			t.Errorf("account %d has %d sessions, want one per visitor (%d)", account, rows, visitors)
		}
	}
}

// visitShape is what one visitor's traffic folded into.
type visitShape struct {
	sessions  int64
	pageviews int64
}

// writeVisitorPageviews sends one visitor's pageviews as single-event batches,
// either one after another or all at once, and reports the visit they made.
func writeVisitorPageviews(t *testing.T, ctx context.Context, count int, together bool) visitShape {
	t.Helper()

	writer, manager := newWriter(t)

	send := func(i int) {
		event := writerEvent(1, EventPageview, fixtureStart.Unix()+int64(i), fmt.Sprintf("/page-%d", i))

		if _, err := writer.Write(ctx, []Event{event}); err != nil {
			t.Error(err)
		}
	}

	if together {
		var wg sync.WaitGroup

		for i := range count {
			wg.Add(1)

			go func() {
				defer wg.Done()
				send(i)
			}()
		}

		wg.Wait()
	} else {
		for i := range count {
			send(i)
		}
	}

	return visitShape{
		sessions:  countRows(t, manager, 1, "SELECT COUNT(*) FROM sessions"),
		pageviews: countRows(t, manager, 1, "SELECT COALESCE(SUM(pageviews), 0) FROM sessions"),
	}
}

// TestOneAccountFailingLeavesTheOthersCommitted is the contract the serial loop
// had and the pool has to keep.
//
// The events belong to different customers and are already in memory.
// Abandoning them because one account could not be written would turn one full
// disk into data loss across every customer on the box.
func TestOneAccountFailingLeavesTheOthersCommitted(t *testing.T) {
	for _, stage := range []string{WriterStageAfterClaim, WriterStageAfterRejection, WriterStageBeforeCommit} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			writer, manager := newWriter(t)
			writer.Concurrency = 4

			const (
				accounts = 6
				doomed   = 3
			)

			writer.Failpoint = func(accountID int64, current string) error {
				if accountID == doomed && current == stage {
					return errors.New("simulated process kill")
				}

				return nil
			}

			batch := make([]Event, 0, accounts)
			for account := int64(1); account <= accounts; account++ {
				batch = append(batch, writerEvent(account, EventPageview, fixtureStart.Unix(), "/"))
			}

			committed, err := writer.Write(ctx, batch)
			if err == nil {
				t.Fatal("the failing account was reported as a clean batch")
			}

			if len(committed) != accounts-1 {
				t.Errorf("%d uuids were named committed, want the %d that got through",
					len(committed), accounts-1)
			}

			for account := int64(1); account <= accounts; account++ {
				want := int64(1)
				if account == doomed {
					want = 0
				}

				if got := countRows(t, manager, account, "SELECT COUNT(*) FROM events"); got != want {
					t.Errorf("account %d stored %d events, want %d", account, got, want)
				}
			}
		})
	}
}

// TestEveryFailureInABatchIsReported is the other half of that contract. Four
// accounts failing for four reasons is four things somebody has to know about,
// and only the first of them used to survive.
func TestEveryFailureInABatchIsReported(t *testing.T) {
	ctx := context.Background()
	writer, _ := newWriter(t)
	writer.Concurrency = 4

	writer.Failpoint = func(accountID int64, stage string) error {
		if stage == WriterStageAfterClaim && accountID <= 3 {
			return fmt.Errorf("account %d could not be written", accountID)
		}

		return nil
	}

	batch := make([]Event, 0, 5)
	for account := int64(1); account <= 5; account++ {
		batch = append(batch, writerEvent(account, EventPageview, fixtureStart.Unix(), "/"))
	}

	_, err := writer.Write(ctx, batch)
	if err == nil {
		t.Fatal("three failing accounts were reported as a clean batch")
	}

	for account := int64(1); account <= 3; account++ {
		if want := fmt.Sprintf("account %d could not be written", account); !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestAPanicInOneAccountStaysInThatAccount is the blast radius the pool must
// not widen.
//
// The write used to run on the caller's goroutine, so a panic was recovered by
// net/http and cost one connection. On a goroutine of its own it would take the
// process instead — every customer's ingestion on a hosted shard, and the
// dashboard as well in the single-process deployment.
func TestAPanicInOneAccountStaysInThatAccount(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)
	writer.Concurrency = 4

	const doomed = 2

	writer.Failpoint = func(accountID int64, stage string) error {
		if accountID == doomed && stage == WriterStageAfterClaim {
			panic("a malformed row")
		}

		return nil
	}

	batch := make([]Event, 0, 4)
	for account := int64(1); account <= 4; account++ {
		batch = append(batch, writerEvent(account, EventPageview, fixtureStart.Unix(), "/"))
	}

	committed, err := writer.Write(ctx, batch)
	if err == nil {
		t.Fatal("a panicking account was reported as a clean batch")
	}

	if !strings.Contains(err.Error(), "a malformed row") {
		t.Errorf("the error does not carry what panicked: %v", err)
	}

	if len(committed) != 3 {
		t.Errorf("%d uuids were named committed, want the 3 accounts that did not panic", len(committed))
	}

	for account := int64(1); account <= 4; account++ {
		want := int64(1)
		if account == doomed {
			want = 0
		}

		if got := countRows(t, manager, account, "SELECT COUNT(*) FROM events"); got != want {
			t.Errorf("account %d stored %d events, want %d", account, got, want)
		}
	}
}

// TestPagelessVisitIsClassifiedOnDisk covers the verdict only a whole visit can
// support: custom events on two paths with no pageview between them, which no
// browser running the tracker can produce.
//
// The two events are written in separate batches on purpose. The deciding one
// is the second, so the row that proves it is already on disk when the verdict
// is reached — and a marking pass that only touched the batch in hand would
// leave the first event counted.
func TestPagelessVisitIsClassifiedOnDisk(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	if _, err := writer.Write(ctx, []Event{writerEvent(1, "signup", base, "/login")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events WHERE bot_reason_id <> 0"); got != 0 {
		t.Fatalf("one custom event on one path was classified, got %d rows — a single-page app calling the API looks like this", got)
	}

	if _, err := writer.Write(ctx, []Event{writerEvent(1, "signup", base+20, "/register")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events WHERE bot_reason_id <> 0"); got != 2 {
		t.Fatalf("%d events carry a bot reason, want both — the earlier row was left behind", got)
	}

	// Nothing is deleted: a wrong verdict has to stay recoverable.
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("events table holds %d rows, want 2 — classification must not delete", got)
	}

	if got := countRows(t, manager, 1,
		"SELECT COUNT(*) FROM events e JOIN dim_bot_reason r ON r.id = e.bot_reason_id WHERE r.value = 'pageless_visit'",
	); got != 2 {
		t.Fatalf("%d events carry the pageless_visit reason, want 2", got)
	}

	// The visit-grain fact a visitor count reads. Without it the events are
	// classified and the visitor is still counted, which is the whole number
	// this was meant to correct.
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM session_sampling WHERE is_bot = 1"); got != 1 {
		t.Fatalf("%d sampled sessions are marked automated, want 1", got)
	}
}

// TestPagelessVisitWithAPageviewIsLeftAlone is the other half of the rule. The
// same events, with the page load in front of them, are a person.
func TestPagelessVisitWithAPageviewIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	base := fixtureStart.Unix()

	if _, err := writer.Write(ctx, []Event{
		writerEvent(1, EventPageview, base, "/login"),
		writerEvent(1, "Form: Submission", base+10, "/login"),
		writerEvent(1, "Form: Submission", base+20, "/register"),
	}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events WHERE bot_reason_id <> 0"); got != 0 {
		t.Fatalf("%d events were classified on a visit that loaded a page, want 0", got)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM session_sampling WHERE is_bot = 1"); got != 0 {
		t.Fatalf("%d sampled sessions were marked automated, want 0", got)
	}
}
