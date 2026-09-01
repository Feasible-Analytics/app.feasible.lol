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
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
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
	return s != nil && s.Stripe != nil && s.Stripe.Configured() && s.Plans.Product != "" &&
		s.Plans.Monthly != "" && s.Plans.Yearly != "" && s.WebhookSecret != ""
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

	lease, err := s.Store.AcquireAccountLease(ctx, teamID)
	if err != nil {
		return false, err
	}
	defer lease.Release()

	return s.reconcileLocked(ctx, lease, teamID, customerID, update)
}

// reconcileLocked applies provider truth while the caller owns the durable
// account lease. The deletion path uses this after quiescing Stripe so it can
// recover a just-paid account without recursively acquiring the same lease.
func (s *Service) reconcileLocked(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerID string, update PaymentUpdate) (bool, error) {
	if err := lease.Renew(ctx); err != nil {
		return false, err
	}
	deleting, err := s.Store.DeletionStarted(ctx, teamID)
	if err != nil {
		return false, err
	}
	if deleting {
		return false, nil
	}

	trigger := update.Trigger
	triggerEventCreated := update.EventCreated
	triggerUpdate := update

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return false, err
	}

	subscriptions, err := s.Stripe.Subscriptions(ctx, customerID)
	if err != nil {
		return false, err
	}
	subscription := stripe.SelectSubscription(subscriptions)

	if update.RequireSubscriptionMatch && (update.SubscriptionID == "" || subscriptionByID(subscriptions, update.SubscriptionID) == nil) {
		return false, nil
	}

	email := ""
	if customer, err := s.Stripe.GetCustomer(ctx, customerID); err == nil && !customer.Deleted {
		email = customer.Email
	}

	mirror := Subscription{
		TeamID:            teamID,
		CustomerID:        customerID,
		Status:            "none",
		BillingEmail:      email,
		PaymentState:      existing.PaymentState,
		PaymentFailedAt:   existing.PaymentFailedAt,
		EvidenceSourceAt:  existing.EvidenceSourceAt,
		EvidenceEventAt:   existing.EvidenceEventAt,
		EvidenceRank:      existing.EvidenceRank,
		ReconciledEventAt: max(existing.ReconciledEventAt, triggerEventCreated),
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
			mirror.EvidenceSourceAt = update.SourceCreated
			mirror.EvidenceEventAt = update.EventCreated
			mirror.EvidenceRank = paymentEvidenceRank(update.State)
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

	// The first failed Stripe event is day zero for the current lapse. A delayed
	// older delivery may correct this backward, while payment clears it so a
	// later lapse starts a new clock.
	switch mirror.PaymentState {
	case PaymentPaid:
		mirror.PaymentFailedAt = time.Time{}
	case PaymentFailed:
		failedAt, err := s.firstFailureInCurrentLapse(ctx, teamID, mirror.SubscriptionID, triggerUpdate)
		if err != nil {
			return false, err
		}
		if failedAt.IsZero() {
			failedAt = paymentFailureTime(triggerUpdate)
		}
		if mirror.PaymentFailedAt.IsZero() || (!failedAt.IsZero() && failedAt.Before(mirror.PaymentFailedAt)) {
			mirror.PaymentFailedAt = failedAt
		}
	}
	if err := lease.Renew(ctx); err != nil {
		return false, err
	}

	saved, err := s.Store.SaveReconciled(ctx, mirror)
	if err != nil {
		return false, err
	}
	if !saved {
		return false, nil
	}

	if trigger == stripe.EventCheckoutCompleted || trigger == stripe.EventCheckoutAsyncPaymentSucceeded ||
		trigger == stripe.EventCheckoutAsyncPaymentFailed {
		if err := s.Store.MarkCheckoutStatus(ctx, teamID, "complete"); err != nil {
			return false, err
		}
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

	signalAt := s.now()
	if signal == lifecycle.SignalPaymentFailed && !mirror.PaymentFailedAt.IsZero() {
		signalAt = mirror.PaymentFailedAt
	}

	transition, err := s.Lifecycle.SignalAt(ctx, teamID, signal, signalAt)
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

// paymentFailureTime turns Stripe's immutable event creation timestamp into
// the lifecycle clock. Local processing time is only a fallback for provider
// state changes that carry no event timestamp.
func paymentFailureTime(update PaymentUpdate) time.Time {
	if update.EventCreated > 0 {
		return time.Unix(update.EventCreated, 0).UTC()
	}
	if update.SourceCreated > 0 {
		return time.Unix(update.SourceCreated, 0).UTC()
	}

	return time.Time{}
}

// subscriptionByID returns the exact provider subscription named by invoice or
// checkout evidence. Evidence for historical subscription A must never change
// the mirror selected for current subscription B.
func subscriptionByID(subscriptions []stripe.Subscription, id string) *stripe.Subscription {
	for i := range subscriptions {
		if subscriptions[i].ID == id {
			return &subscriptions[i]
		}
	}

	return nil
}

// firstFailureInCurrentLapse orders all durable settlement evidence and returns
// the first failure after the most recent successful payment. Reconstructing
// the run makes an older first failure delivered late correct day zero without
// reaching backward across an intervening recovery.
func (s *Service) firstFailureInCurrentLapse(ctx context.Context, teamID int64, subscriptionID string, current PaymentUpdate) (time.Time, error) {
	payloads, err := s.Store.EventPayloads(ctx, teamID, paymentEvidenceEventTypes())
	if err != nil {
		return time.Time{}, err
	}

	updates := make([]PaymentUpdate, 0, len(payloads)+1)
	if current.SubscriptionID == subscriptionID &&
		(current.State == PaymentPaid || current.State == PaymentFailed) {
		updates = append(updates, current)
	}

	for _, payload := range payloads {
		event, err := stripe.DecodeEvent(payload)
		if err != nil {
			continue
		}

		candidate, err := paymentUpdate(event)
		if err != nil || candidate.SubscriptionID != subscriptionID ||
			(candidate.State != PaymentPaid && candidate.State != PaymentFailed) {
			continue
		}
		updates = append(updates, candidate)
	}

	sort.SliceStable(updates, func(i, j int) bool {
		return paymentUpdateAfter(updates[j], updates[i])
	})

	failedAt := time.Time{}
	for _, update := range updates {
		switch update.State {
		case PaymentPaid:
			failedAt = time.Time{}
		case PaymentFailed:
			candidate := paymentFailureTime(update)
			if failedAt.IsZero() || (!candidate.IsZero() && candidate.Before(failedAt)) {
				failedAt = candidate
			}
		}
	}

	return failedAt, nil
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
			continue
		}

		candidate, err := paymentUpdate(event)
		if err != nil {
			continue
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

// AcquireAccountLease exposes the exact durable lease used by checkout and
// reconciliation to the lifecycle purger without making lifecycle depend on
// billing's concrete Store type.
func (s *Service) AcquireAccountLease(ctx context.Context, teamID int64) (lifecycle.AccountLease, error) {
	return s.Store.AcquireAccountLease(ctx, teamID)
}

// QuiesceForDeletion closes every payment opportunity before the local deletion
// claim. Reversible mutations are persisted before Stripe is called; open
// invoices are voided because disabling automatic retries still leaves manual
// portal payment possible. Settlement is read both before and after mutation.
func (s *Service) QuiesceForDeletion(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerID string, lapseStarted time.Time, recoverSettlement bool) (lifecycle.PaymentQuiescence, error) {
	customers := make(map[string]bool)
	if customerID != "" {
		customers[customerID] = true
	}
	if !s.Stripe.Configured() {
		if customerID != "" {
			return lifecycle.PaymentQuiescence{}, fmt.Errorf(
				"billing: account %d has payment customer %s but Stripe credentials are unavailable",
				teamID, customerID,
			)
		}
		return lifecycle.PaymentQuiescence{CustomerIDs: nil, Restore: func(context.Context) error { return nil }}, nil
	}
	restore := func(restoreCtx context.Context) error {
		return s.restoreQuiescence(restoreCtx, lease, teamID, sortedCustomerIDs(customers))
	}
	fail := func(err error) (lifecycle.PaymentQuiescence, error) {
		_ = restore(context.WithoutCancel(ctx))
		return lifecycle.PaymentQuiescence{}, err
	}

	if err := lease.Renew(ctx); err != nil {
		return lifecycle.PaymentQuiescence{}, err
	}
	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return fail(err)
	}
	if recoverSettlement && existing.PaymentState == PaymentPaid {
		paidAt := s.now()
		if existing.EvidenceEventAt > 0 {
			paidAt = time.Unix(existing.EvidenceEventAt, 0).UTC()
		}
		if err := restore(ctx); err != nil {
			return lifecycle.PaymentQuiescence{}, err
		}
		if s.Lifecycle == nil {
			return lifecycle.PaymentQuiescence{}, fmt.Errorf("billing: cannot recover account %d without lifecycle service", teamID)
		}
		if _, err := s.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentSucceeded, paidAt); err != nil {
			return lifecycle.PaymentQuiescence{}, err
		}
		return lifecycle.PaymentQuiescence{Recovered: true, CustomerIDs: sortedCustomerIDs(customers)}, nil
	}

	stable := false
	for attempt := 0; attempt < 3; attempt++ {
		beforeCustomers := len(customers)
		providerCustomers, err := s.Stripe.Customers(ctx)
		if err != nil {
			return fail(fmt.Errorf("billing: discover payment customers for account %d: %w", teamID, err))
		}
		for i := range providerCustomers {
			if !providerCustomers[i].Deleted && providerCustomers[i].Meta.TeamID() == teamID {
				customers[providerCustomers[i].ID] = true
			}
		}
		changed, err := s.cleanupCheckoutSessions(ctx, lease, teamID, false, "", customers)
		if err != nil {
			return fail(err)
		}
		changed = changed || len(customers) != beforeCustomers

		for _, currentCustomerID := range sortedCustomerIDs(customers) {
			if err := lease.Renew(ctx); err != nil {
				return lifecycle.PaymentQuiescence{}, err
			}
			subscriptions, err := s.Stripe.Subscriptions(ctx, currentCustomerID)
			if err != nil {
				return fail(err)
			}
			invoices, err := s.Stripe.Invoices(ctx, currentCustomerID)
			if err != nil {
				return fail(err)
			}
			if recoverSettlement {
				if recovered, err := s.recoverSettlement(ctx, lease, teamID, currentCustomerID, sortedCustomerIDs(customers), lapseStarted,
					s.now(), subscriptions, invoices); err != nil || recovered {
					return lifecycle.PaymentQuiescence{Recovered: recovered, CustomerIDs: sortedCustomerIDs(customers)}, err
				}
			}

			for i := range subscriptions {
				subscription := &subscriptions[i]
				if !subscription.BlocksCheckout() || subscription.Paused() {
					continue
				}
				if err := s.Store.RememberQuiescence(ctx, teamID, "subscription", subscription.ID); err != nil {
					return fail(err)
				}
				if err := lease.Renew(ctx); err != nil {
					return lifecycle.PaymentQuiescence{}, err
				}
				if _, err := s.Stripe.SetSubscriptionCollectionPaused(ctx, subscription.ID, true,
					fmt.Sprintf("deletion-quiesce-%d-%s", teamID, subscription.ID)); err != nil {
					return fail(fmt.Errorf("billing: quiesce subscription %s: %w", subscription.ID, err))
				}
				changed = true
			}

			for i := range invoices {
				invoice := &invoices[i]
				switch invoice.Status {
				case "draft":
					if err := lease.Renew(ctx); err != nil {
						return lifecycle.PaymentQuiescence{}, err
					}
					if err := s.Stripe.DeleteDraftInvoice(ctx, invoice.ID,
						fmt.Sprintf("deletion-delete-draft-%d-%s", teamID, invoice.ID)); err != nil {
						if recoverSettlement {
							current, readErr := s.Stripe.Invoices(ctx, currentCustomerID)
							if readErr == nil {
								if recovered, recoverErr := s.recoverSettlement(ctx, lease, teamID, currentCustomerID, sortedCustomerIDs(customers),
									lapseStarted, s.now(), subscriptions, current); recoverErr != nil || recovered {
									return lifecycle.PaymentQuiescence{Recovered: recovered, CustomerIDs: sortedCustomerIDs(customers)}, recoverErr
								}
							}
						}
						return fail(fmt.Errorf("billing: delete draft invoice %s before deletion: %w", invoice.ID, err))
					}
					changed = true

				case "open":
					if err := lease.Renew(ctx); err != nil {
						return lifecycle.PaymentQuiescence{}, err
					}
					if _, err := s.Stripe.VoidInvoice(ctx, invoice.ID,
						fmt.Sprintf("deletion-void-invoice-%d-%s", teamID, invoice.ID)); err != nil {
						if recoverSettlement {
							current, readErr := s.Stripe.Invoices(ctx, currentCustomerID)
							if readErr == nil {
								if recovered, recoverErr := s.recoverSettlement(ctx, lease, teamID, currentCustomerID, sortedCustomerIDs(customers),
									lapseStarted, s.now(), subscriptions, current); recoverErr != nil || recovered {
									return lifecycle.PaymentQuiescence{Recovered: recovered, CustomerIDs: sortedCustomerIDs(customers)}, recoverErr
								}
							}
						}
						return fail(fmt.Errorf("billing: void invoice %s before deletion: %w", invoice.ID, err))
					}
					changed = true
				}
			}
		}

		if !changed {
			stable = true
			break
		}
	}
	if !stable {
		return fail(fmt.Errorf("billing: account %d payment opportunities did not quiesce", teamID))
	}

	quiescedAt := s.now()
	for _, currentCustomerID := range sortedCustomerIDs(customers) {
		if err := lease.Renew(ctx); err != nil {
			return lifecycle.PaymentQuiescence{}, err
		}
		subscriptions, err := s.Stripe.Subscriptions(ctx, currentCustomerID)
		if err != nil {
			return fail(err)
		}
		for i := range subscriptions {
			if subscriptions[i].BlocksCheckout() && !subscriptions[i].Paused() {
				return fail(fmt.Errorf("billing: subscription %s appeared after provider quiescence", subscriptions[i].ID))
			}
		}
		invoices, err := s.Stripe.Invoices(ctx, currentCustomerID)
		if err != nil {
			return fail(err)
		}
		for i := range invoices {
			if invoices[i].Status == "open" || invoices[i].Status == "draft" {
				return fail(fmt.Errorf("billing: invoice %s remained payable after provider quiescence", invoices[i].ID))
			}
		}
		if recoverSettlement {
			if recovered, err := s.recoverSettlement(ctx, lease, teamID, currentCustomerID, sortedCustomerIDs(customers), lapseStarted,
				quiescedAt, subscriptions, invoices); err != nil || recovered {
				return lifecycle.PaymentQuiescence{Recovered: recovered, CustomerIDs: sortedCustomerIDs(customers)}, err
			}
		}
	}

	return lifecycle.PaymentQuiescence{CustomerIDs: sortedCustomerIDs(customers), Restore: restore}, nil
}

// sortedCustomerIDs makes multi-customer cleanup deterministic for tests,
// logs, and provider mutation order.
func sortedCustomerIDs(customers map[string]bool) []string {
	ids := make([]string, 0, len(customers))
	for customerID := range customers {
		if customerID != "" {
			ids = append(ids, customerID)
		}
	}
	sort.Strings(ids)

	return ids
}

// restoreQuiescence restores every reversible provider object recorded before a
// process crash, then clears the durable list. Terminal invoices are skipped.
func (s *Service) restoreQuiescence(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerIDs []string) error {
	objects, err := s.Store.QuiescenceObjects(ctx, teamID)
	if err != nil || len(objects) == 0 {
		return err
	}
	if len(customerIDs) == 0 {
		return fmt.Errorf("billing: account %d has provider quiescence without a customer", teamID)
	}

	subscriptionByID := make(map[string]*stripe.Subscription)
	invoiceByID := make(map[string]*stripe.Invoice)
	for _, customerID := range customerIDs {
		subscriptions, err := s.Stripe.Subscriptions(ctx, customerID)
		if err != nil {
			return err
		}
		for i := range subscriptions {
			subscription := subscriptions[i]
			subscriptionByID[subscription.ID] = &subscription
		}
		invoices, err := s.Stripe.Invoices(ctx, customerID)
		if err != nil {
			return err
		}
		for i := range invoices {
			invoice := invoices[i]
			invoiceByID[invoice.ID] = &invoice
		}
	}

	for _, object := range objects {
		if err := lease.Renew(ctx); err != nil {
			return err
		}
		switch object.Type {
		case "subscription":
			subscription := subscriptionByID[object.ID]
			if subscription == nil || !subscription.Paused() {
				continue
			}
			if _, err := s.Stripe.SetSubscriptionCollectionPaused(ctx, object.ID, false,
				fmt.Sprintf("deletion-restore-%d-%s", teamID, object.ID)); err != nil {
				return fmt.Errorf("billing: restore subscription %s after deletion check: %w", object.ID, err)
			}
		case "invoice":
			invoice := invoiceByID[object.ID]
			if invoice == nil || (invoice.Status != "draft" && invoice.Status != "open") || invoice.AutoAdvance {
				continue
			}
			if _, err := s.Stripe.SetInvoiceAutoAdvance(ctx, object.ID, true,
				fmt.Sprintf("deletion-restore-invoice-%d-%s", teamID, object.ID)); err != nil {
				return fmt.Errorf("billing: restore invoice %s after deletion check: %w", object.ID, err)
			}
		}
	}

	return s.Store.ForgetQuiescence(ctx, teamID)
}

// recoverSettlement restores provider collection and applies paid_at evidence
// before any deletion claim. The same account lease fences mirror finalization.
func (s *Service) recoverSettlement(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerID string,
	restoreCustomerIDs []string, lapseStarted, quiescedAt time.Time, subscriptions []stripe.Subscription, invoices []stripe.Invoice) (bool, error) {
	invoice, paidAt, err := settledInvoice(subscriptions, invoices, lapseStarted, quiescedAt)
	if err != nil || invoice == nil {
		return false, err
	}
	if err := s.restoreQuiescence(ctx, lease, teamID, restoreCustomerIDs); err != nil {
		return false, err
	}
	if s.Lifecycle == nil {
		return false, fmt.Errorf("billing: cannot recover account %d without lifecycle service", teamID)
	}
	if _, err := s.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentSucceeded, paidAt); err != nil {
		return false, err
	}

	update := PaymentUpdate{
		State: PaymentPaid, SubscriptionID: invoice.SubscriptionID(), SourceID: invoice.ID,
		SourceCreated: invoice.Created, EventCreated: paidAt.Unix(),
		Trigger: stripe.EventInvoicePaymentSucceed, RequireSubscriptionMatch: true,
	}
	if _, err := s.reconcileLocked(ctx, lease, teamID, customerID, update); err != nil {
		return false, err
	}

	return true, nil
}

// settledInvoice returns the newest paid invoice in the current lapse and its
// provider settlement time. Paid invoices without that timestamp stop deletion
// because local receipt time cannot prove whether settlement beat quiescence.
func settledInvoice(subscriptions []stripe.Subscription, invoices []stripe.Invoice, lapseStarted, quiescedAt time.Time) (*stripe.Invoice, time.Time, error) {
	known := make(map[string]bool, len(subscriptions))
	for i := range subscriptions {
		known[subscriptions[i].ID] = true
	}

	var selected *stripe.Invoice
	var selectedAt time.Time
	for i := range invoices {
		invoice := &invoices[i]
		if !invoice.Paid && invoice.Status != "paid" {
			continue
		}
		if !known[invoice.SubscriptionID()] {
			continue
		}
		if invoice.Transitions.PaidAt == 0 {
			return nil, time.Time{}, fmt.Errorf("billing: paid invoice %s has no paid_at evidence", invoice.ID)
		}
		paidAt := time.Unix(invoice.Transitions.PaidAt, 0).UTC()
		if paidAt.Before(lapseStarted.UTC()) {
			continue
		}
		if paidAt.After(quiescedAt.UTC()) {
			return nil, time.Time{}, fmt.Errorf("billing: invoice %s settled after provider quiescence", invoice.ID)
		}
		if selected == nil || paidAt.After(selectedAt) || (paidAt.Equal(selectedAt) && invoice.ID > selected.ID) {
			copy := *invoice
			selected = &copy
			selectedAt = paidAt
		}
	}

	return selected, selectedAt, nil
}

// cleanupCheckoutSessions expires every open provider session carrying this
// account's metadata plus every late session already recorded locally. A
// completed orphan blocks a first customer; historical completions are harmless
// once provider subscription truth says an existing customer is fully terminal.
func (s *Service) cleanupCheckoutSessions(ctx context.Context, lease lifecycle.AccountLease, teamID int64, blockCompleted bool, verifyCustomerID string, deletionCustomers map[string]bool) (bool, error) {
	pending, err := s.Store.CheckoutCleanupSessions(ctx, teamID)
	if err != nil {
		return false, err
	}
	sessions, err := s.Stripe.CheckoutSessions(ctx)
	if err != nil {
		return false, fmt.Errorf("billing: discover open checkout sessions for %d: %w", teamID, err)
	}

	ids := make(map[string]bool, len(pending)+len(sessions))
	for _, id := range pending {
		ids[id] = true
	}
	for i := range sessions {
		if sessions[i].Metadata.TeamID() == teamID {
			ids[sessions[i].ID] = true
		}
	}

	changed := false
	for id := range ids {
		if err := lease.Renew(ctx); err != nil {
			return changed, err
		}
		if err := s.Store.RememberCheckoutCleanup(ctx, teamID, id); err != nil {
			return changed, err
		}
		session, err := s.Stripe.GetCheckoutSession(ctx, id)
		if err != nil {
			return changed, fmt.Errorf("billing: inspect checkout %s before replacement: %w", id, err)
		}
		if deletionCustomers != nil && session.Customer != "" {
			deletionCustomers[session.Customer] = true
		}
		if session.Status == "complete" || session.Subscription != "" {
			if blockCompleted {
				return changed, fmt.Errorf("billing: checkout %s completed before it could be recorded locally", id)
			}
			if deletionCustomers != nil && session.Customer == "" {
				return changed, fmt.Errorf("billing: completed checkout %s has no customer identity for permanent deletion", id)
			}
			if verifyCustomerID != "" {
				if err := s.verifyCompletedCheckoutIsTerminal(ctx, lease, session, verifyCustomerID); err != nil {
					return changed, err
				}
			}
			if err := lease.Renew(ctx); err != nil {
				return changed, err
			}
			if err := s.Store.FinishCheckoutCleanup(ctx, id); err != nil {
				return changed, err
			}
			continue
		}
		if session.Status == "open" {
			if err := lease.Renew(ctx); err != nil {
				return changed, err
			}
			if err := s.Stripe.ExpireCheckoutSession(ctx, id, "expire-replaced-"+id); err != nil {
				return changed, fmt.Errorf("billing: expire checkout %s before replacement: %w", id, err)
			}
			changed = true
		}
		if err := lease.Renew(ctx); err != nil {
			return changed, err
		}
		if err := s.Store.FinishCheckoutCleanup(ctx, id); err != nil {
			return changed, err
		}
	}

	return changed, nil
}

// verifyCompletedCheckoutIsTerminal closes the completion race between the
// initial subscription snapshot and checkout cleanup. A replacement is safe
// only when Stripe can identify the completed session's subscription and every
// current subscription for that customer is now non-chargeable.
func (s *Service) verifyCompletedCheckoutIsTerminal(ctx context.Context, lease lifecycle.AccountLease, session *stripe.CheckoutSession, fallbackCustomerID string) error {
	customerID := session.Customer
	if customerID == "" {
		customerID = fallbackCustomerID
	}
	if customerID == "" || session.Subscription == "" {
		return fmt.Errorf("billing: checkout %s completed without stable customer and subscription evidence", session.ID)
	}
	if err := lease.Renew(ctx); err != nil {
		return err
	}

	subscriptions, err := s.Stripe.Subscriptions(ctx, customerID)
	if err != nil {
		return fmt.Errorf("billing: verify completed checkout %s: %w", session.ID, err)
	}
	found := false
	for i := range subscriptions {
		subscription := &subscriptions[i]
		found = found || subscription.ID == session.Subscription
		if subscription.BlocksCheckout() {
			return fmt.Errorf("billing: checkout %s completed subscription %s in %s; use the billing portal",
				session.ID, subscription.ID, subscription.Status)
		}
	}
	if !found {
		return fmt.Errorf("billing: checkout %s completed subscription %s but provider truth is not visible yet",
			session.ID, session.Subscription)
	}

	return nil
}

// retireCheckoutClaim durably queues the provider session before fencing the
// local claim as expired. Repeated calls are idempotent, and a stale worker
// cannot touch a newer claim because both the account lease and claim token
// must still be current.
func (s *Service) retireCheckoutClaim(ctx context.Context, lease lifecycle.AccountLease, claim CheckoutClaim) error {
	if err := lease.Renew(ctx); err != nil {
		return err
	}
	if claim.SessionID != "" {
		if err := s.Store.RememberCheckoutCleanup(ctx, claim.TeamID, claim.SessionID); err != nil {
			return err
		}
	}
	if err := lease.Renew(ctx); err != nil {
		return err
	}

	return s.Store.MarkCheckoutClaimStatus(ctx, claim, "expired")
}

// Checkout creates or resumes the one hosted checkout allowed for an account.
// The database claim is committed before Stripe is called and carries a stable
// idempotency key, so a crash or a second process can only recover the same
// session rather than create another customer or subscription.
func (s *Service) Checkout(ctx context.Context, teamID int64, planKey, email string) (*stripe.CheckoutSession, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("billing: no payment provider is configured on this install")
	}

	priceID := s.Plans.PriceFor(planKey)
	if priceID == "" {
		return nil, fmt.Errorf("billing: %q is not a plan", planKey)
	}
	if priceID == s.Plans.Monthly {
		planKey = "monthly"
	} else {
		planKey = "yearly"
	}

	lease, err := s.Store.AcquireAccountLease(ctx, teamID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return nil, err
	}

	// The provider is authoritative when an account has a customer. Every status
	// that can still charge or settle must go through the portal; only a fully
	// ended subscription may start a replacement checkout.
	if existing.CustomerID != "" {
		subscriptions, err := s.Stripe.Subscriptions(ctx, existing.CustomerID)
		if err != nil {
			return nil, err
		}
		for i := range subscriptions {
			if subscriptions[i].BlocksCheckout() {
				return nil, fmt.Errorf("billing: account %d already has subscription %s in %s; manage its plan in the billing portal",
					teamID, subscriptions[i].ID, subscriptions[i].Status)
			}
		}
	}

	if existing.BillingEmail != "" && email == "" {
		email = existing.BillingEmail
	}

	claim, found, err := s.Store.CheckoutClaimForAccount(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if found && claim.SessionID != "" && claim.Status != "expired" {
		session, err := s.Stripe.GetCheckoutSession(ctx, claim.SessionID)
		if err != nil {
			return nil, err
		}

		switch {
		case session.Status == "open" && claim.Plan == planKey && claim.PriceID == priceID:
			return session, nil
		case session.Status == "open":
			if err := s.retireCheckoutClaim(ctx, lease, claim); err != nil {
				return nil, err
			}
			claim.Status = "expired"
		case session.Status == "complete" || session.Subscription != "":
			if existing.CustomerID == "" {
				if err := lease.Renew(ctx); err != nil {
					return nil, err
				}
				if err := s.Store.MarkCheckoutClaimStatus(ctx, claim, "complete"); err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("billing: account %d already completed checkout; wait for billing confirmation or use the portal", teamID)
			}
			if err := s.verifyCompletedCheckoutIsTerminal(ctx, lease, session, existing.CustomerID); err != nil {
				return nil, err
			}
			if err := s.retireCheckoutClaim(ctx, lease, claim); err != nil {
				return nil, err
			}
			claim.Status = "expired"
		default:
			if err := s.retireCheckoutClaim(ctx, lease, claim); err != nil {
				return nil, err
			}
			claim.Status = "expired"
		}
	}
	if found && claim.Status == "complete" {
		if existing.CustomerID == "" {
			return nil, fmt.Errorf("billing: account %d already completed checkout; wait for billing confirmation or use the portal", teamID)
		}
		if err := s.retireCheckoutClaim(ctx, lease, claim); err != nil {
			return nil, err
		}
		claim.Status = "expired"
	}
	if found && claim.Expired(s.now()) {
		return nil, fmt.Errorf(
			"billing: account %d has an indeterminate checkout older than the safe Stripe idempotency retry window; inspect provider session metadata before retrying",
			teamID,
		)
	}
	if found && claim.Status == "expired" {
		if _, err := s.cleanupCheckoutSessions(ctx, lease, teamID, existing.CustomerID == "", existing.CustomerID, nil); err != nil {
			return nil, err
		}
	}
	if !found || claim.Status == "expired" {
		if err := lease.Renew(ctx); err != nil {
			return nil, err
		}
		claim, err = s.Store.NewCheckoutClaim(ctx, teamID, planKey, priceID, existing.CustomerID, email)
		if err != nil {
			return nil, err
		}
	}

	for {
		if err := lease.Renew(ctx); err != nil {
			return nil, err
		}

		session, err := s.Stripe.CreateCheckoutSession(ctx, stripe.CheckoutParams{
			TeamID:     teamID,
			PriceID:    claim.PriceID,
			CustomerID: claim.CustomerID,
			Email:      claim.BillingEmail,
			SuccessURL: s.returnURL("/billing/done", url.Values{
				"session": {"{CHECKOUT_SESSION_ID}"},
				"team":    {strconv.FormatInt(teamID, 10)},
			}),
			CancelURL: s.returnURL("/pricing", url.Values{
				"plan": {claim.Plan},
				"team": {strconv.FormatInt(teamID, 10)},
			}),
			IdempotencyKey: claim.IdempotencyKey,
		})
		if err != nil {
			return nil, err
		}
		if err := lease.Renew(ctx); err != nil {
			if rememberErr := s.Store.RememberCheckoutCleanup(context.WithoutCancel(ctx), teamID, session.ID); rememberErr != nil {
				return nil, fmt.Errorf("%w; could not record late session %s: %v", err, session.ID, rememberErr)
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			if cleanupErr := s.Stripe.ExpireCheckoutSession(cleanupCtx, session.ID, "expire-replaced-"+session.ID); cleanupErr == nil {
				_ = s.Store.FinishCheckoutCleanup(cleanupCtx, session.ID)
			}
			return nil, err
		}

		if err := s.Store.SaveCheckoutSession(ctx, claim, session.ID, session.URL, "open"); err != nil {
			if !errors.Is(err, ErrCheckoutClaimReplaced) {
				return nil, err
			}

			// A replaced claim's late provider response is the one persistence
			// failure that must be neutralized. Ordinary database failures keep
			// the stable idempotency result open for the next retry to recover.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			cleanupErr := s.Stripe.ExpireCheckoutSession(cleanupCtx, session.ID, "expire-replaced-"+session.ID)
			if cleanupErr == nil {
				cleanupErr = s.Store.FinishCheckoutCleanup(cleanupCtx, session.ID)
			}
			if cleanupErr != nil {
				return nil, fmt.Errorf("%w; late session %s remains queued for cleanup: %v", err, session.ID, cleanupErr)
			}
			return nil, err
		}

		claim.SessionID = session.ID
		claim.SessionURL = session.URL
		claim.Status = "open"
		if claim.Plan == planKey && claim.PriceID == priceID {
			return session, nil
		}

		// A sessionless crash recovery must first recover the provider's
		// idempotent result, then retire it before honoring a changed plan or
		// configured price. Returning the old result would charge the wrong plan.
		if err := s.retireCheckoutClaim(ctx, lease, claim); err != nil {
			return nil, err
		}
		if _, err := s.cleanupCheckoutSessions(ctx, lease, teamID, existing.CustomerID == "", existing.CustomerID, nil); err != nil {
			return nil, err
		}
		if err := lease.Renew(ctx); err != nil {
			return nil, err
		}
		claim, err = s.Store.NewCheckoutClaim(ctx, teamID, planKey, priceID, existing.CustomerID, email)
		if err != nil {
			return nil, err
		}
	}
}

// returnURL preserves explicit account identity through provider-owned pages.
// Values are encoded as a query rather than concatenated; Stripe's checkout
// placeholder is restored after encoding because its API requires literal
// braces in the success URL.
func (s *Service) returnURL(path string, values url.Values) string {
	raw := values.Encode()
	raw = strings.ReplaceAll(raw, "%7BCHECKOUT_SESSION_ID%7D", "{CHECKOUT_SESSION_ID}")

	return strings.TrimRight(s.BaseURL, "/") + path + "?" + raw
}

// Portal creates a Customer Portal link for an account that has one. Card
// updates, plan switches, invoices and cancellation all live there: the
// provider already handles SCA, 3D Secure and every regional payment method,
// and rebuilding any of that here would handle none of it.
func (s *Service) Portal(ctx context.Context, teamID int64) (*stripe.PortalSession, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("billing: no payment provider is configured on this install")
	}
	lease, err := s.Store.AcquireAccountLease(ctx, teamID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return nil, err
	}

	if existing.CustomerID == "" {
		return nil, fmt.Errorf("billing: account %d has no customer record — it has never been to checkout", teamID)
	}
	if err := lease.Renew(ctx); err != nil {
		return nil, err
	}

	return s.Stripe.CreatePortalSession(ctx, existing.CustomerID, s.returnURL("/billing", url.Values{
		"team": {strconv.FormatInt(teamID, 10)},
	}))
}

// DeleteCustomer removes an account's record at the payment provider. It is
// exposed here rather than reached through the client directly so that the
// lifecycle package can hold a small interface instead of the whole client.
func (s *Service) DeleteCustomer(ctx context.Context, customerID string) error {
	if customerID == "" {
		return nil
	}
	if !s.Stripe.Configured() {
		return fmt.Errorf("billing: cannot delete payment customer %s without Stripe credentials", customerID)
	}

	return s.Stripe.DeleteCustomer(ctx, customerID)
}
