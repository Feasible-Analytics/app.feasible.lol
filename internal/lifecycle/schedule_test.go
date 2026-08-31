//
// schedule_test.go
// The ten emails: on the right days, in order, and each naming the right date.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"testing"
	"time"
)

// TestSequenceMatchesTheSpecification pins the days. These are the dates the
// product promises in writing, and a change to any of them changes what a
// customer was told would happen.
func TestSequenceMatchesTheSpecification(t *testing.T) {
	want := []struct {
		day       int
		template  string
		announces Phase
	}{
		{23, TemplateEndingSoon, PhaseLocked},
		{29, TemplateEndingTomorrow, PhaseLocked},
		{30, TemplateDashboardLocked, PhaseDormant},
		{45, TemplateCollectionStopsIn15, PhaseDormant},
		{59, TemplateCollectionStopsTomorrow, PhaseDormant},
		{60, TemplateCollectionStopped, PhaseDeleted},
		{75, TemplateDeletionIn15, PhaseDeleted},
		{85, TemplateDeletionIn5, PhaseDeleted},
		{89, TemplateDeletionTomorrow, PhaseDeleted},
		{90, TemplateAccountDeleted, PhaseDeleted},
	}

	if len(Sequence) != len(want) {
		t.Fatalf("the sequence has %d entries, want %d", len(Sequence), len(want))
	}

	for i, entry := range Sequence {
		if entry.Day != want[i].day || entry.Template != want[i].template {
			t.Errorf("entry %d is day %d %q, want day %d %q", i, entry.Day, entry.Template, want[i].day, want[i].template)
		}
		if entry.Announces != want[i].announces {
			t.Errorf("%s announces %q, want %q", entry.Template, entry.Announces, want[i].announces)
		}
	}
}

// TestSequenceIsOrdered guards against a new email being inserted in the wrong
// place. The sweeper sends whatever is due in slice order, so an out-of-order
// entry would arrive after a message about a later date.
func TestSequenceIsOrdered(t *testing.T) {
	for i := 1; i < len(Sequence); i++ {
		if Sequence[i].Day <= Sequence[i-1].Day {
			t.Fatalf("%s (day %d) does not come after %s (day %d)",
				Sequence[i].Template, Sequence[i].Day, Sequence[i-1].Template, Sequence[i-1].Day)
		}
	}
}

// TestEveryWarningComesBeforeTheThingItWarnsAbout is the promise "nobody is ever
// surprised", checked rather than assumed. Every message must be due strictly
// before the boundary it announces, except the two that report a boundary as it
// happens.
func TestEveryWarningComesBeforeTheThingItWarnsAbout(t *testing.T) {
	state := trial()

	for _, entry := range Sequence {
		sendAt := state.SendAt(entry)
		announced := state.Announced(entry)

		if announced.IsZero() {
			t.Fatalf("%s announces nothing", entry.Template)
		}

		if sendAt.After(announced) {
			t.Errorf("%s is sent on day %d, after the %s boundary it announces", entry.Template, entry.Day, entry.Announces)
		}
	}
}

// TestDueAtAccumulates is what a sweep that has been down for a week relies on:
// every message that came due while it was down is still owed, in order, rather
// than skipped to the latest.
func TestDueAtAccumulates(t *testing.T) {
	state := trial()

	cases := []struct {
		day  int
		want int
	}{
		{0, 0},
		{22, 0},
		{23, 1},
		{29, 2},
		{30, 3},
		{59, 5},
		{60, 6},
		{89, 9},
		{90, 10},
		{365, 10},
	}

	for _, tc := range cases {
		if got := len(state.DueAt(at(tc.day))); got != tc.want {
			t.Errorf("day %d has %d emails due, want %d", tc.day, got, tc.want)
		}
	}
}

// TestDueAtIsOrdered makes sure a catch-up sweep sends the oldest first. A
// customer receiving "we delete your account tomorrow" before "we have stopped
// collecting" would reasonably conclude the whole system is broken.
func TestDueAtIsOrdered(t *testing.T) {
	due := trial().DueAt(at(90))

	for i := 1; i < len(due); i++ {
		if due[i].Day <= due[i-1].Day {
			t.Fatalf("due list is out of order at %d", i)
		}
	}
}

// TestSendAtIsExact checks the instant an email becomes due. The email quotes a
// date, so a send that drifted a day would contradict its own contents.
func TestSendAtIsExact(t *testing.T) {
	state := trial()

	for _, entry := range Sequence {
		want := day0.Add(time.Duration(entry.Day) * Day)

		if got := state.SendAt(entry); !got.Equal(want) {
			t.Errorf("%s is due at %s, want %s", entry.Template, got, want)
		}
	}
}

// TestAnnouncedDatesAreTheBoundaries closes the loop between what an email says
// and what the sweeper does. Both come from Boundary, and this asserts the
// mapping rather than trusting it.
func TestAnnouncedDatesAreTheBoundaries(t *testing.T) {
	state := trial()

	for _, entry := range Sequence {
		if got, want := state.Announced(entry), state.Boundary(entry.Announces); !got.Equal(want) {
			t.Errorf("%s announces %s, but the boundary is %s", entry.Template, got, want)
		}
	}
}

// TestScheduleIsIdenticalForBothTriggers states the rule that the two paths
// differ only in wording. A dunning customer and a trialling one get the same
// ten messages on the same ten days.
func TestScheduleIsIdenticalForBothTriggers(t *testing.T) {
	for day := 0; day <= 90; day++ {
		trialDue := trial().DueAt(at(day))
		lapseDue := lapsed().DueAt(at(day))

		if len(trialDue) != len(lapseDue) {
			t.Fatalf("day %d: trial has %d due, dunning has %d", day, len(trialDue), len(lapseDue))
		}

		for i := range trialDue {
			if trialDue[i].Template != lapseDue[i].Template {
				t.Fatalf("day %d entry %d differs: %s vs %s", day, i, trialDue[i].Template, lapseDue[i].Template)
			}
		}
	}
}
