//
// preflight.go
// Read-only Stripe configuration checks and an opt-in Managed Payments smoke.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"
	"fmt"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// PreflightStatus is whether one deployment requirement passed, failed, or
// still needs the opt-in checkout smoke that Stripe cannot expose as a field.
type PreflightStatus string

const (
	PreflightPass     PreflightStatus = "PASS"
	PreflightFail     PreflightStatus = "FAIL"
	PreflightRequired PreflightStatus = "REQUIRED"
)

// PreflightCheck is one operator-readable deployment assertion.
type PreflightCheck struct {
	Status PreflightStatus
	Name   string
	Detail string
}

// PreflightReport is the complete Managed Payments deployment result.
type PreflightReport struct {
	Checks []PreflightCheck
}

// Ready reports whether every check, including the checkout smoke, passed.
func (r PreflightReport) Ready() bool {
	if len(r.Checks) == 0 {
		return false
	}

	for _, check := range r.Checks {
		if check.Status != PreflightPass {
			return false
		}
	}

	return true
}

// add appends one result without exposing slice mutation throughout the check.
func (r *PreflightReport) add(status PreflightStatus, name, detail string) {
	r.Checks = append(r.Checks, PreflightCheck{Status: status, Name: name, Detail: detail})
}

// Preflight verifies everything Stripe exposes without changing it. When smoke
// is true, it also creates a customerless Checkout Session and immediately
// expires it. That final step is the only API-level way to prove activation,
// accepted terms, tax-code eligibility, and the actual Checkout parameters.
func (s *Service) Preflight(ctx context.Context, smoke bool) PreflightReport {
	var report PreflightReport

	if s == nil || s.Stripe == nil || !s.Stripe.Configured() {
		report.add(PreflightFail, "Stripe credentials", "FEASIBLE_STRIPE_SECRET_KEY is not configured")
		return report
	}
	report.add(PreflightPass, "Stripe credentials", "a secret key is configured; API access is checked below")

	product, catalogReady := s.preflightProduct(ctx, &report)
	monthlyReady := s.preflightPrice(ctx, &report, "monthly price", s.Plans.Monthly, product, 999, "month")
	yearlyReady := s.preflightPrice(ctx, &report, "yearly price", s.Plans.Yearly, product, 10000, "year")

	if strings.TrimSpace(s.WebhookSecret) == "" {
		report.add(PreflightFail, "Webhook signing secret", "FEASIBLE_STRIPE_WEBHOOK_SECRET is empty")
	} else {
		report.add(PreflightPass, "Webhook signing secret", "a signing secret is configured")
	}

	s.preflightWebhook(ctx, &report)

	if !smoke {
		report.add(PreflightRequired, "Managed Payments checkout smoke",
			"activation, accepted terms, and tax-code eligibility are not readable API fields; rerun with --checkout-smoke")
		return report
	}

	if !catalogReady || !monthlyReady || !yearlyReady {
		report.add(PreflightFail, "Managed Payments checkout smoke", "skipped because the product or prices failed validation")
		return report
	}

	s.preflightCheckout(ctx, &report)

	return report
}

// preflightProduct checks the configured catalogue object and returns whether a
// checkout smoke can safely use its prices.
func (s *Service) preflightProduct(ctx context.Context, report *PreflightReport) (*stripe.Product, bool) {
	if strings.TrimSpace(s.Plans.Product) == "" {
		report.add(PreflightFail, "Product", "FEASIBLE_STRIPE_PRODUCT is empty")
		return nil, false
	}

	product, err := s.Stripe.GetProduct(ctx, s.Plans.Product)
	if err != nil {
		report.add(PreflightFail, "Product", err.Error())
		return nil, false
	}

	var problems []string
	if !product.Active {
		problems = append(problems, "product is inactive")
	}
	if product.TaxCode == "" {
		problems = append(problems, "product has no tax code")
	}
	if product.Shippable != nil && *product.Shippable {
		problems = append(problems, "product is marked shippable, but Managed Payments supports digital products")
	}

	if len(problems) > 0 {
		report.add(PreflightFail, "Product", strings.Join(problems, "; "))
		return product, false
	}

	mode := "test"
	if product.LiveMode {
		mode = "live"
	}
	report.add(PreflightPass, "Product", fmt.Sprintf("%s (%s) is active with tax code %s in %s mode", product.Name, product.ID, product.TaxCode, mode))

	return product, true
}

// preflightPrice checks one configured price against both checkout mechanics
// and the public price copy customers see before clicking the button.
func (s *Service) preflightPrice(ctx context.Context, report *PreflightReport, name, id string, product *stripe.Product, amount int64, interval string) bool {
	if strings.TrimSpace(id) == "" {
		report.add(PreflightFail, name, "price id is empty")
		return false
	}

	price, err := s.Stripe.GetPrice(ctx, id)
	if err != nil {
		report.add(PreflightFail, name, err.Error())
		return false
	}

	var problems []string
	if !price.Active {
		problems = append(problems, "price is inactive")
	}
	if product != nil && price.Product != product.ID {
		problems = append(problems, fmt.Sprintf("belongs to %s, not %s", price.Product, product.ID))
	}
	if product != nil && price.LiveMode != product.LiveMode {
		problems = append(problems, "test/live mode differs from the product")
	}
	if price.Type != "recurring" || price.Recurring == nil {
		problems = append(problems, "price is not recurring")
	} else if price.Recurring.Interval != interval || price.Recurring.IntervalCount != 1 {
		problems = append(problems, fmt.Sprintf("recurs every %d %s, want every 1 %s", price.Recurring.IntervalCount, price.Recurring.Interval, interval))
	}
	if price.Currency != "usd" || price.UnitAmount != amount {
		problems = append(problems, fmt.Sprintf("amount is %d %s, want %d usd", price.UnitAmount, price.Currency, amount))
	}
	if price.TaxBehavior != "exclusive" {
		problems = append(problems, fmt.Sprintf("tax_behavior is %q, want explicit exclusive to match pricing copy", price.TaxBehavior))
	}

	if len(problems) > 0 {
		report.add(PreflightFail, name, strings.Join(problems, "; "))
		return false
	}

	report.add(PreflightPass, name, fmt.Sprintf("%s is active, recurring every %s, and charges %d usd before tax", price.ID, interval, amount))

	return true
}

// preflightWebhook verifies that the deployed endpoint exists and subscribes
// to every event the handler uses, including both asynchronous outcomes.
func (s *Service) preflightWebhook(ctx context.Context, report *PreflightReport) {
	if strings.TrimSpace(s.BaseURL) == "" {
		report.add(PreflightFail, "Webhook endpoint", "FEASIBLE_APP_BASE_URL is empty")
		return
	}

	endpoints, err := s.Stripe.ListWebhookEndpoints(ctx)
	if err != nil {
		report.add(PreflightFail, "Webhook endpoint", err.Error())
		return
	}

	wantURL := strings.TrimRight(s.BaseURL, "/") + "/webhooks/stripe"
	for _, endpoint := range endpoints {
		if endpoint.URL != wantURL {
			continue
		}

		if endpoint.Status != "enabled" {
			report.add(PreflightFail, "Webhook endpoint", fmt.Sprintf("%s is %s", wantURL, endpoint.Status))
			return
		}

		missing := missingWebhookEvents(endpoint.EnabledEvents)
		if len(missing) > 0 {
			report.add(PreflightFail, "Webhook endpoint", "missing events: "+strings.Join(missing, ", "))
			return
		}

		report.add(PreflightPass, "Webhook endpoint", wantURL+" is enabled with every required event")
		return
	}

	report.add(PreflightFail, "Webhook endpoint", "no enabled Stripe webhook matches "+wantURL)
}

// missingWebhookEvents returns the subscriptions absent from one endpoint.
func missingWebhookEvents(enabled []string) []string {
	for _, eventType := range enabled {
		if eventType == "*" {
			return nil
		}
	}

	present := make(map[string]bool, len(enabled))
	for _, eventType := range enabled {
		present[eventType] = true
	}

	required := []string{
		stripe.EventCheckoutCompleted,
		stripe.EventCheckoutAsyncPaymentSucceeded,
		stripe.EventCheckoutAsyncPaymentFailed,
		stripe.EventSubscriptionCreated,
		stripe.EventSubscriptionUpdated,
		stripe.EventSubscriptionDeleted,
		stripe.EventSubscriptionPaused,
		stripe.EventSubscriptionResumed,
		stripe.EventInvoicePaymentSucceed,
		stripe.EventInvoicePaymentFailed,
	}

	var missing []string
	for _, eventType := range required {
		if !present[eventType] {
			missing = append(missing, eventType)
		}
	}

	return missing
}

// preflightCheckout proves Stripe accepts the same Managed Payments parameters
// used for customers, then expires the untouched session immediately.
func (s *Service) preflightCheckout(ctx context.Context, report *PreflightReport) {
	stamp := s.now().Format("20060102T150405.000000000")
	session, err := s.Stripe.CreateCheckoutSession(ctx, stripe.CheckoutParams{
		PriceID:        s.Plans.Monthly,
		SuccessURL:     strings.TrimRight(s.BaseURL, "/") + "/billing/done?session={CHECKOUT_SESSION_ID}",
		CancelURL:      strings.TrimRight(s.BaseURL, "/") + "/pricing",
		IdempotencyKey: "managed-payments-preflight-create-" + stamp,
	})
	if err != nil {
		report.add(PreflightFail, "Managed Payments checkout smoke", err.Error())
		return
	}

	if err := s.Stripe.ExpireCheckoutSession(ctx, session.ID, "managed-payments-preflight-expire-"+session.ID); err != nil {
		report.add(PreflightFail, "Managed Payments checkout smoke",
			fmt.Sprintf("Stripe accepted session %s but it could not be expired: %v; expire it in the Dashboard", session.ID, err))
		return
	}

	report.add(PreflightPass, "Managed Payments checkout smoke",
		"Stripe accepted and expired a customerless session; activation, terms, product tax-code eligibility, and request compatibility are verified")
}
