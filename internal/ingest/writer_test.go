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
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// newWriter builds a writer over a temporary data directory. Account databases
// are created on first use, so nothing has to be set up beforehand.
func newWriter(t testing.TB) (*Writer, *accounts.Manager) {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() { manager.CloseAll() })

	writer := NewWriter(manager, NewSessionCache())
	writer.Now = func() time.Time { return fixtureStart }

	return writer, manager
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
	e.Source = "Google"
	e.Channel = "Organic Search"

	return e
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

// TestDedupeTableIsPruned checks the table stays bounded. Twenty-four hours is
// what keeps the index small enough for the lookup to stay cheap on the write
// path.
func TestDedupeTableIsPruned(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	clock := fixtureStart
	writer.Now = func() time.Time { return clock }

	if _, err := writer.Write(ctx, []Event{writerEvent(1, EventPageview, fixtureStart.Unix(), "/")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM recent_event_ids"); got != 1 {
		t.Fatalf("recent_event_ids holds %d rows, want 1", got)
	}

	// A day and a minute later, the first id is past retention and the next
	// write carries the prune along with it.
	clock = fixtureStart.Add(DedupeRetention + time.Minute)

	if _, err := writer.Write(ctx, []Event{writerEvent(1, EventPageview, clock.Unix(), "/later")}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM recent_event_ids"); got != 1 {
		t.Fatalf("recent_event_ids holds %d rows after pruning, want 1", got)
	}
}

// TestSessionIDsSurviveARestart checks the id allocator reads the file's high
// water mark. Without it a new process would hand out ids that already exist and
// overwrite finished visits.
func TestSessionIDsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first := accounts.NewManager(dir)
	writerOne := NewWriter(first, NewSessionCache())
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
	t.Cleanup(func() { second.CloseAll() })

	writerTwo := NewWriter(second, NewSessionCache())
	writerTwo.Now = func() time.Time { return fixtureStart }

	later := writerEvent(1, EventPageview, fixtureStart.Unix()+100000, "/after-restart")
	if _, err := writerTwo.Write(ctx, []Event{later}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, second, 1, "SELECT COUNT(*) FROM sessions"); got != 2 {
		t.Fatalf("sessions table holds %d rows after a restart, want 2", got)
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
