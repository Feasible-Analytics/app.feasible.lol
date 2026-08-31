//
// sign.go
// Signing a delivery so the receiver can prove it came from us and is not a replay.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package webhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The headers a delivery carries. They are named rather than assembled inline
// so that the sender and the verifier cannot drift apart, which is the classic
// way a webhook signature ends up unverifiable in the field.
const (
	// SignatureHeader is `t=<unix seconds>,v1=<hex hmac>`. The timestamp is
	// inside the header and inside the signed string, so moving it changes the
	// signature — a replay cannot be freshened by editing the header.
	SignatureHeader = "Feasible-Signature"

	// EventHeader is the event type, so a receiver can route without parsing
	// the body first.
	EventHeader = "Feasible-Event"

	// DeliveryHeader is the delivery id, which changes on every retry and every
	// manual redelivery.
	DeliveryHeader = "Feasible-Delivery"

	// EventIDHeader is stable across retries of the same event. It is the value
	// a receiver keys its own idempotency on: at-least-once delivery means the
	// same event will arrive twice eventually, and the receiver is the only
	// place that can decide what to do about it.
	EventIDHeader = "Feasible-Event-Id"
)

// signatureVersion is the scheme marker. It is in the header so a future scheme
// can be sent alongside this one and receivers can migrate at their own pace,
// rather than everyone's integration breaking on the day we change algorithm.
const signatureVersion = "v1"

// DefaultReplayWindow is how far a delivery's timestamp may be from the
// receiver's clock. Five minutes tolerates ordinary clock drift and a queue
// backlog, and is short enough that a captured request is not useful for long.
const DefaultReplayWindow = 5 * time.Minute

// secretBytes is the size of a signing secret. Thirty-two random bytes is a
// full-strength HMAC-SHA256 key.
const secretBytes = 32

// SecretPrefix marks a webhook secret so one pasted into a support ticket is
// recognisable, and so it cannot be confused with an API key.
const SecretPrefix = "whsec_"

// Errors a receiver-side verifier distinguishes. They are separate values
// because "this is not from you" and "this is from you but too old" call for
// different handling: the first is an attack, the second is usually a queue
// that caught up late.
var (
	ErrBadSignature = errors.New("webhook signature does not match")
	ErrStale        = errors.New("webhook timestamp is outside the replay window")
	ErrMalformed    = errors.New("webhook signature header is malformed")
)

// NewSecret mints a signing secret.
func NewSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("webhooks: new secret: %w", err)
	}

	return SecretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Sign builds the signature header for one body at one instant.
//
// The signed string is `<timestamp>.<body>` rather than the body alone. Signing
// only the body lets anybody who ever captured one valid request replay it
// forever; binding the timestamp into the MAC is what makes the receiver's
// freshness check something an attacker cannot route around.
func Sign(secret string, body []byte, at time.Time) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)

	return fmt.Sprintf("t=%s,%s=%s", timestamp, signatureVersion, mac(secret, timestamp, body))
}

// mac computes the hex HMAC over the timestamped body.
func mac(secret, timestamp string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(timestamp))
	h.Write([]byte("."))
	h.Write(body)

	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks a signature header against a body, a set of acceptable secrets
// and a clock. It is written from the receiver's point of view and exported
// because our own tests are the first receiver, and because it is the reference
// a customer's implementation is checked against.
//
// More than one secret is accepted so that a rotation has a grace period: the
// old secret keeps verifying until it expires, which is what lets a customer
// rotate without a window where deliveries fail.
func Verify(header string, body []byte, now time.Time, window time.Duration, secrets ...string) error {
	timestamp, signature, err := parseSignature(header)
	if err != nil {
		return err
	}

	at := time.Unix(timestamp, 0)

	// The check is on the absolute difference: a delivery stamped in the future
	// is as suspicious as one stamped last week, and usually means a clock that
	// is wrong in a way worth noticing.
	if window > 0 {
		drift := now.Sub(at)
		if drift < 0 {
			drift = -drift
		}
		if drift > window {
			return ErrStale
		}
	}

	stamp := strconv.FormatInt(timestamp, 10)

	for _, secret := range secrets {
		if secret == "" {
			continue
		}

		if subtle.ConstantTimeCompare([]byte(mac(secret, stamp, body)), []byte(signature)) == 1 {
			return nil
		}
	}

	return ErrBadSignature
}

// parseSignature splits `t=...,v1=...` into its parts. It tolerates the fields
// arriving in either order and ignores versions it does not know, so that
// adding a second scheme later does not break this parser.
func parseSignature(header string) (int64, string, error) {
	var (
		timestamp int64
		signature string
		haveTime  bool
	)

	for _, part := range strings.Split(header, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}

		switch name {
		case "t":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, "", ErrMalformed
			}
			timestamp, haveTime = parsed, true

		case signatureVersion:
			signature = value
		}
	}

	if !haveTime || signature == "" {
		return 0, "", ErrMalformed
	}

	return timestamp, signature, nil
}
