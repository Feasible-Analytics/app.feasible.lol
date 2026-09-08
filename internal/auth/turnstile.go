//
// turnstile.go
// The human check in front of account creation.
//
// Created: 2026-09-08
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenField is the form field the Turnstile widget writes its token into. The
// name is fixed by Cloudflare's script, not chosen here.
const TokenField = "cf-turnstile-response"

// verifyURL is Cloudflare's server-side check. The token the browser produces
// proves nothing until this endpoint says it does.
const verifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// turnstileTimeout bounds the round trip. A sign-up form that hangs for thirty
// seconds because a third party is slow has already lost the person filling it
// in.
const turnstileTimeout = 5 * time.Second

// ErrTurnstileRejected is a token Cloudflare refused: absent, expired, already
// spent, or produced by something that did not look like a person. It is a
// message for the visitor rather than a fault to report.
var ErrTurnstileRejected = errors.New("auth: the human check was not passed")

// Turnstile verifies the token Cloudflare's widget puts in the sign-up form.
//
// It is a value with no credentials in the common case: a self-hosted install
// has no Cloudflare account, and the whole check switches itself off rather
// than making the operator get one.
type Turnstile struct {
	SiteKey   string
	SecretKey string

	// HTTPClient is a field so a test can answer without a network and so the
	// timeout is set in one place.
	HTTPClient *http.Client
}

// NewTurnstile builds the checker from configuration. Empty credentials are a
// supported state and produce an unconfigured value rather than an error.
func NewTurnstile(siteKey, secretKey string) *Turnstile {
	return &Turnstile{
		SiteKey:    strings.TrimSpace(siteKey),
		SecretKey:  strings.TrimSpace(secretKey),
		HTTPClient: &http.Client{Timeout: turnstileTimeout},
	}
}

// Configured reports whether the check can run at all. Both halves are needed:
// the site key draws the widget and the secret key verifies what it produced,
// and one without the other is a form that collects a token nobody checks.
func (t *Turnstile) Configured() bool {
	return t != nil && t.SiteKey != "" && t.SecretKey != ""
}

// DisabledReason explains in one line why sign-up is unprotected. It is what
// the process logs at start-up, so that an install missing the check knows it
// is missing it.
func (t *Turnstile) DisabledReason() string {
	switch {
	case t == nil || (t.SiteKey == "" && t.SecretKey == ""):
		return "the sign-up human check is off: set FEASIBLE_TURNSTILE_SITE_KEY and FEASIBLE_TURNSTILE_SECRET_KEY"
	case t.SiteKey == "":
		return "the sign-up human check is off: set FEASIBLE_TURNSTILE_SITE_KEY"
	case t.SecretKey == "":
		return "the sign-up human check is off: set FEASIBLE_TURNSTILE_SECRET_KEY"
	}

	return ""
}

// siteVerifyResponse is the answer Cloudflare returns. Only the verdict and the
// codes are read; the timestamp and hostname it also sends add nothing this
// caller acts on.
type siteVerifyResponse struct {
	Success bool     `json:"success"`
	Codes   []string `json:"error-codes"`
}

// Verify asks Cloudflare whether the token came from a person.
//
// It returns ErrTurnstileRejected for a verdict, and a wrapped transport error
// for anything that stopped the question being asked. The caller must treat
// both as a refusal: a check that lets everything through whenever it cannot
// reach Cloudflare is a check an attacker turns off by making it unreachable.
//
// `remoteIP` is optional context for Cloudflare's own scoring, and is sent only
// when the caller resolved one.
func (t *Turnstile) Verify(ctx context.Context, token, remoteIP string) error {
	if !t.Configured() {
		return nil
	}

	if strings.TrimSpace(token) == "" {
		return ErrTurnstileRejected
	}

	form := url.Values{"secret": {t.SecretKey}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, verifyURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build the human check request: %w", err)
	}

	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := t.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("reach the human check: %w", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the human check answered %d", response.StatusCode)
	}

	var decoded siteVerifyResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("read the human check answer: %w", err)
	}

	if !decoded.Success {
		return fmt.Errorf("%w (%s)", ErrTurnstileRejected, strings.Join(decoded.Codes, ", "))
	}

	return nil
}

// turnstileSiteKey is what the sign-up template draws the widget with. It is
// empty whenever the check is not fully configured, so a half-configured
// install renders no widget rather than one whose answer is never read.
func (h *Handler) turnstileSiteKey() string {
	if !h.Turnstile.Configured() {
		return ""
	}

	return h.Turnstile.SiteKey
}
