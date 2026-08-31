//
// api.go
// The objects we read from Stripe and the six calls we make.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package stripe

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Subscription statuses, as Stripe defines them. They are constants rather than
// string literals at each comparison because the difference between `past_due`
// and `unpaid` decides whether an account keeps its dashboard, and a typo in
// either would fail open.
const (
	StatusTrialing          = "trialing"
	StatusActive            = "active"
	StatusPastDue           = "past_due"
	StatusCanceled          = "canceled"
	StatusUnpaid            = "unpaid"
	StatusPaused            = "paused"
	StatusIncomplete        = "incomplete"
	StatusIncompleteExpired = "incomplete_expired"
)

// TeamMetadataKey is the metadata field that ties a Stripe object back to an
// account. It is written on the customer, the checkout session and the
// subscription, so that a webhook arriving with only one of the three can still
// be routed to the right account without a lookup that might not exist yet.
const TeamMetadataKey = "feasible_team_id"

// Subscription is the part of Stripe's subscription this product reads. The
// fields left out are left out deliberately: the handler is meant to be a
// function of the current state, and every extra field is another thing two
// deliveries could disagree about.
type Subscription struct {
	ID                 string  `json:"id"`
	Customer           string  `json:"customer"`
	Status             string  `json:"status"`
	CurrentPeriodEnd   int64   `json:"current_period_end"`
	CancelAtPeriodEnd  bool    `json:"cancel_at_period_end"`
	CanceledAt         int64   `json:"canceled_at"`
	PauseCollection    *Pause  `json:"pause_collection"`
	Items              Items   `json:"items"`
	Metadata           Meta    `json:"metadata"`
	LatestInvoice      string  `json:"latest_invoice"`
	TrialEnd           int64   `json:"trial_end"`
	Currency           string  `json:"currency"`
	CollectionMethod   string  `json:"collection_method"`
	DaysUntilDue       int     `json:"days_until_due"`
	BillingCycleAnchor int64   `json:"billing_cycle_anchor"`
	Discount           *string `json:"-"`
}

// Pause is Stripe's pause_collection block. Its presence — not the subscription
// status — is what tells us collection is paused, because a paused subscription
// can still report `active`.
type Pause struct {
	Behavior  string `json:"behavior"`
	ResumesAt int64  `json:"resumes_at"`
}

// Items is the subscription's line items. Only the first is read: the product
// sells exactly one plan, and a subscription with two items is a manual edit in
// the Stripe dashboard rather than something this code should try to interpret.
type Items struct {
	Data []Item `json:"data"`
}

// Item is one line on a subscription.
type Item struct {
	ID    string `json:"id"`
	Price Price  `json:"price"`
}

// Price is a Stripe price.
type Price struct {
	ID          string     `json:"id"`
	Product     string     `json:"product"`
	Nickname    string     `json:"nickname"`
	UnitAmount  int64      `json:"unit_amount"`
	Currency    string     `json:"currency"`
	Recurring   *Recurring `json:"recurring"`
	Active      bool       `json:"active"`
	LiveMode    bool       `json:"livemode"`
	Type        string     `json:"type"`
	TaxBehavior string     `json:"tax_behavior"`
}

// Recurring is a price's billing interval.
type Recurring struct {
	Interval      string `json:"interval"`
	IntervalCount int    `json:"interval_count"`
}

// Meta is a Stripe metadata map.
type Meta map[string]string

// TeamID reads the account id out of metadata. It returns zero rather than an
// error for anything unparseable, because a Stripe object created by hand in
// the dashboard legitimately has no metadata and must not crash a webhook.
func (m Meta) TeamID() int64 {
	value, ok := m[TeamMetadataKey]
	if !ok {
		return 0
	}

	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0
	}

	return id
}

// PriceID is the plan the subscription is on, or empty for a subscription with
// no items.
func (s *Subscription) PriceID() string {
	if s == nil || len(s.Items.Data) == 0 {
		return ""
	}

	return s.Items.Data[0].Price.ID
}

// Paused reports whether collection is paused. Stripe expresses this through
// the pause_collection block rather than the status, and reading only the
// status is how a paused subscription is mistaken for a healthy one.
func (s *Subscription) Paused() bool {
	return s != nil && s.PauseCollection != nil && s.PauseCollection.Behavior != ""
}

// Paying reports whether this subscription is currently in good standing. It is
// the single question the lifecycle machine asks about Stripe, and it is
// answered from the subscription's own current state rather than from whichever
// event happened to arrive — which is what makes duplicate and out-of-order
// delivery harmless.
func (s *Subscription) Paying() bool {
	if s == nil || s.Paused() {
		return false
	}

	switch s.Status {
	case StatusActive, StatusTrialing:
		return true
	default:
		return false
	}
}

// PeriodEnd is when the current paid period runs out.
func (s *Subscription) PeriodEnd() time.Time {
	if s == nil || s.CurrentPeriodEnd == 0 {
		return time.Time{}
	}

	return time.Unix(s.CurrentPeriodEnd, 0).UTC()
}

// Customer is the part of a Stripe customer this product reads.
type Customer struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	Meta    Meta   `json:"metadata"`
}

// Product is the Stripe catalogue object the two configured prices must share.
type Product struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	TaxCode   string `json:"tax_code"`
	Active    bool   `json:"active"`
	LiveMode  bool   `json:"livemode"`
	Shippable *bool  `json:"shippable"`
}

// CheckoutSession is the object a checkout produces.
type CheckoutSession struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Customer      string `json:"customer"`
	Subscription  string `json:"subscription"`
	Status        string `json:"status"`
	PaymentStatus string `json:"payment_status"`
	Mode          string `json:"mode"`
	Metadata      Meta   `json:"metadata"`

	// CustomerEmail is what they typed at checkout, and is the address every
	// lifecycle email goes to when the account has no other billing contact.
	CustomerEmail string `json:"customer_email"`
}

// WebhookEndpoint is one Stripe destination and the events sent to it.
type WebhookEndpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	Status        string   `json:"status"`
	EnabledEvents []string `json:"enabled_events"`
}

// PortalSession is a Customer Portal link.
type PortalSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Invoice is the part of an invoice the webhook path reads.
type Invoice struct {
	ID           string `json:"id"`
	Customer     string `json:"customer"`
	Subscription string `json:"subscription"`
	Status       string `json:"status"`
	AttemptCount int    `json:"attempt_count"`
	Total        int64  `json:"total"`
	Currency     string `json:"currency"`
	HostedURL    string `json:"hosted_invoice_url"`
	Metadata     Meta   `json:"metadata"`
}

// list is Stripe's paginated envelope.
type list[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// CheckoutParams describes the session to create. Trials are absent on purpose:
// this product's trial takes no card, so no Stripe customer exists until
// somebody pays, and asking Stripe for a trial would create one.
type CheckoutParams struct {
	TeamID     int64
	PriceID    string
	CustomerID string

	// Email prefills the form for an account that has never paid. It is a
	// prefill rather than a lock, because the person paying is often not the
	// person who signed up.
	Email string

	SuccessURL string
	CancelURL  string

	// IdempotencyKey stops a double-click, a retried request or a flaky network
	// creating two sessions.
	IdempotencyKey string
}

// CreateCheckoutSession starts a subscription checkout through Stripe Managed
// Payments. Sold through Link, LLC is merchant of record for supported
// transactions; the seller retains tax duties outside that supported scope.
func (c *Client) CreateCheckoutSession(ctx context.Context, params CheckoutParams) (*CheckoutSession, error) {
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("line_items[0][price]", params.PriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", params.SuccessURL)
	form.Set("cancel_url", params.CancelURL)
	form.Set("billing_address_collection", "required")
	form.Set("allow_promotion_codes", "true")

	// The business arrangement must not depend on an account-level dashboard
	// default. Managed Payments makes Sold through Link, LLC merchant of record
	// for supported transactions and is deliberately enabled on every checkout.
	form.Set("managed_payments[enabled]", "true")

	// The account id goes on the session, the subscription and the customer.
	// Any one of the three can be the only thing a webhook carries, and an
	// event we cannot route to an account is an event we cannot act on.
	if params.TeamID > 0 {
		form.Set("metadata["+TeamMetadataKey+"]", strconv.FormatInt(params.TeamID, 10))
		form.Set("subscription_data[metadata]["+TeamMetadataKey+"]", strconv.FormatInt(params.TeamID, 10))
	}

	// A subscription checkout always creates a customer, so there is nothing to
	// ask for here — only whether to reuse one we already have. An account that
	// has paid before must reuse its customer, or it ends up with two records
	// and two payment histories that nothing can reconcile.
	switch {
	case params.CustomerID != "":
		form.Set("customer", params.CustomerID)

	case params.Email != "":
		form.Set("customer_email", params.Email)
	}

	var session CheckoutSession
	if err := c.postWithVersion(ctx, "/v1/checkout/sessions", form, params.IdempotencyKey, ManagedPaymentsAPIVersion, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// CreatePortalSession returns a Customer Portal link. Card updates, plan
// switches, invoice history and cancellation all live there rather than being
// rebuilt in this product: Stripe already handles SCA, 3D Secure and every
// regional payment method, and a home-grown card form would handle none of it.
func (c *Client) CreatePortalSession(ctx context.Context, customerID, returnURL string) (*PortalSession, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("return_url", returnURL)

	var session PortalSession
	if err := c.post(ctx, "/v1/billing_portal/sessions", form, "", &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// GetSubscription reads a subscription's current state. It is the call the
// webhook handler makes before it decides anything, which is what makes the
// handler a function of Stripe's state rather than of the event that woke it.
func (c *Client) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	form := url.Values{}
	form.Set("expand[]", "items.data.price")

	var subscription Subscription
	if err := c.get(ctx, "/v1/subscriptions/"+url.PathEscape(id), form, &subscription); err != nil {
		return nil, err
	}

	return &subscription, nil
}

// GetCustomer reads a customer. A customer Stripe has deleted comes back with
// `deleted: true` rather than a 404, and treating that as a live customer is
// how an account ends up linked to a record nothing can charge.
func (c *Client) GetCustomer(ctx context.Context, id string) (*Customer, error) {
	var customer Customer
	if err := c.get(ctx, "/v1/customers/"+url.PathEscape(id), nil, &customer); err != nil {
		return nil, err
	}

	return &customer, nil
}

// GetCheckoutSession reads a completed checkout, expanded far enough to carry
// the subscription with it.
func (c *Client) GetCheckoutSession(ctx context.Context, id string) (*CheckoutSession, error) {
	var session CheckoutSession
	if err := c.get(ctx, "/v1/checkout/sessions/"+url.PathEscape(id), nil, &session); err != nil {
		return nil, err
	}

	return &session, nil
}

// ExpireCheckoutSession closes an unused checkout so nobody can complete it.
// Deployment smoke tests call this immediately after Stripe accepts a session.
func (c *Client) ExpireCheckoutSession(ctx context.Context, id, idempotencyKey string) error {
	var session CheckoutSession

	return c.postWithVersion(ctx, "/v1/checkout/sessions/"+url.PathEscape(id)+"/expire", nil, idempotencyKey, ManagedPaymentsAPIVersion, &session)
}

// GetProduct reads the configured catalogue product for deployment checks.
func (c *Client) GetProduct(ctx context.Context, id string) (*Product, error) {
	var product Product
	if err := c.getWithVersion(ctx, "/v1/products/"+url.PathEscape(id), nil, ManagedPaymentsAPIVersion, &product); err != nil {
		return nil, err
	}

	return &product, nil
}

// GetPrice reads one configured recurring price for deployment checks.
func (c *Client) GetPrice(ctx context.Context, id string) (*Price, error) {
	var price Price
	if err := c.getWithVersion(ctx, "/v1/prices/"+url.PathEscape(id), nil, ManagedPaymentsAPIVersion, &price); err != nil {
		return nil, err
	}

	return &price, nil
}

// ListWebhookEndpoints reads every endpoint Stripe can return in one page. The
// API's maximum page size is 100, far above the number one deployment uses.
func (c *Client) ListWebhookEndpoints(ctx context.Context) ([]WebhookEndpoint, error) {
	form := url.Values{}
	form.Set("limit", "100")

	var page list[WebhookEndpoint]
	if err := c.getWithVersion(ctx, "/v1/webhook_endpoints", form, ManagedPaymentsAPIVersion, &page); err != nil {
		return nil, err
	}

	return page.Data, nil
}

// ActiveSubscription finds the subscription a customer is actually paying on.
//
// It reads every subscription rather than trusting the id an event carried,
// because a customer who cancelled and resubscribed has two, and acting on the
// dead one would leave a paying customer's account marked as lapsed. A customer
// with no live subscription returns nil, which is not an error — it is the
// normal state of somebody who cancelled.
func (c *Client) ActiveSubscription(ctx context.Context, customerID string) (*Subscription, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("status", "all")
	form.Set("limit", "20")
	form.Set("expand[]", "data.items.data.price")

	var page list[Subscription]
	if err := c.get(ctx, "/v1/subscriptions", form, &page); err != nil {
		return nil, err
	}

	// A paying subscription wins outright. Otherwise the most recently ended
	// one is returned so the caller can mirror its plan and status rather than
	// showing a customer nothing at all.
	var fallback *Subscription

	for i := range page.Data {
		candidate := &page.Data[i]

		if candidate.Paying() {
			return candidate, nil
		}

		if fallback == nil || candidate.CurrentPeriodEnd > fallback.CurrentPeriodEnd {
			fallback = candidate
		}
	}

	return fallback, nil
}

// DeleteCustomer removes a customer and everything Stripe holds against them,
// including the stored card. It runs at day 90 as part of the irreversible
// deletion, and a customer already gone is not an error: the goal is that they
// are gone, not that this call was the one that removed them.
func (c *Client) DeleteCustomer(ctx context.Context, id string) error {
	var out struct {
		Deleted bool `json:"deleted"`
	}

	err := c.del(ctx, "/v1/customers/"+url.PathEscape(id), &out)

	var apiErr *Error
	if err != nil && asError(err, &apiErr) && apiErr.Status == 404 {
		return nil
	}

	return err
}

// asError unwraps a Stripe API error, so a caller can branch on the status
// without a type switch at every call site.
func asError(err error, target **Error) bool {
	if err == nil {
		return false
	}

	if typed, ok := err.(*Error); ok {
		*target = typed
		return true
	}

	return false
}

// Plan names one of the two prices this product sells. It exists so that a
// price id read back from Stripe can be turned into something a customer
// recognises without a second API call on every page render.
type Plan struct {
	Key      string
	Label    string
	PriceID  string
	Amount   int64
	Interval string
}

// Describe turns a price id into a plan. An id we do not recognise — somebody
// moved to a custom price in the Stripe dashboard — is described honestly as
// custom rather than guessed at.
func Describe(priceID, monthlyID, yearlyID string) Plan {
	switch priceID {
	case monthlyID:
		return Plan{Key: "monthly", Label: "$9.99 / month", PriceID: priceID, Amount: 999, Interval: "month"}
	case yearlyID:
		return Plan{Key: "yearly", Label: "$100 / year", PriceID: priceID, Amount: 10000, Interval: "year"}
	case "":
		return Plan{}
	default:
		return Plan{Key: "custom", Label: "Custom plan", PriceID: priceID}
	}
}

// Amount formats a Stripe minor-unit amount as dollars.
func Amount(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
