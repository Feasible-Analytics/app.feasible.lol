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
	return s.reconcileLockedWithRecovery(ctx, lease, teamID, customerID, update, false)
}

// reconcileLockedWithRecovery permits provider-paid truth through only for an
// unfinished scheduled deletion. Ordinary reconciliation and owner-requested
// deletion remain fenced by the immutable audit.
func (s *Service) reconcileLockedWithRecovery(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerID string, update PaymentUpdate, recoverScheduled bool) (bool, error) {
	if err := lease.Renew(ctx); err != nil {
		return false, err
	}
	deleting, err := s.Store.DeletionStarted(ctx, teamID)
	if err != nil {
		return false, err
	}
	if deleting {
		if !recoverScheduled {
			return false, nil
		}
		recoverable, err := s.Store.RecoverableScheduledDeletion(ctx, teamID)
		if err != nil {
			return false, err
		}
		if !recoverable {
			return false, nil
		}
	}

	trigger := update.Trigger
	triggerEventCreated := update.EventCreated

	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return false, err
	}

	customers, err := s.discoverAccountCustomers(ctx, lease, teamID, false, existing.CustomerID, customerID)
	if err != nil {
		return false, err
	}
	evidence, err := s.storedPaymentUpdates(ctx, teamID)
	if err != nil {
		return false, err
	}
	allSubscriptions := make([]stripe.Subscription, 0)
	subscriptionCustomers := make(map[string]string)
	providerUpdates := make(map[string]PaymentUpdate)
	for _, currentCustomerID := range sortedCustomerIDs(customers) {
		if err := lease.Renew(ctx); err != nil {
			return false, err
		}
		subscriptions, err := s.Stripe.Subscriptions(ctx, currentCustomerID)
		if err != nil {
			return false, err
		}
		invoices, err := s.Stripe.Invoices(ctx, currentCustomerID)
		if err != nil {
			return false, err
		}
		for i := range subscriptions {
			subscriptionCustomers[subscriptions[i].ID] = currentCustomerID
			allSubscriptions = append(allSubscriptions, subscriptions[i])
			providerUpdates[subscriptions[i].ID] = providerInvoiceUpdate(&subscriptions[i], invoices)
		}
	}

	if update.RequireSubscriptionMatch && (update.SubscriptionID == "" || subscriptionByID(allSubscriptions, update.SubscriptionID) == nil) {
		return false, nil
	}

	resolvedUpdates := make(map[string]PaymentUpdate, len(allSubscriptions))
	entitled := make([]stripe.Subscription, 0, len(allSubscriptions))
	for i := range allSubscriptions {
		candidate := providerUpdates[allSubscriptions[i].ID]
		if update.SubscriptionID == allSubscriptions[i].ID && paymentUpdateAfter(update, candidate) {
			candidate = update
		}
		if existing.SubscriptionID == allSubscriptions[i].ID && existing.PaymentState != "" {
			stored := PaymentUpdate{
				State: existing.PaymentState, SubscriptionID: existing.SubscriptionID,
				SourceCreated: existing.EvidenceSourceAt, EventCreated: existing.EvidenceEventAt,
			}
			if paymentUpdateAfter(stored, candidate) {
				candidate = stored
			}
		}
		candidate = latestPaymentUpdate(evidence, allSubscriptions[i].ID, candidate)
		resolvedUpdates[allSubscriptions[i].ID] = candidate
		if allSubscriptions[i].Paying() && candidate.State == PaymentPaid {
			entitled = append(entitled, allSubscriptions[i])
		}
	}

	selection := allSubscriptions
	if len(entitled) > 0 {
		selection = entitled
	}
	subscription := stripe.SelectSubscription(selection)
	selectedUpdate := PaymentUpdate{}
	selectedCustomerID := existing.CustomerID
	if subscription != nil {
		selectedCustomerID = subscriptionCustomers[subscription.ID]
		selectedUpdate = resolvedUpdates[subscription.ID]
	} else if !customers[selectedCustomerID] {
		selectedCustomerID = customerID
	}

	email := ""
	if customer, err := s.Stripe.GetCustomer(ctx, selectedCustomerID); err == nil && !customer.Deleted {
		email = customer.Email
	}

	mirror := Subscription{
		TeamID:            teamID,
		CustomerID:        selectedCustomerID,
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
		plan := s.Plans.Describe(subscription.PriceID())
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
			mirror.PaymentFailedAt = time.Time{}
			mirror.EvidenceSourceAt = 0
			mirror.EvidenceEventAt = 0
			mirror.EvidenceRank = 0
		}

		updateApplies := paymentUpdateApplies(selectedUpdate, subscription)
		terminalPayment := !newSubscription &&
			(existing.PaymentState == PaymentPaid || existing.PaymentState == PaymentFailed)
		if updateApplies && (selectedUpdate.State != PaymentPending || !terminalPayment) {
			mirror.PaymentState = selectedUpdate.State
			mirror.EvidenceSourceAt = selectedUpdate.SourceCreated
			mirror.EvidenceEventAt = selectedUpdate.EventCreated
			mirror.EvidenceRank = paymentEvidenceRank(selectedUpdate.State)
		}

		// A paused subscription can still report `active`, which is the exact
		// shape of the race that has bitten this product category: two
		// contradictory update events arriving together, one of which says the
		// customer is fine. Reading the pause block rather than the status is
		// what makes the answer stable whichever one we look at.
		if subscription.Paused() {
			mirror.Status = stripe.StatusPaused
		}

		pendingCheckout := selectedUpdate.State == PaymentPending && updateApplies
		confirmedPayment := selectedUpdate.State == PaymentPaid && updateApplies
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
		failureEvidence := resolvedUpdates[mirror.SubscriptionID]
		failedAt := firstFailureInCurrentLapse(evidence, mirror.SubscriptionID, failureEvidence)
		if failedAt.IsZero() {
			failedAt = paymentFailureTime(failureEvidence)
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
	if deleting && recoverScheduled {
		return true, nil
	}

	signalAt := s.now()
	if signal == lifecycle.SignalPaymentSucceeded && selectedUpdate.EventCreated > 0 {
		signalAt = time.Unix(selectedUpdate.EventCreated, 0).UTC()
	} else if signal == lifecycle.SignalPaymentFailed && !mirror.PaymentFailedAt.IsZero() {
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

// discoverAccountCustomers returns every provider customer known to belong to
// an account: the durable billing_account_customers rows, the ids the caller
// already holds, and the customers named by Checkout Sessions carrying this
// account's metadata. Every discovered identity is persisted before its
// subscriptions can affect entitlement, because a second customer with a paid
// period is the difference between an account being deleted at day 90 and an
// account that is still paying.
//
// Permanent deletion additionally searches customers by metadata, since a
// customer must not outlive the account it belongs to.
func (s *Service) discoverAccountCustomers(ctx context.Context, lease lifecycle.AccountLease, teamID int64, searchProvider bool, seeds ...string) (map[string]bool, error) {
	customers := make(map[string]bool)
	remember := func(customerID string) error {
		if customerID == "" || customers[customerID] {
			return nil
		}
		if err := s.Store.RememberAccountCustomer(ctx, teamID, customerID); err != nil {
			return err
		}
		customers[customerID] = true
		return nil
	}

	stored, err := s.Store.AccountCustomers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	for _, storedCustomerID := range stored {
		customers[storedCustomerID] = true
	}
	for _, seed := range seeds {
		if err := remember(seed); err != nil {
			return nil, err
		}
	}
	if searchProvider {
		if err := lease.Renew(ctx); err != nil {
			return nil, err
		}
		providerCustomers, err := s.Stripe.SearchCustomersByTeam(ctx, teamID)
		if err != nil {
			return nil, fmt.Errorf("billing: discover payment customers for account %d: %w", teamID, err)
		}
		for i := range providerCustomers {
			if !providerCustomers[i].Deleted && providerCustomers[i].Meta.TeamID() == teamID {
				if err := remember(providerCustomers[i].ID); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := lease.Renew(ctx); err != nil {
		return nil, err
	}
	sessions, err := s.Stripe.CheckoutSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("billing: discover checkout customers for account %d: %w", teamID, err)
	}
	for i := range sessions {
		if sessions[i].Metadata.TeamID() == teamID {
			if err := remember(sessions[i].Customer); err != nil {
				return nil, err
			}
		}
	}

	return customers, nil
}

// providerInvoiceUpdate derives ordered settlement evidence from Stripe's
// current authenticated invoice state. Active status alone never proves paid.
func providerInvoiceUpdate(subscription *stripe.Subscription, invoices []stripe.Invoice) PaymentUpdate {
	best := PaymentUpdate{}
	for i := range invoices {
		invoice := &invoices[i]
		if invoice.SubscriptionID() != subscription.ID {
			continue
		}
		candidate := PaymentUpdate{
			SubscriptionID: subscription.ID, SourceID: invoice.ID,
			SourceCreated: invoice.Created, RequireSubscriptionMatch: true,
		}
		switch {
		case (invoice.Paid || invoice.Status == "paid") && invoice.Transitions.PaidAt > 0:
			candidate.State = PaymentPaid
			candidate.EventCreated = invoice.Transitions.PaidAt
			candidate.Trigger = stripe.EventInvoicePaymentSucceed
		case invoice.Status == "open" && invoice.AttemptCount > 0:
			candidate.State = PaymentFailed
			candidate.EventCreated = invoice.Created
			candidate.Trigger = stripe.EventInvoicePaymentFailed
		default:
			continue
		}
		if paymentUpdateAfter(candidate, best) {
			best = candidate
		}
	}

	return best
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
func firstFailureInCurrentLapse(evidence []PaymentUpdate, subscriptionID string, current PaymentUpdate) time.Time {
	updates := make([]PaymentUpdate, 0, len(evidence)+1)
	if current.SubscriptionID == subscriptionID &&
		(current.State == PaymentPaid || current.State == PaymentFailed) {
		updates = append(updates, current)
	}

	for _, candidate := range evidence {
		if candidate.SubscriptionID != subscriptionID ||
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

	return failedAt
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

// storedPaymentUpdates decodes every signed payment event stored for an
// account once, so a reconcile that examines several subscriptions does not
// re-read and re-decode the whole log for each of them. Payloads that no
// longer decode are skipped: they were accepted under an older provider shape
// and carry no evidence this reconcile can use.
func (s *Service) storedPaymentUpdates(ctx context.Context, teamID int64) ([]PaymentUpdate, error) {
	payloads, err := s.Store.EventPayloads(ctx, teamID, paymentEvidenceEventTypes())
	if err != nil {
		return nil, err
	}

	updates := make([]PaymentUpdate, 0, len(payloads))
	for _, payload := range payloads {
		event, err := stripe.DecodeEvent(payload)
		if err != nil {
			continue
		}

		candidate, err := paymentUpdate(event)
		if err != nil || candidate.State == "" {
			continue
		}
		updates = append(updates, candidate)
	}

	return updates, nil
}

// latestPaymentUpdate resolves the signed evidence stored for one subscription.
// Provider object creation time orders separate Checkout Sessions or invoices;
// event creation time orders changes to the same object; and terminal settlement
// semantics break exact timestamp ties without relying on delivery order.
func latestPaymentUpdate(evidence []PaymentUpdate, subscriptionID string, current PaymentUpdate) PaymentUpdate {
	best := PaymentUpdate{}
	if current.SubscriptionID == subscriptionID && current.State != "" {
		best = current
	}

	for _, candidate := range evidence {
		if candidate.SubscriptionID != subscriptionID {
			continue
		}
		if paymentUpdateAfter(candidate, best) {
			best = candidate
		}
	}

	return best
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

// StartTrial enrols a brand-new account. The trial takes no card, so there is
// no customer at the payment provider and nothing to ask it about — the whole
// trial lives in system.db, which is why this does not touch the provider at
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

// QuiesceForDeletion prepares or finalizes provider state around a durable
// deletion claim. Recoverable preparation uses only reversible Stripe changes;
// irreversible finalization is requested only after lifecycle marks the claim
// authoritative. Settlement is read before and after reversible preparation.
func (s *Service) QuiesceForDeletion(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerID string, lapseStarted time.Time, recoverSettlement bool) (lifecycle.PaymentQuiescence, error) {
	if !s.Stripe.Configured() {
		if customerID != "" {
			return lifecycle.PaymentQuiescence{}, fmt.Errorf(
				"billing: account %d has payment customer %s but Stripe credentials are unavailable",
				teamID, customerID,
			)
		}
		return lifecycle.PaymentQuiescence{CustomerIDs: nil, Restore: func(context.Context) error { return nil }}, nil
	}
	customers, err := s.discoverAccountCustomers(ctx, lease, teamID, true, customerID)
	if err != nil {
		return lifecycle.PaymentQuiescence{}, err
	}
	restore := func(restoreCtx context.Context) error {
		return s.restoreQuiescence(restoreCtx, lease, teamID)
	}
	fail := func(err error) (lifecycle.PaymentQuiescence, error) {
		if recoverSettlement {
			_ = restore(context.WithoutCancel(ctx))
		}
		return lifecycle.PaymentQuiescence{}, err
	}
	existing, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return fail(err)
	}
	if recoverSettlement && existing.PaymentState == PaymentPaid && existing.SubscriptionID != "" {
		recorded, err := s.Store.QuiescenceObjects(ctx, teamID)
		if err != nil {
			return fail(err)
		}
		recordedSubscription := false
		for _, object := range recorded {
			if object.Type == "subscription" && object.ID == existing.SubscriptionID {
				recordedSubscription = true
				break
			}
		}
		for _, currentCustomerID := range sortedCustomerIDs(customers) {
			subscriptions, err := s.Stripe.Subscriptions(ctx, currentCustomerID)
			if err != nil {
				return fail(err)
			}
			current := subscriptionByID(subscriptions, existing.SubscriptionID)
			healthyStatus := current != nil && (current.Status == stripe.StatusActive || current.Status == stripe.StatusTrialing)
			if current == nil || (!current.Paying() && (!healthyStatus || !current.Paused() || !recordedSubscription)) {
				continue
			}
			invoices, err := s.Stripe.Invoices(ctx, currentCustomerID)
			if err != nil {
				return fail(err)
			}
			providerEvidence := providerInvoiceUpdate(current, invoices)
			storedEvidence := PaymentUpdate{
				State: existing.PaymentState, SubscriptionID: existing.SubscriptionID,
				SourceCreated: existing.EvidenceSourceAt, EventCreated: existing.EvidenceEventAt,
			}
			if providerEvidence.State == PaymentFailed && paymentUpdateAfter(providerEvidence, storedEvidence) {
				continue
			}
			if err := restore(ctx); err != nil {
				return lifecycle.PaymentQuiescence{}, err
			}
			if s.Lifecycle == nil {
				return lifecycle.PaymentQuiescence{}, fmt.Errorf("billing: cannot recover account %d without lifecycle service", teamID)
			}
			pendingDeletion, err := s.Store.RecoverableScheduledDeletion(ctx, teamID)
			if err != nil {
				return lifecycle.PaymentQuiescence{}, err
			}
			if !pendingDeletion {
				paidAt := s.now()
				if existing.EvidenceEventAt > 0 {
					paidAt = time.Unix(existing.EvidenceEventAt, 0).UTC()
				}
				if _, err := s.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentSucceeded, paidAt); err != nil {
					return lifecycle.PaymentQuiescence{}, err
				}
			}
			return lifecycle.PaymentQuiescence{Recovered: true, CustomerIDs: sortedCustomerIDs(customers)}, nil
		}
	}

	if err := lease.Renew(ctx); err != nil {
		return lifecycle.PaymentQuiescence{}, err
	}

	stable := false
	irreversible := false
	for attempt := 0; attempt < 3; attempt++ {
		beforeCustomers := len(customers)
		changed, err := s.cleanupCheckoutSessions(ctx, lease, teamID, false, "", customers, !recoverSettlement)
		if err != nil {
			return fail(err)
		}
		if !recoverSettlement && changed {
			irreversible = true
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
				if recovered, err := s.recoverSettlement(ctx, lease, teamID, currentCustomerID, lapseStarted,
					s.now(), subscriptions, invoices); err != nil || recovered {
					return lifecycle.PaymentQuiescence{Recovered: recovered, CustomerIDs: sortedCustomerIDs(customers)}, err
				}
			}

			for i := range subscriptions {
				subscription := &subscriptions[i]
				if !subscription.BlocksCheckout() || subscription.Paused() {
					continue
				}
				if err := s.Store.RememberQuiescence(ctx, teamID, currentCustomerID, "subscription", subscription.ID); err != nil {
					return fail(err)
				}
				if err := lease.Renew(ctx); err != nil {
					return lifecycle.PaymentQuiescence{}, err
				}
				if _, err := s.Stripe.SetSubscriptionCollectionPaused(ctx, subscription.ID, true, "keep_as_draft",
					fmt.Sprintf("deletion-quiesce-%d-%s", teamID, subscription.ID)); err != nil {
					return fail(fmt.Errorf("billing: quiesce subscription %s: %w", subscription.ID, err))
				}
				changed = true
			}

			for i := range invoices {
				invoice := &invoices[i]
				if recoverSettlement && (invoice.Status == "draft" || invoice.Status == "open") {
					if !invoice.AutoAdvance {
						continue
					}
					if err := s.Store.RememberQuiescence(ctx, teamID, currentCustomerID, "invoice", invoice.ID); err != nil {
						return fail(err)
					}
					if err := lease.Renew(ctx); err != nil {
						return lifecycle.PaymentQuiescence{}, err
					}
					if _, err := s.Stripe.SetInvoiceAutoAdvance(ctx, invoice.ID, false,
						fmt.Sprintf("deletion-quiesce-invoice-%d-%s", teamID, invoice.ID)); err != nil {
						return fail(fmt.Errorf("billing: quiesce invoice %s: %w", invoice.ID, err))
					}
					changed = true
					continue
				}
				switch invoice.Status {
				case "draft":
					if err := lease.Renew(ctx); err != nil {
						return lifecycle.PaymentQuiescence{}, err
					}
					if err := s.Stripe.DeleteDraftInvoice(ctx, invoice.ID,
						fmt.Sprintf("deletion-delete-draft-%d-%s", teamID, invoice.ID)); err != nil {
						return fail(fmt.Errorf("billing: delete draft invoice %s before deletion: %w", invoice.ID, err))
					}
					irreversible = true
					changed = true

				case "open":
					if err := lease.Renew(ctx); err != nil {
						return lifecycle.PaymentQuiescence{}, err
					}
					if _, err := s.Stripe.VoidInvoice(ctx, invoice.ID,
						fmt.Sprintf("deletion-void-invoice-%d-%s", teamID, invoice.ID)); err != nil {
						if !recoverSettlement && !irreversible {
							current, readErr := s.Stripe.Invoices(ctx, currentCustomerID)
							if readErr == nil {
								if paid, _, settleErr := settledInvoice(subscriptions, current, lapseStarted, s.now()); settleErr == nil && paid != nil {
									if reopenErr := s.Store.ReopenScheduledDeletionForRecovery(ctx, teamID); reopenErr != nil {
										return lifecycle.PaymentQuiescence{}, reopenErr
									}
									if recovered, recoverErr := s.recoverSettlement(ctx, lease, teamID, currentCustomerID,
										lapseStarted, s.now(), subscriptions, current); recoverErr != nil || recovered {
										return lifecycle.PaymentQuiescence{Recovered: recovered, CustomerIDs: sortedCustomerIDs(customers)}, recoverErr
									}
								}
							}
						}
						return fail(fmt.Errorf("billing: void invoice %s before deletion: %w", invoice.ID, err))
					}
					irreversible = true
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
			if (invoices[i].Status == "open" || invoices[i].Status == "draft") &&
				(!recoverSettlement || invoices[i].AutoAdvance) {
				return fail(fmt.Errorf("billing: invoice %s remained payable after provider quiescence", invoices[i].ID))
			}
		}
		if recoverSettlement {
			if recovered, err := s.recoverSettlement(ctx, lease, teamID, currentCustomerID, lapseStarted,
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

// restoreQuiescence restores each durable object under its recorded customer.
// Rows are removed individually only after successful or idempotent recovery,
// so a partial provider failure leaves the unfinished suffix retryable.
func (s *Service) restoreQuiescence(ctx context.Context, lease lifecycle.AccountLease, teamID int64) error {
	objects, err := s.Store.QuiescenceObjects(ctx, teamID)
	if err != nil || len(objects) == 0 {
		return err
	}

	for _, object := range objects {
		if err := lease.Renew(ctx); err != nil {
			return err
		}
		switch object.Type {
		case "subscription":
			subscriptions, err := s.Stripe.Subscriptions(ctx, object.CustomerID)
			if err != nil {
				return fmt.Errorf("billing: inspect subscription %s under customer %s for recovery: %w", object.ID, object.CustomerID, err)
			}
			subscription := subscriptionByID(subscriptions, object.ID)
			if subscription == nil {
				return fmt.Errorf("billing: subscription %s was not found under recorded customer %s", object.ID, object.CustomerID)
			}
			if subscription.Paused() {
				if _, err := s.Stripe.SetSubscriptionCollectionPaused(ctx, object.ID, false, "",
					fmt.Sprintf("deletion-restore-%d-%s", teamID, object.ID)); err != nil {
					return fmt.Errorf("billing: restore subscription %s after deletion check: %w", object.ID, err)
				}
			}
		case "invoice":
			invoices, err := s.Stripe.Invoices(ctx, object.CustomerID)
			if err != nil {
				return fmt.Errorf("billing: inspect invoice %s under customer %s for recovery: %w", object.ID, object.CustomerID, err)
			}
			var invoice *stripe.Invoice
			for i := range invoices {
				if invoices[i].ID == object.ID {
					invoice = &invoices[i]
					break
				}
			}
			if invoice == nil {
				return fmt.Errorf("billing: invoice %s was not found under recorded customer %s", object.ID, object.CustomerID)
			}
			if (invoice.Status == "draft" || invoice.Status == "open") && !invoice.AutoAdvance {
				if _, err := s.Stripe.SetInvoiceAutoAdvance(ctx, object.ID, true,
					fmt.Sprintf("deletion-restore-invoice-%d-%s", teamID, object.ID)); err != nil {
					return fmt.Errorf("billing: restore invoice %s after deletion check: %w", object.ID, err)
				}
			}
		default:
			return fmt.Errorf("billing: unknown quiescence object type %q", object.Type)
		}
		if err := s.Store.ForgetQuiescenceObject(ctx, teamID, object); err != nil {
			return err
		}
	}

	return nil
}

// recoverSettlement restores provider collection and applies paid_at evidence
// before any deletion claim. The same account lease fences mirror finalization.
func (s *Service) recoverSettlement(ctx context.Context, lease lifecycle.AccountLease, teamID int64, customerID string,
	lapseStarted, quiescedAt time.Time, subscriptions []stripe.Subscription, invoices []stripe.Invoice) (bool, error) {
	invoice, paidAt, err := settledInvoice(subscriptions, invoices, lapseStarted, quiescedAt)
	if err != nil || invoice == nil {
		return false, err
	}
	if err := s.restoreQuiescence(ctx, lease, teamID); err != nil {
		return false, err
	}
	if s.Lifecycle == nil {
		return false, fmt.Errorf("billing: cannot recover account %d without lifecycle service", teamID)
	}

	update := PaymentUpdate{
		State: PaymentPaid, SubscriptionID: invoice.SubscriptionID(), SourceID: invoice.ID,
		SourceCreated: invoice.Created, EventCreated: paidAt.Unix(),
		Trigger: stripe.EventInvoicePaymentSucceed, RequireSubscriptionMatch: true,
	}
	if _, err := s.reconcileLockedWithRecovery(ctx, lease, teamID, customerID, update, true); err != nil {
		return false, err
	}
	mirror, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return false, err
	}
	if mirror.PaymentState != PaymentPaid {
		return false, fmt.Errorf("billing: recovered account %d payment mirror remained %q", teamID, mirror.PaymentState)
	}

	return true, nil
}

// settledInvoice returns the newest authoritative paid invoice and its provider
// settlement time. Evidence from before a false lapse remains eligible only
// while Stripe still reports that subscription healthy and its paid period is
// unexpired. A missing paid_at stops deletion rather than inventing ordering.
func settledInvoice(subscriptions []stripe.Subscription, invoices []stripe.Invoice, lapseStarted, quiescedAt time.Time) (*stripe.Invoice, time.Time, error) {
	known := make(map[string]*stripe.Subscription, len(subscriptions))
	for i := range subscriptions {
		known[subscriptions[i].ID] = &subscriptions[i]
	}

	var selected *stripe.Invoice
	var selectedAt time.Time
	for i := range invoices {
		invoice := &invoices[i]
		if !invoice.Paid && invoice.Status != "paid" {
			continue
		}
		subscription := known[invoice.SubscriptionID()]
		if subscription == nil {
			continue
		}
		if invoice.Transitions.PaidAt == 0 {
			return nil, time.Time{}, fmt.Errorf("billing: paid invoice %s has no paid_at evidence", invoice.ID)
		}
		paidAt := time.Unix(invoice.Transitions.PaidAt, 0).UTC()
		if paidAt.Before(lapseStarted.UTC()) {
			// A paid annual or monthly period can legitimately begin before a
			// stale failure on another customer. Current healthy provider state
			// plus an unexpired paid period remains valid account entitlement.
			if !subscription.Paying() || !subscription.PeriodEnd().After(quiescedAt.UTC()) {
				continue
			}
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

// cleanupCheckoutSessions expires every open provider session this account
// could still complete: the late sessions recorded locally, every session
// carrying this account's metadata, and every session Stripe holds for the
// customers named by the caller. A completed orphan blocks a first customer;
// historical completions are harmless once provider subscription truth says an
// existing customer is fully terminal.
//
// The account-wide listing is what covers an account's first checkout, whose
// session has no customer to list it by until it completes. Without it a
// replacement checkout could be created beside a live orphan.
func (s *Service) cleanupCheckoutSessions(ctx context.Context, lease lifecycle.AccountLease, teamID int64, blockCompleted bool, verifyCustomerID string, deletionCustomers map[string]bool, expireOpen bool) (bool, error) {
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

	listFor := []string{}
	switch {
	case deletionCustomers != nil:
		listFor = sortedCustomerIDs(deletionCustomers)
	case verifyCustomerID != "":
		listFor = []string{verifyCustomerID}
	}
	for _, customerID := range listFor {
		if err := lease.Renew(ctx); err != nil {
			return false, err
		}
		owned, err := s.Stripe.CheckoutSessionsForCustomer(ctx, customerID)
		if err != nil {
			return false, fmt.Errorf("billing: list checkout sessions for customer %s: %w", customerID, err)
		}
		for i := range owned {
			ids[owned[i].ID] = true
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
			if !expireOpen {
				continue
			}
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
		// Stripe forgets an idempotency key after 24 hours and a Checkout Session
		// expires on its own 24 hours after creation, so a claim this old that
		// never received its session cannot be recovered under its key. Retiring
		// it risks at most one orphan session that Stripe closes within the hour;
		// refusing would lock the account out of checkout for good.
		if err := s.retireCheckoutClaim(ctx, lease, claim); err != nil {
			return nil, err
		}
		claim.Status = "expired"
	}
	if !found || claim.Status == "expired" {
		if _, err := s.cleanupCheckoutSessions(ctx, lease, teamID, existing.CustomerID == "", existing.CustomerID, nil, true); err != nil {
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
		if _, err := s.cleanupCheckoutSessions(ctx, lease, teamID, existing.CustomerID == "", existing.CustomerID, nil, true); err != nil {
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
	if s == nil || s.Stripe == nil || !s.Stripe.Configured() {
		return fmt.Errorf("billing: cannot delete payment customer %s without Stripe credentials", customerID)
	}

	return s.Stripe.DeleteCustomer(ctx, customerID)
}
