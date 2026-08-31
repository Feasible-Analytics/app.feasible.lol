//
// client.go
// A small, direct client for the six Stripe calls this product makes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package stripe talks to Stripe over its REST API.
//
// It is written by hand rather than by adding the official SDK because this
// product ships as one binary with a deliberately short dependency list, and
// the surface it actually needs is six calls, one signature check and a handful
// of fields. The SDK is a large dependency with its own release cadence and a
// generated model of every object Stripe has ever had; none of that helps here.
//
// Everything is form-encoded and every response is JSON, which is Stripe's own
// wire format rather than an abstraction over it — so anything in Stripe's
// documentation can be reproduced here directly.
package stripe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBase is Stripe's endpoint. It is a field on the client rather than a
// constant so tests can point at a local server, and so a future proxy is a
// configuration change rather than a code change.
const APIBase = "https://api.stripe.com"

const (
	// APIVersion pins the existing calls' response shapes. Without it Stripe
	// uses whatever version the account was created with, which means the same
	// binary can see different fields on two deployments.
	APIVersion = "2024-06-20"

	// ManagedPaymentsAPIVersion is the oldest API contract that supports Managed
	// Payments. It is scoped to checkout creation so enabling
	// Managed Payments cannot silently change subscription or invoice fields.
	ManagedPaymentsAPIVersion = "2025-03-31.basil"
)

// requestTimeout bounds a call. A webhook handler that reconciles against
// Stripe must not hold an HTTP connection open indefinitely because Stripe's
// own API is slow, since Stripe will retry the delivery anyway.
const requestTimeout = 20 * time.Second

// Client is an authenticated connection to one Stripe account.
type Client struct {
	// SecretKey is the sk_test_ or sk_live_ key. It is the only credential
	// Stripe's REST API takes.
	SecretKey string

	// BaseURL defaults to APIBase.
	BaseURL string

	// HTTP defaults to a client with a timeout. Go's default has none, which is
	// how a stalled TLS handshake becomes a stuck goroutine forever.
	HTTP *http.Client
}

// New builds a client for a secret key.
func New(secretKey string) *Client {
	return &Client{
		SecretKey: secretKey,
		BaseURL:   APIBase,
		HTTP:      &http.Client{Timeout: requestTimeout},
	}
}

// Configured reports whether there is a key to call with. A self-hosted install
// has no Stripe account at all, and every billing feature has to degrade to
// "not available here" rather than to a panic or a confusing 500.
func (c *Client) Configured() bool {
	return c != nil && strings.TrimSpace(c.SecretKey) != ""
}

// Error is a failure Stripe reported, rather than a network failure. Keeping
// the code and type is what lets a caller tell "this card was declined" from
// "this key is wrong", which need very different responses.
type Error struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

// Error renders the failure for a log line.
func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("stripe: %d %s (%s): %s", e.Status, e.Type, e.Code, e.Message)
	}

	return fmt.Sprintf("stripe: %d %s: %s", e.Status, e.Type, e.Message)
}

// errorEnvelope is Stripe's error shape, which nests the useful part.
type errorEnvelope struct {
	Error *Error `json:"error"`
}

// call performs one request with the package's default API version.
func (c *Client) call(ctx context.Context, method, path string, form url.Values, idempotencyKey string, out any) error {
	return c.callWithVersion(ctx, method, path, form, idempotencyKey, APIVersion, out)
}

// callWithVersion performs one request and decodes the result. Every method in
// this package goes through it, so authentication, the pinned API version, the
// idempotency key and error decoding cannot be forgotten on a new call.
//
// The idempotency key is not optional for writes. A create-checkout-session
// call that times out and is retried without one produces two sessions, and in
// the payment path a retry that creates a second object is the difference
// between a customer paying once and paying twice.
func (c *Client) callWithVersion(ctx context.Context, method, path string, form url.Values, idempotencyKey, apiVersion string, out any) error {
	if !c.Configured() {
		return fmt.Errorf("stripe: no secret key configured")
	}

	base := c.BaseURL
	if base == "" {
		base = APIBase
	}

	endpoint := strings.TrimRight(base, "/") + path

	var body io.Reader
	if method != http.MethodGet && form != nil {
		body = strings.NewReader(form.Encode())
	}
	if method == http.MethodGet && len(form) > 0 {
		endpoint += "?" + form.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("stripe: %s %s: %w", method, path, err)
	}

	req.SetBasicAuth(c.SecretKey, "")
	req.Header.Set("Stripe-Version", apiVersion)

	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("stripe: %s %s: read response: %w", method, path, err)
	}

	if response.StatusCode >= 400 {
		var envelope errorEnvelope
		if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != nil {
			envelope.Error.Status = response.StatusCode
			return envelope.Error
		}

		return &Error{Status: response.StatusCode, Type: "api_error", Message: strings.TrimSpace(string(raw))}
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("stripe: %s %s: decode response: %w", method, path, err)
	}

	return nil
}

// post is a write. Every one takes an idempotency key.
func (c *Client) post(ctx context.Context, path string, form url.Values, idempotencyKey string, out any) error {
	return c.call(ctx, http.MethodPost, path, form, idempotencyKey, out)
}

// postWithVersion is a write against an endpoint that requires a newer API
// contract than the rest of the client.
func (c *Client) postWithVersion(ctx context.Context, path string, form url.Values, idempotencyKey, apiVersion string, out any) error {
	return c.callWithVersion(ctx, http.MethodPost, path, form, idempotencyKey, apiVersion, out)
}

// get is a read. Reads need no idempotency key because repeating one changes
// nothing.
func (c *Client) get(ctx context.Context, path string, form url.Values, out any) error {
	return c.call(ctx, http.MethodGet, path, form, "", out)
}

// getWithVersion is a read against an endpoint whose response must use a newer
// API contract than the rest of the client.
func (c *Client) getWithVersion(ctx context.Context, path string, form url.Values, apiVersion string, out any) error {
	return c.callWithVersion(ctx, http.MethodGet, path, form, "", apiVersion, out)
}

// del removes an object.
func (c *Client) del(ctx context.Context, path string, out any) error {
	return c.delWithKey(ctx, path, "", out)
}

// delWithKey removes an object with a stable retry identity when the caller is
// performing a crash-recoverable provider transition.
func (c *Client) delWithKey(ctx context.Context, path, idempotencyKey string, out any) error {
	return c.call(ctx, http.MethodDelete, path, nil, idempotencyKey, out)
}

// decodeJSON is used by the webhook parser, which has a raw body rather than a
// response. It rejects trailing content so that a payload with something
// appended after the JSON object cannot be read as valid.
func decodeJSON(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("stripe: decode: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("stripe: decode: trailing JSON value")
		}
		return fmt.Errorf("stripe: decode: trailing content: %w", err)
	}

	return nil
}
