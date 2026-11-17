//
// posthog.go
// Product events sent to our own PostHog project, off the path that produced them.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package posthog reports to one PostHog project: the product events we act
// on (a signup, a site added, a paid plan starting, changing or ending) and,
// through LogExporter, a copy of every log line.
//
// It is operator configuration, not a customer feature. A self-hosted install
// has no token, and then nothing here sends anything.
//
// Delivery never happens on the request that produced the event, so a signup
// never waits on PostHog. A delivery that fails is logged.
package posthog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// The events this package sends. PostHog keys insights and funnels on these
// names, so a rename splits every chart built on one.
const (
	EventSignedUp   = "user_signed_up"
	EventUpgraded   = "subscription_upgraded"
	EventDowngraded = "subscription_downgraded"
	EventSiteAdded  = "site_added"
)

// Where a site was added from.
const (
	SourceDashboard = "dashboard"
	SourceAPI       = "api"
)

// PlanNone is the plan property of an account that no longer pays.
const PlanNone = "none"

// PostTimeout bounds one delivery.
const PostTimeout = 10 * time.Second

// MaxInFlight caps concurrent deliveries, which bounds what a hung PostHog can
// cost us in goroutines.
const MaxInFlight = 32

// capturePath is PostHog's single-event capture endpoint.
const capturePath = "/i/v0/e/"

// Client sends product events to one PostHog project. The zero value and a nil
// pointer are both usable and send nothing.
type Client struct {
	token    string
	endpoint string
	env      string
	client   *http.Client
	log      *logger.Logger
	now      func() time.Time

	// inflight is a counting semaphore, and pending lets Close wait for the
	// deliveries it admitted.
	inflight chan struct{}
	pending  sync.WaitGroup
}

// Options configures a client.
type Options struct {
	// Token is the project token. Empty disables the client.
	Token string

	// Host is the project's ingestion origin, such as https://us.i.posthog.com.
	Host string

	// Env is sent on every event as the environment property, so a signup on
	// a laptop is never counted as a customer.
	Env string

	Log    *logger.Logger
	Client *http.Client

	// Now stamps each event. It defaults to the wall clock.
	Now func() time.Time
}

// New builds a client. A disabled one is the normal case for a self-hosted
// install, and every method on it is a no-op.
func New(opts Options) *Client {
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: PostTimeout}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Client{
		token:    strings.TrimSpace(opts.Token),
		endpoint: strings.TrimRight(strings.TrimSpace(opts.Host), "/") + capturePath,
		env:      opts.Env,
		client:   client,
		log:      opts.Log,
		now:      now,
		inflight: make(chan struct{}, MaxInFlight),
	}
}

// Enabled reports whether a token is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.token != ""
}

// Signup is a new account.
type Signup struct {
	Email  string
	Name   string
	TeamID int64

	// Method is how they proved who they are: "password" or "google".
	Method string

	// Referrer, Source, Medium, Campaign and Landing are what the signup
	// request carried about where the person came from. Each may be empty.
	Referrer string
	Source   string
	Medium   string
	Campaign string
	Landing  string
}

// PlanChange is a paid plan starting, switching term, or ending.
type PlanChange struct {
	// Email is the team owner's address, which is the person the event is
	// recorded against.
	Email  string
	TeamID int64

	// Plan and Previous are catalogue keys: "monthly", "yearly", or empty for
	// no paid plan.
	Plan     string
	Previous string

	// BillingEmail is the address on the payment provider's customer, which
	// can differ from the owner's.
	BillingEmail string

	// Reason names why a paid plan ended. It is empty for an upgrade.
	Reason string
}

// SiteAdded is a site registered to a team.
type SiteAdded struct {
	// Email is the person who added it.
	Email  string
	TeamID int64
	SiteID int64
	Domain string

	// Source is SourceDashboard or SourceAPI.
	Source string
}

// SignedUp records a new account and sets the email and name on the person.
func (c *Client) SignedUp(s Signup) {
	if !c.Enabled() {
		return
	}

	properties := map[string]any{
		"email":         s.Email,
		"team_id":       s.TeamID,
		"signup_method": s.Method,
		"$set":          map[string]any{"email": s.Email, "name": strings.TrimSpace(s.Name)},
		"$set_once":     map[string]any{"signup_method": s.Method},
	}

	for key, value := range map[string]string{
		"referrer":     s.Referrer,
		"utm_source":   s.Source,
		"utm_medium":   s.Medium,
		"utm_campaign": s.Campaign,
		"landing_page": s.Landing,
	} {
		if value != "" {
			properties[key] = value
		}
	}

	c.capture(EventSignedUp, s.Email, properties)
}

// Upgraded records a paid plan starting, or moving to a longer term. The plan
// property says whether it is monthly or yearly.
func (c *Client) Upgraded(p PlanChange) {
	if !c.Enabled() {
		return
	}

	c.capture(EventUpgraded, p.Email, c.planProperties(p))
}

// Downgraded records a paid plan ending, or moving to a shorter term.
func (c *Client) Downgraded(p PlanChange) {
	if !c.Enabled() {
		return
	}

	properties := c.planProperties(p)
	if p.Reason != "" {
		properties["reason"] = p.Reason
	}

	c.capture(EventDowngraded, p.Email, properties)
}

// SiteAdded records a site registered to a team.
func (c *Client) SiteAdded(s SiteAdded) {
	if !c.Enabled() {
		return
	}

	c.capture(EventSiteAdded, s.Email, map[string]any{
		"team_id": s.TeamID,
		"site_id": s.SiteID,
		"domain":  s.Domain,
		"source":  s.Source,
	})
}

// planProperties is the body both plan events share. The plan is also set on
// the person, so a cohort of yearly customers is one filter.
func (c *Client) planProperties(p PlanChange) map[string]any {
	plan := planKey(p.Plan)

	properties := map[string]any{
		"team_id":       p.TeamID,
		"plan":          plan,
		"previous_plan": planKey(p.Previous),
		"$set":          map[string]any{"plan": plan},
	}

	if p.BillingEmail != "" {
		properties["billing_email"] = p.BillingEmail
	}

	return properties
}

// planKey names the empty plan.
func planKey(key string) string {
	if key == "" {
		return PlanNone
	}

	return key
}

// Close waits for deliveries already admitted, or for ctx to end.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		c.pending.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// event is one capture request.
type event struct {
	APIKey     string         `json:"api_key"`
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
	Timestamp  string         `json:"timestamp"`
}

// build assembles the request body. The person is keyed by their lowercased
// address, so every event about one person joins up whichever path sent it.
func (c *Client) build(name, email string, properties map[string]any) (event, bool) {
	distinctID := strings.ToLower(strings.TrimSpace(email))
	if distinctID == "" {
		return event{}, false
	}

	properties["environment"] = c.env

	return event{
		APIKey:     c.token,
		Event:      name,
		DistinctID: distinctID,
		Properties: properties,
		Timestamp:  c.now().UTC().Format(time.RFC3339Nano),
	}, true
}

// capture delivers off the caller's goroutine. An event with no address, and
// one that finds every delivery slot taken, is dropped and logged.
func (c *Client) capture(name, email string, properties map[string]any) {
	ev, ok := c.build(name, email, properties)
	if !ok {
		c.warn("posthog event dropped: no email to record it against", "event", name)
		return
	}

	select {
	case c.inflight <- struct{}{}:
	default:
		c.warn("posthog event dropped: too many deliveries in flight", "event", name, "distinct_id", ev.DistinctID)
		return
	}

	c.pending.Add(1)
	go func() {
		defer c.pending.Done()
		defer func() { <-c.inflight }()

		ctx, cancel := context.WithTimeout(context.Background(), PostTimeout)
		defer cancel()

		if err := c.post(ctx, ev); err != nil && c.log != nil {
			c.log.Error("could not send a posthog event", "event", name, "distinct_id", ev.DistinctID, "error", err)
		}
	}()
}

// post sends one event and reports what went wrong.
func (c *Client) post(ctx context.Context, ev event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // the status is the whole answer

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("posthog: capture answered %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}

	return nil
}

// warn logs when there is a logger to log to.
func (c *Client) warn(message string, args ...any) {
	if c.log != nil {
		c.log.Warn(message, args...)
	}
}
