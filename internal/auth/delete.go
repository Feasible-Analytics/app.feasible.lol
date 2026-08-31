//
// delete.go
// Deleting an account for real: the rows, the database file and the customer record.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// stripeTimeout caps the call that deletes a customer. A payment provider that
// is slow must not hold a delete request open, because the local deletion is
// what the user actually asked for.
const stripeTimeout = 15 * time.Second

// Deleter removes an account and everything belonging to it.
//
// The three pieces are done in a deliberate order: the payment provider first,
// then the control rows, then the file. The provider is first because it is the
// only step that can fail for a reason worth stopping on — a customer record we
// cannot delete is a customer record that can still be charged — while a file
// that fails to unlink after the rows are gone is an orphan we can find and
// remove later.
type Deleter struct {
	store   *Store
	manager *accounts.Manager
	dataDir string
	stripe  *Stripe
	log     *logger.Logger
}

// NewDeleter wires up the pieces account deletion touches.
func NewDeleter(store *Store, manager *accounts.Manager, dataDir string, stripe *Stripe, log *logger.Logger) *Deleter {
	return &Deleter{store: store, manager: manager, dataDir: dataDir, stripe: stripe, log: log}
}

// DeleteAccount removes a team, its owner and every trace of both.
//
// It really deletes. A privacy product whose "delete my account" leaves a
// hidden row and an orphaned database file has no honest answer to "what do you
// still hold about me", and the answer has to be "nothing" rather than "nothing
// you can see".
func (d *Deleter) DeleteAccount(ctx context.Context, userID, teamID int64) error {
	customerID, err := d.store.StripeCustomerID(ctx, teamID)
	if err != nil {
		return err
	}

	if customerID != "" {
		if err := d.stripe.DeleteCustomer(ctx, customerID); err != nil {
			return err
		}
	}

	if err := d.store.DeleteTeamRows(ctx, teamID, userID); err != nil {
		return err
	}

	dir := accounts.Dir(d.dataDir, teamID)
	if err := d.manager.Delete(teamID); err != nil {
		return fmt.Errorf("auth: delete account database %s: %w", dir, err)
	}

	if d.log != nil {
		d.log.Info("account deleted", "team", teamID, "user", userID, "dir", dir, "stripe_customer", customerID != "")
	}

	return nil
}

// Stripe is the smallest possible client for the one call this package makes.
//
// It is a hand-rolled request rather than the vendor SDK because deleting a
// customer is a single DELETE against one URL, and pulling in a large
// dependency — with its own HTTP stack and telemetry — to make one call would
// cost more than it saves in a binary whose whole pitch is that it has no
// moving parts.
type Stripe struct {
	SecretKey  string
	HTTPClient *http.Client
	Log        *logger.Logger
}

// NewStripe builds the client. An empty secret key is valid and means billing
// is not configured, which is the normal state of a self-hosted install.
func NewStripe(secretKey string, log *logger.Logger) *Stripe {
	return &Stripe{
		SecretKey:  strings.TrimSpace(secretKey),
		HTTPClient: &http.Client{Timeout: stripeTimeout},
		Log:        log,
	}
}

// Configured reports whether there is a payment provider to talk to.
func (s *Stripe) Configured() bool {
	return s != nil && s.SecretKey != ""
}

// DeleteCustomer removes the customer record, which cancels any subscription
// attached to it.
//
// A customer that is already gone is a success rather than an error: the goal
// is "this customer does not exist", and a delete that fails because somebody
// removed it in the dashboard first would block the account deletion for no
// reason.
func (s *Stripe) DeleteCustomer(ctx context.Context, customerID string) error {
	if !s.Configured() {
		if s != nil && s.Log != nil {
			s.Log.Warn("skipping the payment provider on account deletion: FEASIBLE_STRIPE_SECRET_KEY is not set",
				"customer", customerID)
		}

		return nil
	}

	endpoint := "https://api.stripe.com/v1/customers/" + url.PathEscape(customerID)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("auth: delete stripe customer: %w", err)
	}

	req.SetBasicAuth(s.SecretKey, "")

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: stripeTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: delete stripe customer: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	return fmt.Errorf("auth: delete stripe customer: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
