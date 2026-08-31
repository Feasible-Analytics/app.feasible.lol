//
// sign_test.go
// The signature contract, from the receiver's side.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package webhooks

import (
	"errors"
	"testing"
	"time"
)

// signedAt is the instant every signature in this file is made at.
var signedAt = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// TestSignatureVerifies is the happy path a customer's own implementation has to
// reproduce, so it is written the way they would write it: sign a body, hand the
// header and the body to a verifier, get nil back.
func TestSignatureVerifies(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_1","type":"goal.converted"}`)

	header := Sign(secret, body, signedAt)

	if err := Verify(header, body, signedAt, DefaultReplayWindow, secret); err != nil {
		t.Fatalf("a freshly signed delivery did not verify: %v", err)
	}
}

// TestSignatureRefusesEverythingElse walks the ways a delivery can fail to be
// ours. Each is a separate error value, because "this is not from you" and
// "this is from you but too old" call for different handling on the receiving
// end: the first is an attack and the second is usually a queue catching up.
func TestSignatureRefusesEverythingElse(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_1","type":"goal.converted"}`)
	header := Sign(secret, body, signedAt)

	cases := []struct {
		name   string
		header string
		body   []byte
		at     time.Time
		secret string
		want   error
	}{
		{
			name:   "a body that was edited in flight",
			header: header,
			body:   []byte(`{"id":"evt_1","type":"goal.converted","amount":9999}`),
			at:     signedAt,
			secret: secret,
			want:   ErrBadSignature,
		},
		{
			name:   "somebody else's secret",
			header: header,
			body:   body,
			at:     signedAt,
			secret: "whsec_not_ours",
			want:   ErrBadSignature,
		},
		{
			name:   "a captured delivery replayed an hour later",
			header: header,
			body:   body,
			at:     signedAt.Add(time.Hour),
			secret: secret,
			want:   ErrStale,
		},
		{
			name:   "a delivery stamped in the future",
			header: header,
			body:   body,
			at:     signedAt.Add(-time.Hour),
			secret: secret,
			want:   ErrStale,
		},
		{
			name:   "a header with no timestamp",
			header: "v1=abcdef",
			body:   body,
			at:     signedAt,
			secret: secret,
			want:   ErrMalformed,
		},
		{
			name:   "a header with no signature",
			header: "t=1756555200",
			body:   body,
			at:     signedAt,
			secret: secret,
			want:   ErrMalformed,
		},
		{
			name:   "a timestamp that is not a number",
			header: "t=lunchtime,v1=abcdef",
			body:   body,
			at:     signedAt,
			secret: secret,
			want:   ErrMalformed,
		},
		{
			name:   "an empty header",
			header: "",
			body:   body,
			at:     signedAt,
			secret: secret,
			want:   ErrMalformed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.header, tc.body, tc.at, DefaultReplayWindow, tc.secret)

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestTimestampIsInsideTheSignature is the point of binding the timestamp into
// the MAC. Without it, anybody who captured one valid delivery could freshen it
// forever by editing the header, and the replay window would be decoration.
func TestTimestampIsInsideTheSignature(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_1"}`)

	original := Sign(secret, body, signedAt)

	// Take the signature from the original and pair it with a fresh timestamp,
	// which is exactly what a replay attempt looks like.
	_, signature, err := parseSignature(original)
	if err != nil {
		t.Fatal(err)
	}

	later := signedAt.Add(30 * time.Minute)
	forged := "t=" + itoa(later.Unix()) + ",v1=" + signature

	if err := Verify(forged, body, later, DefaultReplayWindow, secret); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a re-stamped replay verified: %v", err)
	}
}

// TestRotationGracePeriodAcceptsBothSecrets checks that rotating does not break
// the deliveries already in flight. Without the grace period, every delivery
// between a rotation and the receiver's redeploy fails — which teaches people
// never to rotate, and a secret nobody rotates is not rotatable.
func TestRotationGracePeriodAcceptsBothSecrets(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)

	old := "whsec_old"
	next := "whsec_new"

	signedWithOld := Sign(old, body, signedAt)
	signedWithNew := Sign(next, body, signedAt)

	for _, header := range []string{signedWithOld, signedWithNew} {
		if err := Verify(header, body, signedAt, DefaultReplayWindow, next, old); err != nil {
			t.Fatalf("a delivery signed during the grace period did not verify: %v", err)
		}
	}

	// Once the old secret is out of the accepted set, a delivery signed with it
	// stops verifying — which is what makes rotation mean anything at all.
	if err := Verify(signedWithOld, body, signedAt, DefaultReplayWindow, next); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("the old secret still verified after the grace period: %v", err)
	}
}

// TestSecretsAreDistinct guards against a generator that is not actually random.
func TestSecretsAreDistinct(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 100; i++ {
		secret, err := NewSecret()
		if err != nil {
			t.Fatal(err)
		}

		if seen[secret] {
			t.Fatalf("NewSecret repeated itself after %d calls", i)
		}

		seen[secret] = true
	}
}

// itoa renders a unix timestamp for a header.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}

	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}

	return digits
}
