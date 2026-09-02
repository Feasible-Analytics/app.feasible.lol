//
// ses.go
// Sending through Amazon SES, and signing the request by hand.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// The SES v2 send endpoint. The host carries the region because SES is
// regional: an identity verified in one region does not exist in another, and a
// request signed for the wrong one is rejected with a message that names
// neither.
const (
	sesHostFormat = "email.%s.amazonaws.com"
	sesSendPath   = "/v2/email/outbound-emails"
)

// SigV4 constants. The service name is part of the signing key, so it has to
// match what SES expects rather than what the endpoint is called.
const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	sigV4Service   = "ses"
	sigV4Terminal  = "aws4_request"
)

// maxSESErrorBody caps how much of a failed response is read. SES answers an
// error with a short JSON object, but a proxy or a captive portal in front of
// it can answer with a whole HTML page, and putting that in a log line and an
// outcome column helps nobody.
const maxSESErrorBody = 8 << 10

// SESConfig is the AWS side of sending. The credentials are a static key pair
// rather than a provider chain because the deployment is a binary on a machine
// we own: there is no instance profile to inherit from, and a chain that
// silently found a developer's personal credentials would send a customer's
// password reset from the wrong account.
type SESConfig struct {
	// Region is where the sending identity is verified. It is separate from
	// every other AWS region this product might use, because SES lives where
	// the domain was verified and object storage will live somewhere else.
	Region string

	AccessKeyID     string
	SecretAccessKey string

	// From is the envelope sender. SES rejects an address outside a verified
	// identity, which is the same failure a relay produces for a From it does
	// not own.
	From string

	// ConfigurationSet names the SES configuration set that publishes bounce,
	// complaint and delivery events. Empty means send without one, which is
	// what an installation that has not set up event publishing wants.
	ConfigurationSet string

	// Timeout bounds the whole request. Without it a hung connection holds a
	// registration request open until the browser gives up, which reads to the
	// person signing up as "the product is broken".
	Timeout time.Duration
}

// SESTransport sends through the SES v2 API.
//
// It speaks HTTP and signs the request itself rather than taking the AWS SDK,
// for the same reason the SMTP transport is written against net/smtp: the
// product ships as one binary, and the whole of what we need is one signed POST
// against one endpoint. The SDK would add several hundred packages to build a
// request this file builds in eighty lines.
//
// The API is used rather than the SES SMTP interface because it reports the
// message identifier SES itself indexes by, so "did this message leave" and
// "what did SES do with it" are the same question rather than two.
type SESTransport struct {
	Config SESConfig
	Log    *logger.Logger

	// Client is injectable so a test can assert on the signed request without
	// reaching AWS. Nil means a client built from the configured timeout.
	Client *http.Client

	// Now is injectable so a test can assert on a fixed signature. Nil means
	// the real clock.
	Now func() time.Time
}

// sesRequest is the JSON body of a raw SES send.
//
// Raw content is used rather than SES's simple content so that both transports
// put the identical MIME document on the wire. A message that renders one way
// through a relay and another way through SES would make every rendering bug
// depend on which transport an installation happened to choose.
type sesRequest struct {
	FromEmailAddress     string         `json:"FromEmailAddress"`
	Destination          sesDestination `json:"Destination"`
	Content              sesContent     `json:"Content"`
	ConfigurationSetName string         `json:"ConfigurationSetName,omitempty"`
}

// sesDestination is the envelope recipient list. Only one address is ever
// filled: every message this product sends is addressed to one person, and a
// second recipient on a password reset would be a disclosure rather than a
// convenience.
type sesDestination struct {
	ToAddresses []string `json:"ToAddresses"`
}

// sesContent wraps the raw MIME document.
type sesContent struct {
	Raw sesRawMessage `json:"Raw"`
}

// sesRawMessage carries the base64 of the whole RFC 5322 message.
type sesRawMessage struct {
	Data string `json:"Data"`
}

// sesResponse is the useful half of what SES answers with. A success carries
// the message identifier; a failure carries a type and a sentence. Both
// spellings of the message key appear across AWS services, so both are read.
type sesResponse struct {
	MessageID string `json:"MessageId"`
	Type      string `json:"__type"`
	Message   string `json:"message"`
	Message2  string `json:"Message"`
}

// Send delivers one message through SES and returns what SES said.
//
// The SES message identifier is recorded in Detail because it is the only
// handle that ties a row in our outcome column to a line in SES's own event
// stream. Without it, answering "we sent it, what happened next" means guessing
// from a timestamp.
func (t *SESTransport) Send(ctx context.Context, msg Message) (Result, error) {
	result := Result{Transport: "ses"}

	region, err := sesRegion(t.Config.Region)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: %w", err)
	}

	if t.Config.AccessKeyID == "" || t.Config.SecretAccessKey == "" {
		result.Detail = "no AWS credentials configured"
		return result, fmt.Errorf("mail: the ses transport needs FEASIBLE_AWS_ACCESS_KEY_ID and FEASIBLE_AWS_SECRET_ACCESS_KEY")
	}

	from := t.Config.From
	if from == "" {
		from = DefaultFrom
	}

	// The MIME document is built without SMTP's dot-stuffing. SES takes a
	// document, not a DATA stream, so a doubled leading full stop would be
	// delivered to the reader rather than removed by the relay.
	raw := RenderMIME(from, msg)

	body, err := json.Marshal(sesRequest{
		FromEmailAddress:     from,
		Destination:          sesDestination{ToAddresses: []string{envelopeAddress(msg.To)}},
		Content:              sesContent{Raw: sesRawMessage{Data: base64.StdEncoding.EncodeToString([]byte(raw))}},
		ConfigurationSetName: strings.TrimSpace(t.Config.ConfigurationSet),
	})
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: build ses request: %w", err)
	}

	host := fmt.Sprintf(sesHostFormat, region)
	endpoint := "https://" + host + sesSendPath

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	t.sign(request, host, region, body)

	response, err := t.client().Do(request)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: post %s: %w", endpoint, err)
	}
	defer response.Body.Close() //nolint:errcheck // a read-only response body has nothing to report on close.

	// A rejection is read and reported verbatim. SES declines for reasons an
	// operator can act on — an unverified identity, a suppressed address, a
	// throttle — and collapsing all of them into "send failed" turns a
	// five-minute fix into an afternoon.
	if response.StatusCode < 200 || response.StatusCode > 299 {
		detail := sesFailure(response)
		result.Detail = detail

		return result, fmt.Errorf("mail: ses rejected the message: %s", detail)
	}

	var decoded sesResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxSESErrorBody)).Decode(&decoded); err != nil {
		// SES answered 200, so the message is accepted whatever the body looked
		// like. Losing the identifier is worth a note, not a failed send that
		// would be retried into a duplicate.
		result.Accepted = true
		result.Detail = "accepted by " + host + ", but its response could not be read: " + err.Error()

		return result, nil
	}

	result.Accepted = true
	result.Detail = "accepted by " + host + " as " + decoded.MessageID

	if t.Log != nil {
		t.Log.Info("mail sent", "to", msg.To, "subject", msg.Subject, "tag", msg.Tag, "relay", host, "ses_message_id", decoded.MessageID)
	}

	return result, nil
}

// client returns the HTTP client to send with, defaulting to one bounded by the
// configured timeout.
func (t *SESTransport) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}

	timeout := t.Config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &http.Client{Timeout: timeout}
}

// stamp returns the transport's clock, defaulting to the real one. The signing
// timestamp is the one input a test cannot supply any other way, and SES
// refuses a signature more than five minutes out of date.
func (t *SESTransport) stamp() time.Time {
	if t.Now == nil {
		return time.Now().UTC()
	}

	return t.Now().UTC()
}

// sign adds the SigV4 headers to the request.
//
// Only three headers are signed — content type, host and date — because those
// are the three that must not be rewritten in transit. Signing more would mean
// every proxy that adds a header of its own breaks the signature, and signing
// fewer would let one be altered without invalidating it.
func (t *SESTransport) sign(request *http.Request, host, region string, body []byte) {
	now := t.stamp()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	request.Header.Set("X-Amz-Date", amzDate)

	payloadHash := sha256Hex(body)

	// The canonical request is the exact byte sequence AWS rebuilds on its side
	// and hashes. Every newline and every lower-cased header name below is part
	// of the agreed format; a difference of one produces a signature mismatch
	// whose error message says only that the signature did not match.
	canonicalHeaders := "content-type:application/json\n" +
		"host:" + host + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "content-type;host;x-amz-date"

	canonicalRequest := strings.Join([]string{
		http.MethodPost,
		sesSendPath,
		"",
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, sigV4Service, sigV4Terminal}, "/")

	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// The signing key is derived rather than stored, so the secret access key
	// itself never travels and a leaked signature is usable only for that one
	// date, region and service.
	key := hmacSHA256([]byte("AWS4"+t.Config.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, sigV4Service)
	key = hmacSHA256(key, sigV4Terminal)

	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	request.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigV4Algorithm, t.Config.AccessKeyID, scope, signedHeaders, signature))
}

// sesFailure turns a rejection into one sentence worth logging. SES puts the
// reason in the body rather than in the status line, so a transport that
// reported only the status code would tell an operator "400" for a suppressed
// address, an unverified identity and a malformed request alike.
func sesFailure(response *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSESErrorBody))
	if err != nil {
		return fmt.Sprintf("HTTP %d, and its body could not be read: %v", response.StatusCode, err)
	}

	var decoded sesResponse
	if json.Unmarshal(raw, &decoded) == nil {
		message := decoded.Message
		if message == "" {
			message = decoded.Message2
		}

		switch {
		case decoded.Type != "" && message != "":
			return fmt.Sprintf("HTTP %d %s: %s", response.StatusCode, decoded.Type, message)
		case message != "":
			return fmt.Sprintf("HTTP %d: %s", response.StatusCode, message)
		case decoded.Type != "":
			return fmt.Sprintf("HTTP %d %s", response.StatusCode, decoded.Type)
		}
	}

	return fmt.Sprintf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(raw)))
}

// sesRegion validates the region before it is pasted into a hostname. A region
// is the one piece of SES configuration that becomes a URL, so anything outside
// the shape AWS actually uses is rejected here rather than turned into a
// request to somewhere unintended.
func sesRegion(region string) (string, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return "", fmt.Errorf("the ses transport needs FEASIBLE_AWS_SES_REGION")
	}

	for _, r := range region {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}

		return "", fmt.Errorf("FEASIBLE_AWS_SES_REGION: %q is not an AWS region", region)
	}

	return region, nil
}

// hmacSHA256 is one round of the signing-key derivation.
func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))

	return mac.Sum(nil)
}

// sha256Hex is the lower-case hex digest SigV4 asks for in two places: the
// payload hash inside the canonical request, and the hash of the canonical
// request inside the string to sign.
func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)

	return hex.EncodeToString(sum[:])
}
