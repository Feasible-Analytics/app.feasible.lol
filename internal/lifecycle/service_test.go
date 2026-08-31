//
// service_test.go
// A simulated ninety-one days, ending with the data actually gone.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// harness is a whole install in a temporary directory: a migrated control
// database, one team with an owner, and that team's analytics database on disk.
//
// It is a real database rather than a fake because the thing under test is the
// code that deletes files and rows. A mock that "deleted" nothing would pass the
// most important test in the package while the real path left a customer's data
// on the disk — or removed the wrong one.
type harness struct {
	t       *testing.T
	dataDir string
	control *sql.DB
	store   *Store
	service *Service
	purger  *Purger
	manager *accounts.Manager

	// clock is the injected time. Nothing in the test waits for anything.
	clock time.Time

	mu   sync.Mutex
	sent []Notice
}

// newHarness builds the install and puts one team in it.
func newHarness(t *testing.T) *harness {
	t.Helper()

	dataDir := t.TempDir()

	control, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	now := day0.Unix()

	if _, err := control.Exec(`
		INSERT INTO users (id, email, name, created_at, updated_at) VALUES (1, 'owner@example.com', 'Owner', ?, ?)
	`, now, now); err != nil {
		t.Fatal(err)
	}

	if _, err := control.Exec(`
		INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)
	`, now, now); err != nil {
		t.Fatal(err)
	}

	if _, err := control.Exec(`
		INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (1, 1, 'owner', ?)
	`, now); err != nil {
		t.Fatal(err)
	}

	if _, err := control.Exec(`
		INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', ?, ?)
	`, now, now); err != nil {
		t.Fatal(err)
	}

	manager := accounts.NewManager(dataDir)

	// Opening the account creates its database file and brings it up to the
	// current schema, which is exactly the state a real account is in on day 0.
	if _, err := manager.Open(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	h := &harness{
		t:       t,
		dataDir: dataDir,
		control: control,
		store:   NewStore(control),
		manager: manager,
		clock:   day0,
	}

	h.purger = &Purger{Store: h.store, Accounts: manager, DataDir: dataDir}

	h.service = &Service{
		Store:  h.store,
		Notify: NotifierFunc(h.record),
		Purger: h.purger,
		Links:  Links{BaseURL: "https://feasible.lol"},
		Now:    func() time.Time { return h.now() },
	}

	return h
}

// now is the injected clock, read under a lock because the service reads it
// while the test writes it.
func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.clock
}

// travel moves the clock to a whole number of days after day 0.
func (h *harness) travel(days int) {
	h.setClock(at(days))
}

// setClock moves the injected clock to an exact instant.
func (h *harness) setClock(now time.Time) {
	h.mu.Lock()
	h.clock = now
	h.mu.Unlock()
}

// record captures a notice instead of sending it.
func (h *harness) record(_ context.Context, notice Notice) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.sent = append(h.sent, notice)

	return "captured", nil
}

// templates lists what has been sent so far, in order.
func (h *harness) templates() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]string, 0, len(h.sent))
	for _, notice := range h.sent {
		out = append(out, notice.Template)
	}

	return out
}

// sweep runs one pass at the current clock.
func (h *harness) sweep() {
	h.t.Helper()

	if _, err := h.service.Sweep(context.Background()); err != nil {
		h.t.Fatalf("sweep at day %d: %v", h.service.now().Sub(day0)/Day, err)
	}
}

// accountFile is the path to the team's analytics database.
func (h *harness) accountFile() string {
	return accounts.Path(h.dataDir, 1)
}

// exists reports whether a path is on disk.
func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// TestNinetyOneDayTimeline is the integration test the specification asks for.
// It drives a full lifecycle a day at a time on an injected clock and asserts,
// at the end, that the data is actually gone from the disk.
func TestNinetyOneDayTimeline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if !exists(h.accountFile()) {
		t.Fatal("the account database was never created")
	}

	if _, err := h.service.Signal(ctx, 1, SignalTrialStarted); err != nil {
		t.Fatal(err)
	}

	// A day-by-day sweep, which is what a real deployment does hourly. Anything
	// that only works when the sweep lands exactly on a boundary would fail here.
	for day := 0; day <= 91; day++ {
		h.travel(day)
		h.sweep()
	}

	// Every message in the sequence, once, in order.
	got := h.templates()

	want := []string{
		TemplateEndingSoon,
		TemplateEndingTomorrow,
		TemplateDashboardLocked,
		TemplateCollectionStopsIn15,
		TemplateCollectionStopsTomorrow,
		TemplateCollectionStopped,
		TemplateDeletionIn15,
		TemplateDeletionIn5,
		TemplateDeletionTomorrow,
		TemplateAccountDeleted,
	}

	if len(got) != len(want) {
		t.Fatalf("sent %d emails, want %d: %v", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("email %d is %q, want %q", i, got[i], want[i])
		}
	}

	// The whole point of the exercise: the data is gone.
	if exists(h.accountFile()) {
		t.Error("the account database is still on disk after day 90")
	}
	if exists(accounts.Dir(h.dataDir, 1)) {
		t.Error("the account directory is still on disk after day 90")
	}

	var teams int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM teams WHERE id = 1`).Scan(&teams); err != nil {
		t.Fatal(err)
	}
	if teams != 0 {
		t.Error("the team row survived the deletion")
	}

	var sites int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM sites WHERE account_id = 1`).Scan(&sites); err != nil {
		t.Fatal(err)
	}
	if sites != 0 {
		t.Error("the site rows survived the deletion — the cascade is not doing its job")
	}

	// The audit record is the one thing that survives, and it has to, or nobody
	// could answer "did you delete my account, and when".
	var deletedAt sql.NullInt64
	if err := h.control.QueryRow(`SELECT completed_at FROM account_deletions WHERE team_id = 1`).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if !deletedAt.Valid {
		t.Error("the deletion was never recorded as complete")
	}
}

// TestPayingOnDayEightyNineRestoresTheAccount is the recovery path end to end:
// the file survives, the clock stops, and every pending email is cancelled.
func TestPayingOnDayEightyNineRestoresTheAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	for day := 0; day <= 89; day++ {
		h.travel(day)
		h.sweep()
	}

	if !exists(h.accountFile()) {
		t.Fatal("the account was deleted before day 90")
	}

	transition, err := h.service.Signal(ctx, 1, SignalPaymentSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if transition.To != PhaseActive {
		t.Fatalf("paying on day 89 left the account %q", transition.To)
	}

	// Two more days, past the deletion date the clock had been counting to.
	for day := 90; day <= 95; day++ {
		h.travel(day)
		h.sweep()
	}

	if !exists(h.accountFile()) {
		t.Fatal("the account was deleted after it had been paid for")
	}

	// The account must also be accepting traffic again. This reads the column the
	// ingest path's snapshot is built from, which is the one that decides whether
	// a customer's site is being counted.
	var acceptUntil sql.NullInt64
	if err := h.control.QueryRow(`SELECT accept_traffic_until FROM teams WHERE id = 1`).Scan(&acceptUntil); err != nil {
		t.Fatal(err)
	}
	if acceptUntil.Valid {
		t.Errorf("accept_traffic_until is still set to %d after payment", acceptUntil.Int64)
	}

	// Nothing about deletion may have been sent.
	for _, template := range h.templates() {
		if template == TemplateAccountDeleted {
			t.Fatal("a deletion confirmation was sent for an account that was paid for")
		}
	}
}

// TestPendingEmailsAreCancelledOnPayment is the rule stated on its own: the
// moment an account returns to Active, nothing that has not gone out may go out.
func TestPendingEmailsAreCancelledOnPayment(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalTrialStarted); err != nil {
		t.Fatal(err)
	}

	h.travel(30)
	h.sweep()

	before := len(h.templates())
	if before == 0 {
		t.Fatal("nothing was sent by day 30")
	}

	if _, err := h.service.Signal(ctx, 1, SignalPaymentSucceeded); err != nil {
		t.Fatal(err)
	}

	// A long way past every remaining boundary.
	for day := 31; day <= 120; day++ {
		h.travel(day)
		h.sweep()
	}

	if after := len(h.templates()); after != before {
		t.Fatalf("%d more emails went out after the account was paid for", after-before)
	}
}

// TestLapseNoticeLinksToSafeSelectedBillingPage keeps email prefetchers away
// from the side-effecting portal action while retaining the intended account.
func TestLapseNoticeLinksToSafeSelectedBillingPage(t *testing.T) {
	h := newHarness(t)
	account := Account{
		TeamID:   1,
		TeamName: "Example Co",
		Email:    "owner@example.com",
		State:    State{Trigger: TriggerLapse, StartedAt: day0},
	}

	notice := h.service.notice(account, Sequence[0], day0)
	if notice.PortalURL != "https://feasible.lol/billing?team=1" {
		t.Fatalf("lapse notice portal URL is %q", notice.PortalURL)
	}
	if notice.UpgradeURL != "https://feasible.lol/billing/upgrade?team=1" ||
		notice.ExportURL != "https://feasible.lol/billing/export?team=1" {
		t.Fatalf("lapse account URLs are upgrade=%q export=%q", notice.UpgradeURL, notice.ExportURL)
	}
}

// TestEmailsAreIdempotentAcrossRepeatedSweeps is the guarantee that a retried
// job cannot send twice. The sweep is run many times on the same day, which is
// what an hourly sweeper does.
func TestEmailsAreIdempotentAcrossRepeatedSweeps(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalTrialStarted); err != nil {
		t.Fatal(err)
	}

	h.travel(45)

	for i := 0; i < 24; i++ {
		h.sweep()
	}

	seen := map[string]int{}
	for _, template := range h.templates() {
		seen[template]++
	}

	for template, count := range seen {
		if count != 1 {
			t.Errorf("%s was sent %d times", template, count)
		}
	}

	if len(seen) != 4 {
		t.Errorf("day 45 sent %d distinct emails, want 4", len(seen))
	}
}

// TestLifecycleOutboxRetriesExpiredClaimsAndExcludesLiveWorkers proves a crash
// before send becomes retryable, while concurrent workers cannot both own the
// message during the live lease.
func TestLifecycleOutboxRetriesExpiredClaimsAndExcludesLiveWorkers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.Signal(ctx, 1, SignalTrialStarted); err != nil {
		t.Fatal(err)
	}
	h.travel(Sequence[0].Day)
	started := day0
	now := h.service.now()

	secondControl, err := store.Open(filepath.Join(h.dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondControl.Close() })
	stores := []*Store{h.store, NewStore(secondControl)}
	start := make(chan struct{})
	results := make(chan bool, len(stores))
	var workers sync.WaitGroup
	for _, outbox := range stores {
		workers.Add(1)
		go func(outbox *Store) {
			defer workers.Done()
			<-start
			_, claimed, err := outbox.ClaimEmail(ctx, 1, started, Sequence[0].Template, "owner@example.com", now)
			if err != nil {
				t.Errorf("claim email: %v", err)
			}
			results <- claimed
		}(outbox)
	}
	close(start)
	workers.Wait()
	close(results)

	claimed := 0
	for result := range results {
		if result {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent workers won %d live email leases, want 1", claimed)
	}

	if err := h.service.sendDue(ctx, Account{TeamID: 1, TeamName: "Example Co", Email: "owner@example.com",
		State: State{Trigger: TriggerTrial, StartedAt: started}}, now); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 0 {
		t.Fatal("a second worker sent through a live email lease")
	}

	retryAt := now.Add(emailLeaseDuration + time.Second)
	h.setClock(retryAt)
	if err := h.service.sendDue(ctx, Account{TeamID: 1, TeamName: "Example Co", Email: "owner@example.com",
		State: State{Trigger: TriggerTrial, StartedAt: started}}, retryAt); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("expired pre-send claim produced %d messages, want 1", len(h.sent))
	}
}

// TestLifecycleOutboxAcceptedBeforeAckRetryKeepsMessageIdentity documents the
// unavoidable SMTP window: a crash after relay acceptance but before the local
// completion transaction may duplicate, but the retry carries one Message-ID.
func TestLifecycleOutboxAcceptedBeforeAckRetryKeepsMessageIdentity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.Signal(ctx, 1, SignalTrialStarted); err != nil {
		t.Fatal(err)
	}
	started := day0
	now := at(Sequence[0].Day)
	h.setClock(now)
	account := Account{TeamID: 1, TeamName: "Example Co", Email: "owner@example.com",
		State: State{Trigger: TriggerTrial, StartedAt: started}}

	notice := h.service.notice(account, Sequence[0], now)
	claim, claimed, err := h.store.ClaimNotice(ctx, started, notice, now)
	if err != nil || !claimed {
		t.Fatalf("first claim is %+v claimed=%t error=%v", claim, claimed, err)
	}
	notice.MessageKey = claim.MessageKey
	if _, err := h.service.Notify.Notify(ctx, notice); err != nil {
		t.Fatal(err)
	}
	// Simulated crash: the accepted transport result is never acknowledged to
	// the outbox. The next worker may send again after the lease expires.
	retryAt := now.Add(emailLeaseDuration + time.Second)
	h.setClock(retryAt)
	if err := h.service.sendDue(ctx, account, retryAt); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 2 || h.sent[0].MessageKey == "" || h.sent[0].MessageKey != h.sent[1].MessageKey {
		t.Fatalf("accepted-before-ack retry identities are %+v", []string{h.sent[0].MessageKey, h.sent[1].MessageKey})
	}

	if _, claimed, err := h.store.ClaimEmail(ctx, 1, started, Sequence[0].Template,
		account.Email, now.Add(2*emailLeaseDuration)); err != nil || claimed {
		t.Fatalf("completed outbox row was reclaimable: claimed=%t error=%v", claimed, err)
	}
}

// TestDeletionConfirmationTakesAFreshLeaseAfterASlowSweep ensures processing an
// earlier recipient cannot make a later confirmation lease expired on arrival.
func TestDeletionConfirmationTakesAFreshLeaseAfterASlowSweep(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	started := day0.Add(-DeletionDays * Day)
	for teamID := int64(1); teamID <= 2; teamID++ {
		if _, err := h.control.Exec(`
			INSERT INTO account_deletions
				(team_id, team_name, contact_email, clock_started_at, started_at, completed_at,
				 local_removed_at, provider_removed_at, control_removed_at)
			VALUES (?, 'Deleted Team', 'owner@example.com', ?, ?, ?, ?, ?, ?)
		`, teamID, started.Unix(), day0.Unix(), day0.Unix(), day0.Unix(), day0.Unix(), day0.Unix()); err != nil {
			t.Fatal(err)
		}
	}

	competitor := NewStore(h.control)
	call := 0
	h.service.Notify = NotifierFunc(func(_ context.Context, notice Notice) (string, error) {
		call++
		if call == 1 {
			h.setClock(h.now().Add(emailLeaseDuration + time.Second))
		}
		if call == 2 {
			_, claimed, err := competitor.ClaimNotice(ctx, started, notice, h.now())
			if err != nil {
				t.Fatalf("competing confirmation claim: %v", err)
			}
			if claimed {
				t.Fatal("second confirmation was claimed with an already-expired lease")
			}
		}

		return "captured", nil
	})

	if err := h.service.sendConfirmations(ctx); err != nil {
		t.Fatal(err)
	}
	if call != 2 {
		t.Fatalf("sent %d confirmations, want 2", call)
	}
}

// TestLifecycleOutboxRetryUsesItsOriginalPayload changes every mutable input
// after a pre-send crash. The retry must keep the first recipient, team name,
// day, dates, and selected-team links under the same stable message identity.
func TestLifecycleOutboxRetryUsesItsOriginalPayload(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.Signal(ctx, 1, SignalTrialStarted); err != nil {
		t.Fatal(err)
	}
	firstAt := at(Sequence[0].Day)
	h.setClock(firstAt)
	account := Account{TeamID: 1, TeamName: "Original Co", Email: "first@example.com",
		State: State{Trigger: TriggerTrial, StartedAt: day0}}
	original := h.service.notice(account, Sequence[0], firstAt)
	claim, claimed, err := h.store.ClaimNotice(ctx, day0, original, firstAt)
	if err != nil || !claimed {
		t.Fatalf("original claim=%+v claimed=%t error=%v", claim, claimed, err)
	}

	changed := account
	changed.TeamName = "Renamed Co"
	changed.Email = "second@example.com"
	retryAt := firstAt.Add(24 * time.Hour)
	h.setClock(retryAt)
	if err := h.service.sendDue(ctx, changed, retryAt); err != nil {
		t.Fatal(err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("payload retry sent %d notices", len(h.sent))
	}
	got := h.sent[0]
	if got.To != original.To || got.TeamName != original.TeamName || got.Day != original.Day ||
		got.UpgradeURL != original.UpgradeURL || got.ExportURL != original.ExportURL || got.MessageKey != claim.MessageKey {
		t.Fatalf("payload changed across retry: original=%+v retry=%+v", original, got)
	}
}

// TestASecondClockSendsTheWholeSequenceAgain covers an account that lapses,
// pays, and lapses again. The emails are keyed by the clock they belong to, so
// the second lapse gets every warning rather than being silently skipped.
func TestASecondClockSendsTheWholeSequenceAgain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	h.travel(30)
	h.sweep()

	firstRun := len(h.templates())
	if firstRun != 3 {
		t.Fatalf("the first clock sent %d emails by day 30, want 3", firstRun)
	}

	if _, err := h.service.Signal(ctx, 1, SignalPaymentSucceeded); err != nil {
		t.Fatal(err)
	}

	// A year later, the card expires again.
	h.mu.Lock()
	h.clock = day0.Add(365 * Day)
	secondStart := h.clock
	h.mu.Unlock()

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	for day := 0; day <= 30; day++ {
		h.mu.Lock()
		h.clock = secondStart.Add(time.Duration(day) * Day)
		h.mu.Unlock()

		h.sweep()
	}

	if got := len(h.templates()) - firstRun; got != 3 {
		t.Fatalf("the second clock sent %d emails by day 30, want 3", got)
	}
}

// TestTrafficMirrorFollowsTheClock checks the two columns the ingest path reads.
// A state that said dormant while the routing map still accepted traffic would
// be invisible until somebody noticed months of data that should not exist.
func TestTrafficMirrorFollowsTheClock(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	var before sql.NullInt64
	if err := h.control.QueryRow(`SELECT accept_traffic_until FROM teams WHERE id = 1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before.Valid {
		t.Fatal("a fresh team already has a traffic deadline")
	}

	transition, err := h.service.Signal(ctx, 1, SignalTrialStarted)
	if err != nil {
		t.Fatal(err)
	}

	var after sql.NullInt64
	if err := h.control.QueryRow(`SELECT accept_traffic_until FROM teams WHERE id = 1`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Valid {
		t.Fatal("starting a clock did not set the traffic deadline")
	}

	if want := transition.State.Boundary(PhaseDormant).Unix(); after.Int64 != want {
		t.Errorf("accept_traffic_until is %d, want %d (the dormant boundary)", after.Int64, want)
	}
}

// TestCollectionGapIsRecorded checks that the dormant window is written down, so
// the graph can draw it as a labelled gap instead of a run of zeroes.
func TestCollectionGapIsRecorded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	h.travel(65)
	h.sweep()

	gaps, err := h.store.Gaps(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 {
		t.Fatalf("recorded %d gaps, want 1", len(gaps))
	}
	if !gaps[0].StartedAt.Equal(at(LockedDays)) {
		t.Errorf("the gap starts at %s, want %s", gaps[0].StartedAt, at(LockedDays))
	}
	if !gaps[0].EndedAt.IsZero() {
		t.Error("the gap is already closed while the account is still dormant")
	}

	if _, err := h.service.Signal(ctx, 1, SignalPaymentSucceeded); err != nil {
		t.Fatal(err)
	}

	gaps, err = h.store.Gaps(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if gaps[0].EndedAt.IsZero() {
		t.Error("paying did not close the collection gap")
	}
}

// TestSuccessfulPaymentRepairsFinalizationAfterACrash simulates a process that
// committed Active and died before cancelling mail or closing the collection
// gap. Replaying the same payment must finish both idempotent side effects.
func TestSuccessfulPaymentRepairsFinalizationAfterACrash(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}
	started := h.service.now()
	if _, claimed, err := h.store.ClaimEmail(ctx, 1, started, TemplateDeletionTomorrow,
		"owner@example.com", started); err != nil || !claimed {
		t.Fatalf("claim pending email: claimed=%t error=%v", claimed, err)
	}
	if err := h.store.OpenGap(ctx, 1, started.Add(LockedDays*Day)); err != nil {
		t.Fatal(err)
	}

	// This is the crash point: the lifecycle row reached Active, but the two
	// cleanup calls in Service.Signal never ran.
	if err := h.store.Save(ctx, 1, State{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Signal(ctx, 1, SignalPaymentSucceeded); err != nil {
		t.Fatal(err)
	}

	var pending int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM lifecycle_outbox WHERE team_id = 1 AND completed_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("payment replay left %d pending lifecycle emails", pending)
	}

	gaps, err := h.store.Gaps(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].EndedAt.IsZero() {
		t.Fatalf("payment replay did not close the gap: %+v", gaps)
	}
}

// TestPaymentWinsAgainstAStaleDayNinetySnapshot proves the destructive race at
// its exact boundary. The billing mirror commits before lifecycle finalization;
// even in that crash window, a stale sweeper cannot claim the paid account.
func TestPaymentWinsAgainstAStaleDayNinetySnapshot(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}
	h.travel(DeletionDays)

	running, err := h.store.Running(ctx)
	if err != nil || len(running) != 1 {
		t.Fatalf("day-90 snapshot is %+v, error %v", running, err)
	}
	stale := running[0]

	if _, err := h.control.Exec(`
		INSERT INTO subscriptions
			(team_id, status, payment_state, created_at, updated_at)
		VALUES (1, 'active', 'paid', ?, ?)
	`, h.service.now().Unix(), h.service.now().Unix()); err != nil {
		t.Fatal(err)
	}

	// This is the precise crash window: payment is durable, but the webhook has
	// not yet cleared the lifecycle clock that the sweeper already loaded.
	if err := h.purger.Purge(ctx, stale, h.service.now()); err != nil {
		t.Fatal(err)
	}

	var teams, deletions int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM teams WHERE id = 1`).Scan(&teams); err != nil {
		t.Fatal(err)
	}
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM account_deletions WHERE team_id = 1`).Scan(&deletions); err != nil {
		t.Fatal(err)
	}
	if teams != 1 || deletions != 0 {
		t.Fatalf("paid account was claimed from a stale snapshot: teams=%d deletions=%d", teams, deletions)
	}

	if _, err := h.service.SignalAt(ctx, 1, SignalPaymentSucceeded, h.service.now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	state, err := h.store.Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.At(h.service.now()) != PhaseActive {
		t.Fatalf("stale purge left teams=%d deletions=%d state=%+v", teams, deletions, state)
	}
}

// TestDeletionRemovesTheStripeCustomer proves the third thing day 90 destroys.
// Leaving a customer record behind would mean a stored card outliving the
// account it belonged to.
func TestDeletionRemovesTheStripeCustomer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.control.Exec(`
		INSERT INTO subscriptions (team_id, stripe_customer_id, status, created_at, updated_at)
		VALUES (1, 'cus_test_123', 'canceled', ?, ?)
	`, day0.Unix(), day0.Unix()); err != nil {
		t.Fatal(err)
	}

	removed := ""
	h.purger.Customers = removerFunc(func(_ context.Context, id string) error {
		removed = id
		return nil
	})

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	h.travel(90)
	h.sweep()

	if removed != "cus_test_123" {
		t.Fatalf("the payment customer removed was %q, want cus_test_123", removed)
	}
	if exists(h.accountFile()) {
		t.Error("the account database survived")
	}
}

// TestDeletionSurvivesAFailingPaymentProvider removes local data on schedule but
// keeps the immutable audit retryable until the provider customer is also gone.
// No confirmation may claim the stored card was removed before that succeeds.
func TestDeletionSurvivesAFailingPaymentProvider(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.control.Exec(`
		INSERT INTO subscriptions (team_id, stripe_customer_id, status, created_at, updated_at)
		VALUES (1, 'cus_broken', 'canceled', ?, ?)
	`, day0.Unix(), day0.Unix()); err != nil {
		t.Fatal(err)
	}

	h.purger.Customers = removerFunc(func(context.Context, string) error {
		return errFake
	})

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	h.travel(90)
	if _, err := h.service.Sweep(ctx); err == nil || !contains(err.Error(), "payment provider is unreachable") {
		t.Fatalf("failed provider deletion returned %v", err)
	}

	if exists(h.accountFile()) {
		t.Fatal("a payment provider outage stopped the data being deleted")
	}

	var notes string
	var completed, providerRemoved sql.NullInt64
	if err := h.control.QueryRow(`
		SELECT notes, completed_at, provider_removed_at FROM account_deletions WHERE team_id = 1
	`).Scan(&notes, &completed, &providerRemoved); err != nil {
		t.Fatal(err)
	}
	if notes == "" || !contains(notes, "NOT removed") || completed.Valid || providerRemoved.Valid {
		t.Errorf("provider failure was not left retryable: notes=%q completed=%v provider=%v", notes, completed, providerRemoved)
	}
	if len(h.templates()) != len(Sequence)-1 {
		t.Fatal("a deletion confirmation was sent before provider cleanup")
	}

	h.purger.Customers = removerFunc(func(context.Context, string) error { return nil })
	if _, err := h.service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.control.QueryRow(`
		SELECT completed_at, provider_removed_at FROM account_deletions WHERE team_id = 1
	`).Scan(&completed, &providerRemoved); err != nil {
		t.Fatal(err)
	}
	if !completed.Valid || !providerRemoved.Valid || h.templates()[len(h.templates())-1] != TemplateAccountDeleted {
		t.Fatalf("provider retry completion=%v provider=%v templates=%v", completed, providerRemoved, h.templates())
	}
}

// TestDeletionIsIdempotent covers a crash between any two steps: running the
// whole thing again must succeed rather than fail on something already gone.
func TestDeletionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}

	state, err := h.store.Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	account := Account{TeamID: 1, TeamName: "Example Co", State: state, Email: "owner@example.com"}

	for i := 0; i < 3; i++ {
		if err := h.purger.Purge(ctx, account, at(90)); err != nil {
			t.Fatalf("purge %d: %v", i, err)
		}
	}

	var records int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM account_deletions WHERE team_id = 1`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 1 {
		t.Errorf("three purges wrote %d deletion records, want 1", records)
	}
}

// TestDeletionControlTransactionRecoversAroundTeamRemoval injects crashes on
// both sides of the team DELETE. The transaction must roll back the team and
// confirmation together, while the immutable pending audit remains sufficient
// to finish the deletion on the next pass.
func TestDeletionControlTransactionRecoversAroundTeamRemoval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
		teams   int
	}{
		{
			name: "immediately before team removal",
			trigger: `CREATE TRIGGER crash_before_team_delete
				BEFORE DELETE ON teams
				BEGIN SELECT RAISE(FAIL, 'crash before team removal'); END`,
			teams: 1,
		},
		{
			name: "immediately after team removal",
			trigger: `CREATE TRIGGER crash_after_team_delete
				BEFORE UPDATE OF completed_at ON account_deletions
				WHEN NEW.completed_at IS NOT NULL
				BEGIN SELECT RAISE(FAIL, 'crash after team removal'); END`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
				t.Fatal(err)
			}
			state, err := h.store.Load(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.control.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			account := Account{TeamID: 1, TeamName: "Example Co", Email: "owner@example.com", State: state}
			if err := h.purger.Purge(ctx, account, at(DeletionDays)); err == nil {
				t.Fatal("injected deletion crash did not fail")
			}

			var teams, confirmations int
			var completed sql.NullInt64
			if err := h.control.QueryRow(`SELECT COUNT(*) FROM teams WHERE id = 1`).Scan(&teams); err != nil {
				t.Fatal(err)
			}
			if err := h.control.QueryRow(`SELECT completed_at FROM account_deletions WHERE team_id = 1`).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if err := h.control.QueryRow(`SELECT COUNT(*) FROM lifecycle_outbox WHERE team_id = 1 AND template = ?`, TemplateAccountDeleted).Scan(&confirmations); err != nil {
				t.Fatal(err)
			}
			if teams != tc.teams || completed.Valid || confirmations != 0 {
				t.Fatalf("crash split control state: teams=%d completed=%v confirmations=%d", teams, completed, confirmations)
			}

			if _, err := h.control.Exec(`DROP TRIGGER ` + map[string]string{
				"immediately before team removal": "crash_before_team_delete",
				"immediately after team removal":  "crash_after_team_delete",
			}[tc.name]); err != nil {
				t.Fatal(err)
			}
			pending, err := h.purger.PendingDeletions(ctx)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending deletion recovery is %+v error=%v", pending, err)
			}
			if err := h.purger.Purge(ctx, pending[0], at(DeletionDays)); err != nil {
				t.Fatal(err)
			}
			if err := h.control.QueryRow(`SELECT COUNT(*) FROM teams WHERE id = 1`).Scan(&teams); err != nil {
				t.Fatal(err)
			}
			if err := h.control.QueryRow(`SELECT COUNT(*) FROM lifecycle_outbox WHERE team_id = 1 AND template = ? AND completed_at IS NULL`, TemplateAccountDeleted).Scan(&confirmations); err != nil {
				t.Fatal(err)
			}
			eligible, err := h.purger.PendingConfirmations(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if teams != 0 || confirmations != 1 || len(eligible) != 1 {
				t.Fatalf("recovered deletion teams=%d confirmations=%d eligible=%+v", teams, confirmations, eligible)
			}
		})
	}
}

// TestLegacyCrashAfterTeamRemovalRemainsDiscoverable covers the state emitted
// by the older non-atomic implementation: the team is already gone but its
// immutable audit is incomplete. A sweep must rediscover it, complete the
// audit, and send the leased confirmation without any membership rows left.
func TestLegacyCrashAfterTeamRemovalRemainsDiscoverable(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.Signal(ctx, 1, SignalPaymentFailed); err != nil {
		t.Fatal(err)
	}
	state, err := h.store.Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	account := Account{TeamID: 1, TeamName: "Example Co", Email: "owner@example.com", State: state}
	claimed, err := h.purger.claim(ctx, account, at(DeletionDays))
	if err != nil || !claimed {
		t.Fatalf("legacy deletion claim=%t error=%v", claimed, err)
	}
	if _, err := h.control.Exec(`DELETE FROM teams WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	h.travel(DeletionDays)
	if _, err := h.service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var completed, notified sql.NullInt64
	if err := h.control.QueryRow(`SELECT completed_at, notified_at FROM account_deletions WHERE team_id = 1`).Scan(&completed, &notified); err != nil {
		t.Fatal(err)
	}
	if !completed.Valid || !notified.Valid {
		t.Fatalf("legacy audit completion=%v notification=%v", completed, notified)
	}
	if templates := h.templates(); len(templates) != 1 || templates[0] != TemplateAccountDeleted {
		t.Fatalf("legacy recovery sent %v", templates)
	}
}

// removerFunc adapts a function to the CustomerRemover interface.
type removerFunc func(ctx context.Context, id string) error

// DeleteCustomer calls the function.
func (f removerFunc) DeleteCustomer(ctx context.Context, id string) error {
	return f(ctx, id)
}

// errFake is a stand-in for a payment provider that will not answer.
var errFake = &fakeError{}

// fakeError is a minimal error type, so the test does not need errors.New at
// package scope where it would read as something meaningful.
type fakeError struct{}

// Error describes the simulated outage.
func (*fakeError) Error() string { return "the payment provider is unreachable" }

// contains is strings.Contains, kept local so the test file's imports stay to
// what it actually exercises.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
