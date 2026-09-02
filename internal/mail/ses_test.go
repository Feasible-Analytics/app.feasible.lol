//
// ses_test.go
// What SES is actually sent, and what happens when it says no.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// capturedRoundTrip records the request the transport built and answers with a
// canned response. It replaces the network so the signature and the body can be
// asserted on without an AWS account.
type capturedRoundTrip struct {
	request *http.Request
	body    []byte

	status int
	answer string
}

// RoundTrip records and answers.
func (c *capturedRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	c.request = request

	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		c.body = body
	}

	status := c.status
	if status == 0 {
		status = http.StatusOK
	}

	answer := c.answer
	if answer == "" {
		answer = `{"MessageId":"0100019112ab-example"}`
	}

	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(answer)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    request,
	}, nil
}

// fixedClock is the signing timestamp a signature test needs, because SigV4
// folds the date into both the credential scope and the key.
func fixedClock() time.Time {
	return time.Date(2026, 9, 2, 22, 12, 0, 0, time.UTC)
}

// testSESTransport builds a transport wired to a captured round trip.
func testSESTransport(capture *capturedRoundTrip) *SESTransport {
	return &SESTransport{
		Config: SESConfig{
			Region:          "us-east-1",
			AccessKeyID:     "AKIAEXAMPLEKEYID0000",
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			From:            "feasible.lol <hello@feasible.lol>",
		},
		Client: &http.Client{Transport: capture},
		Now:    fixedClock,
	}
}

// TestSESSendPostsASignedRawMessage is the whole happy path: the endpoint, the
// signature headers, the envelope, and the MIME document SES is handed.
func TestSESSendPostsASignedRawMessage(t *testing.T) {
	capture := &capturedRoundTrip{}
	transport := testSESTransport(capture)

	result, err := transport.Send(context.Background(), Message{
		To:      "Owner <owner@example.com>",
		Subject: "Verify your email",
		Text:    "Your code is 123456",
		HTML:    "<p>Your code is 123456</p>",
		Tag:     "verify",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if !result.Accepted {
		t.Fatalf("result was not accepted: %s", result.Detail)
	}
	if result.Transport != "ses" {
		t.Fatalf("transport = %q, want ses", result.Transport)
	}
	if !strings.Contains(result.Detail, "0100019112ab-example") {
		t.Fatalf("detail does not carry the SES message id: %q", result.Detail)
	}

	if got := capture.request.URL.String(); got != "https://email.us-east-1.amazonaws.com/v2/email/outbound-emails" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := capture.request.Header.Get("X-Amz-Date"); got != "20260902T221200Z" {
		t.Fatalf("x-amz-date = %q", got)
	}

	// The scope and the signed-header list are the two halves of the
	// Authorization header AWS rebuilds its own signature from, so a change to
	// either is a change to what SES will accept.
	auth := capture.request.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=AKIAEXAMPLEKEYID0000/20260902/us-east-1/ses/aws4_request",
		"SignedHeaders=content-type;host;x-amz-date",
	} {
		if !strings.Contains(auth, want) {
			t.Fatalf("authorization header %q is missing %q", auth, want)
		}
	}

	var sent sesRequest
	if err := json.Unmarshal(capture.body, &sent); err != nil {
		t.Fatalf("decode request body: %v", err)
	}

	if sent.FromEmailAddress != "feasible.lol <hello@feasible.lol>" {
		t.Fatalf("from = %q", sent.FromEmailAddress)
	}

	// The display name must not reach the envelope. SES takes an address there,
	// and the header form is what produces a rejection naming nothing useful.
	if len(sent.Destination.ToAddresses) != 1 || sent.Destination.ToAddresses[0] != "owner@example.com" {
		t.Fatalf("destination = %v", sent.Destination.ToAddresses)
	}

	if sent.ConfigurationSetName != "" {
		t.Fatalf("configuration set = %q, want it omitted", sent.ConfigurationSetName)
	}

	raw, err := base64.StdEncoding.DecodeString(sent.Content.Raw.Data)
	if err != nil {
		t.Fatalf("decode raw message: %v", err)
	}
	for _, want := range []string{
		"From: feasible.lol <hello@feasible.lol>",
		"To: Owner <owner@example.com>",
		"Subject: Verify your email",
		"Content-Type: multipart/alternative;",
		"Your code is 123456",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("raw message is missing %q:\n%s", want, raw)
		}
	}
}

// TestSESSignatureMatchesTheSigV4Specification signs a fixed body with a fixed
// key and clock and compares the result against a value derived from the
// published algorithm by an unrelated implementation.
//
// It is worth pinning because SES answers a signing mistake with "the signature
// we calculated does not match", which names neither the header nor the field
// that differed. The body here is a literal rather than a rendered message,
// because a real one carries a Date header and would change every second.
func TestSESSignatureMatchesTheSigV4Specification(t *testing.T) {
	const want = "b80651ce3fa2b8896545e444cc17d187ee0d7cb470e7391ceb48efca68a84d81"

	transport := testSESTransport(&capturedRoundTrip{})

	request, err := http.NewRequest(http.MethodPost, "https://email.us-east-1.amazonaws.com"+sesSendPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")

	transport.sign(request, "email.us-east-1.amazonaws.com", "us-east-1", []byte(`{"probe":"fixed-body"}`))

	auth := request.Header.Get("Authorization")
	if !strings.Contains(auth, "Signature="+want) {
		t.Fatalf("authorization header %q does not carry signature %s", auth, want)
	}
}

// TestSESRawMessageIsNotDotStuffed keeps SMTP's wire escaping off the API path.
// A relay strips the doubled full stop back off; SES never saw a DATA command
// and would deliver the second stop to the reader.
func TestSESRawMessageIsNotDotStuffed(t *testing.T) {
	capture := &capturedRoundTrip{}
	transport := testSESTransport(capture)

	msg := Message{
		To:      "owner@example.com",
		Subject: "leading stop",
		Text:    "line one\n.hidden\nline three",
		HTML:    "<p>ok</p>",
		Tag:     "verify",
	}

	if _, err := transport.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	var sent sesRequest
	if err := json.Unmarshal(capture.body, &sent); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sent.Content.Raw.Data)
	if err != nil {
		t.Fatalf("decode raw message: %v", err)
	}

	if strings.Contains(string(raw), "..hidden") {
		t.Fatalf("the SES body was dot-stuffed:\n%s", raw)
	}
	if !strings.Contains(string(raw), "\r\n.hidden\r\n") {
		t.Fatalf("the SES body lost its leading stop:\n%s", raw)
	}

	// The SMTP form of the same message must still be stuffed, because that one
	// does end up in a DATA stream where a bare leading stop truncates it.
	if !strings.Contains(Render("hello@feasible.lol", msg), "\r\n..hidden\r\n") {
		t.Fatal("the SMTP body was not dot-stuffed")
	}
}

// TestSESRejectionCarriesTheReason keeps an operator's five-minute fix from
// becoming an afternoon. SES puts the actionable half in the body, so a
// transport reporting only the status code would say "400" for a suppressed
// address, an unverified identity and a malformed request alike.
func TestSESRejectionCarriesTheReason(t *testing.T) {
	capture := &capturedRoundTrip{
		status: http.StatusBadRequest,
		answer: `{"__type":"MessageRejected","message":"Email address is not verified."}`,
	}
	transport := testSESTransport(capture)

	result, err := transport.Send(context.Background(), Message{
		To: "owner@example.com", Subject: "hello", Text: "hello", HTML: "<p>hello</p>", Tag: "verify",
	})
	if err == nil {
		t.Fatal("a rejected message was reported as sent")
	}
	if result.Accepted {
		t.Fatal("a rejected message was marked accepted")
	}
	for _, want := range []string{"400", "MessageRejected", "Email address is not verified."} {
		if !strings.Contains(result.Detail, want) {
			t.Fatalf("detail %q is missing %q", result.Detail, want)
		}
	}
}

// TestSESConfigurationSetIsSentWhenConfigured checks the one optional field.
// Event publishing is how a bounce becomes visible to us at all, so a set that
// was configured and then silently dropped would take the reporting with it.
func TestSESConfigurationSetIsSentWhenConfigured(t *testing.T) {
	capture := &capturedRoundTrip{}
	transport := testSESTransport(capture)
	transport.Config.ConfigurationSet = "feasible-lol"

	if _, err := transport.Send(context.Background(), Message{
		To: "owner@example.com", Subject: "hello", Text: "hello", HTML: "<p>hello</p>", Tag: "verify",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var sent sesRequest
	if err := json.Unmarshal(capture.body, &sent); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if sent.ConfigurationSetName != "feasible-lol" {
		t.Fatalf("configuration set = %q", sent.ConfigurationSetName)
	}
}

// TestSESRegionIsValidatedBeforeItBecomesAHostname covers the one configured
// string that is pasted into a URL. A region carrying a separator would send a
// signed request somewhere nobody chose.
func TestSESRegionIsValidatedBeforeItBecomesAHostname(t *testing.T) {
	for _, region := range []string{"", "us-east-1/../evil", "us-east-1.example.com", "us east 1", "us-east-1@evil"} {
		if _, err := sesRegion(region); err == nil {
			t.Fatalf("region %q was accepted", region)
		}
	}

	got, err := sesRegion("  US-EAST-1  ")
	if err != nil {
		t.Fatalf("a real region was refused: %v", err)
	}
	if got != "us-east-1" {
		t.Fatalf("region = %q, want us-east-1", got)
	}
}

// TestSESSendRefusesWithoutCredentials fails the send rather than posting an
// unsigned request, so the error names the missing variables instead of arriving
// as an AWS signature complaint.
func TestSESSendRefusesWithoutCredentials(t *testing.T) {
	transport := &SESTransport{Config: SESConfig{Region: "us-east-1"}}

	result, err := transport.Send(context.Background(), Message{
		To: "owner@example.com", Subject: "hello", Text: "hello", HTML: "<p>hello</p>",
	})
	if err == nil {
		t.Fatal("a send with no credentials was reported as sent")
	}
	if result.Accepted {
		t.Fatal("a send with no credentials was marked accepted")
	}
	if !strings.Contains(err.Error(), "FEASIBLE_AWS_ACCESS_KEY_ID") {
		t.Fatalf("error does not name the missing variable: %v", err)
	}
}

// TestNewBuildsTheSESTransport checks the wiring from configuration, including
// that an incomplete one is refused at start-up rather than at the moment a
// password reset is due.
func TestNewBuildsTheSESTransport(t *testing.T) {
	mailer, err := New(Options{
		Transport:          "ses",
		From:               "feasible.lol <hello@feasible.lol>",
		BaseURL:            "https://app.feasible.lol",
		AWSAccessKeyID:     "AKIAEXAMPLEKEYID0000",
		AWSSecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SESRegion:          "us-east-1",
	})
	if err != nil {
		t.Fatalf("build the ses mailer: %v", err)
	}
	if _, ok := mailer.transport.(*SESTransport); !ok {
		t.Fatalf("transport is %T, want *SESTransport", mailer.transport)
	}

	if _, err := New(Options{Transport: "ses", SESRegion: "us-east-1"}); err == nil {
		t.Fatal("a ses mailer with no credentials was built")
	}

	if _, err := New(Options{
		Transport:          "ses",
		AWSAccessKeyID:     "AKIAEXAMPLEKEYID0000",
		AWSSecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SESRegion:          "not a region",
	}); err == nil {
		t.Fatal("a ses mailer with a malformed region was built")
	}
}

// TestSESAcceptsAnUnreadableSuccessBody keeps a 200 from becoming a retry. SES
// took the message; losing the identifier is worth a note in the detail, not a
// second copy in somebody's inbox.
func TestSESAcceptsAnUnreadableSuccessBody(t *testing.T) {
	capture := &capturedRoundTrip{answer: "not json"}
	transport := testSESTransport(capture)

	result, err := transport.Send(context.Background(), Message{
		To: "owner@example.com", Subject: "hello", Text: "hello", HTML: "<p>hello</p>", Tag: "verify",
	})
	if err != nil {
		t.Fatalf("an accepted message was reported as failed: %v", err)
	}
	if !result.Accepted {
		t.Fatal("an accepted message was not marked accepted")
	}
	if !strings.Contains(result.Detail, "could not be read") {
		t.Fatalf("detail does not say the response was unreadable: %q", result.Detail)
	}
}
