//
// slack.go
// Operational notices to our own Slack, off the path that produced them.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package slack posts the handful of commercial events we want to know about
// the moment they happen — a signup, a subscription starting, a subscription
// ending, an account closing — to one Slack incoming webhook.
//
// It is deliberately not the customer webhook system in internal/webhooks. That
// one is a product feature with endpoints, secrets, retries and a delivery log
// a customer reads. This is a single operator-configured URL carrying our own
// business events, and a missed message costs us a notification rather than a
// customer's data.
//
// Two rules shape it. Delivery never happens on the request that produced the
// event, because a signup must not fail or hang because Slack is slow. And a
// delivery that fails is logged rather than swallowed, because a notification
// system nobody can tell is broken is worse than none at all.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// PostTimeout bounds one delivery. Slack answers an incoming webhook in
// milliseconds, so a request still running after this is a network problem and
// waiting longer only holds a goroutine open.
const PostTimeout = 10 * time.Second

// MaxInFlight caps concurrent deliveries. The events here are commercial ones —
// a busy day is dozens, not thousands — so this is not a throughput limit but a
// bound on what a hung Slack can cost us in goroutines.
const MaxInFlight = 32

// Notifier posts to one Slack incoming webhook. The zero value and a nil
// pointer are both usable and do nothing, so a self-hosted install and every
// test that does not care about Slack need no wiring at all.
type Notifier struct {
	webhookURL string
	baseURL    string
	env        string
	client     *http.Client
	log        *logger.Logger

	// inflight is a counting semaphore rather than a queue. A dropped message
	// is reported, and reporting it is what separates "Slack is quiet because
	// nothing happened" from "Slack is quiet because we stopped sending".
	inflight chan struct{}
}

// Options configures a notifier.
type Options struct {
	// WebhookURL is the incoming webhook. Empty disables the notifier, which is
	// the normal state for a self-hosted install and for development.
	WebhookURL string

	// BaseURL turns a team id into a link somebody can click from Slack.
	BaseURL string

	// Env labels the message when it is not production, so a signup from a
	// staging box is never mistaken for a real customer.
	Env string

	Log    *logger.Logger
	Client *http.Client
}

// New builds a notifier. A nil result is impossible; a disabled one is the
// normal case, and every method on it is a no-op.
func New(opts Options) *Notifier {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: PostTimeout}
	}

	return &Notifier{
		webhookURL: strings.TrimSpace(opts.WebhookURL),
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		env:        opts.Env,
		client:     client,
		log:        opts.Log,
		inflight:   make(chan struct{}, MaxInFlight),
	}
}

// Enabled reports whether a webhook is configured.
func (n *Notifier) Enabled() bool {
	return n != nil && n.webhookURL != ""
}

// Referral is what we could see about where a person came from. Every field is
// optional and usually empty: we persist no attribution today, so this is only
// what the signup request itself carried.
type Referral struct {
	// Referrer is the Referer header on the request that created the account.
	Referrer string

	Source   string
	Medium   string
	Campaign string
}

// Empty reports whether we learned nothing at all.
func (r Referral) Empty() bool {
	return r.Referrer == "" && r.Source == "" && r.Medium == "" && r.Campaign == ""
}

// Signup is a new account.
type Signup struct {
	Name   string
	Email  string
	TeamID int64

	// Method is how they proved who they are: "password" or "google". It is
	// worth a line because the two funnels convert differently.
	Method string

	Referral Referral
}

// PlanChange is a subscription starting, switching term, or ending.
type PlanChange struct {
	TeamID int64
	Email  string

	// Plan and Previous are the catalogue keys — "monthly", "yearly", or empty
	// for no paid plan.
	Plan     string
	Previous string

	// Reason names the provider state behind the change, so a cancellation and
	// a card that stopped working are not the same line in Slack.
	Reason string
}

// Closure is an account the owner deleted.
type Closure struct {
	TeamID int64
	Name   string
	Email  string
}

// SignedUp announces a new account.
func (n *Notifier) SignedUp(s Signup) {
	if !n.Enabled() {
		return
	}

	n.send(n.signupText(s))
}

// signupText builds the message. The builders are separate from the senders so
// a test asserts on wording without a server and without a goroutine.
func (n *Notifier) signupText(s Signup) string {
	lines := []string{
		fmt.Sprintf(":wave: *New signup* — %s", person(s.Name, s.Email)),
		bullet("Team", n.teamLink(s.TeamID)),
	}

	if s.Method != "" {
		lines = append(lines, bullet("Signed up with", s.Method))
	}

	lines = append(lines, referralLines(s.Referral)...)

	return strings.Join(lines, "\n")
}

// Upgraded announces a subscription starting, or moving to a longer term.
func (n *Notifier) Upgraded(c PlanChange) {
	if !n.Enabled() {
		return
	}

	n.send(n.upgradeText(c))
}

// upgradeText builds the message.
func (n *Notifier) upgradeText(c PlanChange) string {
	headline := fmt.Sprintf(":tada: *Upgrade* — %s is now on the *%s* plan", n.teamLink(c.TeamID), planName(c.Plan))
	if c.Previous != "" {
		headline = fmt.Sprintf(":tada: *Upgrade* — %s moved from *%s* to *%s*",
			n.teamLink(c.TeamID), planName(c.Previous), planName(c.Plan))
	}

	return strings.Join(n.planLines(headline, c), "\n")
}

// Downgraded announces a subscription ending, or moving to a shorter term.
func (n *Notifier) Downgraded(c PlanChange) {
	if !n.Enabled() {
		return
	}

	n.send(n.downgradeText(c))
}

// downgradeText builds the message.
func (n *Notifier) downgradeText(c PlanChange) string {
	headline := fmt.Sprintf(":arrow_down: *Downgrade* — %s is no longer on a paid plan", n.teamLink(c.TeamID))
	if c.Plan != "" {
		headline = fmt.Sprintf(":arrow_down: *Downgrade* — %s moved from *%s* to *%s*",
			n.teamLink(c.TeamID), planName(c.Previous), planName(c.Plan))
	}

	return strings.Join(n.planLines(headline, c), "\n")
}

// AccountClosed announces an owner deleting their account. It is the one notice
// here that can never be followed up on, so it carries the address while we
// still have it.
func (n *Notifier) AccountClosed(c Closure) {
	if !n.Enabled() {
		return
	}

	n.send(n.closureText(c))
}

// closureText builds the message.
func (n *Notifier) closureText(c Closure) string {
	return strings.Join([]string{
		fmt.Sprintf(":door: *Account closed* — %s", person(c.Name, c.Email)),
		bullet("Team", n.teamLink(c.TeamID)),
	}, "\n")
}

// planLines is the body both subscription notices share.
func (n *Notifier) planLines(headline string, c PlanChange) []string {
	lines := []string{headline}

	if c.Email != "" {
		lines = append(lines, bullet("Billing email", c.Email))
	}
	if c.Reason != "" {
		lines = append(lines, bullet("Reason", c.Reason))
	}

	return lines
}

// referralLines renders what we know about where somebody came from, and says
// so plainly when that is nothing. An absent line would read as "direct", which
// is a stronger claim than we can make.
func referralLines(r Referral) []string {
	if r.Empty() {
		return []string{bullet("Referral", "none recorded")}
	}

	var lines []string
	for _, field := range []struct{ label, value string }{
		{"Referrer", r.Referrer},
		{"Source", r.Source},
		{"Medium", r.Medium},
		{"Campaign", r.Campaign},
	} {
		if field.value != "" {
			lines = append(lines, bullet(field.label, field.value))
		}
	}

	return lines
}

// teamLink is the team id, linked into the app when we know our own address.
func (n *Notifier) teamLink(teamID int64) string {
	if n == nil || n.baseURL == "" {
		return fmt.Sprintf("team %d", teamID)
	}

	return fmt.Sprintf("<%s/billing?team=%d|team %d>", n.baseURL, teamID, teamID)
}

// person renders a name and address, or just the address when we have no name.
func person(name, email string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return email
	}

	return fmt.Sprintf("%s <%s>", name, email)
}

// planName turns a catalogue key into something readable, including the empty
// key that means no paid plan at all.
func planName(key string) string {
	switch key {
	case "":
		return "no plan"
	case "monthly":
		return "monthly"
	case "yearly":
		return "yearly"
	}

	return key
}

// bullet is one labelled fact.
func bullet(label, value string) string {
	return "• " + label + ": " + value
}

// send delivers off the caller's goroutine, because the caller is finishing a
// signup or a Stripe webhook and neither may wait on Slack.
//
// A full semaphore drops the message and says so. Blocking here would push the
// delay back onto the request this function exists to keep clear of it.
func (n *Notifier) send(text string) {
	if n.env != "" && n.env != "production" {
		text = "[" + n.env + "] " + text
	}

	select {
	case n.inflight <- struct{}{}:
	default:
		if n.log != nil {
			n.log.Warn("slack notice dropped: too many deliveries in flight", "text", text)
		}

		return
	}

	go func() {
		defer func() { <-n.inflight }()

		ctx, cancel := context.WithTimeout(context.Background(), PostTimeout)
		defer cancel()

		if err := n.post(ctx, text); err != nil && n.log != nil {
			n.log.Error("could not post a slack notice", "error", err, "text", text)
		}
	}()
}

// post sends one message and reports what went wrong. It is separate from send
// so a test drives a real delivery without a goroutine.
func (n *Notifier) post(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // the status is the whole answer

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("slack: webhook answered %s", resp.Status)
	}

	return nil
}
