//
// webhook.go
// The one endpoint the payment provider talks to, and the three ways it lies.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// WebhookPath is where the provider posts. It is a constant so the route, the
// documentation and the `stripe listen --forward-to` line in the README cannot
// drift apart.
const WebhookPath = "POST /webhooks/stripe"

// maxWebhookBody caps what we read. The provider's own payloads are a few
// kilobytes; a megabyte is generous, and unbounded is how an endpoint that
// accepts unauthenticated requests becomes a memory exhaustion bug.
const maxWebhookBody = 1 << 20

// Webhook is the HTTP handler for provider deliveries.
//
// It assumes three things about every delivery, because all three are true:
// it may be a forgery, it may be a duplicate, and it may be out of order and
// describe a world that no longer exists. The signature check handles the
// first, the event log handles the second, and reconciling from the provider's
// current state rather than from the payload handles the third.
type Webhook struct {
	Service *Service
	Log     *logger.Logger
}

// NewWebhook builds the handler.
func NewWebhook(service *Service, log *logger.Logger) *Webhook {
	return &Webhook{Service: service, Log: log}
}

// ServeHTTP verifies, records and dispatches one delivery.
//
// The status codes matter to the provider, which retries on anything that is
// not a 2xx: a signature failure is a 400 so it stops retrying a forgery, and a
// handler failure is a 500 so it retries something that might work next time.
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}

	event, err := stripe.ParseWebhook(payload, r.Header.Get("Stripe-Signature"), h.Service.WebhookSecret, h.Service.now())
	if err != nil {
		// A rejected signature is logged at warn rather than swallowed. It is
		// either a misconfigured secret — which makes every payment invisible
		// to us — or somebody probing the endpoint, and both are worth seeing.
		if h.Log != nil {
			h.Log.Warn("stripe webhook rejected", "error", err, "remote", r.RemoteAddr)
		}

		http.Error(w, "signature verification failed", http.StatusBadRequest)
		return
	}

	outcome, err := h.Handle(r.Context(), event)
	if err != nil {
		if h.Log != nil {
			h.Log.Error("stripe webhook failed", "event", event.ID, "type", event.Type, "error", err)
		}

		http.Error(w, "the webhook could not be handled", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"received":true,"outcome":%q}`, outcome)
}

// Handle applies one verified event and returns what it did. It is exported
// separately from ServeHTTP so tests drive the real logic without an HTTP
// round trip, and so a support command can replay a stored event by hand.
func (h *Webhook) Handle(ctx context.Context, event *stripe.Event) (string, error) {
	teamID, err := h.route(ctx, event)
	if err != nil {
		return "", err
	}

	claim, err := h.Service.Store.ClaimEvent(ctx, event.ID, event.Type, teamID, event.Raw)
	if err != nil {
		return "", err
	}

	if !claim.Claimed {
		if claim.Processing {
			return "", fmt.Errorf("billing: stripe event %s is already processing", event.ID)
		}

		if h.Log != nil {
			h.Log.Debug("stripe webhook already applied", "event", event.ID, "type", event.Type, "team", teamID)
		}

		return OutcomeDuplicate, nil
	}

	outcome, handlerErr := h.apply(ctx, event, teamID)

	if err := h.Service.Store.FinishEvent(ctx, event.ID, claim, outcome, teamID, handlerErr); err != nil {
		return outcome, err
	}

	if handlerErr != nil {
		return outcome, handlerErr
	}

	if h.Log != nil {
		h.Log.Info("stripe webhook handled", "event", event.ID, "type", event.Type, "team", teamID, "outcome", outcome)
	}

	return outcome, nil
}

// apply does the work for the event types this product acts on.
//
// Every branch does the same thing — reconcile the account against the
// provider's current state — and that is the point. The event type decides
// whether we care, never what the answer is, so a delivery that arrives twice
// or in the wrong order cannot produce a different outcome from the right one.
func (h *Webhook) apply(ctx context.Context, event *stripe.Event, teamID int64) (string, error) {
	switch event.Type {
	case stripe.EventCheckoutCompleted,
		stripe.EventCheckoutAsyncPaymentSucceeded,
		stripe.EventCheckoutAsyncPaymentFailed,
		stripe.EventSubscriptionCreated,
		stripe.EventSubscriptionUpdated,
		stripe.EventSubscriptionDeleted,
		stripe.EventSubscriptionPaused,
		stripe.EventSubscriptionResumed,
		stripe.EventInvoicePaymentSucceed,
		stripe.EventInvoicePaymentFailed,
		stripe.EventInvoiceFinalizationFailed:

		if teamID < 1 {
			// An event we cannot route to an account is recorded and left
			// alone rather than guessed at. It is almost always somebody
			// clicking around the provider's dashboard against a customer this
			// install has never seen.
			return OutcomeIgnored, nil
		}

		customerID := event.CustomerID()
		if customerID == "" {
			return OutcomeIgnored, nil
		}

		update, err := paymentUpdate(event)
		if err != nil {
			return OutcomeError, err
		}
		if update.RequireSubscriptionMatch && update.SubscriptionID == "" {
			return OutcomeIgnored, nil
		}

		applied, err := h.Service.Reconcile(ctx, teamID, customerID, update)
		if err != nil {
			return OutcomeError, err
		}
		if !applied {
			return OutcomeIgnored, nil
		}

		return OutcomeApplied, nil

	default:
		return OutcomeIgnored, nil
	}
}

// paymentUpdate separates conclusive payment evidence from subscription state.
// A completed Checkout Session with payment_status=unpaid is only pending; the
// later async success or failure event is what may change account access.
func paymentUpdate(event *stripe.Event) (PaymentUpdate, error) {
	update := PaymentUpdate{
		SubscriptionID: event.SubscriptionID(),
		EventCreated:   event.Created,
		Trigger:        event.Type,
	}

	switch event.Type {
	case stripe.EventCheckoutCompleted,
		stripe.EventCheckoutAsyncPaymentSucceeded,
		stripe.EventCheckoutAsyncPaymentFailed:
		session, err := event.CheckoutSession()
		if err != nil {
			return PaymentUpdate{}, err
		}
		update.SubscriptionID = session.Subscription
		update.SourceID = session.ID
		update.SourceCreated = session.Created
		update.RequireSubscriptionMatch = true

		switch event.Type {
		case stripe.EventCheckoutAsyncPaymentSucceeded:
			update.State = PaymentPaid
		case stripe.EventCheckoutAsyncPaymentFailed:
			update.State = PaymentFailed
		default:
			switch session.PaymentStatus {
			case "paid", "no_payment_required":
				update.State = PaymentPaid
			default:
				update.State = PaymentPending
			}
		}

	case stripe.EventInvoicePaymentSucceed,
		stripe.EventInvoicePaymentFailed,
		stripe.EventInvoiceFinalizationFailed:
		invoice, err := event.Invoice()
		if err != nil {
			return PaymentUpdate{}, err
		}
		update.SubscriptionID = invoice.SubscriptionID()
		update.SourceID = invoice.ID
		update.SourceCreated = invoice.Created
		update.RequireSubscriptionMatch = true

		if event.Type == stripe.EventInvoicePaymentSucceed {
			update.State = PaymentPaid
		} else {
			update.State = PaymentFailed
		}
	}

	return update, nil
}

// paymentEvidenceEventTypes lists the signed event objects that can establish a
// pending, paid, or failed payment state. Subscription lifecycle events are not
// settlement evidence and therefore never appear here.
func paymentEvidenceEventTypes() []string {
	return []string{
		stripe.EventCheckoutCompleted,
		stripe.EventCheckoutAsyncPaymentSucceeded,
		stripe.EventCheckoutAsyncPaymentFailed,
		stripe.EventInvoicePaymentSucceed,
		stripe.EventInvoicePaymentFailed,
		stripe.EventInvoiceFinalizationFailed,
	}
}

// route works out which account an event belongs to.
//
// Metadata is tried first because we set it on the customer, the checkout
// session and the subscription, so at least one of the three is almost always
// present. The customer id is the fallback, and it is the one that works for
// anything created by hand in the provider's dashboard.
func (h *Webhook) route(ctx context.Context, event *stripe.Event) (int64, error) {
	if teamID := event.TeamID(); teamID > 0 {
		return teamID, nil
	}

	return h.Service.Store.TeamForCustomer(ctx, event.CustomerID())
}
