//
// sweeper_test.go
// The ladder end to end: counters in, emails out, exactly once each.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// sweepStart is the instant the sweeper tests run at: the twentieth of a month,
// far enough in for a projection to mean something.
var sweepStart = time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

// fixture is a system database with one team and a sweeper wired to it.
type fixture struct {
	t       *testing.T
	control *sql.DB
	store   *Store
	sweeper *Sweeper

	mu    sync.Mutex
	clock time.Time
	sent  []Notice
}

// newFixture builds the database and the sweeper.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	control, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.System()); err != nil {
		t.Fatal(err)
	}

	stamp := sweepStart.Unix()

	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, control: control, clock: sweepStart}
	f.store = NewStore(control)
	f.store.Now = f.now

	f.sweeper = &Sweeper{
		Store:      f.store,
		Notify:     notifierFunc(f.capture),
		Contacts:   contactsFunc(func(context.Context, int64) (string, string, error) { return "Example Co", "owner@example.com", nil }),
		Now:        f.now,
		SalesEmail: "sales@feasible.lol",
		BillingURL: "https://feasible.lol/billing",
	}

	return f
}

// now is the injected clock.
func (f *fixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.clock
}

// setClock moves the clock.
func (f *fixture) setClock(at time.Time) {
	f.mu.Lock()
	f.clock = at
	f.mu.Unlock()
}

// capture records a notice instead of sending it.
func (f *fixture) capture(_ context.Context, notice Notice) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sent = append(f.sent, notice)

	return "captured", nil
}

// levels lists the notices sent so far.
func (f *fixture) levels() []Level {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Level, 0, len(f.sent))
	for _, notice := range f.sent {
		out = append(out, notice.Level)
	}

	return out
}

// add writes usage into a period.
func (f *fixture) add(period string, billable int64) {
	f.t.Helper()

	if err := f.store.Add(context.Background(), 1, period, Counts{Pageviews: billable}); err != nil {
		f.t.Fatal(err)
	}
}

// sweep runs one pass.
func (f *fixture) sweep() {
	f.t.Helper()

	if _, err := f.sweeper.Sweep(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// TestThresholdEmailsAreSentOncePerMonth is the core idempotency guarantee for
// the ladder. The sweeper runs hourly, and a customer must not receive the same
// warning twenty-four times a day.
func TestThresholdEmailsAreSentOncePerMonth(t *testing.T) {
	f := newFixture(t)

	f.add("2026-03", 720_000)

	for i := 0; i < 24; i++ {
		f.sweep()
	}

	if got := f.levels(); len(got) != 1 || got[0] != LevelWarn {
		t.Fatalf("sent %v, want one warn", got)
	}
}

// TestClimbingSendsEachRungOnce walks an account up through the three rungs and
// checks each one fires exactly once, in order.
func TestClimbingSendsEachRungOnce(t *testing.T) {
	f := newFixture(t)

	f.add("2026-03", 700_000)
	f.sweep()
	f.sweep()

	f.add("2026-03", 160_000) // 860,000
	f.sweep()

	f.add("2026-03", 200_000) // 1,060,000
	f.sweep()
	f.sweep()

	got := f.levels()
	want := []Level{LevelWarn, LevelNear, LevelReached}

	if len(got) != len(want) {
		t.Fatalf("sent %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("notice %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAJumpStraightToTheLimitStillSendsTheSeventyPercentEmail is why the sweeper
// sends every rung rather than only the highest. The 70% message is the useful
// one, and an account that grew fast between two sweeps is still owed it.
func TestAJumpStraightToTheLimitStillSendsTheSeventyPercentEmail(t *testing.T) {
	f := newFixture(t)

	f.add("2026-03", 1_200_000)
	f.sweep()

	got := f.levels()
	if len(got) != 3 {
		t.Fatalf("sent %v, want all three rungs", got)
	}
	if got[0] != LevelWarn {
		t.Errorf("the first notice is %q, want warn", got[0])
	}
}

// TestReachingTheLimitDoesNotLockAnything is the rule that going over is not a
// payment failure. Nothing is locked, nothing is deleted, and the deletion clock
// is not touched.
func TestReachingTheLimitDoesNotLockAnything(t *testing.T) {
	f := newFixture(t)

	var beforeTrigger string
	var beforeStarted int64
	var beforeDeleted sql.NullInt64
	if err := f.control.QueryRow(`
		SELECT trigger, started_at, deleted_at FROM account_lifecycle WHERE team_id = 1
	`).Scan(&beforeTrigger, &beforeStarted, &beforeDeleted); err != nil {
		t.Fatal(err)
	}

	f.add("2026-03", 1_500_000)
	f.sweep()

	overage, err := f.store.Overage(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	if overage.Locked() {
		t.Fatal("one month over locked the dashboard")
	}

	// Team creation starts the trial clock atomically. Volume overage must leave
	// that existing clock byte-for-byte unchanged rather than starting a lapse.
	var afterTrigger string
	var afterStarted int64
	var afterDeleted sql.NullInt64
	if err := f.control.QueryRow(`
		SELECT trigger, started_at, deleted_at FROM account_lifecycle WHERE team_id = 1
	`).Scan(&afterTrigger, &afterStarted, &afterDeleted); err != nil {
		t.Fatal(err)
	}
	if afterTrigger != beforeTrigger || afterStarted != beforeStarted || afterDeleted != beforeDeleted {
		t.Fatalf("going over the plan changed lifecycle from (%q,%d,%v) to (%q,%d,%v)",
			beforeTrigger, beforeStarted, beforeDeleted, afterTrigger, afterStarted, afterDeleted)
	}
}

// TestTwoConsecutiveMonthsAsksThenLocks is the whole ladder, driven on a clock.
func TestTwoConsecutiveMonthsAsksThenLocks(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// January and February both over, and March in progress and over.
	f.add("2026-01", 1_100_000)
	f.add("2026-02", 1_200_000)
	f.add("2026-03", 1_300_000)

	f.sweep()

	overage, err := f.store.Overage(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if overage.AskedAt.IsZero() {
		t.Fatal("two consecutive months did not open a conversation")
	}
	if overage.Locked() {
		t.Fatal("the dashboard locked immediately")
	}

	found := false
	for _, level := range f.levels() {
		if level == "second_month" {
			found = true
		}
	}
	if !found {
		t.Fatal("the second-month email was not sent")
	}

	// Thirteen days later, still nothing.
	f.setClock(sweepStart.Add(13 * 24 * time.Hour))
	f.sweep()

	overage, err = f.store.Overage(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if overage.Locked() {
		t.Fatal("the dashboard locked before the two weeks were up")
	}

	// Two weeks and a day.
	f.setClock(sweepStart.Add(15 * 24 * time.Hour))
	f.sweep()

	overage, err = f.store.Overage(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !overage.Locked() {
		t.Fatal("the dashboard did not lock after the reply window expired")
	}

	locked, err := f.store.LockedTeams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 1 || locked[0] != 1 {
		t.Fatalf("the locked set is %v", locked)
	}
}

// TestAReplyUnlocks covers the human ending the conversation.
func TestAReplyUnlocks(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.add("2026-01", 1_100_000)
	f.add("2026-02", 1_200_000)
	f.add("2026-03", 1_300_000)

	f.sweep()
	f.setClock(sweepStart.Add(15 * 24 * time.Hour))
	f.sweep()

	if err := f.store.MarkReplied(ctx, 1); err != nil {
		t.Fatal(err)
	}

	locked, err := f.store.LockedTeams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 0 {
		t.Fatalf("the account is still locked after a reply: %v", locked)
	}

	// And a later sweep must not lock it again.
	f.setClock(sweepStart.Add(30 * 24 * time.Hour))
	f.sweep()

	overage, err := f.store.Overage(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if overage.Locked() {
		t.Fatal("an account that replied was locked again by a later sweep")
	}
}

// TestConsecutiveOverExcludesTheMonthInProgress is the arithmetic behind "two
// consecutive months". Counting a partial month would lock somebody on the third
// of the month for a month that has barely started.
func TestConsecutiveOverExcludesTheMonthInProgress(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	f.add("2026-03", 5_000_000)

	count, err := f.store.ConsecutiveOver(ctx, 1, "2026-03")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("the month in progress counted as %d complete months", count)
	}

	f.add("2026-02", 1_100_000)
	f.add("2026-01", 1_100_000)

	count, err = f.store.ConsecutiveOver(ctx, 1, "2026-03")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("two complete months over counted as %d", count)
	}

	// A month inside the plan breaks the run.
	f.add("2025-12", 100)

	count, err = f.store.ConsecutiveOver(ctx, 1, "2026-03")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("a month inside the plan did not end the run: %d", count)
	}
}

// TestCountersAccumulate checks the upsert the shard flushes into, including
// that two flushes add rather than overwrite.
func TestCountersAccumulate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	if err := f.store.Add(ctx, 1, "2026-03", Counts{Pageviews: 100, CustomEvents: 5}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Add(ctx, 1, "2026-03", Counts{Pageviews: 50, CustomEvents: 2}); err != nil {
		t.Fatal(err)
	}

	counts, err := f.store.Get(ctx, 1, "2026-03")
	if err != nil {
		t.Fatal(err)
	}

	if counts.Pageviews != 150 || counts.CustomEvents != 7 {
		t.Fatalf("counters are %+v", counts)
	}

	// A month with no row is zero rather than an error: that is the normal state
	// of a new account, not a missing record.
	empty, err := f.store.Get(ctx, 1, "2026-04")
	if err != nil {
		t.Fatal(err)
	}
	if empty.Billable() != 0 {
		t.Fatalf("an empty month reads as %+v", empty)
	}
}

// TestRecorderFlushesAndSurvivesAMonthBoundary covers the in-memory counter the
// shard writes into. Events either side of a month boundary must land in the
// month they happened in, not the month the flush ran in.
func TestRecorderFlushesAndSurvivesAMonthBoundary(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	recorder := NewRecorder(f.store)

	march := time.Date(2026, time.March, 31, 23, 59, 0, 0, time.UTC)
	april := time.Date(2026, time.April, 1, 0, 1, 0, 0, time.UTC)

	at := march
	recorder.Now = func() time.Time { return at }

	recorder.Record(1, Counts{Pageviews: 10})

	at = april
	recorder.Record(1, Counts{Pageviews: 4, CustomEvents: 1})

	if err := recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	marchCounts, err := f.store.Get(ctx, 1, "2026-03")
	if err != nil {
		t.Fatal(err)
	}
	if marchCounts.Billable() != 10 {
		t.Errorf("March holds %d, want 10", marchCounts.Billable())
	}

	aprilCounts, err := f.store.Get(ctx, 1, "2026-04")
	if err != nil {
		t.Fatal(err)
	}
	if aprilCounts.Billable() != 5 {
		t.Errorf("April holds %d, want 5", aprilCounts.Billable())
	}

	if recorder.Pending() != 0 {
		t.Errorf("%d account-months are still pending after a flush", recorder.Pending())
	}
}

// TestRecorderIgnoresNothingToCount keeps the hot path free of pointless work
// and keeps zero rows out of the counters table.
func TestRecorderIgnoresNothingToCount(t *testing.T) {
	f := newFixture(t)
	recorder := NewRecorder(f.store)

	recorder.Record(1, Counts{})
	recorder.Record(0, Counts{Pageviews: 5})

	if recorder.Pending() != 0 {
		t.Fatalf("%d account-months pending, want 0", recorder.Pending())
	}
}

// notifierFunc adapts a function to the Notifier interface.
type notifierFunc func(ctx context.Context, notice Notice) (string, error)

// Notify calls the function.
func (f notifierFunc) Notify(ctx context.Context, notice Notice) (string, error) {
	return f(ctx, notice)
}

// contactsFunc adapts a function to the Contacts interface.
type contactsFunc func(ctx context.Context, teamID int64) (string, string, error)

// Contact calls the function.
func (f contactsFunc) Contact(ctx context.Context, teamID int64) (string, string, error) {
	return f(ctx, teamID)
}
