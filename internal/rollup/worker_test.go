//
// worker_test.go
// What the hourly job decides to build, and where it stops.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package rollup_test

import (
	"context"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
)

// workerOver builds a worker for one already-open account, with a fixed clock.
func workerOver(t *testing.T, dir string, accountID int64, now time.Time) *rollup.Worker {
	t.Helper()

	manager := accounts.NewManager(dir)
	t.Cleanup(func() { _ = manager.CloseAll() })

	return &rollup.Worker{
		Accounts: manager,
		Now:      func() time.Time { return now },
		Sites: func(context.Context) ([]rollup.SiteRef, error) {
			return []rollup.SiteRef{{AccountID: accountID, Site: testSite}}, nil
		},
	}
}

// TestTheWorkerNeverCoversToday is the assembly rule as a test. Today is still
// filling up, so a report drawn from its bucket would be missing however much
// of the day is left — and every dashboard in the product shows today.
func TestTheWorkerNeverCoversToday(t *testing.T) {
	dir := t.TempDir()

	manager := accounts.NewManager(dir)
	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	sessions, events := carryFixture()
	writeFixture(t, account, sessions, events)

	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	worker := workerOver(t, dir, 1, fixtureNow)

	if err := worker.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened, err := worker.Accounts.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	builder := rollup.New(reopened.Writer())
	builder.Now = func() time.Time { return fixtureNow }

	today := query.RollupBucketStart(fixtureNow.In(losAngeles), query.GrainDay, losAngeles)
	boundary := query.RollupLocalUnix(today, losAngeles)

	for _, grain := range []query.Grain{query.GrainDay, query.GrainHour} {
		coverage, found, err := builder.Coverage(context.Background(), testSite.ID, grain)
		if err != nil {
			t.Fatal(err)
		}

		if !found {
			t.Fatalf("%s: the worker built nothing", grain)
		}

		if coverage.Through != boundary {
			t.Errorf("%s coverage reaches %d, want the start of today at %d", grain, coverage.Through, boundary)
		}

		if coverage.Timezone != testSite.Timezone {
			t.Errorf("%s coverage is recorded against %q, want %q", grain, coverage.Timezone, testSite.Timezone)
		}
	}

	// Today's daily row is built even though it is not covered, so that sealing
	// the day at midnight is one small rebuild rather than a day's work.
	var todayRows int
	if err := reopened.Reader().QueryRow(
		"SELECT COUNT(*) FROM rollup_visitors WHERE site_id = ? AND grain = ? AND bucket = ?",
		testSite.ID, int64(query.GrainDay), boundary).Scan(&todayRows); err != nil {
		t.Fatal(err)
	}

	if todayRows == 0 {
		t.Error("today's daily row was not built, so sealing the day will be a full day's work")
	}
}

// TestASecondPassIsCheapAndChangesNothing covers the hourly loop's steady
// state. The worker runs every hour forever, so a pass that re-did the whole
// history would spend more time rebuilding old days than serving reports.
func TestASecondPassIsCheapAndChangesNothing(t *testing.T) {
	dir := t.TempDir()

	manager := accounts.NewManager(dir)
	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	// A month of history, so a second pass has plenty it could pointlessly
	// rebuild.
	var (
		sessions []sessionRow
		events   []eventRow
	)

	for day := 1; day <= 28; day++ {
		sessions = append(sessions, sessionRow{
			id: int64(day), user: int64(5000 + day), startedAt: local(day, 10), lastSeen: local(day, 10),
			bounce: 1, pageviews: 1, entryPage: "/home", exitPage: "/home", source: "Google", country: "US",
		})
		events = append(events, eventRow{
			session: int64(day), user: int64(5000 + day), at: local(day, 10),
			name: ingest.EventPageview, page: "/home", source: "Google", country: "US",
		})
	}

	writeFixture(t, account, sessions, events)

	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	worker := workerOver(t, dir, 1, fixtureNow)

	if err := worker.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	reopened, err := worker.Accounts.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	before := snapshot(t, reopened)

	started := time.Now()
	if err := worker.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := time.Since(started)

	if after := snapshot(t, reopened); after != before {
		t.Errorf("a second pass changed the numbers:\n first  %s\n second %s", before, after)
	}

	// Not a performance assertion so much as a shape one: a steady-state pass
	// rewrites a couple of days, so it cannot take seconds on twenty-eight rows.
	if second > 5*time.Second {
		t.Errorf("a steady-state pass took %v, which means it is rebuilding all of history", second)
	}
}

// TestAWorkerWithNothingToDoDoesNothing covers the empty install. A site with no
// events at all must not produce a coverage row, because a covered window with
// no buckets behind it is a report that reads zero and shows it as fact.
func TestAWorkerWithNothingToDoDoesNothing(t *testing.T) {
	dir := t.TempDir()

	manager := accounts.NewManager(dir)
	if _, err := manager.Open(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	worker := workerOver(t, dir, 1, fixtureNow)

	if err := worker.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	account, err := worker.Accounts.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	builder := rollup.New(account.Writer())

	if _, found, err := builder.Coverage(context.Background(), testSite.ID, query.GrainDay); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("a site with no events was recorded as covered")
	}
}

// TestOneSiteFailingDoesNotStopTheRest is why the loop swallows and reports
// rather than returning at the first error. One unreadable account database
// would otherwise leave every other customer's dashboard on the raw path.
func TestOneSiteFailingDoesNotStopTheRest(t *testing.T) {
	dir := t.TempDir()

	manager := accounts.NewManager(dir)
	account, err := manager.Open(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}

	sessions, events := carryFixture()
	writeFixture(t, account, sessions, events)

	if err := manager.CloseAll(); err != nil {
		t.Fatal(err)
	}

	reopened := accounts.NewManager(dir)
	t.Cleanup(func() { _ = reopened.CloseAll() })

	worker := &rollup.Worker{
		Accounts: reopened,
		Now:      func() time.Time { return fixtureNow },
		Sites: func(context.Context) ([]rollup.SiteRef, error) {
			return []rollup.SiteRef{
				// Account 0 is not a valid id, so opening it fails.
				{AccountID: 0, Site: testSite},
				{AccountID: 2, Site: testSite},
			}, nil
		},
	}

	if err := worker.Once(context.Background()); err == nil {
		t.Error("the pass reported success even though a site failed")
	}

	good, err := reopened.Open(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}

	builder := rollup.New(good.Writer())

	if _, found, err := builder.Coverage(context.Background(), testSite.ID, query.GrainDay); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Error("the site after the failing one was never built")
	}
}
