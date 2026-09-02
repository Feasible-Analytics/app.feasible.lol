//
// lifecycle.go
// The account lifecycle state machine: one instant in, one phase and one deadline out.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package lifecycle decides when we stop showing a customer their dashboard,
// when we stop collecting their traffic, and when we permanently delete
// everything we hold for them.
//
// It is written as an explicit state machine over a single stored instant
// because it is the code in this product that destroys data. Date comparisons
// spread through handlers cannot be exhaustively tested, cannot be replayed,
// and give no one place to look when an account is in the wrong phase — and the
// consequence of the wrong answer here is irreversible.
//
// Everything in this file is pure. The clock is a parameter, nothing reads
// time.Now, nothing touches a database, and no function has a side effect. The
// persistence, the emails and the deletion live in store.go, mail.go and
// deletion.go, and all three are driven by the decisions made here.
package lifecycle

import (
	"fmt"
	"time"
)

// Phase is where an account sits on the clock. The set is closed and ordered:
// an account only ever moves forward through it while the clock runs, and any
// successful payment jumps it straight back to Active.
type Phase string

// The five phases.
//
//	Active   the clock is not running at all
//	Grace    days 0-30    full dashboard, with a banner. Still collecting.
//	Locked   days 30-60   dashboard blocked. STILL COLLECTING — see below.
//	Dormant  days 60-90   dashboard blocked, collection stopped, data kept
//	Deleted  day 90       the account database, the system rows and the
//	                      payment-provider customer are gone, irreversibly
//
// Collection continuing through Locked is deliberate and is the kindest
// available outcome for somebody whose card expired while they were away. The
// customer cannot see any of it, so there is no way to use the product free;
// the only thing collecting buys them is a complete history the moment they
// pay.
const (
	PhaseActive  Phase = "active"
	PhaseGrace   Phase = "grace"
	PhaseLocked  Phase = "locked"
	PhaseDormant Phase = "dormant"
	PhaseDeleted Phase = "deleted"
)

// The phase boundaries, in whole days from day 0. They are named rather than
// written as literals at each comparison so that the table in the specification
// and the code enforcing it are the same four numbers.
const (
	// GraceDays is when the dashboard locks.
	GraceDays = 30

	// LockedDays is when collection stops and a labelled gap begins.
	LockedDays = 60

	// DeletionDays is when everything is destroyed.
	DeletionDays = 90
)

// Day is one day of the clock, so that arithmetic on it reads as days rather
// than as an unexplained 86400.
const Day = 24 * time.Hour

// Trigger records why the clock is running. Both triggers follow the identical
// timetable; the only difference is the words the emails use, because "your
// trial is ending" and "your card was declined" are not the same message even
// though they lead to the same place on the same dates.
type Trigger string

// The triggers. TriggerNone is the zero value and means the clock is stopped,
// which is what makes the zero State a healthy, paying account.
const (
	TriggerNone  Trigger = ""
	TriggerTrial Trigger = "trial"
	TriggerLapse Trigger = "lapse"
)

// Valid reports whether a trigger is one this build understands. A value read
// back from a database that this binary does not recognise must be refused
// rather than treated as "no clock", because "no clock" is the answer that
// silently keeps a lapsed account running forever.
func (t Trigger) Valid() bool {
	switch t {
	case TriggerNone, TriggerTrial, TriggerLapse:
		return true
	}

	return false
}

// State is everything the machine remembers about one account. Three fields,
// because every phase, deadline and email date is arithmetic on StartedAt and
// storing any of them separately would let two of them disagree.
type State struct {
	// Trigger is why the clock is running, or TriggerNone when it is not.
	Trigger Trigger

	// StartedAt is day 0. It never moves while a clock runs: a second failed
	// charge mid-grace must not push the deadline out, because the customer has
	// already been told the date and moving it would make every email we sent a
	// lie.
	StartedAt time.Time

	// DeletedAt is when the account was destroyed. Non-zero is terminal: there
	// is nothing left to restore, so no later event can move the state again.
	DeletedAt time.Time
}

// Running reports whether a clock is ticking. A deleted account is not running:
// there is nothing left for it to count down to.
func (s State) Running() bool {
	return s.DeletedAt.IsZero() && s.Trigger != TriggerNone && !s.StartedAt.IsZero()
}

// Deleted reports whether the account has already been destroyed.
func (s State) Deleted() bool {
	return !s.DeletedAt.IsZero()
}

// Elapsed is how long the clock has run at a given instant. It is negative for
// an instant before day 0, which a clock rewind or a badly ordered webhook can
// produce, and the callers below treat that as day 0 rather than as a phase
// several steps ahead.
func (s State) Elapsed(now time.Time) time.Duration {
	if !s.Running() {
		return 0
	}

	return now.Sub(s.StartedAt)
}

// DayAt is the whole number of days the clock has run. Days are truncated
// rather than rounded, so day 29 lasts until the instant day 30 begins — which
// is what makes "you have until <date>" in an email exactly true.
func (s State) DayAt(now time.Time) int {
	elapsed := s.Elapsed(now)
	if elapsed < 0 {
		return 0
	}

	return int(elapsed / Day)
}

// At is the phase an account is in at a given instant. This is the whole
// machine: every other question in the package is answered from its result.
func (s State) At(now time.Time) Phase {
	if s.Deleted() {
		return PhaseDeleted
	}

	if !s.Running() {
		return PhaseActive
	}

	switch day := s.DayAt(now); {
	case day >= DeletionDays:
		return PhaseDeleted
	case day >= LockedDays:
		return PhaseDormant
	case day >= GraceDays:
		return PhaseLocked
	default:
		return PhaseGrace
	}
}

// Boundary is the instant a phase begins for this clock. It is what every email
// quotes: "upgrade any time before <date>" has to name the same instant the
// machine will act on, and deriving both from one function is the only way to
// be sure it does.
//
// The zero time means the phase has no boundary for this state — a stopped
// clock never reaches any of them, and Active has no start because it is where
// an account already is.
func (s State) Boundary(phase Phase) time.Time {
	if !s.Running() {
		return time.Time{}
	}

	switch phase {
	case PhaseGrace:
		return s.StartedAt
	case PhaseLocked:
		return s.StartedAt.Add(GraceDays * Day)
	case PhaseDormant:
		return s.StartedAt.Add(LockedDays * Day)
	case PhaseDeleted:
		return s.StartedAt.Add(DeletionDays * Day)
	default:
		return time.Time{}
	}
}

// NextBoundary is when the next thing happens, and what it is. Every email in
// the sequence names this date, so it is derived here once rather than
// recomputed per template where one of them would eventually be off by a day.
//
// The boolean is false for an account with nothing scheduled: one that is
// paying, or one that is already gone.
func (s State) NextBoundary(now time.Time) (time.Time, Phase, bool) {
	if !s.Running() || s.At(now) == PhaseDeleted {
		return time.Time{}, "", false
	}

	for _, phase := range []Phase{PhaseLocked, PhaseDormant, PhaseDeleted} {
		if at := s.Boundary(phase); at.After(now) {
			return at, phase, true
		}
	}

	return time.Time{}, "", false
}

// Access is what an account may do in a phase. It is a struct of explicit
// booleans rather than a comparison at each call site, because "may this
// account still export" is asked from several places and the answer must be
// the same in all of them.
type Access struct {
	// Dashboard is whether the reports render at all.
	Dashboard bool

	// Collect is whether the ingest path still accepts events.
	Collect bool

	// Export is whether the customer can download everything we hold. It is
	// true in every phase without exception. It is their data, and GDPR
	// portability is not a feature we may switch off for non-payment.
	Export bool

	// Settings is whether account and billing screens are reachable. They stay
	// open in every phase for the same reason export does: locking somebody out
	// of the page where they would pay us is self-defeating as well as unkind.
	Settings bool
}

// Capabilities maps a phase onto what it permits. Deleted permits nothing
// because there is nothing left to permit.
func Capabilities(phase Phase) Access {
	switch phase {
	case PhaseActive, PhaseGrace:
		return Access{Dashboard: true, Collect: true, Export: true, Settings: true}
	case PhaseLocked:
		return Access{Dashboard: false, Collect: true, Export: true, Settings: true}
	case PhaseDormant:
		return Access{Dashboard: false, Collect: false, Export: true, Settings: true}
	default:
		return Access{}
	}
}

// Signal is something that happened to an account and might move the machine.
// They are named after the fact rather than after the payment provider's event
// names on purpose: several provider events map to one signal here, and the
// machine must not grow a branch every time the provider adds an event type.
type Signal string

// The signals the machine accepts.
const (
	// SignalTrialStarted is a new account with no card on file. Day 0 is signup.
	SignalTrialStarted Signal = "trial_started"

	// SignalPaymentFailed is the FIRST failed charge, a cancellation, a
	// chargeback or a paused subscription — every way a subscription stops
	// paying. Day 0 is that moment, not the end of the provider's retry window,
	// because the retry window is the provider's business and the customer's
	// thirty days of grace are ours.
	SignalPaymentFailed Signal = "payment_failed"

	// SignalPaymentSucceeded is money arriving, by any route. It resets the
	// machine to Active and cancels every pending email.
	SignalPaymentSucceeded Signal = "payment_succeeded"

	// SignalDeleted records that the destruction actually happened. It is a
	// signal rather than a field write so that the terminal transition goes
	// through the same table as every other one.
	SignalDeleted Signal = "deleted"
)

// Transition is the result of applying a signal: the new state, where it moved
// from and to, and the two side effects the caller owes the rest of the system.
type Transition struct {
	State State
	From  Phase
	To    Phase

	// Changed reports whether the state moved at all. A repeated signal — a
	// second failed charge during grace, a webhook delivered twice — is a
	// no-op, and the caller uses this to avoid writing a row and logging a
	// change that did not happen.
	Changed bool

	// CancelEmails is set when the account returned to Active. Every pending
	// lifecycle email must be cancelled at that moment: a customer who has just
	// paid must never receive "we delete your account tomorrow".
	CancelEmails bool

	// StartEmails is set when a clock began, so the caller knows a fresh
	// sequence is now due.
	StartEmails bool
}

// Apply is the transition table. Every path an account can take goes through
// this one function, which is what makes the machine exhaustively testable: a
// test can enumerate every (phase, signal) pair and assert the result, and
// nothing anywhere else may move an account between phases.
//
// The clock is a parameter. No part of this package reads the system clock, so
// a ninety-one day timeline runs in a test in microseconds and produces exactly
// the transitions a real ninety-one days would.
func Apply(state State, signal Signal, now time.Time) (Transition, error) {
	from := state.At(now)

	// Deleted is terminal, and this is the branch that makes it so. The
	// customer's database file, system rows and payment-provider customer are
	// gone; a payment arriving afterwards buys a new account, not the old one
	// back, and pretending otherwise would leave an "active" account pointing
	// at a database that does not exist.
	if from == PhaseDeleted {
		return Transition{State: state, From: from, To: from}, nil
	}

	switch signal {
	case SignalTrialStarted:
		// Enrolling an account that is already on a clock would move day 0 and
		// invalidate every date we have already put in writing.
		if state.Running() {
			return Transition{State: state, From: from, To: from}, nil
		}

		next := State{Trigger: TriggerTrial, StartedAt: now}

		return Transition{State: next, From: from, To: next.At(now), Changed: true, StartEmails: true}, nil

	case SignalPaymentFailed:
		// Day 0 is the first failure. Stripe's Smart Retries will fail again on
		// days 3, 5, 7 and so on, and each of those arrives here; taking the
		// latest would quietly hand out an extra week of grace and, worse,
		// contradict the date already emailed to the customer.
		if state.Running() {
			return Transition{State: state, From: from, To: from}, nil
		}

		next := State{Trigger: TriggerLapse, StartedAt: now}

		return Transition{State: next, From: from, To: next.At(now), Changed: true, StartEmails: true}, nil

	case SignalPaymentSucceeded:
		// Paying before the durable deletion claim restores everything instantly.
		// This includes the day-90 instant when the sweep has not claimed deletion
		// yet; the store's CAS decides which concurrent operation won.
		if !state.Running() {
			return Transition{State: state, From: from, To: from}, nil
		}

		next := State{}

		return Transition{State: next, From: from, To: PhaseActive, Changed: true, CancelEmails: true}, nil

	case SignalDeleted:
		next := state
		next.DeletedAt = now

		return Transition{State: next, From: from, To: PhaseDeleted, Changed: true}, nil

	default:
		return Transition{State: state, From: from, To: from}, fmt.Errorf("lifecycle: unknown signal %q", signal)
	}
}

// DueForDeletion reports whether an account has reached day 90 and has not been
// destroyed yet. It is a named predicate rather than a comparison inline in the
// sweeper because it is the single condition that authorises irreversible
// deletion, and it should be impossible to write that condition slightly
// differently somewhere else.
func (s State) DueForDeletion(now time.Time) bool {
	return s.Running() && !s.Deleted() && s.At(now) == PhaseDeleted
}
