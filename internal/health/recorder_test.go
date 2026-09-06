//
// recorder_test.go
// Everything that arrived, counted and kept.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// fixture is a system database, an account manager and the two stores, with a
// clock the test drives.
type fixture struct {
	control  *sql.DB
	accounts *accounts.Manager
	sites    *sites.Cache
	recorder *Recorder
	store    *Store
	now      time.Time

	teamID int64
	siteID int64
	domain string
}

// newFixture builds and seeds everything.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	dir := t.TempDir()

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}

	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.System()); err != nil {
		t.Fatalf("migrate control: %v", err)
	}

	f := &fixture{
		control: control,
		now:     time.Date(2026, 8, 30, 12, 30, 0, 0, time.UTC),
		domain:  "acme.example",
	}

	team, err := control.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', ?, ?)`,
		f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}
	f.teamID, _ = team.LastInsertId()

	site, err := control.Exec(`INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		f.teamID, f.domain, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}
	f.siteID, _ = site.LastInsertId()

	f.accounts = accounts.NewManager(dir)
	t.Cleanup(func() {
		if err := f.accounts.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	f.sites = sites.New(control)
	if err := f.sites.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh sites: %v", err)
	}

	f.recorder = NewRecorder(f.accounts, f.sites, nil)
	f.recorder.Now = func() time.Time { return f.now }

	f.store = NewStore(f.accounts, f.sites, control)
	f.store.Now = func() time.Time { return f.now }

	return f
}

// observe records one request with the fixture's ids filled in.
func (f *fixture) observe(o ingest.Observation) {
	o.SiteID = f.siteID
	o.AccountID = f.teamID

	if o.ReceivedAt == 0 {
		o.ReceivedAt = f.now.Unix()
	}

	if o.Debug.Domain == "" {
		o.Debug.Domain = f.domain
	}

	f.recorder.Observe(o)
}

// TestAcceptedAndDroppedAreCountedSeparately checks the two numbers the panel
// leads with.
func TestAcceptedAndDroppedAreCountedSeparately(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 7; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: clientip.SourceForwardedFor}})
	}

	for i := 0; i < 3; i++ {
		f.observe(ingest.Observation{DropReason: ingest.ReasonRateLimited})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 7 || panel.Dropped != 3 {
		t.Fatalf("accepted=%d dropped=%d, want 7 and 3", panel.Accepted, panel.Dropped)
	}

	if len(panel.Drops) != 1 || panel.Drops[0].Reason != ingest.ReasonRateLimited {
		t.Fatalf("the drops are %+v", panel.Drops)
	}

	if panel.Drops[0].Explanation == "" {
		t.Fatal("a drop reason reached the panel with no explanation beside it")
	}
}

// TestAClassifiedEventIsCountedAsBothAcceptedAndClassified checks the
// distinction the incumbent blurs.
//
// A bot is filed rather than thrown away — the row is written with its reason —
// so counting it only as a drop would tell a customer their data is gone when
// it is not, and counting it only as accepted would hide the bot filter.
func TestAClassifiedEventIsCountedAsBothAcceptedAndClassified(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		f.observe(ingest.Observation{Accepted: true, DropReason: ingest.ReasonBot})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 4 {
		t.Errorf("a classified event was not counted as accepted: %d", panel.Accepted)
	}

	if panel.Classified != 4 {
		t.Errorf("a classified event was not counted as classified: %d", panel.Classified)
	}

	if panel.Dropped != 0 {
		t.Errorf("a classified event was counted as dropped: %d", panel.Dropped)
	}
}

// TestTruncationsAreCountedPerReason checks that nothing an event carried
// vanishes without a number beside it.
func TestTruncationsAreCountedPerReason(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.observe(ingest.Observation{
		Accepted: true,
		Truncation: ingest.Truncation{
			PropsDropped:      5,
			PropsUnsupported:  2,
			URLTruncated:      true,
			EngagementClamped: true,
		},
	})

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	counts := map[string]int64{}
	for _, line := range panel.Truncations {
		counts[line.Reason] = line.Count
	}

	want := map[string]int64{
		ingest.TruncationProps:           1,
		ingest.TruncationPropUnsupported: 2,
		ingest.TruncationURL:             1,
		ingest.TruncationEngagement:      1,
	}

	for reason, expected := range want {
		if counts[reason] != expected {
			t.Errorf("%s counted %d, want %d", reason, counts[reason], expected)
		}
	}
}

// TestTheLastRequestIsKeptWhole checks the debug output that would otherwise
// need a curl.
func TestTheLastRequestIsKeptWhole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.observe(ingest.Observation{
		Accepted:       true,
		UserAgent:      "Mozilla/5.0 (a browser)",
		TrackerVersion: 1,
		Debug: ingest.Debug{
			ClientIP:       "203.0.113.9",
			ClientIPSource: clientip.SourceForwardedFor,
			TrustedProxy:   true,
			Hostname:       "acme.example",
			Pathname:       "/pricing",
			Country:        "GB",
		},
	})

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	last := panel.LastRequest
	if last == nil {
		t.Fatal("no last request was recorded")
	}

	if last.ClientIP != "203.0.113.9" {
		t.Errorf("the resolved address is %q", last.ClientIP)
	}

	if last.ClientIPSource != clientip.SourceForwardedFor {
		t.Errorf("the address source is %q — this is the field the whole panel exists for", last.ClientIPSource)
	}

	if !last.TrustedProxy {
		t.Error("the trusted-proxy flag did not survive")
	}

	if last.Debug["country"] != "GB" {
		t.Errorf("the derived event was not kept whole: %+v", last.Debug)
	}
}

// TestFlushIsIdempotentAcrossTwoCalls checks that a flush clears what it wrote,
// so a minute's traffic is not counted twice.
func TestFlushIsIdempotentAcrossTwoCalls(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.observe(ingest.Observation{Accepted: true})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 5 {
		t.Fatalf("accepted=%d after two flushes, want 5", panel.Accepted)
	}
}

// TestCountsAccumulateAcrossFlushes checks the upsert, since the recorder
// flushes every minute and the panel reads a whole day.
func TestCountsAccumulateAcrossFlushes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for round := 0; round < 3; round++ {
		for i := 0; i < 4; i++ {
			f.observe(ingest.Observation{Accepted: true})
		}

		if _, err := f.recorder.Flush(ctx); err != nil {
			t.Fatalf("flush %d: %v", round, err)
		}
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 12 {
		t.Fatalf("accepted=%d across three flushes, want 12", panel.Accepted)
	}
}

// TestPendingRequestDoesNotClaimAWrite checks the gap between answering 202
// and the shard making its final decision. The request details should be
// visible immediately, but no accepted or dropped count exists yet.
func TestPendingRequestDoesNotClaimAWrite(t *testing.T) {
	f := newFixture(t)

	f.observe(ingest.Observation{
		Pending: true,
		Debug: ingest.Debug{
			ClientIP:       "203.0.113.9",
			ClientIPSource: clientip.SourceForwardedFor,
		},
	})

	if _, err := f.recorder.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(context.Background(), f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 0 || panel.Dropped != 0 {
		t.Fatalf("pending request counted as accepted=%d dropped=%d", panel.Accepted, panel.Dropped)
	}
	if panel.LastRequest == nil || panel.LastRequest.ClientIP != "203.0.113.9" {
		t.Fatalf("pending request details were not kept: %+v", panel.LastRequest)
	}
}

// TestFailedFlushRequeuesEveryRow checks that a transient account write error
// delays health evidence rather than discarding it. All three destination
// tables are hidden for one flush so counts, observations and last-request
// state each exercise their own requeue path.
func TestFailedFlushRequeuesEveryRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.observe(ingest.Observation{
		Accepted: true,
		Debug: ingest.Debug{
			ClientIP:       "203.0.113.9",
			ClientIPSource: clientip.SourceForwardedFor,
		},
	})

	account, err := f.accounts.Open(ctx, f.teamID)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}

	for _, table := range []string{"ingest_health", "ingest_observations", "ingest_last_request"} {
		if _, err := account.Writer().ExecContext(ctx, "ALTER TABLE "+table+" RENAME TO unavailable_"+table); err != nil {
			t.Fatalf("hide %s: %v", table, err)
		}
	}

	if _, err := f.recorder.Flush(ctx); err == nil {
		t.Fatal("flush succeeded while every health table was unavailable")
	}

	for _, table := range []string{"ingest_health", "ingest_observations", "ingest_last_request"} {
		if _, err := account.Writer().ExecContext(ctx, "ALTER TABLE unavailable_"+table+" RENAME TO "+table); err != nil {
			t.Fatalf("restore %s: %v", table, err)
		}
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("retry flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 1 {
		t.Fatalf("accepted=%d after retry, want 1", panel.Accepted)
	}
	if len(panel.IPSources) != 1 || panel.IPSources[0].Count != 1 {
		t.Fatalf("IP source observation was lost or duplicated: %+v", panel.IPSources)
	}
	if panel.LastRequest == nil || panel.LastRequest.ClientIP != "203.0.113.9" {
		t.Fatalf("last request was lost: %+v", panel.LastRequest)
	}
}

// TestAnEventWithNoSiteIsSkipped checks that an unknown domain does not try to
// write into an account database that does not exist.
func TestAnEventWithNoSiteIsSkipped(t *testing.T) {
	f := newFixture(t)

	f.recorder.Observe(ingest.Observation{DropReason: ingest.ReasonUnknownSite})

	written, err := f.recorder.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	if written != 0 {
		t.Fatalf("%d rows were written for an event with no site", written)
	}
}

// TestAnUnexpectedHostnameIsRecorded checks the observation the "events
// arriving from a hostname you did not expect" warning is built from.
//
// A snippet copied onto a staging domain, a scraper mirror or somebody else's
// site is one of the few analytics problems that is completely invisible from
// the numbers alone.
func TestAnUnexpectedHostnameIsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The site's own domain and a subdomain of it are expected.
	f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "acme.example"}})
	f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "blog.acme.example"}})

	// Somebody else's is not.
	for i := 0; i < 3; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "staging.other.example"}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if len(panel.UnknownHostnames) != 1 {
		t.Fatalf("the unexpected hostnames are %+v", panel.UnknownHostnames)
	}

	if panel.UnknownHostnames[0].Value != "staging.other.example" || panel.UnknownHostnames[0].Count != 3 {
		t.Fatalf("the observation is %+v", panel.UnknownHostnames[0])
	}
}

// TestTheReportedAutomationSignalsReachThePanel is how an automation verdict
// becomes checkable.
//
// The letters are recorded whether or not they classified. A panel that only
// showed the ones that convicted could not answer "how often did we see this
// and let it through", which is the question that catches a bad threshold.
func TestTheReportedAutomationSignalsReachThePanel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for range 4 {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{AutomationSignals: "o"}})
	}

	f.observe(ingest.Observation{
		Accepted:   true,
		DropReason: ingest.ReasonAutomation,
		Debug:      ingest.Debug{AutomationSignals: "os"},
	})

	// A browser that reported nothing takes no slot at all.
	f.observe(ingest.Observation{Accepted: true})

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	seen := map[string]int64{}
	for _, signal := range panel.AutomationSignals {
		seen[signal.Value] = signal.Count
	}

	for value, want := range map[string]int64{"o": 4, "os": 1} {
		if seen[value] != want {
			t.Errorf("the panel saw %q %d times, want %d — all of them are %+v",
				value, seen[value], want, panel.AutomationSignals)
		}
	}

	if len(panel.AutomationSignals) != 2 {
		t.Errorf("the panel holds %d signal values, want two", len(panel.AutomationSignals))
	}
}

// TestAnAllowListChangesWhatCountsAsUnexpected checks the other branch: once a
// customer sets an explicit list, "unexpected" means "not on it".
func TestAnAllowListChangesWhatCountsAsUnexpected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := f.store.AllowHostname(ctx, f.domain, "staging.other.example"); err != nil {
		t.Fatalf("allow: %v", err)
	}

	// The site's own domain is on the list now, so it is expected. A subdomain
	// that was expected before no longer is.
	f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "acme.example"}})
	f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "blog.acme.example"}})

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if len(panel.UnknownHostnames) != 1 || panel.UnknownHostnames[0].Value != "blog.acme.example" {
		t.Fatalf("the unexpected hostnames are %+v", panel.UnknownHostnames)
	}
}

// TestObservingIsSafeUnderConcurrency checks the one thing that runs inline on
// the busiest path in the system.
func TestObservingIsSafeUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	done := make(chan struct{})

	for worker := 0; worker < 8; worker++ {
		go func() {
			defer func() { done <- struct{}{} }()

			for i := 0; i < 250; i++ {
				f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: clientip.SourceSocket}})
			}
		}()
	}

	for worker := 0; worker < 8; worker++ {
		<-done
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if panel.Accepted != 2000 {
		t.Fatalf("accepted=%d, want 2000 — an increment was lost", panel.Accepted)
	}
}

// TestNoisySiteCannotExhaustAnotherSitesObservationBudget proves the per-site
// cap is applied before the bounded global ceiling.
func TestNoisySiteCannotExhaustAnotherSitesObservationBudget(t *testing.T) {
	f := newFixture(t)
	result, err := f.control.Exec(`
		INSERT INTO sites (account_id, domain, created_at, updated_at)
		VALUES (?, 'quiet.example', ?, ?)
	`, f.teamID, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	quietID, _ := result.LastInsertId()
	if err := f.sites.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < MaxTrackedValues+20; index++ {
		f.recorder.Observe(ingest.Observation{
			SiteID: f.siteID, AccountID: f.teamID, Accepted: true, ReceivedAt: f.now.Unix(),
			Debug: ingest.Debug{Domain: f.domain, Hostname: fmt.Sprintf("noise-%d.other.test", index)},
		})
	}
	f.recorder.Observe(ingest.Observation{
		SiteID: quietID, AccountID: f.teamID, Accepted: true, ReceivedAt: f.now.Unix(),
		Debug: ingest.Debug{Domain: "quiet.example", Hostname: "unexpected.other.test"},
	})

	trackedNoisy, trackedQuiet := 0, 0
	for key := range f.recorder.observed {
		switch key.siteID {
		case f.siteID:
			trackedNoisy++
		case quietID:
			trackedQuiet++
		}
	}
	if trackedNoisy != MaxTrackedValues || trackedQuiet != 1 {
		t.Fatalf("noisy/quiet tracked values = %d/%d, want %d/1", trackedNoisy, trackedQuiet, MaxTrackedValues)
	}
}

// TestGlobalObservationBudgetIsFairAcrossManySites fills more than eight sites
// in arrival order and verifies the hard 400-value ceiling is shared rather
// than consumed by whichever sites happened to report first.
func TestGlobalObservationBudgetIsFairAcrossManySites(t *testing.T) {
	f := newFixture(t)
	siteIDs := []int64{f.siteID}
	for index := 1; index < 10; index++ {
		result, err := f.control.Exec(`
			INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)
		`, f.teamID, fmt.Sprintf("site-%d.example", index), f.now.Unix(), f.now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		siteID, _ := result.LastInsertId()
		siteIDs = append(siteIDs, siteID)
	}

	for _, siteID := range siteIDs {
		for value := 0; value < MaxTrackedValues; value++ {
			f.recorder.note(f.teamID, siteID, KindIPSource, fmt.Sprintf("source-%d", value), f.now.Unix())
		}
	}

	counts := map[int64]int{}
	for key := range f.recorder.observed {
		counts[key.siteID]++
	}
	if len(f.recorder.observed) != MaxTrackedValuesGlobal {
		t.Fatalf("global observations = %d, want %d", len(f.recorder.observed), MaxTrackedValuesGlobal)
	}
	minimum, maximum := MaxTrackedValues, 0
	for _, siteID := range siteIDs {
		if counts[siteID] == 0 {
			t.Fatalf("site %d retained no evidence: %+v", siteID, counts)
		}
		minimum = min(minimum, counts[siteID])
		maximum = max(maximum, counts[siteID])
	}
	if maximum-minimum > 1 {
		t.Fatalf("global allocation is not fair: min=%d max=%d counts=%+v", minimum, maximum, counts)
	}
}

// countingOpener wraps a real manager and counts how many times a flush reached
// for an account database.
type countingOpener struct {
	inner  *accounts.Manager
	opens  int
	failOn map[int64]bool

	// before runs on each open, which is how a test puts the hot path inside
	// the window where a flush has already swapped the buffers out.
	before func()
}

// Acquire counts the call and hands back a real lease, unless the test wants
// this account to be unreachable.
func (c *countingOpener) Acquire(ctx context.Context, id int64) (*accounts.Lease, error) {
	c.opens++

	if c.before != nil {
		c.before()
	}

	if c.failOn[id] {
		return nil, errors.New("this account is busy")
	}

	return c.inner.Acquire(ctx, id)
}

// TestAFlushOpensEachAccountOnceWhateverItHolds is the acceptance criterion.
//
// A flush writes as many rows as the minute produced, and each one is a
// statement in the account's single transaction rather than its own open and
// its own disk sync.
func TestAFlushOpensEachAccountOnceWhateverItHolds(t *testing.T) {
	f := newFixture(t)

	counting := &countingOpener{inner: f.accounts, failOn: map[int64]bool{}}
	f.recorder.Accounts = counting

	// Several counter keys, several observation values and a last request, all
	// for the one account the fixture has.
	for _, reason := range []string{ingest.ReasonHostnameNotAllowed, ingest.ReasonShieldIP, ingest.ReasonBot} {
		f.observe(ingest.Observation{DropReason: reason})
	}

	for _, hostname := range []string{"a.example", "b.example", "c.example"} {
		f.observe(ingest.Observation{Debug: ingest.Debug{Hostname: hostname}})
	}

	written, err := f.recorder.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if written == 0 {
		t.Fatal("the flush wrote nothing, so this proves nothing")
	}

	if counting.opens != 1 {
		t.Errorf("the flush opened the account %d times, want once for %d rows", counting.opens, written)
	}

	// The count it reports has to be the number of rows it wrote. A flush that
	// over-reports is a recorder that has half stopped and still looks healthy,
	// and the panel it feeds says "no drops" when there were some.
	if landed := f.rowsFor(t, f.teamID); written != landed {
		t.Errorf("the flush reported %d rows written and %d landed", written, landed)
	}
}

// TestAFlushGroupsByAccountNotBySite keeps two sites in one account from
// costing two opens, and two accounts from sharing one.
//
// It drives the recorder directly rather than through the fixture's helper,
// which fills in the fixture's own ids and would hide the distinction this
// grouping is about.
func TestAFlushGroupsByAccountNotBySite(t *testing.T) {
	f := newFixture(t)

	second := f.addSite(t, "second.example")
	other := f.addAccount(t, "other.example")

	counting := &countingOpener{inner: f.accounts, failOn: map[int64]bool{}}
	f.recorder.Accounts = counting

	for _, seen := range []ingest.Observation{
		{SiteID: f.siteID, AccountID: f.teamID, DropReason: ingest.ReasonBot, ReceivedAt: f.now.Unix()},
		{SiteID: second, AccountID: f.teamID, DropReason: ingest.ReasonBot, ReceivedAt: f.now.Unix()},
		{SiteID: other.siteID, AccountID: other.accountID, DropReason: ingest.ReasonBot, ReceivedAt: f.now.Unix()},
	} {
		f.recorder.Observe(seen)
	}

	written, err := f.recorder.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if counting.opens != 2 {
		t.Errorf("three sites across two accounts cost %d opens, want two", counting.opens)
	}

	landed := f.rowsFor(t, f.teamID) + f.rowsFor(t, other.accountID)
	if written != landed {
		t.Errorf("the flush reported %d rows written and %d landed", written, landed)
	}

	// Both accounts were written, not one twice.
	if f.rowsFor(t, other.accountID) == 0 {
		t.Error("the second account was not written to")
	}
}

// TestADeletedAccountIsDroppedRatherThanRetriedForEver keeps health rows for a
// customer who no longer exists from being held in memory and reported as a
// failure every minute for the life of the process.
func TestADeletedAccountIsDroppedRatherThanRetriedForEver(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.observe(ingest.Observation{DropReason: ingest.ReasonBot})

	if err := f.accounts.Delete(f.teamID); err != nil {
		t.Fatal(err)
	}

	written, err := f.recorder.Flush(ctx)
	if err != nil {
		t.Errorf("a deleted account was reported as a flush failure: %v", err)
	}

	if written != 0 {
		t.Errorf("%d rows were written to a deleted account", written)
	}

	// Nothing left behind: a second flush has no work and therefore no failure.
	f.recorder.mu.Lock()
	held := len(f.recorder.counts) + len(f.recorder.observed) + len(f.recorder.last)
	f.recorder.mu.Unlock()

	if held != 0 {
		t.Errorf("%d rows are still queued for an account that no longer exists", held)
	}
}

// TestARequeueCannotOutgrowTheAdmissionCap is what stops a flush that keeps
// failing turning a site behind wildcard DNS into unbounded memory.
func TestARequeueCannotOutgrowTheAdmissionCap(t *testing.T) {
	f := newFixture(t)

	counting := &countingOpener{inner: f.accounts, failOn: map[int64]bool{f.teamID: true}}
	f.recorder.Accounts = counting

	round := 0

	// The hot path runs after the flush has swapped the buffers out and before
	// it fails, which is the window where fifty fresh values are admitted and
	// the failed flush then puts the previous fifty back on top.
	counting.before = func() {
		for i := range MaxTrackedValues {
			f.observe(ingest.Observation{
				Debug: ingest.Debug{Hostname: fmt.Sprintf("r%d-h%d.example", round, i)},
			})
		}
	}

	for range MaxTrackedValues {
		f.observe(ingest.Observation{Debug: ingest.Debug{Hostname: "seed.example"}})
	}

	for ; round < 5; round++ {
		if _, err := f.recorder.Flush(context.Background()); err == nil {
			t.Fatal("the unreachable account was reported as a successful flush")
		}

		f.recorder.mu.Lock()
		held := len(f.recorder.observed)
		f.recorder.mu.Unlock()

		if held > MaxTrackedValues {
			t.Fatalf("round %d holds %d observations, want at most the cap of %d",
				round+1, held, MaxTrackedValues)
		}
	}
}

// TestOneUnreachableAccountRequeuesItsWholeShare is the requeue guarantee at its
// new granularity. The transaction rolled back, so none of the account's rows
// landed and all of them have to come back.
func TestOneUnreachableAccountRequeuesItsWholeShare(t *testing.T) {
	f := newFixture(t)

	counting := &countingOpener{inner: f.accounts, failOn: map[int64]bool{f.teamID: true}}
	f.recorder.Accounts = counting

	for _, reason := range []string{ingest.ReasonHostnameNotAllowed, ingest.ReasonShieldIP} {
		f.observe(ingest.Observation{DropReason: reason})
	}

	f.observe(ingest.Observation{Debug: ingest.Debug{Hostname: "a.example"}})

	written, err := f.recorder.Flush(context.Background())
	if err == nil {
		t.Fatal("an unreachable account was reported as a successful flush")
	}

	if written != 0 {
		t.Errorf("%d rows were counted as written for an account that could not be opened", written)
	}

	// The next flush, with the account reachable, writes everything the failed
	// one was holding.
	counting.failOn = map[int64]bool{}

	again, err := f.recorder.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if again < 3 {
		t.Errorf("the retry wrote %d rows, want everything the failed flush was holding", again)
	}
}

// TestARolledBackAccountLeavesNoHalfWrittenRows is what the transaction buys.
func TestARolledBackAccountLeavesNoHalfWrittenRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.observe(ingest.Observation{DropReason: ingest.ReasonBot})

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// A write that cannot land: the observations table is renamed out from
	// under the flush.
	lease, err := f.accounts.Acquire(ctx, f.teamID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := lease.Account.Writer().ExecContext(ctx,
		"ALTER TABLE ingest_observations RENAME TO ingest_observations_gone"); err != nil {
		t.Fatal(err)
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}

	f.observe(ingest.Observation{DropReason: ingest.ReasonShieldIP})
	f.observe(ingest.Observation{Debug: ingest.Debug{Hostname: "a.example"}})

	before := f.countRows(t, "ingest_health")

	if _, err := f.recorder.Flush(ctx); err == nil {
		t.Fatal("a broken table was reported as a successful flush")
	}

	if after := f.countRows(t, "ingest_health"); after != before {
		t.Errorf("the counts table moved from %d rows to %d despite the flush failing", before, after)
	}
}

// addSite puts a second site in the fixture's one account.
func (f *fixture) addSite(t *testing.T, domain string) int64 {
	t.Helper()

	result, err := f.control.Exec(
		`INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		f.teamID, domain, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	if err := f.sites.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	return id
}

// addAccount inserts a second team with a site of its own, so a test can tell
// grouping by account apart from grouping by site.
func (f *fixture) addAccount(t *testing.T, domain string) struct{ accountID, siteID int64 } {
	t.Helper()

	team, err := f.control.Exec(
		`INSERT INTO teams (name, created_at, updated_at) VALUES (?, ?, ?)`,
		domain, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}

	accountID, err := team.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	site, err := f.control.Exec(
		`INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		accountID, domain, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}

	siteID, err := site.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	if err := f.sites.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	return struct{ accountID, siteID int64 }{accountID, siteID}
}

// countRows counts one table in the fixture's own account database.
func (f *fixture) countRows(t *testing.T, table string) int {
	t.Helper()

	return f.countIn(t, f.teamID, table)
}

// rowsFor is every health row one account holds, which is what a flush's
// reported count has to equal.
func (f *fixture) rowsFor(t *testing.T, accountID int64) int {
	t.Helper()

	total := 0
	for _, table := range []string{"ingest_health", "ingest_observations", "ingest_last_request"} {
		total += f.countIn(t, accountID, table)
	}

	return total
}

// countIn counts one table in one account's database.
func (f *fixture) countIn(t *testing.T, accountID int64, table string) int {
	t.Helper()

	lease, err := f.accounts.Acquire(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}

	defer lease.Release() //nolint:errcheck // the count is the point

	var rows int

	//nolint:gosec // the table name is a constant from this test
	if err := lease.Account.Reader().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&rows); err != nil {
		t.Fatal(err)
	}

	return rows
}

// TestARecorderThatStopsWritingSaysSo is the failure the written count exists
// to catch: a panel reading "no drops" because nothing is being recorded looks
// exactly like a healthy quiet site.
func TestARecorderThatStopsWritingSaysSo(t *testing.T) {
	f := newFixture(t)

	var said []string

	f.recorder.Log = logger.New(logger.Options{Level: "error", Output: io.Discard})

	// The flush is driven directly rather than through Run, so the test does
	// not wait a minute per tick.
	quiet := func() {
		f.recorder.noteFlush(0, nil)

		if f.recorder.quiet == QuietFlushes {
			said = append(said, "warned")
		}
	}

	// Events arriving, nothing written.
	for range QuietFlushes {
		f.observe(ingest.Observation{DropReason: ingest.ReasonBot})
		quiet()
	}

	if len(said) != 1 {
		t.Errorf("a recorder that wrote nothing for %d flushes said nothing", QuietFlushes)
	}

	// A genuinely quiet install says nothing at all.
	f.recorder.quiet = 0
	said = nil

	for range QuietFlushes * 2 {
		quiet()
	}

	if len(said) != 0 {
		t.Error("an install with no traffic was reported as a broken recorder")
	}

	// And a flush that wrote something clears it.
	f.recorder.quiet = QuietFlushes - 1
	f.recorder.noteFlush(1, nil)

	if f.recorder.quiet != 0 {
		t.Errorf("a successful flush left the quiet count at %d", f.recorder.quiet)
	}
}
