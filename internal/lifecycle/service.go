//
// service.go
// The sweeper: apply signals, send what is due, and delete what has run out of days.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// SweepInterval is how often the clock is advanced. Hourly is the right
// granularity for a machine whose smallest unit is a day: it means an email due
// on day 45 goes out within an hour of the boundary rather than at whatever
// time of day the process happened to start.
const SweepInterval = time.Hour

// Links builds the three URLs every lifecycle email carries. It is a function
// rather than three strings because the upgrade and export links are per
// account, and building them here keeps the base URL in one place.
type Links struct {
	// BaseURL is the address people actually type. Every link below hangs off
	// it, so a deployment that gets it wrong produces emails full of links
	// nobody can click — which is why it is required rather than defaulted.
	BaseURL string
}

// Upgrade is the one-click upgrade link for an explicit account. A zero id is
// used only by the post-deletion "start again" message.
func (l Links) Upgrade(teamID int64) string {
	if teamID < 1 {
		return l.BaseURL + "/billing/upgrade"
	}

	return fmt.Sprintf("%s/billing/upgrade?team=%d", l.BaseURL, teamID)
}

// Portal is the safe billing page. The email link performs no provider-side
// action; the signed-in person explicitly submits the CSRF-protected portal
// form from there.
func (l Links) Portal(teamID int64) string {
	return fmt.Sprintf("%s/billing?team=%d", l.BaseURL, teamID)
}

// Export is the download-everything link, which works in every phase.
func (l Links) Export(teamID int64) string {
	return fmt.Sprintf("%s/billing/export?team=%d", l.BaseURL, teamID)
}

// Service drives the machine: it applies signals from the outside world, and it
// sweeps the running clocks on a timer.
type Service struct {
	Store  *Store
	Notify Notifier
	Purger *Purger
	Links  Links
	Log    *logger.Logger

	// Now is injectable. It is the single most important test hook in this
	// package: a ninety-one day timeline runs in microseconds and produces
	// exactly the transitions ninety-one real days would.
	Now func() time.Time

	// mu serialises sweeps. Two sweeps running at once would both see the same
	// account due for deletion, and while the deletion itself is idempotent,
	// serialising is cheaper than reasoning about whether every step stays that
	// way forever.
	mu sync.Mutex
}

// now returns the service's clock.
func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Signal applies one outside event to an account and persists the result. It is
// the only way anything outside this package moves an account between phases:
// the webhook handler, the signup path and the support commands all come
// through here, so there is exactly one place where a phase can change.
func (s *Service) Signal(ctx context.Context, teamID int64, signal Signal) (Transition, error) {
	return s.SignalAt(ctx, teamID, signal, s.now())
}

// SignalAt applies an outside event at the instant it actually happened. Stripe
// failure events use their signed creation time so delayed delivery cannot move
// the contractual day-zero date to local processing time.
func (s *Service) SignalAt(ctx context.Context, teamID int64, signal Signal, at time.Time) (Transition, error) {
	lease, err := s.Store.AcquireTransitionLease(ctx, teamID)
	if err != nil {
		return Transition{}, err
	}
	defer lease.Release()

	comped, err := s.Store.IsComped(ctx, teamID)
	if err != nil {
		return Transition{}, err
	}
	if comped && signal != SignalDeleted {
		state, loadErr := s.Store.Load(ctx, teamID)
		if loadErr != nil {
			return Transition{}, loadErr
		}

		return Transition{State: state, From: PhaseActive, To: PhaseActive}, nil
	}

	state, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return Transition{}, err
	}

	at = at.UTC()
	if at.IsZero() {
		at = s.now()
	}

	transition, err := Apply(state, signal, at)
	if err != nil {
		return transition, err
	}

	// Out-of-order delivery can reveal that the first failure happened before
	// the failure that originally started the clock. Correct only within the
	// same uninterrupted lapse; a successful payment clears the old clock.
	if !transition.Changed && signal == SignalPaymentFailed && state.Trigger == TriggerLapse &&
		state.DeletedAt.IsZero() && !state.StartedAt.IsZero() && at.Before(state.StartedAt) {
		transition.State.StartedAt = at
		transition.From = state.At(s.now())
		transition.To = transition.State.At(s.now())
		transition.Changed = true
	}

	if !transition.Changed {
		if signal == SignalPaymentSucceeded && transition.To == PhaseActive {
			if err := s.finalizeActive(ctx, teamID, s.now()); err != nil {
				return transition, err
			}
		}

		return transition, nil
	}

	saved, err := s.Store.SaveIfState(ctx, teamID, state, transition.State)
	if err != nil {
		return transition, err
	}
	if !saved {
		latest, loadErr := s.Store.Load(ctx, teamID)
		if loadErr != nil {
			return transition, loadErr
		}

		return Transition{State: latest, From: latest.At(s.now()), To: latest.At(s.now())}, nil
	}

	// Returning to Active cancels every pending email in the same breath as the
	// state change. A customer who has just paid must never receive "we delete
	// your account tomorrow", and the only way to be sure is to do it here
	// rather than to check at send time.
	if transition.CancelEmails {
		if err := s.finalizeActive(ctx, teamID, s.now()); err != nil {
			return transition, err
		}

		return transition, nil
	}

	if s.Log != nil {
		s.Log.Warn("account lifecycle clock started",
			"team", teamID, "trigger", string(transition.State.Trigger),
			"locks_at", transition.State.Boundary(PhaseLocked).Format(time.RFC3339),
			"stops_at", transition.State.Boundary(PhaseDormant).Format(time.RFC3339),
			"deletes_at", transition.State.Boundary(PhaseDeleted).Format(time.RFC3339))
	}

	return transition, nil
}

// finalizeActive repairs every side effect owed by a successful payment even
// when the lifecycle row is already active. That makes a crash after the state
// commit recoverable by replaying the same signed event.
func (s *Service) finalizeActive(ctx context.Context, teamID int64, at time.Time) error {
	cancelled, err := s.Store.CancelPending(ctx, teamID)
	if err != nil {
		return err
	}

	if err := s.Store.CloseGap(ctx, teamID, at); err != nil {
		return err
	}

	if s.Log != nil {
		s.Log.Info("account returned to active", "team", teamID, "cancelled_emails", cancelled)
	}

	return nil
}

// Sweep advances every running clock once. It returns how many accounts it
// looked at, and it never stops at the first failure: one account with an
// unreadable contact must not stop another account's deletion happening on the
// day it was promised.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var firstErr error

	// The immutable audit is the recovery index for a process that died during
	// deletion. It is consulted before the live-team join so even a legacy crash
	// after team removal remains finishable.
	if s.Purger != nil {
		pending, err := s.Purger.PendingDeletions(ctx)
		if err != nil {
			return 0, err
		}
		for _, account := range pending {
			if err := s.Purger.Purge(ctx, account, now); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	running, err := s.Store.Running(ctx)
	if err != nil {
		return 0, err
	}

	for _, account := range running {
		if err := s.advance(ctx, account, now); err != nil {
			if s.Log != nil {
				s.Log.Error("lifecycle sweep failed for one account", "team", account.TeamID, "error", err)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// Confirmations are sent after the walk so that a mail relay being down
	// never delays a deletion. The data is destroyed on the promised day either
	// way; the email catches up when the relay comes back.
	if err := s.sendConfirmations(ctx); err != nil && firstErr == nil {
		firstErr = err
	}

	return len(running), firstErr
}

// advance moves one account: open a collection gap the moment it goes dormant,
// send every email that has come due, and destroy it if it has reached day 90.
func (s *Service) advance(ctx context.Context, account Account, now time.Time) error {
	if account.State.Deleted() {
		if s.Purger == nil {
			return fmt.Errorf("lifecycle: account %d has an unfinished deletion but no purger is configured", account.TeamID)
		}

		return s.Purger.Purge(ctx, account, now)
	}

	phase := account.State.At(now)

	// The gap is recorded at the moment collection stops rather than when
	// somebody comes back, so a graph drawn during the dormant phase already
	// shows the labelled gap instead of a run of zeroes.
	if phase == PhaseDormant || phase == PhaseDeleted {
		if err := s.Store.OpenGap(ctx, account.TeamID, account.State.Boundary(PhaseDormant)); err != nil {
			return err
		}
	}

	if err := s.sendDue(ctx, account, now); err != nil {
		return err
	}

	if !account.State.DueForDeletion(now) {
		return nil
	}

	if s.Purger == nil {
		return fmt.Errorf("lifecycle: account %d is due for deletion but no purger is configured", account.TeamID)
	}

	return s.Purger.Purge(ctx, account, now)
}

// sendDue sends every email whose day has arrived and which has not been sent
// for this clock. It sends them in order and it sends all of them: a sweep that
// has been down for a week owes the customer the warnings it missed, even late,
// because each one names a different date and skipping to the last would hide
// the ones that were still actionable.
func (s *Service) sendDue(ctx context.Context, account Account, now time.Time) error {
	due := account.State.DueAt(now)
	if len(due) == 0 {
		return nil
	}
	lease, err := s.Store.AcquireTransitionLease(ctx, account.TeamID)
	if err != nil {
		return err
	}
	defer lease.Release()

	current, err := s.Store.Load(ctx, account.TeamID)
	if err != nil {
		return err
	}
	if current.Trigger != account.State.Trigger || !current.StartedAt.Equal(account.State.StartedAt) ||
		!current.DeletedAt.Equal(account.State.DeletedAt) || !current.Running() {
		return nil
	}

	sent, err := s.Store.SentEmails(ctx, account.TeamID, account.State.StartedAt)
	if err != nil {
		return err
	}

	for _, entry := range due {
		if sent[entry.Template] {
			continue
		}

		// The day-90 confirmation is not sent from here. It has to go out after
		// the account is gone, and by then every row that would let us find it
		// has been destroyed — so it is driven from the deletion record instead.
		if entry.Template == TemplateAccountDeleted {
			continue
		}

		if account.Email == "" {
			return fmt.Errorf("lifecycle: account %d has no billing contact and is due %s", account.TeamID, entry.Template)
		}

		notice := s.notice(account, entry, now)
		claim, claimed, err := s.Store.ClaimNotice(ctx, account.State.StartedAt, notice, s.now())
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		if err := lease.Renew(ctx); err != nil {
			return err
		}

		claim.Notice.MessageKey = claim.MessageKey
		outcome, err := s.Notify.Notify(ctx, claim.Notice)

		if err != nil {
			if recordErr := s.Store.FailEmail(ctx, account.TeamID, account.State.StartedAt,
				entry.Template, claim, outcomeText(outcome, err), s.now()); recordErr != nil {
				return recordErr
			}
			return err
		}
		if err := s.Store.FinishEmail(ctx, account.TeamID, account.State.StartedAt,
			entry.Template, claim, outcomeText(outcome, nil), s.now()); err != nil {
			return err
		}

		if s.Log != nil {
			s.Log.Info("lifecycle email sent",
				"team", account.TeamID, "template", entry.Template,
				"day", account.State.DayAt(now), "outcome", outcome)
		}
	}

	return nil
}

// notice builds the message the mailer renders. Every date on it comes from the
// state machine's own boundaries, so the date in the email and the date the
// sweeper will act on are the same arithmetic rather than two implementations
// of it.
func (s *Service) notice(account Account, entry Scheduled, now time.Time) Notice {
	notice := Notice{
		TeamID:     account.TeamID,
		TeamName:   account.TeamName,
		To:         account.Email,
		Template:   entry.Template,
		Trigger:    account.State.Trigger,
		Phase:      account.State.At(now),
		Day:        account.State.DayAt(now),
		LocksAt:    account.State.Boundary(PhaseLocked),
		StopsAt:    account.State.Boundary(PhaseDormant),
		DeletesAt:  account.State.Boundary(PhaseDeleted),
		Announced:  account.State.Announced(entry),
		UpgradeURL: s.Links.Upgrade(account.TeamID),
		ExportURL:  s.Links.Export(account.TeamID),
	}

	// The card-update link only makes sense for somebody who has a card. On the
	// trial path there is no customer at the payment provider at all, so the
	// link would lead to an error page.
	if account.State.Trigger == TriggerLapse {
		notice.PortalURL = s.Links.Portal(account.TeamID)
	}

	return notice
}

// sendConfirmations sends the day-90 "we deleted your account" email for every
// completed deletion that has not had one.
func (s *Service) sendConfirmations(ctx context.Context) error {
	if s.Purger == nil {
		return nil
	}

	pending, err := s.Purger.PendingConfirmations(ctx)
	if err != nil {
		return err
	}

	var firstErr error

	for _, deletion := range pending {
		notice := Notice{
			TeamID: deletion.TeamID, TeamName: deletion.TeamName, To: deletion.Email,
			Template: TemplateAccountDeleted, Trigger: TriggerNone, Phase: PhaseDeleted,
			Day: DeletionDays, UpgradeURL: s.Links.Upgrade(0),
		}
		claim, claimed, err := s.Store.ClaimNotice(ctx, deletion.StartedAt, notice, s.now())
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !claimed {
			continue
		}

		claim.Notice.MessageKey = claim.MessageKey
		outcome, err := s.Notify.Notify(ctx, claim.Notice)
		if err != nil {
			if recordErr := s.Store.FailEmail(ctx, deletion.TeamID, deletion.StartedAt,
				TemplateAccountDeleted, claim, outcomeText(outcome, err), s.now()); recordErr != nil && firstErr == nil {
				firstErr = recordErr
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if err := s.Store.FinishEmail(ctx, deletion.TeamID, deletion.StartedAt,
			TemplateAccountDeleted, claim, outcomeText(outcome, nil), s.now()); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// outcomeText turns a transport answer and an error into the one string stored
// on the row. A failure is recorded as a failure rather than as an empty
// string, because an empty outcome is how a pending email is recognised.
func outcomeText(outcome string, err error) string {
	if err != nil {
		if outcome == "" {
			return "failed: " + err.Error()
		}

		return outcome + " (failed: " + err.Error() + ")"
	}

	if outcome == "" {
		return "sent"
	}

	return outcome
}

// Run sweeps on a ticker until the context is cancelled. One sweep runs
// immediately so that a process restarting after a long outage catches up
// rather than waiting an hour to notice an account is a month overdue.
func (s *Service) Run(ctx context.Context) {
	if _, err := s.Sweep(ctx); err != nil && s.Log != nil {
		s.Log.Error("lifecycle sweep failed", "error", err)
	}

	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && s.Log != nil {
				s.Log.Error("lifecycle sweep failed", "error", err)
			}
		}
	}
}
