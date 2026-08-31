//
// schedule.go
// Which lifecycle email is due, on which day, and what date it has to name.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import "time"

// The templates, in the order they are sent. The name is the identity: it is
// what the lifecycle_emails row stores, so renaming one would re-send the whole
// sequence to every account currently mid-clock. Add new ones, never rename.
const (
	TemplateEndingSoon              = "ending_soon"
	TemplateEndingTomorrow          = "ending_tomorrow"
	TemplateDashboardLocked         = "dashboard_locked"
	TemplateCollectionStopsIn15     = "collection_stops_in_15"
	TemplateCollectionStopsTomorrow = "collection_stops_tomorrow"
	TemplateCollectionStopped       = "collection_stopped"
	TemplateDeletionIn15            = "deletion_in_15"
	TemplateDeletionIn5             = "deletion_in_5"
	TemplateDeletionTomorrow        = "deletion_tomorrow"
	TemplateAccountDeleted          = "account_deleted"
)

// Scheduled is one email in the sequence.
type Scheduled struct {
	// Day is the clock day it becomes due. A sweep that has been down for a
	// week sends everything that came due while it was down, in order, rather
	// than skipping to the latest — a customer who missed "we stop collecting
	// tomorrow" is owed it even late.
	Day int

	// Template names the message.
	Template string

	// Announces is the phase whose date this email exists to warn about. The
	// message quotes that boundary, so the date in the email and the date the
	// machine acts on are the same arithmetic.
	Announces Phase
}

// Sequence is every lifecycle email, in order. Both triggers send the identical
// set on the identical days; only the wording differs, which is decided in the
// mail package from the trigger on the state.
//
// The dates here are the whole promise the product makes about not surprising
// anybody: three warnings before the dashboard locks, three before collection
// stops, three before deletion, and one confirming it happened.
var Sequence = []Scheduled{
	{Day: 23, Template: TemplateEndingSoon, Announces: PhaseLocked},
	{Day: 29, Template: TemplateEndingTomorrow, Announces: PhaseLocked},
	{Day: GraceDays, Template: TemplateDashboardLocked, Announces: PhaseDormant},
	{Day: 45, Template: TemplateCollectionStopsIn15, Announces: PhaseDormant},
	{Day: 59, Template: TemplateCollectionStopsTomorrow, Announces: PhaseDormant},
	{Day: LockedDays, Template: TemplateCollectionStopped, Announces: PhaseDeleted},
	{Day: 75, Template: TemplateDeletionIn15, Announces: PhaseDeleted},
	{Day: 85, Template: TemplateDeletionIn5, Announces: PhaseDeleted},
	{Day: 89, Template: TemplateDeletionTomorrow, Announces: PhaseDeleted},
	{Day: DeletionDays, Template: TemplateAccountDeleted, Announces: PhaseDeleted},
}

// DueAt lists every email whose day has arrived for this clock, oldest first.
// It is a pure function of the state and the instant, so the caller's only job
// is to subtract the ones it has already sent — which it reads from a table
// with a unique constraint, making a duplicate send impossible rather than
// merely unlikely.
func (s State) DueAt(now time.Time) []Scheduled {
	if !s.Running() {
		return nil
	}

	day := s.DayAt(now)

	var due []Scheduled
	for _, entry := range Sequence {
		if entry.Day <= day {
			due = append(due, entry)
		}
	}

	return due
}

// SendAt is when one scheduled email becomes due, as a real instant. It is what
// a queued job's scheduled_at is set from, and what a test asserts against.
func (s State) SendAt(entry Scheduled) time.Time {
	if !s.Running() {
		return time.Time{}
	}

	return s.StartedAt.Add(time.Duration(entry.Day) * Day)
}

// Announced is the date one email has to name: the instant the phase it warns
// about begins. Every message in the sequence quotes it, and it comes from the
// same Boundary the sweeper acts on so the two can never disagree.
func (s State) Announced(entry Scheduled) time.Time {
	return s.Boundary(entry.Announces)
}
