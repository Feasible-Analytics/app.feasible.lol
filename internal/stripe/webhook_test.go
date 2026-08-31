//
// webhook_test.go
// The signature check, which is the only thing between a public URL and billing state.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package stripe

import (
	"strings"
	"testing"
	"time"
)

// secret is the signing secret every test in this file verifies against.
const secret = "whsec_test_only_do_not_use_anywhere"

// sentAt is a fixed instant so the tolerance arithmetic is deterministic.
var sentAt = time.Date(2026, time.March, 3, 12, 0, 0, 0, time.UTC)

// payload is a minimal but real-shaped delivery.
const payload = `{
  "id": "evt_test_1",
  "type": "invoice.payment_failed",
  "created": 1772539200,
  "data": {"object": {"id": "in_1", "object": "invoice", "customer": "cus_1", "subscription": "sub_1",
                      "metadata": {"feasible_team_id": "7"}}}
}`

// TestValidSignatureIsAccepted is the happy path, exercised through the real
// verification rather than a bypass. A test that skipped the signature would
// leave the one check protecting billing state completely untested.
func TestValidSignatureIsAccepted(t *testing.T) {
	header := SignPayload([]byte(payload), secret, sentAt)

	event, err := ParseWebhook([]byte(payload), header, secret, sentAt)
	if err != nil {
		t.Fatal(err)
	}

	if event.ID != "evt_test_1" {
		t.Errorf("event id is %q", event.ID)
	}
	if event.Type != EventInvoicePaymentFailed {
		t.Errorf("event type is %q", event.Type)
	}
	if got := event.CustomerID(); got != "cus_1" {
		t.Errorf("customer is %q, want cus_1", got)
	}
	if got := event.TeamID(); got != 7 {
		t.Errorf("team is %d, want 7", got)
	}

	// The raw bytes are kept verbatim, because the stored payload is what a
	// support person reads a month later — not what our structs could parse.
	if string(event.Raw) != payload {
		t.Error("the raw payload was not preserved")
	}
}

// TestForgedSignatureIsRejected is the whole point of the check. Without it,
// anybody who guesses the endpoint can mark any account as paid.
func TestForgedSignatureIsRejected(t *testing.T) {
	header := SignPayload([]byte(payload), "some-other-secret", sentAt)

	if _, err := ParseWebhook([]byte(payload), header, secret, sentAt); err == nil {
		t.Fatal("a delivery signed with the wrong secret was accepted")
	}
}

// TestATamperedBodyIsRejected covers a valid signature over different bytes,
// which is what an attacker who captured one real delivery would try.
func TestATamperedBodyIsRejected(t *testing.T) {
	header := SignPayload([]byte(payload), secret, sentAt)
	tampered := strings.Replace(payload, `"7"`, `"9"`, 1)

	if _, err := ParseWebhook([]byte(tampered), header, secret, sentAt); err == nil {
		t.Fatal("a modified payload was accepted with the original signature")
	}
}

// TestAStaleDeliveryIsRejected is the replay guard. A delivery far outside the
// tolerance is a replay of something we may already have acted on.
func TestAStaleDeliveryIsRejected(t *testing.T) {
	header := SignPayload([]byte(payload), secret, sentAt)

	tooLate := sentAt.Add(SignatureTolerance + time.Minute)
	if _, err := ParseWebhook([]byte(payload), header, secret, tooLate); err == nil {
		t.Fatal("a delivery older than the tolerance was accepted")
	}

	tooEarly := sentAt.Add(-SignatureTolerance - time.Minute)
	if _, err := ParseWebhook([]byte(payload), header, secret, tooEarly); err == nil {
		t.Fatal("a delivery from the future was accepted")
	}

	// Just inside the window still works, or a box with a slightly slow clock
	// would silently stop receiving payments.
	justInside := sentAt.Add(SignatureTolerance - time.Second)
	if _, err := ParseWebhook([]byte(payload), header, secret, justInside); err != nil {
		t.Fatalf("a delivery inside the tolerance was rejected: %v", err)
	}
}

// TestSecretRotationAcceptsEitherSignature covers the window during a rotation,
// when the provider signs with both the old and the new secret.
func TestSecretRotationAcceptsEitherSignature(t *testing.T) {
	old := SignPayload([]byte(payload), "whsec_old", sentAt)
	current := SignPayload([]byte(payload), secret, sentAt)

	// Both signatures in one header, as the provider sends during a rotation.
	combined := old + ",v1=" + strings.SplitN(current, "v1=", 2)[1]

	if _, err := ParseWebhook([]byte(payload), combined, secret, sentAt); err != nil {
		t.Fatalf("a header carrying two signatures was rejected: %v", err)
	}
}

// TestMalformedHeadersAreRejected covers the shapes a probe produces.
func TestMalformedHeadersAreRejected(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"no timestamp":     "v1=deadbeef",
		"no signature":     "t=1772539200",
		"bad timestamp":    "t=not-a-number,v1=deadbeef",
		"unknown scheme":   "t=1772539200,v0=deadbeef",
		"not hex":          "t=1772539200,v1=zzzz",
		"nonsense entries": "hello,world",
	}

	for name, header := range cases {
		if _, err := ParseWebhook([]byte(payload), header, secret, sentAt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestNoSecretConfiguredRefusesEverything is the default an install starts in.
// An endpoint that changes billing state must refuse rather than trust when it
// has nothing to verify against.
func TestNoSecretConfiguredRefusesEverything(t *testing.T) {
	header := SignPayload([]byte(payload), secret, sentAt)

	if _, err := ParseWebhook([]byte(payload), header, "", sentAt); err == nil {
		t.Fatal("a delivery was accepted with no signing secret configured")
	}
}

// TestAnEventWithNoIDIsRejected guards the deduplication key. An event with no
// id could never be recognised as a duplicate, so it must not be handled at all.
func TestAnEventWithNoIDIsRejected(t *testing.T) {
	body := `{"type":"invoice.payment_failed","data":{"object":{}}}`
	header := SignPayload([]byte(body), secret, sentAt)

	if _, err := ParseWebhook([]byte(body), header, secret, sentAt); err == nil {
		t.Fatal("an event with no id was accepted")
	}
}

// TestCustomerIDIsFoundOnEveryObjectShape checks the routing fallback. Each of
// the object types this product acts on puts the customer somewhere slightly
// different, and an event we cannot route is an event we cannot act on.
func TestCustomerIDIsFoundOnEveryObjectShape(t *testing.T) {
	cases := map[string]string{
		`{"id":"evt_1","type":"customer.subscription.updated","data":{"object":{"id":"sub_1","object":"subscription","customer":"cus_a"}}}`: "cus_a",
		`{"id":"evt_2","type":"invoice.payment_succeeded","data":{"object":{"id":"in_1","object":"invoice","customer":"cus_b"}}}`:           "cus_b",
		`{"id":"evt_3","type":"checkout.session.completed","data":{"object":{"id":"cs_1","object":"checkout.session","customer":"cus_c"}}}`: "cus_c",
		`{"id":"evt_4","type":"customer.deleted","data":{"object":{"id":"cus_d","object":"customer"}}}`:                                     "cus_d",
		`{"id":"evt_5","type":"customer.subscription.deleted","data":{"object":{"id":"sub_2","object":"subscription","customer":"cus_e"}}}`: "cus_e",
	}

	for body, want := range cases {
		header := SignPayload([]byte(body), secret, sentAt)

		event, err := ParseWebhook([]byte(body), header, secret, sentAt)
		if err != nil {
			t.Fatal(err)
		}

		if got := event.CustomerID(); got != want {
			t.Errorf("customer for %s is %q, want %q", event.Type, got, want)
		}
	}
}

// TestMetadataWithNoTeamIsZero makes sure an object created by hand in the
// provider's dashboard — which has no metadata — does not crash the handler or
// route to a nonsense account.
func TestMetadataWithNoTeamIsZero(t *testing.T) {
	body := `{"id":"evt_9","type":"customer.subscription.updated","data":{"object":{"customer":"cus_x"}}}`
	header := SignPayload([]byte(body), secret, sentAt)

	event, err := ParseWebhook([]byte(body), header, secret, sentAt)
	if err != nil {
		t.Fatal(err)
	}

	if got := event.TeamID(); got != 0 {
		t.Errorf("team is %d, want 0", got)
	}

	for _, bad := range []Meta{{}, {TeamMetadataKey: ""}, {TeamMetadataKey: "abc"}, {TeamMetadataKey: "0"}, {TeamMetadataKey: "-3"}} {
		if got := bad.TeamID(); got != 0 {
			t.Errorf("%v gave team %d, want 0", bad, got)
		}
	}
}
