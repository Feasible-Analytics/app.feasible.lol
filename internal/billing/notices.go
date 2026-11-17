//
// notices.go
// Turning a reconciled subscription into the one line worth telling ourselves.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/posthog"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/slack"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// announcePlanChange posts a subscription that started, changed term, or ended
// to Slack, and records it in PostHog.
//
// It compares the stored mirror before and after reconciliation rather than
// reading the triggering event, because the same commercial change arrives as
// several different Stripe events and can arrive twice. A comparison of two
// stored states says a thing happened once; an event says only that Stripe sent
// something.
func (s *Service) announcePlanChange(ctx context.Context, before, after Subscription) {
	if s == nil || (!s.Slack.Enabled() && !s.PostHog.Enabled()) {
		return
	}

	change, upgraded, changed := planChange(before, after)
	if !changed {
		return
	}

	if upgraded {
		s.Slack.Upgraded(change)
	} else {
		s.Slack.Downgraded(change)
	}

	s.recordPlanChange(ctx, change, upgraded)
}

// planChange classifies two stored states. It reports the change, whether it
// is an upgrade, and whether anything commercial changed at all.
func planChange(before, after Subscription) (slack.PlanChange, bool, bool) {
	change := slack.PlanChange{
		TeamID:   after.TeamID,
		Email:    after.BillingEmail,
		Plan:     after.Plan,
		Previous: before.Plan,
	}

	beforePaid := before.PaymentState == PaymentPaid
	afterPaid := after.PaymentState == PaymentPaid

	switch {
	case !beforePaid && afterPaid:
		// A first paid period. Previous is cleared because "moved from no plan"
		// reads worse than the plain statement that they are now on one.
		change.Previous = ""
		return change, true, true

	case beforePaid && !afterPaid:
		change.Plan = ""
		change.Reason = endedReason(after)
		return change, false, true

	case beforePaid && afterPaid && before.Plan != after.Plan:
		// The catalogue has no tiers, only terms, so the longer term is the
		// upgrade. Anything unrecognised is reported as a downgrade, because a
		// change we cannot classify is worth a person looking at.
		return change, after.Plan == Yearly.Key, true
	}

	return change, false, false
}

// recordPlanChange sends the change to PostHog against the team owner, the
// person who signed up, so the plan joins that person's signup. The billing
// email stands in when the owner cannot be read.
func (s *Service) recordPlanChange(ctx context.Context, change slack.PlanChange, upgraded bool) {
	if !s.PostHog.Enabled() {
		return
	}

	email := change.Email
	if s.Store != nil {
		owner, err := s.Store.OwnerEmail(ctx, change.TeamID)
		switch {
		case err != nil:
			if s.Log != nil {
				s.Log.Warn("could not read the team owner for a plan event", "team", change.TeamID, "error", err)
			}
		case owner != "":
			email = owner
		}
	}

	event := posthog.PlanChange{
		Email:        email,
		TeamID:       change.TeamID,
		Plan:         change.Plan,
		Previous:     change.Previous,
		BillingEmail: change.Email,
		Reason:       change.Reason,
	}

	if upgraded {
		s.PostHog.Upgraded(event)
		return
	}

	s.PostHog.Downgraded(event)
}

// endedReason says why a paid subscription stopped being paid, in the words a
// person reading Slack needs: whether they left or whether their card did.
func endedReason(after Subscription) string {
	switch after.Status {
	case stripe.StatusCanceled, stripe.StatusIncompleteExpired:
		return "subscription cancelled"
	case stripe.StatusPaused:
		return "subscription paused"
	case stripe.StatusPastDue, stripe.StatusUnpaid:
		return "payment failed"
	}

	if after.CancelAtPeriodEnd {
		return "cancelling at period end"
	}

	if after.PaymentState == PaymentFailed {
		return "payment failed"
	}

	return "no longer paying"
}
