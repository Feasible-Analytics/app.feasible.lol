//
// service.go
// Reconciling an account against the payment provider's current state.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// Plans names the two prices this product sells. They are read from
// configuration rather than hard-coded so that a self-hoster, a staging
// deployment and production can each point at their own payment provider
// account without a rebuild.
type Plans struct {
	Product string
	Monthly string
	Yearly  string
}

// PriceFor maps a plan key from a URL onto a price id. An unknown key returns
// empty, which the handler turns into a 400 rather than silently charging
// somebody for the wrong thing.
func (p Plans) PriceFor(key string) string {
	switch key {
	case "monthly", "month":
		return p.Monthly
	case "yearly", "year", "annual":
		return p.Yearly
	default:
		return ""
	}
}

// Service is the whole billing integration: the provider client, the mirror,
// and the lifecycle machine it drives.
type Service struct {
	Stripe    *stripe.Client
	Store     *Store
	Lifecycle *lifecycle.Service
	Plans     Plans
	Log       *logger.Logger

	// WebhookSecret verifies every delivery. An empty secret makes the endpoint
	// refuse everything, which is the correct behaviour: an unverified webhook
	// endpoint is a public URL that changes billing state.
	WebhookSecret string

	// BaseURL builds the success, cancel and return URLs.
	BaseURL string

	// Now is injectable so the tests can drive the signature window and the
	// lifecycle clock together.
	Now func() time.Time
}

// now returns the service's clock.
func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Enabled reports whether billing is configured at all. A self-hosted install
// has no payment provider, and every screen and endpoint here has to say so
// plainly rather than failing.
func (s *Service) Enabled() bool {
	return s != nil && s.Stripe.Configured() && s.Plans.Monthly != "" && s.Plans.Yearly != ""
}

// Reconcile brings one account into line with the payment provider's current
// state, and is the only function in this package that changes anything.
//
// It is deliberately a function of (account, provider state) and of nothing
// else — not of the event that triggered it, not of the local mirror, and not
// of the order deliveries arrived in. That is what makes duplicate and
// out-of-order webhooks harmless: running it twice produces the same answer,
// and running it on a stale event produces the answer for the world as it is
// now rather than as it was when the event was created.
func (s *Service) Reconcile(ctx context.Context, teamID int64, customerID string) error {
	if teamID < 1 {
		return fmt.Errorf("billing: cannot reconcile without an account id")
	}

	if customerID == "" {
		return fmt.Errorf("billing: cannot reconcile account %d without a customer id", teamID)
	}

	subscription, err := s.Stripe.ActiveSubscription(ctx, customerID)
	if err != nil {
		return err
	}

	email := ""
	if customer, err := s.Stripe.GetCustomer(ctx, customerID); err == nil && !customer.Deleted {
		email = customer.Email
	}

	mirror := Subscription{
		TeamID:       teamID,
		CustomerID:   customerID,
		Status:       "none",
		BillingEmail: email,
	}

	if subscription != nil {
		plan := stripe.Describe(subscription.PriceID(), s.Plans.Monthly, s.Plans.Yearly)

		mirror.SubscriptionID = subscription.ID
		mirror.Status = subscription.Status
		mirror.Plan = plan.Key
		mirror.PriceID = subscription.PriceID()
		mirror.CurrentPeriodEnd = subscription.PeriodEnd()
		mirror.CancelAtPeriodEnd = subscription.CancelAtPeriodEnd

		// A paused subscription can still report `active`, which is the exact
		// shape of the race that has bitten this product category: two
		// contradictory update events arriving together, one of which says the
		// customer is fine. Reading the pause block rather than the status is
		// what makes the answer stable whichever one we look at.
		if subscription.Paused() {
			mirror.Status = stripe.StatusPaused
		}
	}

	if err := s.Store.Save(ctx, mirror); err != nil {
		return err
	}

	// One question, asked of the provider's current state, decides everything:
	// is this account paying right now? Paying resets the machine to Active and
	// cancels every pending email. Not paying starts the clock — and because
	// the machine ignores a start signal for a clock already running, day 0
	// stays at the first failure however many times this runs.
	signal := lifecycle.SignalPaymentFailed
	if subscription.Paying() {
		signal = lifecycle.SignalPaymentSucceeded
	}

	transition, err := s.Lifecycle.Signal(ctx, teamID, signal)
	if err != nil {
		return err
	}

	if s.Log != nil {
		s.Log.Info("billing reconciled",
			"team", teamID, "customer", customerID,
			"status", mirror.Status, "plan", mirror.Plan,
			"phase", string(transition.To), "changed", transition.Changed)
	}

	return nil
}

// StartTrial enrols a brand-new account. The trial takes no card, so there is
// no customer at the payment provider and nothing to ask it about — the whole
// trial lives in control.db, which is why this does not touch the provider at
// all.
func (s *Service) StartTrial(ctx context.Context, teamID int64) (lifecycle.Transition, error) {
	return s.Lifecycle.Signal(ctx, teamID, lifecycle.SignalTrialStarted)
}

// Checkout creates a hosted checkout session for one account and plan.
//
// The idempotency key is built from the account, the plan and the minute, so a
// double-click or a retried request reuses the existing session rather than
// creating a second one. It deliberately does not include a random value: a
// fresh key on every attempt is the same as having none.
func (s *Service) Checkout(ctx context.Context, teamID int64, planKey, email string) (*stripe.CheckoutSession, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("billing: no payment provider is configured on this install")
	}

	priceID := s.Plans.PriceFor(planKey)
	if priceID == "" {
		return nil, fmt.Errorf("billing: %q is not a plan", planKey)
	}

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return nil, err
	}

	if existing.BillingEmail != "" && email == "" {
		email = existing.BillingEmail
	}

	return s.Stripe.CreateCheckoutSession(ctx, stripe.CheckoutParams{
		TeamID:         teamID,
		PriceID:        priceID,
		CustomerID:     existing.CustomerID,
		Email:          email,
		SuccessURL:     s.BaseURL + "/billing/done?session={CHECKOUT_SESSION_ID}",
		CancelURL:      s.BaseURL + "/pricing",
		IdempotencyKey: fmt.Sprintf("checkout-%d-%s-%s", teamID, planKey, s.now().Format("2006-01-02T15:04")),
	})
}

// Portal creates a Customer Portal link for an account that has one. Card
// updates, plan switches, invoices and cancellation all live there: the
// provider already handles SCA, 3D Secure and every regional payment method,
// and rebuilding any of that here would handle none of it.
func (s *Service) Portal(ctx context.Context, teamID int64) (*stripe.PortalSession, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("billing: no payment provider is configured on this install")
	}

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return nil, err
	}

	if existing.CustomerID == "" {
		return nil, fmt.Errorf("billing: account %d has no customer record — it has never been to checkout", teamID)
	}

	return s.Stripe.CreatePortalSession(ctx, existing.CustomerID, s.BaseURL+"/billing")
}

// DeleteCustomer removes an account's record at the payment provider. It is
// exposed here rather than reached through the client directly so that the
// lifecycle package can hold a small interface instead of the whole client.
func (s *Service) DeleteCustomer(ctx context.Context, customerID string) error {
	if !s.Stripe.Configured() || customerID == "" {
		return nil
	}

	return s.Stripe.DeleteCustomer(ctx, customerID)
}
