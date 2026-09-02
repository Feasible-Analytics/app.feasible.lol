//
// api.go
// The objects we read from Stripe and the calls we make against them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package stripe

import (
	"context"
	"errors"
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
	ID                 string `json:"id"`
	Created            int64  `json:"created"`
	Customer           string `json:"customer"`
	Status             string `json:"status"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	CanceledAt         int64  `json:"canceled_at"`
	PauseCollection    *Pause `json:"pause_collection"`
	Items              Items  `json:"items"`
	Metadata           Meta   `json:"metadata"`
	LatestInvoice      string `json:"latest_invoice"`
	TrialEnd           int64  `json:"trial_end"`
	Currency           string `json:"currency"`
	CollectionMethod   string `json:"collection_method"`
	DaysUntilDue       int    `json:"days_until_due"`
	BillingCycleAnchor int64  `json:"billing_cycle_anchor"`
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

// BlocksCheckout reports whether a subscription can still charge, retry, or
// settle. Only Stripe's terminal states are safe beside a replacement;
// unknown future states fail closed so a new status cannot create a second
// chargeable subscription by surprise.
func (s *Subscription) BlocksCheckout() bool {
	if s == nil {
		return false
	}

	switch s.Status {
	case StatusCanceled, StatusIncompleteExpired:
		return false
	default:
		return true
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
	Created       int64  `json:"created"`
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
	APIVersion    string   `json:"api_version"`
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
	ID           string                   `json:"id"`
	Created      int64                    `json:"created"`
	Customer     string                   `json:"customer"`
	Subscription string                   `json:"subscription"`
	Parent       *InvoiceParent           `json:"parent"`
	Status       string                   `json:"status"`
	AutoAdvance  bool                     `json:"auto_advance"`
	AttemptCount int                      `json:"attempt_count"`
	Total        int64                    `json:"total"`
	Currency     string                   `json:"currency"`
	HostedURL    string                   `json:"hosted_invoice_url"`
	Metadata     Meta                     `json:"metadata"`
	Paid         bool                     `json:"paid"`
	Transitions  InvoiceStatusTransitions `json:"status_transitions"`
}

// InvoiceStatusTransitions carries provider evidence timestamps. PaidAt is
// Stripe's durable settlement instant and is used instead of webhook receipt
// time when day-90 deletion races a delayed payment event.
type InvoiceStatusTransitions struct {
	PaidAt int64 `json:"paid_at"`
}

// InvoiceParent is the Basil location of the object that generated an invoice.
// Stripe removed the top-level subscription fields in 2025-03-31.basil, so a
// subscription invoice is identified only when the parent type agrees with the
// nested details.
type InvoiceParent struct {
	Type                string                      `json:"type"`
	SubscriptionDetails *InvoiceSubscriptionDetails `json:"subscription_details"`
}

// InvoiceSubscriptionDetails identifies the subscription that generated an
// invoice and carries the immutable metadata snapshot Stripe puts on it.
type InvoiceSubscriptionDetails struct {
	Subscription string `json:"subscription"`
	Metadata     Meta   `json:"metadata"`
}

// SubscriptionID returns the Basil subscription parent. The top-level fallback
// is retained only for stored events rendered before Basil; once parent exists,
// its type and nested id must be valid rather than falling back ambiguously.
func (i *Invoice) SubscriptionID() string {
	if i == nil {
		return ""
	}

	if i.Parent != nil {
		if i.Parent.Type != "subscription_details" || i.Parent.SubscriptionDetails == nil {
			return ""
		}

		return i.Parent.SubscriptionDetails.Subscription
	}

	return i.Subscription
}

// TeamID returns the invoice metadata account id, including the Basil parent
// snapshot used when the invoice itself has no metadata.
func (i *Invoice) TeamID() int64 {
	if i == nil {
		return 0
	}

	if teamID := i.Metadata.TeamID(); teamID > 0 {
		return teamID
	}

	if i.Parent != nil && i.Parent.Type == "subscription_details" && i.Parent.SubscriptionDetails != nil {
		return i.Parent.SubscriptionDetails.Metadata.TeamID()
	}

	return 0
}

// list is Stripe's paginated envelope.
type list[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// searchList is the envelope of Stripe's search endpoints, which page by an
// opaque cursor rather than by the last object's id.
type searchList[T any] struct {
	Data     []T    `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
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

// CheckoutSessions reads every provider session so a caller can match ours by
// metadata. Stripe exposes no metadata filter on this list and no session
// search endpoint, and a session created before its customer existed can be
// found no other way — an orphan that already created a customer and a
// subscription must still be reachable when its id never reached system.db.
func (c *Client) CheckoutSessions(ctx context.Context) ([]CheckoutSession, error) {
	return c.checkoutSessions(ctx, url.Values{})
}

// CheckoutSessionsForCustomer reads every session Stripe holds for one
// customer, including the ones that carry none of our metadata. Completed
// sessions are included because one may have created a subscription before its
// id reached system.db.
func (c *Client) CheckoutSessionsForCustomer(ctx context.Context, customerID string) ([]CheckoutSession, error) {
	form := url.Values{}
	form.Set("customer", customerID)

	return c.checkoutSessions(ctx, form)
}

// checkoutSessions walks every page of one checkout-session listing.
func (c *Client) checkoutSessions(ctx context.Context, form url.Values) ([]CheckoutSession, error) {
	form.Set("limit", "100")

	var sessions []CheckoutSession
	for {
		var page list[CheckoutSession]
		if err := c.getWithVersion(ctx, "/v1/checkout/sessions", form, ManagedPaymentsAPIVersion, &page); err != nil {
			return nil, err
		}
		sessions = append(sessions, page.Data...)
		if !page.HasMore {
			return sessions, nil
		}
		if len(page.Data) == 0 {
			return nil, fmt.Errorf("stripe: checkout sessions page said has_more without any data")
		}
		form.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
}

// SearchCustomersByTeam finds every customer carrying an account's metadata,
// which is how permanent deletion discovers a duplicate record left by a lost
// Checkout response. Stripe's search index lags writes by up to a minute, so
// it is a discovery aid for the rare deletion path, not the routing source
// for webhooks.
func (c *Client) SearchCustomersByTeam(ctx context.Context, teamID int64) ([]Customer, error) {
	form := url.Values{}
	form.Set("query", fmt.Sprintf("metadata['%s']:'%d'", TeamMetadataKey, teamID))
	form.Set("limit", "100")

	var customers []Customer
	for {
		var page searchList[Customer]
		if err := c.get(ctx, "/v1/customers/search", form, &page); err != nil {
			return nil, err
		}
		customers = append(customers, page.Data...)
		if !page.HasMore {
			return customers, nil
		}
		if page.NextPage == "" {
			return nil, fmt.Errorf("stripe: customer search said has_more without a next page")
		}
		form.Set("page", page.NextPage)
	}
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

// SetSubscriptionCollectionPaused quiesces or restores collection while day-90
// deletion checks provider truth. Callers select Stripe's documented pause
// behavior explicitly so reversible preparation never inherits void semantics.
func (c *Client) SetSubscriptionCollectionPaused(ctx context.Context, id string, paused bool, behavior, idempotencyKey string) (*Subscription, error) {
	form := url.Values{}
	if paused {
		form.Set("pause_collection[behavior]", behavior)
	} else {
		form.Set("pause_collection", "")
	}

	var subscription Subscription
	if err := c.post(ctx, "/v1/subscriptions/"+url.PathEscape(id), form, idempotencyKey, &subscription); err != nil {
		return nil, err
	}

	return &subscription, nil
}

// SetInvoiceAutoAdvance disables or restores Stripe's automatic finalization,
// reminders, reconciliation, and payment retries for one draft or open invoice.
// Day-90 deletion uses it because pausing a subscription does not affect an
// invoice that was created before the pause.
func (c *Client) SetInvoiceAutoAdvance(ctx context.Context, id string, enabled bool, idempotencyKey string) (*Invoice, error) {
	form := url.Values{}
	form.Set("auto_advance", strconv.FormatBool(enabled))

	var invoice Invoice
	if err := c.post(ctx, "/v1/invoices/"+url.PathEscape(id), form, idempotencyKey, &invoice); err != nil {
		return nil, err
	}

	return &invoice, nil
}

// VoidInvoice irreversibly removes the manual and portal payment opportunity
// from an already-open invoice. Disabling auto_advance only stops automatic
// retries, so day-90 deletion must void open invoices before its final read.
func (c *Client) VoidInvoice(ctx context.Context, id, idempotencyKey string) (*Invoice, error) {
	var invoice Invoice
	if err := c.post(ctx, "/v1/invoices/"+url.PathEscape(id)+"/void", nil, idempotencyKey, &invoice); err != nil {
		return nil, err
	}

	return &invoice, nil
}

// DeleteDraftInvoice removes an invoice that is not payable yet but could be
// manually finalized after the day-90 final read. Draft deletion is the only
// way to make that provider transition impossible.
func (c *Client) DeleteDraftInvoice(ctx context.Context, id, idempotencyKey string) error {
	var invoice Invoice

	return c.delWithKey(ctx, "/v1/invoices/"+url.PathEscape(id), idempotencyKey, &invoice)
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

// Subscriptions reads every page of a customer's subscription history. Callers
// need the whole set both to detect any chargeable state and to keep evidence
// attached to the subscription that produced it.
func (c *Client) Subscriptions(ctx context.Context, customerID string) ([]Subscription, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("status", "all")
	form.Set("limit", "100")
	form.Set("expand[]", "data.items.data.price")

	var subscriptions []Subscription

	for {
		var page list[Subscription]
		if err := c.get(ctx, "/v1/subscriptions", form, &page); err != nil {
			return nil, err
		}
		subscriptions = append(subscriptions, page.Data...)

		if !page.HasMore {
			return subscriptions, nil
		}
		if len(page.Data) == 0 {
			return nil, fmt.Errorf("stripe: subscriptions page said has_more without any data")
		}

		form.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
}

// SelectSubscription chooses the deterministic row displayed and reconciled.
// Healthy subscriptions outrank settling ones, settling ones outrank terminal
// history, and recency uses creation time before period end and id. This keeps
// an older canceled annual plan from hiding a newer monthly subscription.
func SelectSubscription(subscriptions []Subscription) *Subscription {
	var selected *Subscription

	for i := range subscriptions {
		candidate := &subscriptions[i]
		if selected == nil || subscriptionAfter(candidate, selected) {
			copy := *candidate
			selected = &copy
		}
	}

	return selected
}

// subscriptionAfter applies the deterministic display precedence.
func subscriptionAfter(candidate, current *Subscription) bool {
	candidateRank := subscriptionDisplayRank(candidate)
	currentRank := subscriptionDisplayRank(current)
	if candidateRank != currentRank {
		return candidateRank > currentRank
	}
	if candidate.Created != current.Created {
		return candidate.Created > current.Created
	}
	if candidate.CurrentPeriodEnd != current.CurrentPeriodEnd {
		return candidate.CurrentPeriodEnd > current.CurrentPeriodEnd
	}

	return candidate.ID > current.ID
}

// subscriptionDisplayRank keeps useful live truth ahead of terminal history.
func subscriptionDisplayRank(subscription *Subscription) int {
	if subscription.Paying() {
		return 3
	}
	if subscription.BlocksCheckout() {
		return 2
	}

	return 1
}

// Invoices reads every invoice for one customer, including historical paid
// evidence needed to close the day-90 webhook delay window.
func (c *Client) Invoices(ctx context.Context, customerID string) ([]Invoice, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("limit", "100")

	var invoices []Invoice
	for {
		var page list[Invoice]
		if err := c.get(ctx, "/v1/invoices", form, &page); err != nil {
			return nil, err
		}
		invoices = append(invoices, page.Data...)

		if !page.HasMore {
			return invoices, nil
		}
		if len(page.Data) == 0 {
			return nil, fmt.Errorf("stripe: invoices page said has_more without any data")
		}

		form.Set("starting_after", page.Data[len(page.Data)-1].ID)
	}
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
	if errors.As(err, &apiErr) && apiErr.Status == 404 {
		return nil
	}

	return err
}
