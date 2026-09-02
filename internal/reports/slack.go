//
// slack.go
// Delivering a report or an alert to a channel instead of an inbox.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/outbound"
)

// SlackTimeout bounds a webhook call. A notifier that blocks on a hung webhook
// holds a queue running at concurrency one, which would stop every other site's
// report behind one broken integration.
const SlackTimeout = 10 * time.Second

// SlackPoster delivers a message to a webhook. It is an interface so tests
// assert on what would have been posted without a network, and so a future
// second chat destination is a second implementation rather than a branch.
type SlackPoster interface {
	Post(ctx context.Context, webhookURL, text string) error
}

// ValidateWebhookURL refuses a destination the poster would not be allowed to
// reach, at the moment the customer saves it, so the reason appears on the
// form rather than in a failed job hours later.
//
// It checks the address only when the URL already carries one, so saving a
// form costs no DNS lookup. A hostname is resolved and checked again by the
// poster's guarded client at connect time, which is the check that stops the
// request.
func ValidateWebhookURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return errors.New("the chat webhook must be a full URL, starting with https://")
	}

	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return errors.New("the chat webhook must start with https:// or http://")
	}

	if addr, addrErr := netip.ParseAddr(parsed.Hostname()); addrErr == nil {
		if clientip.IsPrivateOrLocal(addr) && !addr.Unmap().IsLoopback() {
			return errors.New("the chat webhook points at a private or local network address, which is not allowed")
		}
	}

	return nil
}

// Slack posts to an incoming webhook.
type Slack struct {
	// Client dials through outbound.Policy, so the webhook URL a customer
	// typed into the reports form is resolved and checked again at connect
	// time. Without it the field is a way to make this process POST to
	// anything on its own network and hand the answer back in the failure the
	// screen renders.
	Client *http.Client
}

// NewSlack builds a poster whose client refuses a private or local
// destination. The caller passes the policy the process runs under: a
// self-hoster's chat relay is often on the same box, so loopback is allowed
// there and refused in hosted production.
func NewSlack(policy outbound.Policy) *Slack {
	return &Slack{Client: policy.NewClient(SlackTimeout)}
}

// Post sends one message.
//
// The payload is the minimal `{"text": ...}` form on purpose. A rich block
// layout is a second rendering of every report to keep in step with the email
// one, and a chat message is read in three seconds — the value is in the
// numbers arriving at all, not in their typography.
func (s *Slack) Post(ctx context.Context, webhookURL, text string) error {
	if strings.TrimSpace(webhookURL) == "" {
		return fmt.Errorf("reports: no Slack webhook is configured")
	}

	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("reports: encode Slack payload: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reports: build Slack request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")

	// A poster built as a literal rather than through NewSlack still gets the
	// guarded client, so there is no shape of this struct that dials anywhere
	// the policy refuses.
	client := s.Client
	if client == nil {
		client = outbound.Policy{AllowLoopback: true}.NewClient(SlackTimeout)
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("reports: post to Slack: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	// A webhook answers a plain-text reason on failure, and that reason —
	// "no_service", "invalid_payload" — is the only thing that tells somebody
	// their webhook has been revoked. Swallowing it leaves them with a channel
	// that simply stopped receiving reports.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))

		return fmt.Errorf("reports: Slack answered %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}

	return nil
}

// SlackText renders a report as the plain text a chat message carries. It is
// built from the same Rendered value the email uses so that the two cannot
// report different numbers.
func SlackText(rendered Rendered, dashboardURL string) string {
	var out strings.Builder

	out.WriteString("*" + rendered.Subject + "*\n")

	// The text alternative is already a readable summary and is generated from
	// the same data as the HTML, which is exactly what a chat message wants.
	body := strings.TrimSpace(rendered.Text)
	if body != "" {
		out.WriteString("```\n" + body + "\n```\n")
	}

	if dashboardURL != "" {
		out.WriteString(dashboardURL)
	}

	return out.String()
}
