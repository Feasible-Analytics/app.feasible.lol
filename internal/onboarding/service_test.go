//
// service_test.go
// The sweep: no data against data, sent once, no backfill, and several workers.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
)

// fakeTraffic says whether each team has data, and counts how often it was
// asked.
type fakeTraffic struct {
	mu     sync.Mutex
	data   map[int64]bool
	asked  int
	failed error
}

// HasEvents answers from the map.
func (f *fakeTraffic) HasEvents(_ context.Context, teamID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked++

	return f.data[teamID], f.failed
}

// fakeLinker mints a fixed stop link, or none.
type fakeLinker struct {
	none bool
}

// Link returns a link naming the team.
func (f fakeLinker) Link(teamID int64) string {
	if f.none {
		return ""
	}

	return "https://app.example/unsubscribe/team-" + string(rune('0'+teamID))
}

// sweeper is a service over a real system database with a movable clock and a
// notifier that records what it was handed.
type sweeper struct {
	t       *testing.T
	db      *sql.DB
	service *Service
	traffic *fakeTraffic

	mu    sync.Mutex
	clock time.Time
	sent  []Notice
	fail  error
}

// newSweeper builds the harness with the clock at the onboarding start.
func newSweeper(t *testing.T) *sweeper {
	t.Helper()

	db, _ := openSystem(t)

	h := &sweeper{t: t, db: db, traffic: &fakeTraffic{data: map[int64]bool{}}, clock: start}
	h.service = h.worker()

	return h
}

// worker builds one more service over the same database, as a second process
// would be.
func (h *sweeper) worker() *Service {
	return &Service{
		Store:   NewStore(h.db),
		Outbox:  lifecycle.NewStore(h.db),
		Traffic: h.traffic,
		Notify:  NotifierFunc(h.record),
		Stop:    fakeLinker{},
		BaseURL: "https://app.example",
		Now:     h.now,
	}
}

// record is the notifier: it keeps the notice, or fails when told to.
func (h *sweeper) record(_ context.Context, notice Notice) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.fail != nil {
		return "", h.fail
	}

	h.sent = append(h.sent, notice)

	return "test/accepted", nil
}

// now is the harness clock.
func (h *sweeper) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.clock
}

// at moves the clock.
func (h *sweeper) at(now time.Time) {
	h.mu.Lock()
	h.clock = now
	h.mu.Unlock()
}

// sweep runs one pass and fails the test on an error.
func (h *sweeper) sweep() int {
	h.t.Helper()

	sent, err := h.service.Sweep(context.Background())
	if err != nil {
		h.t.Fatalf("sweep: %v", err)
	}

	return sent
}

// templates lists what has been sent so far, in order.
func (h *sweeper) templates() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := []string{}
	for _, notice := range h.sent {
		out = append(out, notice.Template)
	}

	return out
}

// expect fails unless exactly these templates have been sent, in this order.
func (h *sweeper) expect(want ...string) {
	h.t.Helper()

	got := h.templates()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		h.t.Fatalf("sent = %v, want %v", got, want)
	}
}

// addSite gives a team its first site.
func (h *sweeper) addSite(teamID int64, domain string) {
	h.t.Helper()

	mustExec(h.t, h.db, `INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`,
		teamID*10, teamID, domain)
}

// TestAnAccountWithNoDataGetsAllFourOnceEach walks one account through its
// first week with hourly sweeps and checks every email went out exactly once,
// on its day.
func TestAnAccountWithNoDataGetsAllFourOnceEach(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)
	h.addSite(1, "example.com")

	sentOn := map[string]time.Duration{}

	for elapsed := time.Duration(0); elapsed <= 10*Day; elapsed += time.Hour {
		h.at(signedUp.Add(elapsed))

		before := len(h.templates())
		h.sweep()

		for _, template := range h.templates()[before:] {
			sentOn[template] = elapsed
		}
	}

	h.expect(TemplateWelcome, TemplateNoDataDay1, TemplateNoDataDay3, TemplateNoDataDay7)

	for template, want := range map[string]time.Duration{
		TemplateWelcome: 0, TemplateNoDataDay1: Day, TemplateNoDataDay3: 3 * Day, TemplateNoDataDay7: 7 * Day,
	} {
		if sentOn[template] != want {
			t.Errorf("%s went out %s after sign-up, want %s", template, sentOn[template], want)
		}
	}

	first := h.sent[1]
	if first.To != "owner1@example.com" || first.FirstName != "Sam" || first.SiteDomain != "example.com" ||
		first.InstallURL != "https://app.example/onboarding/10" || first.AddSiteURL != "https://app.example/sites/new" ||
		first.StopURL == "" || first.MessageKey == "" {
		t.Fatalf("day-1 notice = %+v", first)
	}
}

// TestNothingGoesOutOnceDataArrives covers the account that installs the
// snippet on day two: it got the welcome and day 1, and never hears again.
func TestNothingGoesOutOnceDataArrives(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)
	h.addSite(1, "example.com")

	h.at(signedUp)
	h.sweep()
	h.at(signedUp.Add(Day))
	h.sweep()
	h.expect(TemplateWelcome, TemplateNoDataDay1)

	h.traffic.data[1] = true

	for elapsed := 2 * Day; elapsed <= 10*Day; elapsed += 6 * time.Hour {
		h.at(signedUp.Add(elapsed))
		h.sweep()
	}

	h.expect(TemplateWelcome, TemplateNoDataDay1)

	// The remaining steps are settled as skipped, so the account database is
	// not opened again on every sweep and a removed site cannot restart them.
	var skipped int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_outbox WHERE team_id = 1 AND outcome = ?`, SkippedOutcome).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if skipped != 2 {
		t.Fatalf("skipped markers = %d, want 2", skipped)
	}

	asked := h.traffic.asked

	h.traffic.data[1] = false
	h.at(signedUp.Add(7 * Day))
	h.sweep()

	if h.traffic.asked != asked {
		t.Fatal("the account was asked about its data again after the steps were settled")
	}

	h.expect(TemplateWelcome, TemplateNoDataDay1)
}

// TestAnAccountWithDataFromTheStartGetsOnlyTheWelcome is the customer who
// installed straight away.
func TestAnAccountWithDataFromTheStartGetsOnlyTheWelcome(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)
	h.addSite(1, "example.com")
	h.traffic.data[1] = true

	for elapsed := time.Duration(0); elapsed <= 9*Day; elapsed += 12 * time.Hour {
		h.at(signedUp.Add(elapsed))
		h.sweep()
	}

	h.expect(TemplateWelcome)
}

// TestAnAccountWithNoSiteIsStillHelped proves an account that never added a
// site is sent the install help without its account database being opened.
func TestAnAccountWithNoSiteIsStillHelped(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)

	h.at(signedUp)
	h.sweep()
	h.at(signedUp.Add(Day))
	h.sweep()

	h.expect(TemplateWelcome, TemplateNoDataDay1)

	if h.traffic.asked != 0 {
		t.Fatalf("traffic was asked %d times about an account with no site", h.traffic.asked)
	}

	if notice := h.sent[1]; notice.SiteDomain != "" || notice.InstallURL != "" {
		t.Fatalf("day-1 notice names a site the account does not have: %+v", notice)
	}
}

// TestNoBackfill proves an account that signed up before the emails began is
// never written to, however the clock moves.
func TestNoBackfill(t *testing.T) {
	h := newSweeper(t)

	addTeam(t, h.db, 1, start.Add(-time.Minute))
	addTeam(t, h.db, 2, start.Add(-20*Day))

	for elapsed := time.Duration(0); elapsed <= 10*Day; elapsed += 6 * time.Hour {
		h.at(start.Add(elapsed))
		h.sweep()
	}

	h.expect()
}

// TestNothingIsSentWithoutAStartRow covers a database whose start row is
// missing: the sweeper sends nothing rather than treating every account as new.
func TestNothingIsSentWithoutAStartRow(t *testing.T) {
	h := newSweeper(t)

	addTeam(t, h.db, 1, start.Add(time.Hour))
	mustExec(t, h.db, `DELETE FROM onboarding_start`)

	h.at(start.Add(2 * time.Hour))
	if sent := h.sweep(); sent != 0 {
		t.Fatalf("sent %d with no start row", sent)
	}
}

// TestAnUnverifiedOwnerGetsNothingUntilTheyVerify keeps the welcome behind the
// verification email.
func TestAnUnverifiedOwnerGetsNothingUntilTheyVerify(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)
	mustExec(t, h.db, `UPDATE users SET email_verified_at = NULL WHERE id = 1`)

	h.at(signedUp.Add(time.Minute))
	h.sweep()
	h.expect()

	mustExec(t, h.db, `UPDATE users SET email_verified_at = ? WHERE id = 1`, signedUp.Add(2*time.Minute).Unix())

	h.at(signedUp.Add(5 * time.Minute))
	h.sweep()
	h.expect(TemplateWelcome)
}

// TestARestartSendsNothingTwice runs the same instant through a fresh service,
// as a restarted process would, and through repeated sweeps.
func TestARestartSendsNothingTwice(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)

	h.at(signedUp.Add(Day))

	h.sweep()
	h.sweep()

	h.service = h.worker()
	h.sweep()

	h.expect(TemplateWelcome)
}

// TestSeveralWorkersSendEachEmailOnce runs many services over one database at
// the same instant, as several app instances would.
func TestSeveralWorkersSendEachEmailOnce(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	for id := int64(1); id <= 5; id++ {
		addTeam(t, h.db, id, signedUp)
	}

	h.at(signedUp.Add(time.Minute))

	var wg sync.WaitGroup
	for range 8 {
		worker := h.worker()

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = worker.Sweep(context.Background())
		}()
	}
	wg.Wait()

	seen := map[int64]int{}
	for _, notice := range h.sent {
		seen[notice.TeamID]++
	}

	for id := int64(1); id <= 5; id++ {
		if seen[id] != 1 {
			t.Errorf("team %d was sent the welcome %d times", id, seen[id])
		}
	}
}

// TestAFailedSendIsRetried proves a transport failure leaves the email owed.
func TestAFailedSendIsRetried(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)
	addTeam(t, h.db, 2, signedUp)

	h.at(signedUp.Add(time.Minute))
	h.fail = errors.New("relay down")

	if _, err := h.service.Sweep(context.Background()); err == nil {
		t.Fatal("a failed send was not reported")
	}
	h.expect()

	h.fail = nil
	h.at(signedUp.Add(6 * time.Minute))

	if sent := h.sweep(); sent != 2 {
		t.Fatalf("retry sent %d, want 2", sent)
	}
	h.expect(TemplateWelcome, TemplateWelcome)
}

// TestNoEmailLeavesWithoutAStopLink refuses to send when the way out cannot be
// minted.
func TestNoEmailLeavesWithoutAStopLink(t *testing.T) {
	h := newSweeper(t)
	h.service.Stop = fakeLinker{none: true}

	addTeam(t, h.db, 1, start.Add(time.Hour))
	h.at(start.Add(2 * time.Hour))

	if _, err := h.service.Sweep(context.Background()); err == nil {
		t.Fatal("an email with no stop link was not refused")
	}
	h.expect()
}

// TestAnOptOutStopsTheRest is the "stop these" link doing its job.
func TestAnOptOutStopsTheRest(t *testing.T) {
	h := newSweeper(t)

	signedUp := start.Add(time.Hour)
	addTeam(t, h.db, 1, signedUp)

	h.at(signedUp)
	h.sweep()

	if _, err := h.service.Store.OptOut(context.Background(), 1, h.now()); err != nil {
		t.Fatal(err)
	}

	for elapsed := Day; elapsed <= 8*Day; elapsed += 12 * time.Hour {
		h.at(signedUp.Add(elapsed))
		h.sweep()
	}

	h.expect(TemplateWelcome)
}

// TestFirstNameGreetsOnlyByAPlainWord covers the names that would greet worse
// than no name.
func TestFirstNameGreetsOnlyByAPlainWord(t *testing.T) {
	for name, want := range map[string]string{
		"Sam Ellis":             "Sam",
		"  Zoë  ":               "Zoë",
		"Jean-Luc Picard":       "Jean-Luc",
		"":                      "",
		"sam@example.com":       "",
		"S":                     "",
		"Agent007":              "",
		strings.Repeat("a", 30): "",
	} {
		if got := FirstName(name); got != want {
			t.Errorf("FirstName(%q) = %q, want %q", name, got, want)
		}
	}
}
