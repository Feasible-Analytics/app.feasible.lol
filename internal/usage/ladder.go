//
// ladder.go
// Two months over is a conversation; no reply is a lock; back in range is an unlock.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import "time"

// Overage is the state of the conversation with an account that has been over
// the limit for two consecutive months. It is separate from the lifecycle
// machine on purpose: exceeding the plan is not a payment failure, it never
// starts the deletion clock, and conflating the two would put somebody's data
// at risk for the crime of growing.
type Overage struct {
	// Period is the month the second consecutive overage was observed.
	Period string

	// AskedAt is when we asked them to reply, and ReplyDeadline is two weeks
	// later. The deadline is stored rather than recomputed so that a change to
	// ReplyWindow cannot retroactively shorten somebody's window.
	AskedAt       time.Time
	ReplyDeadline time.Time

	// RepliedAt is set by a person. "Talk to us about Enterprise" has no
	// machine-readable answer, and locking an account that did reply because a
	// mailbox rule swallowed the thread would be unforgivable.
	RepliedAt time.Time

	// LockedAt is when the dashboard was locked. Collection never stops, and
	// settings and exports stay open — the lock is a prompt to reply, not a
	// punishment.
	LockedAt time.Time
}

// Locked reports whether the dashboard is currently locked for volume.
func (o Overage) Locked() bool {
	return !o.LockedAt.IsZero()
}

// Action is what the sweeper should do about an account's volume. It is a
// struct of decisions rather than a method with side effects so the rule can be
// tested exhaustively without a database.
type Action struct {
	// Ask sends the second-month email and starts the two-week window.
	Ask bool

	// Lock blocks the dashboard because the window expired with no reply.
	Lock bool

	// Unlock releases it because usage came back into range, which happens
	// immediately rather than at the next billing boundary.
	Unlock bool

	// Clear drops the overage record entirely, because the account is back
	// inside its plan and the conversation is over.
	Clear bool

	// Deadline is the reply-by date, set when Ask is true so the caller stores
	// exactly the date the email quotes.
	Deadline time.Time
}

// Decide is the whole volume rule. Its inputs are the stored conversation, how
// many consecutive complete months the account has been over, whether it is
// over right now, and the time — and nothing else.
//
// The ordering below is the rule, in the order the specification states it:
//
//	one full month over        nothing at all
//	two consecutive months     ask, and give them two weeks
//	no reply after two weeks   lock the dashboard, keep collecting
//	back in range              unlock immediately
func Decide(current Overage, consecutiveMonths int, overNow bool, now time.Time) Action {
	// Back in range wins over everything else, and it wins immediately. An
	// account that has fixed the problem must not stay locked until some later
	// boundary, and it must not be asked about a month that is now behind it.
	if !overNow && consecutiveMonths < 2 {
		if current.Locked() {
			return Action{Unlock: true, Clear: true}
		}

		if !current.AskedAt.IsZero() || current.Period != "" {
			return Action{Clear: true}
		}

		return Action{}
	}

	// One month over does nothing. This branch is the promise that a single
	// spike — a launch, a link that went around — costs a customer nothing but
	// an email telling them it happened.
	if consecutiveMonths < 2 {
		return Action{}
	}

	if current.AskedAt.IsZero() {
		return Action{Ask: true, Deadline: now.Add(ReplyWindow)}
	}

	// A reply ends it. Whatever the outcome of the conversation, it is now a
	// human's to continue, and no timer should overtake them.
	if !current.RepliedAt.IsZero() {
		return Action{}
	}

	if !current.Locked() && !current.ReplyDeadline.IsZero() && !now.Before(current.ReplyDeadline) {
		return Action{Lock: true}
	}

	return Action{}
}

// Access is what a volume lock permits. Collection continues, settings and
// exports stay open, and only the reports are blocked — the point is to start a
// conversation, and none of the other three would help start one.
type Access struct {
	Dashboard bool
	Collect   bool
	Export    bool
	Settings  bool
}

// Capabilities maps a lock state onto what it permits.
func Capabilities(locked bool) Access {
	return Access{Dashboard: !locked, Collect: true, Export: true, Settings: true}
}
