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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// Slack posts to an incoming webhook.
type Slack struct {
	Client *http.Client
}

// NewSlack builds a poster with a bounded client.
func NewSlack() *Slack {
	return &Slack{Client: &http.Client{Timeout: SlackTimeout}}
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

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: SlackTimeout}
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
