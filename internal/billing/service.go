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

// PaymentState is durable evidence about the latest payment attempt for a
// subscription. Stripe can report the subscription itself as active before an
// asynchronous checkout payment settles or while invoice finalization is
// broken, so subscription status cannot answer this.
const (
	PaymentPending = "pending"
	PaymentPaid    = "paid"
	PaymentFailed  = "failed"
)

// PaymentUpdate attaches ordered Checkout Session or invoice evidence to the
// subscription it describes. An empty state means the webhook only asks for
// reconciliation and cannot itself establish payment.
type PaymentUpdate struct {
	State          string
	SubscriptionID string
	SourceID       string
	SourceCreated  int64
	EventCreated   int64
	Trigger        string

	// RequireSubscriptionMatch prevents an invoice or Checkout Session from
	// changing an unrelated subscription owned by the same customer.
	RequireSubscriptionMatch bool
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
// state, and is the only function in this package that changes anything. The
// boolean reports whether the triggering object matched the current account
// subscription and was therefore reconciled.
//
// Current provider state remains authoritative for the subscription, while a
// successful or failed payment event is durable evidence about whether that
// otherwise-active subscription may grant access. This distinction is required
// for asynchronous methods, where subscription status can get ahead of money.
func (s *Service) Reconcile(ctx context.Context, teamID int64, customerID string, update PaymentUpdate) (bool, error) {
	if teamID < 1 {
		return false, fmt.Errorf("billing: cannot reconcile without an account id")
	}

	if customerID == "" {
		return false, fmt.Errorf("billing: cannot reconcile account %d without a customer id", teamID)
	}

	unlock := s.Store.LockAccount(teamID)
	defer unlock()
	trigger := update.Trigger

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return false, err
	}

	subscription, err := s.Stripe.ActiveSubscription(ctx, customerID)
	if err != nil {
		return false, err
	}

	if update.RequireSubscriptionMatch && (update.SubscriptionID == "" || subscription == nil || update.SubscriptionID != subscription.ID) {
		return false, nil
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
		PaymentState: existing.PaymentState,
	}

	if subscription != nil {
		plan := stripe.Describe(subscription.PriceID(), s.Plans.Monthly, s.Plans.Yearly)
		newSubscription := existing.SubscriptionID != subscription.ID

		mirror.SubscriptionID = subscription.ID
		mirror.Status = subscription.Status
		mirror.Plan = plan.Key
		mirror.PriceID = subscription.PriceID()
		mirror.CurrentPeriodEnd = subscription.PeriodEnd()
		mirror.CancelAtPeriodEnd = subscription.CancelAtPeriodEnd

		// A never-before-seen subscription is not paid merely because Stripe calls
		// it active. The checkout or invoice success event supplies that evidence.
		if newSubscription {
			mirror.PaymentState = PaymentPending
		} else if mirror.PaymentState == "" && (existing.Status == stripe.StatusActive || existing.Status == stripe.StatusTrialing) {
			// Existing subscriptions predate the payment_state column and were
			// already gated from a paid provider status before this migration.
			mirror.PaymentState = PaymentPaid
		}

		if update.State != "" || newSubscription || mirror.PaymentState == PaymentPending {
			update, err = s.latestPaymentUpdate(ctx, teamID, subscription.ID, update)
			if err != nil {
				return false, err
			}
		}

		updateApplies := paymentUpdateApplies(update, subscription)
		terminalPayment := !newSubscription &&
			(existing.PaymentState == PaymentPaid || existing.PaymentState == PaymentFailed)
		if updateApplies && !(update.State == PaymentPending && terminalPayment) {
			mirror.PaymentState = update.State
		}

		// A paused subscription can still report `active`, which is the exact
		// shape of the race that has bitten this product category: two
		// contradictory update events arriving together, one of which says the
		// customer is fine. Reading the pause block rather than the status is
		// what makes the answer stable whichever one we look at.
		if subscription.Paused() {
			mirror.Status = stripe.StatusPaused
		}

		pendingCheckout := update.State == PaymentPending && updateApplies
		confirmedPayment := update.State == PaymentPaid && updateApplies
		settling := subscription.Status == stripe.StatusIncomplete && !subscription.Paused()
		if !subscription.Paying() && !pendingCheckout && !confirmedPayment && !settling {
			mirror.PaymentState = PaymentFailed
		}
	} else if update.State == PaymentPending {
		mirror.PaymentState = PaymentPending
	} else {
		mirror.PaymentState = PaymentFailed
	}

	if err := s.Store.Save(ctx, mirror); err != nil {
		return false, err
	}

	// Pending means exactly that: preserve the trial or lapse clock as-is until
	// Stripe supplies a final async event. Paid resets the clock only while the
	// subscription is also currently healthy. Failed starts the clock even when
	// Stripe has left an async subscription's own status as active.
	var signal lifecycle.Signal
	switch {
	case subscription != nil && subscription.Paying() && mirror.PaymentState == PaymentPaid && trigger != stripe.EventSubscriptionResumed:
		signal = lifecycle.SignalPaymentSucceeded
	case mirror.PaymentState == PaymentFailed:
		signal = lifecycle.SignalPaymentFailed
	case subscription != nil && !subscription.Paying() && mirror.PaymentState != PaymentPending:
		signal = lifecycle.SignalPaymentFailed
	}

	if signal == "" {
		if s.Log != nil {
			s.Log.Info("billing reconciled without changing access",
				"team", teamID, "customer", customerID,
				"status", mirror.Status, "payment_state", mirror.PaymentState)
		}

		return true, nil
	}

	transition, err := s.Lifecycle.Signal(ctx, teamID, signal)
	if err != nil {
		return false, err
	}

	if s.Log != nil {
		s.Log.Info("billing reconciled",
			"team", teamID, "customer", customerID,
			"status", mirror.Status, "payment_state", mirror.PaymentState, "plan", mirror.Plan,
			"phase", string(transition.To), "changed", transition.Changed)
	}

	return true, nil
}

// paymentUpdateApplies rejects evidence for a different subscription. A customer
// can have an old paid subscription and a second failed checkout, and the failed
// attempt must not lapse the subscription that is actually paying.
func paymentUpdateApplies(update PaymentUpdate, subscription *stripe.Subscription) bool {
	if update.State == "" {
		return false
	}
	if update.SubscriptionID != "" && update.SubscriptionID != subscription.ID {
		return false
	}

	return true
}

// latestPaymentUpdate resolves the signed evidence stored for one subscription.
// Provider object creation time orders separate Checkout Sessions or invoices;
// event creation time orders changes to the same object; and terminal settlement
// semantics break exact timestamp ties without relying on delivery order.
func (s *Service) latestPaymentUpdate(ctx context.Context, teamID int64, subscriptionID string, current PaymentUpdate) (PaymentUpdate, error) {
	payloads, err := s.Store.EventPayloads(ctx, teamID, paymentEvidenceEventTypes())
	if err != nil {
		return PaymentUpdate{}, err
	}

	best := PaymentUpdate{}
	if current.SubscriptionID == subscriptionID && current.State != "" {
		best = current
	}

	for _, payload := range payloads {
		event, err := stripe.DecodeEvent(payload)
		if err != nil {
			return PaymentUpdate{}, fmt.Errorf("billing: decode stored payment event: %w", err)
		}

		candidate, err := paymentUpdate(event)
		if err != nil {
			return PaymentUpdate{}, fmt.Errorf("billing: decode stored payment evidence %s: %w", event.ID, err)
		}
		if candidate.SubscriptionID != subscriptionID || candidate.State == "" {
			continue
		}

		if paymentUpdateAfter(candidate, best) {
			best = candidate
		}
	}

	return best, nil
}

// paymentUpdateAfter compares payment evidence independently of delivery order.
// Pending is non-terminal and can never replace terminal evidence. Between paid
// and failed, the newer provider object and then newer event wins; settlement
// wins only when both Stripe timestamps are exactly tied.
func paymentUpdateAfter(candidate, current PaymentUpdate) bool {
	if current.State == "" {
		return true
	}

	candidateTerminal := candidate.State == PaymentPaid || candidate.State == PaymentFailed
	currentTerminal := current.State == PaymentPaid || current.State == PaymentFailed
	if candidateTerminal != currentTerminal {
		return candidateTerminal
	}

	candidateSource := candidate.SourceCreated
	if candidateSource == 0 {
		candidateSource = candidate.EventCreated
	}
	currentSource := current.SourceCreated
	if currentSource == 0 {
		currentSource = current.EventCreated
	}

	if candidateSource != currentSource {
		return candidateSource > currentSource
	}
	if candidate.EventCreated != current.EventCreated {
		return candidate.EventCreated > current.EventCreated
	}

	return paymentEvidenceRank(candidate.State) > paymentEvidenceRank(current.State)
}

// paymentEvidenceRank supplies the monotonic tie-break for one logical payment
// object: settled outranks failed, and either terminal result outranks pending.
func paymentEvidenceRank(state string) int {
	switch state {
	case PaymentPaid:
		return 3
	case PaymentFailed:
		return 2
	case PaymentPending:
		return 1
	default:
		return 0
	}
}

// CheckoutPaymentStatus reads the return session only to choose honest copy.
// Account activation still belongs exclusively to the signed webhook path.
func (s *Service) CheckoutPaymentStatus(ctx context.Context, sessionID string) (string, error) {
	if s == nil || s.Stripe == nil || !s.Stripe.Configured() {
		return "", fmt.Errorf("billing: no payment provider is configured on this install")
	}
	if sessionID == "" {
		return "", fmt.Errorf("billing: checkout return has no session id")
	}

	session, err := s.Stripe.GetCheckoutSession(ctx, sessionID)
	if err != nil {
		return "", err
	}

	return session.PaymentStatus, nil
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
