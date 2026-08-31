//
// lifecycle_test.go
// The transition table, exhaustively, on a clock that never moves by itself.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"testing"
	"time"
)

// day0 is the instant every test's clock starts from. It is a fixed date rather
// than time.Now so that a failure reproduces exactly, and it is deliberately not
// midnight: a machine that only works when day 0 is midnight would pass a test
// suite and delete somebody's data a day early in production.
var day0 = time.Date(2026, time.March, 3, 14, 27, 11, 0, time.UTC)

// at returns the instant a whole number of days after day 0.
func at(days int) time.Time {
	return day0.Add(time.Duration(days) * Day)
}

// trial is a clock started by a trial at day 0.
func trial() State {
	return State{Trigger: TriggerTrial, StartedAt: day0}
}

// lapsed is a clock started by a failed charge at day 0.
func lapsed() State {
	return State{Trigger: TriggerLapse, StartedAt: day0}
}

// TestPhaseBoundaries walks the whole clock a day at a time. This is the table
// from the specification, and it is the single most important assertion in the
// package: every other rule is a consequence of getting these four numbers
// right, and the last one deletes customer data.
func TestPhaseBoundaries(t *testing.T) {
	cases := []struct {
		day  int
		want Phase
	}{
		{0, PhaseGrace},
		{1, PhaseGrace},
		{29, PhaseGrace},
		{30, PhaseLocked},
		{45, PhaseLocked},
		{59, PhaseLocked},
		{60, PhaseDormant},
		{75, PhaseDormant},
		{89, PhaseDormant},
		{90, PhaseDeleted},
		{200, PhaseDeleted},
	}

	for _, tc := range cases {
		if got := trial().At(at(tc.day)); got != tc.want {
			t.Errorf("day %d is %q, want %q", tc.day, got, tc.want)
		}

		// Both triggers follow the identical timetable. A difference here would
		// mean a lapsed customer and a trialling one being deleted on different
		// days despite being told the same thing.
		if got := lapsed().At(at(tc.day)); got != tc.want {
			t.Errorf("day %d on the dunning path is %q, want %q", tc.day, got, tc.want)
		}
	}
}

// TestBoundaryIsExactlyOnTheInstant pins the edge. A phase that started a
// microsecond early or late would still pass a per-day test, and the email
// telling somebody "you have until <date>" would be wrong.
func TestBoundaryIsExactlyOnTheInstant(t *testing.T) {
	state := trial()

	justBefore := state.Boundary(PhaseLocked).Add(-time.Nanosecond)
	if got := state.At(justBefore); got != PhaseGrace {
		t.Errorf("a nanosecond before the lock is %q, want grace", got)
	}

	if got := state.At(state.Boundary(PhaseLocked)); got != PhaseLocked {
		t.Errorf("the instant of the lock is %q, want locked", got)
	}

	if got := state.At(state.Boundary(PhaseDeleted).Add(-time.Nanosecond)); got != PhaseDormant {
		t.Errorf("a nanosecond before deletion is %q, want dormant", got)
	}
}

// TestStoppedClockIsActive is the zero value's contract. A team with no
// lifecycle row at all must read as a paying account, because the alternative —
// a missing row reading as "overdue" — deletes every account that was never
// enrolled.
func TestStoppedClockIsActive(t *testing.T) {
	var state State

	if got := state.At(at(1000)); got != PhaseActive {
		t.Fatalf("a stopped clock is %q, want active", got)
	}

	if state.Running() {
		t.Fatal("a zero state reports a running clock")
	}

	if state.DueForDeletion(at(1000)) {
		t.Fatal("a zero state is due for deletion")
	}

	if len(state.DueAt(at(1000))) != 0 {
		t.Fatal("a stopped clock has emails due")
	}
}

// TestCapabilities is the access table. Two rows matter more than the rest:
// collection continues while the dashboard is locked, and export is available in
// every phase an account still exists in.
func TestCapabilities(t *testing.T) {
	cases := []struct {
		phase                              Phase
		dashboard, collect, export, config bool
	}{
		{PhaseActive, true, true, true, true},
		{PhaseGrace, true, true, true, true},
		{PhaseLocked, false, true, true, true},
		{PhaseDormant, false, false, true, true},
		{PhaseDeleted, false, false, false, false},
	}

	for _, tc := range cases {
		got := Capabilities(tc.phase)

		if got.Dashboard != tc.dashboard {
			t.Errorf("%s dashboard is %v, want %v", tc.phase, got.Dashboard, tc.dashboard)
		}
		if got.Collect != tc.collect {
			t.Errorf("%s collect is %v, want %v", tc.phase, got.Collect, tc.collect)
		}
		if got.Export != tc.export {
			t.Errorf("%s export is %v, want %v", tc.phase, got.Export, tc.export)
		}
		if got.Settings != tc.config {
			t.Errorf("%s settings is %v, want %v", tc.phase, got.Settings, tc.config)
		}
	}
}

// TestCollectionContinuesWhileLocked states the rule on its own, because it is
// the one somebody "tidying up" would most plausibly break. Days 30 to 60 are
// the come-back-and-nothing-is-missing window, and the customer cannot see any
// of it, so there is no way to use the product free.
func TestCollectionContinuesWhileLocked(t *testing.T) {
	access := Capabilities(PhaseLocked)

	if access.Dashboard {
		t.Error("a locked account can still see its dashboard")
	}
	if !access.Collect {
		t.Error("a locked account has stopped collecting — the whole point of the phase is that it has not")
	}
}

// TestExportSurvivesEveryPhase is the GDPR portability guarantee, asserted
// separately so that removing it is a deliberate act with a failing test
// attached rather than a one-line edit in a table.
func TestExportSurvivesEveryPhase(t *testing.T) {
	for _, phase := range []Phase{PhaseActive, PhaseGrace, PhaseLocked, PhaseDormant} {
		if !Capabilities(phase).Export {
			t.Errorf("export is off in %s", phase)
		}
	}
}

// TestTrialStarts covers enrolment, including that enrolling twice does not move
// day 0 — which would invalidate every date already emailed.
func TestTrialStarts(t *testing.T) {
	transition, err := Apply(State{}, SignalTrialStarted, day0)
	if err != nil {
		t.Fatal(err)
	}

	if !transition.Changed || transition.To != PhaseGrace {
		t.Fatalf("starting a trial gave %+v", transition)
	}
	if !transition.StartEmails {
		t.Error("starting a trial did not schedule the email sequence")
	}
	if transition.State.Trigger != TriggerTrial {
		t.Errorf("trigger is %q, want trial", transition.State.Trigger)
	}

	again, err := Apply(transition.State, SignalTrialStarted, at(5))
	if err != nil {
		t.Fatal(err)
	}

	if again.Changed {
		t.Error("enrolling an account that is already on a clock changed it")
	}
	if !again.State.StartedAt.Equal(day0) {
		t.Errorf("day 0 moved to %s", again.State.StartedAt)
	}
}

// TestTrialConvertsOnDayOne is one of the named cases from the specification.
// Somebody who pays the day after signing up must land on Active with a stopped
// clock and every pending email cancelled.
func TestTrialConvertsOnDayOne(t *testing.T) {
	transition, err := Apply(trial(), SignalPaymentSucceeded, at(1))
	if err != nil {
		t.Fatal(err)
	}

	if transition.To != PhaseActive {
		t.Fatalf("paying on day 1 left the account in %q", transition.To)
	}
	if !transition.CancelEmails {
		t.Error("paying on day 1 did not cancel the pending emails")
	}
	if transition.State.Running() {
		t.Error("paying on day 1 left a clock running")
	}
	if transition.State.Trigger != TriggerNone {
		t.Errorf("trigger after payment is %q, want empty", transition.State.Trigger)
	}
}

// TestSecondFailedChargeMidGraceDoesNotMoveDayZero is the case that separates a
// correct implementation from a plausible one. Stripe's Smart Retries fail again
// on days 3, 5 and 7, and each of those arrives as another event; taking the
// latest would quietly hand out extra grace and contradict the date already in
// the customer's inbox.
func TestSecondFailedChargeMidGraceDoesNotMoveDayZero(t *testing.T) {
	state := lapsed()
	deletesAt := state.Boundary(PhaseDeleted)

	for _, day := range []int{3, 5, 7, 21, 29} {
		transition, err := Apply(state, SignalPaymentFailed, at(day))
		if err != nil {
			t.Fatal(err)
		}

		if transition.Changed {
			t.Fatalf("a repeat failure on day %d changed the state", day)
		}

		state = transition.State
	}

	if !state.StartedAt.Equal(day0) {
		t.Fatalf("day 0 moved to %s", state.StartedAt)
	}
	if !state.Boundary(PhaseDeleted).Equal(deletesAt) {
		t.Fatalf("the deletion date moved to %s, want %s", state.Boundary(PhaseDeleted), deletesAt)
	}
	if got := state.At(at(30)); got != PhaseLocked {
		t.Fatalf("after five repeat failures day 30 is %q, want locked", got)
	}
}

// TestPayingOnDayEightyNineRestoresEverything is the other named case. One day
// before deletion is still recoverable, and the machine has to say so without
// any unwinding — which is why it stores an instant rather than a phase.
func TestPayingOnDayEightyNine(t *testing.T) {
	state := lapsed()

	transition, err := Apply(state, SignalPaymentSucceeded, at(89))
	if err != nil {
		t.Fatal(err)
	}

	if transition.From != PhaseDormant {
		t.Errorf("day 89 was %q, want dormant", transition.From)
	}
	if transition.To != PhaseActive {
		t.Fatalf("paying on day 89 left the account in %q", transition.To)
	}
	if !transition.CancelEmails {
		t.Error("paying on day 89 did not cancel the pending emails")
	}

	access := Capabilities(transition.State.At(at(89)))
	if !access.Dashboard || !access.Collect {
		t.Errorf("after paying on day 89 the account has %+v", access)
	}
}

// TestPayingOnDayNinetyOneCannotResurrect is the mirror image, and the reason
// PhaseDeleted is terminal. By day 91 the database file, the control rows and
// the payment customer are gone; marking the account active would leave it
// pointing at nothing.
func TestPayingOnDayNinetyOne(t *testing.T) {
	// The account as the purger leaves it: deleted at day 90.
	deleted := lapsed()
	deleted.DeletedAt = at(90)

	transition, err := Apply(deleted, SignalPaymentSucceeded, at(91))
	if err != nil {
		t.Fatal(err)
	}

	if transition.Changed {
		t.Fatal("paying after deletion changed the state")
	}
	if transition.To != PhaseDeleted {
		t.Fatalf("after paying on day 91 the account is %q, want deleted", transition.To)
	}
	if transition.State.At(at(91)) != PhaseDeleted {
		t.Fatal("the account came back from deletion")
	}
}

// TestReachingDayNinetyWithoutDeletionIsStillTerminal covers the account whose
// sweep did not run — a process that was down over the boundary. It is deleted
// as a phase before the purger has touched it, and it must not accept a payment
// in that window either: the purger is about to run, and a payment landing
// between the phase and the deletion would be taken and then destroyed.
func TestPhaseDeletedBeforeThePurgerRuns(t *testing.T) {
	state := lapsed()

	if state.At(at(90)) != PhaseDeleted {
		t.Fatal("day 90 is not the deleted phase")
	}
	if !state.DueForDeletion(at(90)) {
		t.Fatal("day 90 is not due for deletion")
	}

	transition, err := Apply(state, SignalPaymentSucceeded, at(90))
	if err != nil {
		t.Fatal(err)
	}

	if transition.Changed {
		t.Fatal("a payment on day 90 revived an account that is being deleted")
	}
}

// TestEverySignalOnEveryPhase is the exhaustive sweep. It asserts the whole
// (phase, signal) grid produces a legal phase and never an error, which is what
// makes "the machine cannot get into a state nobody thought about" a checked
// claim rather than a hope.
func TestEverySignalOnEveryPhase(t *testing.T) {
	states := map[string]State{
		"active":  {},
		"grace":   trial(),
		"locked":  trial(),
		"dormant": trial(),
		"deleted": {Trigger: TriggerLapse, StartedAt: day0, DeletedAt: at(90)},
	}

	when := map[string]time.Time{
		"active":  at(1),
		"grace":   at(10),
		"locked":  at(40),
		"dormant": at(70),
		"deleted": at(95),
	}

	legal := map[Phase]bool{
		PhaseActive: true, PhaseGrace: true, PhaseLocked: true,
		PhaseDormant: true, PhaseDeleted: true,
	}

	signals := []Signal{SignalTrialStarted, SignalPaymentFailed, SignalPaymentSucceeded, SignalDeleted}

	for name, state := range states {
		for _, signal := range signals {
			transition, err := Apply(state, signal, when[name])
			if err != nil {
				t.Fatalf("%s + %s: %v", name, signal, err)
			}

			if !legal[transition.To] {
				t.Errorf("%s + %s produced the phase %q", name, signal, transition.To)
			}

			// Nothing may move an account out of deleted, whatever happens.
			if name == "deleted" && transition.To != PhaseDeleted {
				t.Errorf("%s moved a deleted account to %q", signal, transition.To)
			}

			// Only a payment may cancel emails, and only in a phase where there
			// were emails pending.
			if transition.CancelEmails && signal != SignalPaymentSucceeded {
				t.Errorf("%s + %s cancelled emails", name, signal)
			}
		}
	}
}

// TestUnknownSignalIsAnError makes sure a typo cannot pass through as a no-op.
// Silently ignoring an unrecognised signal is how a webhook stops moving the
// machine and nobody notices for a month.
func TestUnknownSignalIsAnError(t *testing.T) {
	if _, err := Apply(trial(), Signal("resurrect"), at(1)); err == nil {
		t.Fatal("an unknown signal was accepted")
	}
}

// TestNextBoundary is what every email quotes. The date in the message and the
// date the sweeper acts on come from the same arithmetic, so this asserts the
// arithmetic rather than the wording.
func TestNextBoundary(t *testing.T) {
	state := trial()

	cases := []struct {
		day   int
		phase Phase
		want  int
	}{
		{0, PhaseLocked, GraceDays},
		{29, PhaseLocked, GraceDays},
		{30, PhaseDormant, LockedDays},
		{59, PhaseDormant, LockedDays},
		{60, PhaseDeleted, DeletionDays},
		{89, PhaseDeleted, DeletionDays},
	}

	for _, tc := range cases {
		when, phase, ok := state.NextBoundary(at(tc.day))
		if !ok {
			t.Fatalf("day %d has no next boundary", tc.day)
		}

		if phase != tc.phase {
			t.Errorf("day %d next phase is %q, want %q", tc.day, phase, tc.phase)
		}
		if !when.Equal(at(tc.want)) {
			t.Errorf("day %d next boundary is %s, want %s", tc.day, when, at(tc.want))
		}
	}

	if _, _, ok := state.NextBoundary(at(90)); ok {
		t.Error("a deleted account still has something scheduled")
	}
	if _, _, ok := (State{}).NextBoundary(at(1)); ok {
		t.Error("a paying account has something scheduled")
	}
}

// TestClockRunningBackwardsIsClampedToDayZero covers an instant before day 0,
// which a badly ordered webhook or a corrected system clock can produce. It must
// read as day 0 rather than as a negative that divides into a later phase.
func TestClockRunningBackwards(t *testing.T) {
	state := trial()
	before := day0.Add(-48 * time.Hour)

	if got := state.DayAt(before); got != 0 {
		t.Errorf("an instant before day 0 is day %d, want 0", got)
	}
	if got := state.At(before); got != PhaseGrace {
		t.Errorf("an instant before day 0 is %q, want grace", got)
	}
}

// TestTriggerValidation makes sure a value this build does not understand is
// refused rather than read as "no clock" — the one value that would keep a
// lapsed account running forever.
func TestTriggerValidation(t *testing.T) {
	for _, trigger := range []Trigger{TriggerNone, TriggerTrial, TriggerLapse} {
		if !trigger.Valid() {
			t.Errorf("%q is rejected", trigger)
		}
	}

	if Trigger("suspended").Valid() {
		t.Error("an unknown trigger was accepted")
	}
}
