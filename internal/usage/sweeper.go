//
// sweeper.go
// Walking the accounts once an hour and running the ladder on each one.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import (
	"context"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// SweepInterval is how often the ladder runs. Hourly is often enough that a
// customer crossing 70% hears about it the same morning, and rare enough that
// the sweep never competes with ingestion for the system database.
const SweepInterval = time.Hour

// Notice is one volume email the ladder has decided to send. Like the lifecycle
// notice, it carries the numbers already computed so the mail package cannot
// arrive at a different total from the one the decision was made on.
type Notice struct {
	TeamID   int64
	TeamName string
	To       string

	Level  Level
	Period string

	Billable  int64
	Limit     int64
	Projected int64

	// Deadline is set only on the second-month email, and is the date that
	// message quotes as the reply-by.
	Deadline time.Time

	SalesEmail string
	BillingURL string
}

// Notifier sends one volume notice and reports what the transport observed.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) (string, error)
}

// Contacts resolves an account's billing contact and display name. It is an
// interface because the account records live in another package's tables and
// this one has no business reading them directly.
type Contacts interface {
	Contact(ctx context.Context, teamID int64) (name string, email string, err error)
}

// Sweeper runs the ladder over every account with usage this month.
type Sweeper struct {
	Store    *Store
	Notify   Notifier
	Contacts Contacts
	Log      *logger.Logger

	// Now is injectable so the whole ladder — three rungs, two months and a
	// two-week window — can be driven in a test without waiting.
	Now func() time.Time

	// SalesEmail is where a growing customer is pointed. The 70% email exists
	// to start this conversation while there is still a month of runway.
	SalesEmail string

	// BillingURL is the in-app usage meter, linked from every message so the
	// customer can see the number for themselves.
	BillingURL string
}

// now returns the sweeper's clock.
func (s *Sweeper) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Sweep runs one pass. It returns the number of accounts examined so a caller
// can log something more useful than "done", and it never stops at the first
// failure: one account with an unreadable contact must not silence the warnings
// for every other account on the box.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	now := s.now()
	period := Period(now)

	teams, err := s.Store.Teams(ctx, period)
	if err != nil {
		return 0, err
	}

	// Accounts with a conversation in progress are swept whether or not they
	// have sent anything this month. Driving the walk from this month's counters
	// alone would leave an account that went quiet locked forever, with its
	// reply deadline running and nobody looking at it.
	conversing, err := s.Store.OverageTeams(ctx)
	if err != nil {
		return 0, err
	}

	teams = merge(teams, conversing)

	var firstErr error

	for _, teamID := range teams {
		if err := s.sweepTeam(ctx, teamID, period, now); err != nil {
			if s.Log != nil {
				s.Log.Error("usage sweep failed for one account", "team", teamID, "error", err)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return len(teams), firstErr
}

// merge unions two sorted id lists without duplicates, so an account that is
// both over this month and mid-conversation is swept once rather than twice.
func merge(a, b []int64) []int64 {
	if len(b) == 0 {
		return a
	}

	seen := make(map[int64]struct{}, len(a)+len(b))
	out := make([]int64, 0, len(a)+len(b))

	for _, list := range [][]int64{a, b} {
		for _, id := range list {
			if _, ok := seen[id]; ok {
				continue
			}

			seen[id] = struct{}{}
			out = append(out, id)
		}
	}

	return out
}

// sweepTeam runs the ladder for one account: the three threshold emails for the
// month in progress, then the two-consecutive-month conversation.
func (s *Sweeper) sweepTeam(ctx context.Context, teamID int64, period string, now time.Time) error {
	counts, err := s.Store.Get(ctx, teamID, period)
	if err != nil {
		return err
	}

	billable := counts.Billable()

	if err := s.sendThresholds(ctx, teamID, period, billable, now); err != nil {
		return err
	}

	return s.runOverage(ctx, teamID, period, billable, now)
}

// sendThresholds sends every rung's email the account has passed and not yet
// been told about. Every rung is sent rather than only the highest, because the
// 70% message is the one that is actually useful and an account that jumped
// straight to a million between two sweeps is still owed it.
func (s *Sweeper) sendThresholds(ctx context.Context, teamID int64, period string, billable int64, now time.Time) error {
	levels := Reached(billable)
	if len(levels) == 0 {
		return nil
	}

	name, email, err := s.contact(ctx, teamID)
	if err != nil {
		return err
	}

	for _, level := range levels {
		sent, err := s.Store.NoticeSent(ctx, teamID, period, level)
		if err != nil {
			return err
		}
		if sent {
			continue
		}

		// The row is claimed before the message is rendered. If two sweeps race,
		// exactly one wins the insert and exactly one email is sent; the loser
		// finding the row already there is the correct outcome, not an error.
		claimed, err := s.Store.RecordNotice(ctx, teamID, period, level)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}

		notice := Notice{
			TeamID:     teamID,
			TeamName:   name,
			To:         email,
			Level:      level,
			Period:     period,
			Billable:   billable,
			Limit:      MonthlyLimit,
			Projected:  Projection(billable, now),
			SalesEmail: s.SalesEmail,
			BillingURL: s.BillingURL,
		}

		outcome, err := s.Notify.Notify(ctx, notice)
		if err != nil {
			return err
		}

		if s.Log != nil {
			s.Log.Info("usage notice sent", "team", teamID, "level", string(level),
				"billable", billable, "period", period, "outcome", outcome)
		}
	}

	return nil
}

// runOverage advances the two-consecutive-months conversation. Nothing here
// touches the deletion clock, and collection is never stopped: going over the
// plan is a reason to talk, not a reason to lose somebody's data.
func (s *Sweeper) runOverage(ctx context.Context, teamID int64, period string, billable int64, now time.Time) error {
	current, err := s.Store.Overage(ctx, teamID)
	if err != nil {
		return err
	}

	consecutive, err := s.Store.ConsecutiveOver(ctx, teamID, period)
	if err != nil {
		return err
	}

	action := Decide(current, consecutive, billable > MonthlyLimit, now)

	switch {
	case action.Ask:
		name, email, err := s.contact(ctx, teamID)
		if err != nil {
			return err
		}

		claimed, err := s.Store.RecordNotice(ctx, teamID, period, "second_month")
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}

		current.Period = period
		current.AskedAt = now
		current.ReplyDeadline = action.Deadline

		if err := s.Store.SaveOverage(ctx, teamID, current); err != nil {
			return err
		}

		outcome, err := s.Notify.Notify(ctx, Notice{
			TeamID: teamID, TeamName: name, To: email,
			Level: "second_month", Period: period,
			Billable: billable, Limit: MonthlyLimit,
			Deadline:   action.Deadline,
			SalesEmail: s.SalesEmail, BillingURL: s.BillingURL,
		})
		if err != nil {
			return err
		}

		if s.Log != nil {
			s.Log.Info("usage overage conversation opened", "team", teamID,
				"period", period, "reply_by", action.Deadline.Format(time.RFC3339), "outcome", outcome)
		}

	case action.Lock:
		current.LockedAt = now

		if err := s.Store.SaveOverage(ctx, teamID, current); err != nil {
			return err
		}

		if s.Log != nil {
			s.Log.Warn("dashboard locked for volume — collection continues", "team", teamID, "period", period)
		}

	case action.Unlock, action.Clear:
		if err := s.Store.ClearOverage(ctx, teamID); err != nil {
			return err
		}

		if action.Unlock && s.Log != nil {
			s.Log.Info("dashboard unlocked — usage is back in range", "team", teamID, "period", period)
		}
	}

	return nil
}

// contact resolves an account's billing contact, or reports why it could not.
// An account with no reachable contact is a real problem rather than a reason
// to skip quietly: it means nobody will hear about a limit until it bites.
func (s *Sweeper) contact(ctx context.Context, teamID int64) (string, string, error) {
	if s.Contacts == nil {
		return "", "", nil
	}

	return s.Contacts.Contact(ctx, teamID)
}

// Run sweeps on a ticker until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil && s.Log != nil {
				s.Log.Error("usage sweep failed", "error", err)
			}
		}
	}
}
