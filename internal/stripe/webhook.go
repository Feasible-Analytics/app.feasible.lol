//
// webhook.go
// Verifying a webhook signature, and reading only what the payload can be trusted for.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The event types this product acts on. Everything else is recorded and
// ignored: Stripe sends dozens of types, and a handler that grew a branch per
// type would be a handler nobody could reason about.
const (
	EventCheckoutCompleted             = "checkout.session.completed"
	EventCheckoutAsyncPaymentSucceeded = "checkout.session.async_payment_succeeded"
	EventCheckoutAsyncPaymentFailed    = "checkout.session.async_payment_failed"
	EventSubscriptionCreated           = "customer.subscription.created"
	EventSubscriptionUpdated           = "customer.subscription.updated"
	EventSubscriptionDeleted           = "customer.subscription.deleted"
	EventSubscriptionPaused            = "customer.subscription.paused"
	EventSubscriptionResumed           = "customer.subscription.resumed"
	EventInvoicePaymentSucceed         = "invoice.payment_succeeded"
	EventInvoicePaymentFailed          = "invoice.payment_failed"
)

// SignatureTolerance is how far a webhook's timestamp may be from ours. Five
// minutes is Stripe's own recommendation: it is the window a replayed request
// is accepted in, and widening it to be forgiving about clock drift widens the
// replay window by exactly the same amount.
const SignatureTolerance = 5 * time.Minute

// Event is a webhook delivery.
type Event struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    EventData       `json:"data"`
	Request json.RawMessage `json:"request"`

	// Raw is the exact bytes the signature was computed over. It is kept so the
	// event can be stored verbatim: a support person reading it a month later
	// needs what Stripe sent, not what our structs could parse.
	Raw []byte `json:"-"`
}

// EventData wraps the object the event is about.
type EventData struct {
	Object json.RawMessage `json:"object"`
}

// Subscription decodes the event's object as a subscription. The result is used
// only for the ids on it — the customer and the subscription — and never for
// its status, because the payload is a snapshot from when the event was created
// and may be several states out of date by the time it arrives.
func (e *Event) Subscription() (*Subscription, error) {
	var subscription Subscription
	if err := decodeJSON(e.Data.Object, &subscription); err != nil {
		return nil, err
	}

	return &subscription, nil
}

// Invoice decodes the event's object as an invoice.
func (e *Event) Invoice() (*Invoice, error) {
	var invoice Invoice
	if err := decodeJSON(e.Data.Object, &invoice); err != nil {
		return nil, err
	}

	return &invoice, nil
}

// CheckoutSession decodes the event's object as a checkout session.
func (e *Event) CheckoutSession() (*CheckoutSession, error) {
	var session CheckoutSession
	if err := decodeJSON(e.Data.Object, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// CustomerID digs the customer id out of whichever object this event carries.
// It exists because routing an event to an account needs the customer, and
// every one of the six object types puts it somewhere slightly different.
func (e *Event) CustomerID() string {
	var object struct {
		Customer string `json:"customer"`
		ID       string `json:"id"`
		Object   string `json:"object"`
	}

	if err := decodeJSON(e.Data.Object, &object); err != nil {
		return ""
	}

	if object.Object == "customer" {
		return object.ID
	}

	return object.Customer
}

// SubscriptionID reads the subscription from a checkout or invoice, or the id
// of a subscription event itself. It lets payment evidence be attached to the
// subscription it actually describes when a customer has more than one.
func (e *Event) SubscriptionID() string {
	var object struct {
		ID           string `json:"id"`
		Object       string `json:"object"`
		Subscription string `json:"subscription"`
	}

	if err := decodeJSON(e.Data.Object, &object); err != nil {
		return ""
	}

	if object.Object == "subscription" {
		return object.ID
	}

	return object.Subscription
}

// TeamID reads the account id out of the object's metadata, if it has any.
func (e *Event) TeamID() int64 {
	var object struct {
		Metadata Meta `json:"metadata"`
	}

	if err := decodeJSON(e.Data.Object, &object); err != nil {
		return 0
	}

	return object.Metadata.TeamID()
}

// ParseWebhook verifies a delivery's signature and decodes it.
//
// The signature check is not optional and is not a formality: the webhook
// endpoint is a public URL that changes billing state, and without verification
// anybody who guesses it can mark any account as paid. The body must be the raw
// bytes as received — re-encoding the JSON first changes them and the signature
// will not match.
func ParseWebhook(payload []byte, header, secret string, now time.Time) (*Event, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("stripe: no webhook signing secret configured")
	}

	timestamp, signatures, err := parseSignatureHeader(header)
	if err != nil {
		return nil, err
	}

	// A delivery far outside the tolerance is a replay of a message we may have
	// already acted on, so it is refused before anything is decoded.
	age := now.Sub(time.Unix(timestamp, 0))
	if age > SignatureTolerance || age < -SignatureTolerance {
		return nil, fmt.Errorf("stripe: webhook timestamp is %s away from now", age.Round(time.Second))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := mac.Sum(nil)

	// The header can carry several signatures during a secret rotation, and any
	// one of them matching is a valid delivery. The comparison is constant-time
	// because a timing-variable compare on a MAC is forgeable given enough
	// attempts, and this endpoint accepts unauthenticated requests by design.
	matched := false
	for _, candidate := range signatures {
		decoded, err := hex.DecodeString(candidate)
		if err != nil {
			continue
		}

		if hmac.Equal(decoded, expected) {
			matched = true
			break
		}
	}

	if !matched {
		return nil, fmt.Errorf("stripe: webhook signature does not match")
	}

	var event Event
	if err := decodeJSON(payload, &event); err != nil {
		return nil, err
	}

	if event.ID == "" {
		return nil, fmt.Errorf("stripe: webhook has no event id")
	}

	event.Raw = payload

	return &event, nil
}

// parseSignatureHeader splits Stripe-Signature into its timestamp and its v1
// signatures. The header is a comma-separated list of key=value pairs, and a
// scheme we do not know is skipped rather than rejected so that a future scheme
// alongside v1 does not break every delivery.
func parseSignatureHeader(header string) (int64, []string, error) {
	if strings.TrimSpace(header) == "" {
		return 0, nil, fmt.Errorf("stripe: no Stripe-Signature header")
	}

	var (
		timestamp  int64
		signatures []string
	)

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}

		switch key {
		case "t":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("stripe: webhook timestamp %q is not a number", value)
			}
			timestamp = parsed
		case "v1":
			signatures = append(signatures, value)
		}
	}

	if timestamp == 0 {
		return 0, nil, fmt.Errorf("stripe: Stripe-Signature has no timestamp")
	}
	if len(signatures) == 0 {
		return 0, nil, fmt.Errorf("stripe: Stripe-Signature has no v1 signature")
	}

	return timestamp, signatures, nil
}

// SignPayload produces the header Stripe would send for a payload. It exists so
// the webhook tests exercise the real verification path rather than a bypass —
// a test that skipped the signature would leave the one check protecting
// billing state completely untested.
func SignPayload(payload []byte, secret string, at time.Time) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)

	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
