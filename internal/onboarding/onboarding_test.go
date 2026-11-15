//
// onboarding_test.go
// The schedule: which email is due on which day, and when none is.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"testing"
	"time"
)

// signup is day zero for every schedule test.
var signup = time.Date(2026, 11, 17, 14, 0, 0, 0, time.UTC)

// after is an instant measured from sign-up.
func after(d time.Duration) time.Time {
	return signup.Add(d)
}

// welcomed is the sent map of an account that got its welcome at sign-up, plus
// any install-help templates already settled.
func welcomed(templates ...string) map[string]time.Time {
	sent := map[string]time.Time{TemplateWelcome: signup}
	for _, template := range templates {
		sent[template] = signup
	}

	return sent
}

// TestNextFollowsTheDayBoundaries walks the clock across every boundary and
// checks the email named on each side of it.
func TestNextFollowsTheDayBoundaries(t *testing.T) {
	for name, test := range map[string]struct {
		at       time.Time
		sent     map[string]time.Time
		want     string
		wantData bool
	}{
		"welcome at sign-up":            {at: after(0), sent: nil, want: TemplateWelcome},
		"nothing a second before day 1": {at: after(Day - time.Second), sent: welcomed(), want: ""},
		"day 1 on the boundary":         {at: after(Day), sent: welcomed(), want: TemplateNoDataDay1, wantData: true},
		"day 1 is sent once":            {at: after(Day + time.Hour), sent: welcomed(TemplateNoDataDay1), want: ""},
		"nothing a second before day 3": {at: after(3*Day - time.Second), sent: welcomed(TemplateNoDataDay1), want: ""},
		"day 3 on the boundary":         {at: after(3 * Day), sent: welcomed(TemplateNoDataDay1), want: TemplateNoDataDay3, wantData: true},
		"day 3 is sent once":            {at: after(5 * Day), sent: welcomed(TemplateNoDataDay1, TemplateNoDataDay3), want: ""},
		"nothing a second before day 7": {at: after(7*Day - time.Second), sent: welcomed(TemplateNoDataDay1, TemplateNoDataDay3), want: ""},
		"day 7 on the boundary":         {at: after(7 * Day), sent: welcomed(TemplateNoDataDay1, TemplateNoDataDay3), want: TemplateNoDataDay7, wantData: true},
		"day 7 is sent once":            {at: after(8 * Day), sent: welcomed(TemplateNoDataDay1, TemplateNoDataDay3, TemplateNoDataDay7), want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, noData := Next(Progress{CreatedAt: signup, Verified: true, Sent: test.sent}, test.at)
			if got != test.want || noData != test.wantData {
				t.Fatalf("Next = %q (no-data %v), want %q (no-data %v)", got, noData, test.want, test.wantData)
			}
		})
	}
}

// TestNextSendsOnlyTheLatestMissedStep covers a sweeper that was down: an
// account that missed day 1 is sent day 3 on day 3, never both.
func TestNextSendsOnlyTheLatestMissedStep(t *testing.T) {
	got, _ := Next(Progress{CreatedAt: signup, Verified: true, Sent: welcomed()}, after(4*Day))
	if got != TemplateNoDataDay3 {
		t.Fatalf("Next = %q, want the day-3 email", got)
	}

	got, _ = Next(Progress{CreatedAt: signup, Verified: true, Sent: welcomed(TemplateNoDataDay3)}, after(4*Day))
	if got != "" {
		t.Fatalf("Next = %q after day 3 was sent, want nothing: day 1 must not follow it", got)
	}
}

// TestNextWaitsForVerification keeps the welcome from arriving beside the
// verification code, and from going to an address nobody has proved.
func TestNextWaitsForVerification(t *testing.T) {
	got, _ := Next(Progress{CreatedAt: signup, Verified: false}, after(2*Day))
	if got != "" {
		t.Fatalf("Next = %q for an unverified owner, want nothing", got)
	}
}

// TestNextLeavesAGapAfterALateWelcome covers an owner who verifies a day late:
// the welcome goes first and the install help waits.
func TestNextLeavesAGapAfterALateWelcome(t *testing.T) {
	late := after(Day + time.Hour)

	got, _ := Next(Progress{CreatedAt: signup, Verified: true}, late)
	if got != TemplateWelcome {
		t.Fatalf("Next = %q, want the welcome first", got)
	}

	sent := map[string]time.Time{TemplateWelcome: late}

	got, _ = Next(Progress{CreatedAt: signup, Verified: true, Sent: sent}, late.Add(WelcomeGap-time.Second))
	if got != "" {
		t.Fatalf("Next = %q inside the gap, want nothing", got)
	}

	got, _ = Next(Progress{CreatedAt: signup, Verified: true, Sent: sent}, late.Add(WelcomeGap))
	if got != TemplateNoDataDay1 {
		t.Fatalf("Next = %q after the gap, want the day-1 email", got)
	}
}

// TestNextStopsAtTheWindow keeps a long outage from writing to somebody about
// an install they gave up on.
func TestNextStopsAtTheWindow(t *testing.T) {
	for _, sent := range []map[string]time.Time{nil, welcomed()} {
		got, _ := Next(Progress{CreatedAt: signup, Verified: true, Sent: sent}, after(WindowDays*Day))
		if got != "" {
			t.Fatalf("Next = %q at the end of the window, want nothing", got)
		}
	}

	got, _ := Next(Progress{CreatedAt: signup, Verified: true, Sent: welcomed()}, signup.Add(-time.Hour))
	if got != "" {
		t.Fatalf("Next = %q before sign-up, want nothing", got)
	}
}

// TestTemplatesListsTheWelcomeAndEveryStep pins the set the mail inventory is
// built from.
func TestTemplatesListsTheWelcomeAndEveryStep(t *testing.T) {
	got := Templates()
	want := []string{TemplateWelcome, TemplateNoDataDay1, TemplateNoDataDay3, TemplateNoDataDay7}

	if len(got) != len(want) {
		t.Fatalf("Templates = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Templates = %v, want %v", got, want)
		}
	}
}
