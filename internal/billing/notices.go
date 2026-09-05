//
// notices.go
// Turning a reconciled subscription into the one line worth telling ourselves.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"github.com/Feasible-Analytics/app.feasible.lol/internal/slack"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// announcePlanChange posts a subscription that started, changed term, or ended.
//
// It compares the stored mirror before and after reconciliation rather than
// reading the triggering event, because the same commercial change arrives as
// several different Stripe events and can arrive twice. A comparison of two
// stored states says a thing happened once; an event says only that Stripe sent
// something.
func (s *Service) announcePlanChange(before, after Subscription) {
	if s == nil || !s.Slack.Enabled() {
		return
	}

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
		s.Slack.Upgraded(change)

	case beforePaid && !afterPaid:
		change.Plan = ""
		change.Reason = endedReason(after)
		s.Slack.Downgraded(change)

	case beforePaid && afterPaid && before.Plan != after.Plan:
		// The catalogue has no tiers, only terms, so the longer term is the
		// upgrade. Anything unrecognised is reported as a downgrade, because a
		// change we cannot classify is worth a person looking at.
		if after.Plan == Yearly.Key {
			s.Slack.Upgraded(change)
			return
		}

		s.Slack.Downgraded(change)
	}
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
