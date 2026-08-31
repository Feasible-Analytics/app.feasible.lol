//
// ladder_test.go
// One month over does nothing; two starts a conversation; silence locks the dashboard.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import (
	"testing"
	"time"
)

// ladderNow is the instant the ladder tests run at.
var ladderNow = time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)

// TestOneMonthOverDoesNothing is the promise that a single spike — a launch, a
// link that went around — costs a customer nothing but an email telling them it
// happened.
func TestOneMonthOverDoesNothing(t *testing.T) {
	action := Decide(Overage{}, 1, true, ladderNow)

	if action.Ask || action.Lock || action.Unlock || action.Clear {
		t.Fatalf("one month over produced %+v", action)
	}
}

// TestTwoMonthsOpensAConversation checks the second consecutive month, and that
// the reply window is exactly the two weeks the email quotes.
func TestTwoMonthsOpensAConversation(t *testing.T) {
	action := Decide(Overage{}, 2, true, ladderNow)

	if !action.Ask {
		t.Fatal("two consecutive months over did not open a conversation")
	}
	if action.Lock {
		t.Fatal("two months over locked the dashboard immediately")
	}
	if !action.Deadline.Equal(ladderNow.Add(ReplyWindow)) {
		t.Errorf("the deadline is %s, want %s", action.Deadline, ladderNow.Add(ReplyWindow))
	}
}

// TestAskingHappensOnlyOnce guards against the sweeper reopening the same
// conversation every hour for two weeks.
func TestAskingHappensOnlyOnce(t *testing.T) {
	opened := Overage{Period: "2026-03", AskedAt: ladderNow, ReplyDeadline: ladderNow.Add(ReplyWindow)}

	action := Decide(opened, 2, true, ladderNow.Add(time.Hour))

	if action.Ask {
		t.Fatal("the conversation was opened twice")
	}
	if action.Lock {
		t.Fatal("the dashboard locked before the deadline")
	}
}

// TestNothingHappensBeforeTheDeadline is the two weeks, honoured to the second.
func TestNothingHappensBeforeTheDeadline(t *testing.T) {
	deadline := ladderNow.Add(ReplyWindow)
	opened := Overage{Period: "2026-03", AskedAt: ladderNow, ReplyDeadline: deadline}

	if Decide(opened, 2, true, deadline.Add(-time.Second)).Lock {
		t.Fatal("the dashboard locked a second early")
	}

	if !Decide(opened, 2, true, deadline).Lock {
		t.Fatal("the dashboard did not lock at the deadline")
	}
}

// TestAReplyStopsTheClock is the rule that protects somebody who did answer. An
// email thread is not machine-readable, so a person records the reply, and after
// that no timer may overtake them.
func TestAReplyStopsTheClock(t *testing.T) {
	deadline := ladderNow.Add(ReplyWindow)

	replied := Overage{
		Period:        "2026-03",
		AskedAt:       ladderNow,
		ReplyDeadline: deadline,
		RepliedAt:     ladderNow.Add(time.Hour),
	}

	if Decide(replied, 2, true, deadline.Add(30*24*time.Hour)).Lock {
		t.Fatal("an account that replied was locked anyway")
	}
}

// TestBackInRangeUnlocksImmediately is the promise that fixing the problem is
// enough. It must not wait for a billing boundary or the next month.
func TestBackInRangeUnlocksImmediately(t *testing.T) {
	locked := Overage{
		Period:        "2026-03",
		AskedAt:       ladderNow.Add(-30 * 24 * time.Hour),
		ReplyDeadline: ladderNow.Add(-16 * 24 * time.Hour),
		LockedAt:      ladderNow.Add(-time.Hour),
	}

	action := Decide(locked, 0, false, ladderNow)

	if !action.Unlock {
		t.Fatal("coming back into range did not unlock the dashboard")
	}
	if !action.Clear {
		t.Fatal("coming back into range did not end the conversation")
	}
}

// TestBackInRangeWithoutALockJustClears covers an account that came back before
// the deadline. The conversation ends and nothing was ever locked.
func TestBackInRangeWithoutALockJustClears(t *testing.T) {
	opened := Overage{Period: "2026-03", AskedAt: ladderNow, ReplyDeadline: ladderNow.Add(ReplyWindow)}

	action := Decide(opened, 0, false, ladderNow.Add(3*24*time.Hour))

	if action.Unlock {
		t.Error("an account that was never locked was unlocked")
	}
	if !action.Clear {
		t.Error("the conversation was left open after usage came back into range")
	}
}

// TestAHealthyAccountProducesNothing makes sure the common case is silent.
func TestAHealthyAccountProducesNothing(t *testing.T) {
	action := Decide(Overage{}, 0, false, ladderNow)

	if action.Ask || action.Lock || action.Unlock || action.Clear {
		t.Fatalf("a healthy account produced %+v", action)
	}
}

// TestVolumeLockKeepsEverythingElseWorking is the difference between this lock
// and the lifecycle one. Collection continues, exports work, settings are open —
// the lock is a prompt to reply, not a punishment, and it never touches data.
func TestVolumeLockKeepsEverythingElseWorking(t *testing.T) {
	access := Capabilities(true)

	if access.Dashboard {
		t.Error("a volume-locked account can still see its dashboard")
	}
	if !access.Collect {
		t.Error("a volume lock stopped collection — going over the plan must never lose data")
	}
	if !access.Export {
		t.Error("a volume lock blocked the export")
	}
	if !access.Settings {
		t.Error("a volume lock blocked the settings, including the page where they would upgrade")
	}

	if unlocked := Capabilities(false); !unlocked.Dashboard {
		t.Error("an unlocked account cannot see its dashboard")
	}
}

// TestLockedReportsTheState is a small helper, asserted because the gate reads
// it on every request.
func TestLockedReportsTheState(t *testing.T) {
	if (Overage{}).Locked() {
		t.Error("an empty conversation reports a lock")
	}
	if !(Overage{LockedAt: ladderNow}).Locked() {
		t.Error("a locked conversation reports no lock")
	}
}
