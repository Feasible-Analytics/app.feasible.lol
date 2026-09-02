//
// store.go
// Subscriptions, alert rules, and the ledger that stops an incident becoming a flood.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The alert kinds. Same strings as the schema's CHECK constraint.
const (
	KindSpike = "spike"
	KindDrop  = "drop"
)

// The defaults a new alert starts at.
//
// Ten current visitors is a spike for the median site and noise for a large
// one, which is exactly why the number is editable — but a feature that makes
// somebody choose a threshold before it does anything is a feature nobody turns
// on. One unique visitor in twelve hours is the drop threshold because the
// question a drop alert really answers is "has my tracking stopped", and zero
// is the only reading that means yes.
const (
	DefaultSpikeThreshold  = 10
	DefaultDropThreshold   = 1
	DefaultDropWindowHours = 12
)

// MaxAlertsPerDay is the rate limit.
//
// It counts alerts only, not scheduled reports. Reports are already limited by
// their own schedule — at most one weekly and one monthly per site per day —
// and letting them consume the alert budget would mean a site whose report went
// out this morning is silent through an outage this afternoon.
//
// Two is the number because an alert repeats for as long as the condition
// holds, and a condition can hold for days. Without the cap a single incident
// sends a message every time the job runs until the recipient filters the
// sender, at which point the feature is worse than not existing.
const MaxAlertsPerDay = 2

// MaxRecipients bounds fan-out and the durable destination snapshot. It is
// enforced at settings writes and again while claiming legacy rows.
const MaxRecipients = 25

// RateWindow is the period MaxAlertsPerDay is counted over.
const RateWindow = 24 * time.Hour

// DeliveryLease is how long one notifier owns a logical notification. It
// matches the queue's stale-job window: after a process crash, the job and the
// notification become claimable together instead of one suppressing the other.
const DeliveryLease = 15 * time.Minute

// ErrDeliveryLeaseLost means another worker recovered an expired claim before
// this one could persist its destination result.
var ErrDeliveryLeaseLost = errors.New("reports: the notification delivery lease was lost")

// Destination channels, matching the schema's CHECK constraint.
const (
	ChannelEmail = "email"
	ChannelSlack = "slack"
)

// DestinationTarget is one configured endpoint captured when a notification
// is first claimed. The snapshot is durable, so editing a subscription while a
// partial delivery retries does not resend successful endpoints or silently
// change the remaining audience.
type DestinationTarget struct {
	Channel string
	Target  string
}

// Destination is one unsent endpoint belonging to a claimed notification.
type Destination struct {
	ID      int64
	Channel string
	Target  string
}

// DeliveryClaim is a lease over one logical report or alert and only the
// destinations that have not already succeeded.
type DeliveryClaim struct {
	ID           int64
	Token        string
	SiteID       int64
	Kind         string
	PeriodKey    string
	Payload      string
	Destinations []Destination
}

// Subscription is a scheduled report for one site.
type Subscription struct {
	ID              int64
	SiteID          int64
	Kind            string
	Recipients      []string
	SlackWebhookURL string
	Enabled         bool
}

// AlertRule is a spike or drop alert for one site.
type AlertRule struct {
	ID     int64
	SiteID int64
	Kind   string

	// Threshold is current visitors for a spike, and unique visitors over the
	// window for a drop.
	Threshold int

	// WindowHours is how far back a drop alert looks. A spike alert reads the
	// live visitor count and ignores it.
	WindowHours int

	Recipients      []string
	SlackWebhookURL string
	Enabled         bool
}

// Store is the system-database side of reports and alerts.
type Store struct {
	db *sql.DB

	// Now is the clock the rate limiter and the ledger stamp against.
	Now func() time.Time

	// Lease overrides DeliveryLease in deterministic slow-delivery tests.
	Lease time.Duration
}

// leaseDuration returns the configured delivery lease.
func (s *Store) leaseDuration() time.Duration {
	if s.Lease <= 0 {
		return DeliveryLease
	}

	return s.Lease
}

// NewStore builds a store over the system database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the injected clock, falling back to the real one.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// SaveSubscription creates or replaces a site's report of one kind. It is an
// upsert because the settings screen offers one weekly and one monthly toggle
// per site, and an insert that failed on the second save would make the screen
// lie about what it just did.
func (s *Store) SaveSubscription(ctx context.Context, subscription Subscription) error {
	return s.saveSubscription(ctx, subscription, 0)
}

// SaveSubscriptionForOwner saves only while expectedOwnerTeamID still owns the
// site. This closes the authorization-to-write race with site transfer.
func (s *Store) SaveSubscriptionForOwner(ctx context.Context, subscription Subscription,
	expectedOwnerTeamID int64) error {
	return s.saveSubscription(ctx, subscription, expectedOwnerTeamID)
}

// saveSubscription validates and writes either directly for trusted internal
// setup or through an ownership-fenced transaction for request paths.
func (s *Store) saveSubscription(ctx context.Context, subscription Subscription, expectedOwnerTeamID int64) error {
	if subscription.Kind != KindWeekly && subscription.Kind != KindMonthly {
		return fmt.Errorf("reports: %q is not weekly or monthly", subscription.Kind)
	}

	recipients, err := encodeRecipients(subscription.Recipients)
	if err != nil {
		return err
	}

	now := s.now().Unix()

	if expectedOwnerTeamID == 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO report_subscriptions
				(site_id, kind, recipients, slack_webhook_url, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (site_id, kind) DO UPDATE SET
				recipients = excluded.recipients,
				slack_webhook_url = excluded.slack_webhook_url,
				enabled = excluded.enabled,
				updated_at = excluded.updated_at
		`, subscription.SiteID, subscription.Kind, recipients, subscription.SlackWebhookURL,
			boolToInt(subscription.Enabled), now, now)
		if err != nil {
			return fmt.Errorf("reports: save subscription: %w", err)
		}

		return nil
	}

	tx, err := s.ownerMutation(ctx, subscription.SiteID, expectedOwnerTeamID)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	_, err = tx.ExecContext(ctx, `
		INSERT INTO report_subscriptions
			(site_id, kind, recipients, slack_webhook_url, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id, kind) DO UPDATE SET
			recipients = excluded.recipients,
			slack_webhook_url = excluded.slack_webhook_url,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
	`, subscription.SiteID, subscription.Kind, recipients, subscription.SlackWebhookURL,
		boolToInt(subscription.Enabled), now, now)
	if err != nil {
		return fmt.Errorf("reports: save subscription: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reports: save subscription: %w", err)
	}

	return nil
}

// Subscriptions lists a site's scheduled reports.
func (s *Store) Subscriptions(ctx context.Context, siteID int64) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, site_id, kind, recipients, slack_webhook_url, enabled
		FROM report_subscriptions WHERE site_id = ? ORDER BY kind
	`, siteID)
	if err != nil {
		return nil, fmt.Errorf("reports: list subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Subscription

	for rows.Next() {
		var (
			subscription Subscription
			recipients   string
			enabled      int
		)

		if err := rows.Scan(&subscription.ID, &subscription.SiteID, &subscription.Kind,
			&recipients, &subscription.SlackWebhookURL, &enabled); err != nil {
			return nil, fmt.Errorf("reports: list subscriptions: %w", err)
		}

		subscription.Recipients = decodeRecipients(recipients)
		subscription.Enabled = enabled != 0
		out = append(out, subscription)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: list subscriptions: %w", err)
	}

	return out, nil
}

// SubscriptionFor reads one site's report of one kind, or ErrNoSubscription.
func (s *Store) SubscriptionFor(ctx context.Context, siteID int64, kind string) (Subscription, error) {
	var (
		subscription Subscription
		recipients   string
		enabled      int
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, site_id, kind, recipients, slack_webhook_url, enabled
		FROM report_subscriptions WHERE site_id = ? AND kind = ?
	`, siteID, kind).Scan(&subscription.ID, &subscription.SiteID, &subscription.Kind,
		&recipients, &subscription.SlackWebhookURL, &enabled)

	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNoSubscription
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("reports: read subscription: %w", err)
	}

	subscription.Recipients = decodeRecipients(recipients)
	subscription.Enabled = enabled != 0

	return subscription, nil
}

// ErrNoSubscription means the site has no report of that kind configured.
var ErrNoSubscription = errors.New("reports: no subscription")

// ScheduledSites is the list the hourly scheduler walks: every site with at
// least one enabled subscription, with its timezone.
//
// It is one query rather than a query per site because the scheduler runs every
// hour over every site in the install, and the arithmetic it then does is
// nanoseconds — making the database the slow part would be a strange way to
// build the cheapest thing in the system.
func (s *Store) ScheduledSites(ctx context.Context) ([]ScheduledSite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sites.id, sites.domain, sites.timezone,
		       MAX(CASE WHEN report_subscriptions.kind = 'weekly'  THEN report_subscriptions.enabled ELSE 0 END),
		       MAX(CASE WHEN report_subscriptions.kind = 'monthly' THEN report_subscriptions.enabled ELSE 0 END)
		FROM sites
		JOIN report_subscriptions ON report_subscriptions.site_id = sites.id
		GROUP BY sites.id
		ORDER BY sites.id
	`)
	if err != nil {
		return nil, fmt.Errorf("reports: list scheduled sites: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ScheduledSite

	for rows.Next() {
		var (
			site            ScheduledSite
			weekly, monthly int
		)

		if err := rows.Scan(&site.SiteID, &site.Domain, &site.Timezone, &weekly, &monthly); err != nil {
			return nil, fmt.Errorf("reports: list scheduled sites: %w", err)
		}

		site.Weekly = weekly != 0
		site.Monthly = monthly != 0
		out = append(out, site)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: list scheduled sites: %w", err)
	}

	return out, nil
}

// SaveAlertRule creates or replaces a site's spike or drop alert.
func (s *Store) SaveAlertRule(ctx context.Context, rule AlertRule) error {
	return s.saveAlertRule(ctx, rule, 0)
}

// SaveAlertRuleForOwner saves only while the expected team still owns the site.
func (s *Store) SaveAlertRuleForOwner(ctx context.Context, rule AlertRule, expectedOwnerTeamID int64) error {
	return s.saveAlertRule(ctx, rule, expectedOwnerTeamID)
}

// saveAlertRule validates and writes an alert with an optional ownership fence.
func (s *Store) saveAlertRule(ctx context.Context, rule AlertRule, expectedOwnerTeamID int64) error {
	if rule.Kind != KindSpike && rule.Kind != KindDrop {
		return fmt.Errorf("reports: %q is not spike or drop", rule.Kind)
	}

	if rule.Threshold <= 0 {
		rule.Threshold = DefaultSpikeThreshold
		if rule.Kind == KindDrop {
			rule.Threshold = DefaultDropThreshold
		}
	}

	if rule.WindowHours <= 0 {
		rule.WindowHours = DefaultDropWindowHours
	}

	recipients, err := encodeRecipients(rule.Recipients)
	if err != nil {
		return err
	}

	now := s.now().Unix()

	if expectedOwnerTeamID == 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO alert_rules
				(site_id, kind, threshold, window_hours, recipients, slack_webhook_url, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (site_id, kind) DO UPDATE SET
				threshold = excluded.threshold,
				window_hours = excluded.window_hours,
				recipients = excluded.recipients,
				slack_webhook_url = excluded.slack_webhook_url,
				enabled = excluded.enabled,
				updated_at = excluded.updated_at
		`, rule.SiteID, rule.Kind, rule.Threshold, rule.WindowHours, recipients,
			rule.SlackWebhookURL, boolToInt(rule.Enabled), now, now)
		if err != nil {
			return fmt.Errorf("reports: save alert rule: %w", err)
		}

		return nil
	}

	tx, err := s.ownerMutation(ctx, rule.SiteID, expectedOwnerTeamID)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	_, err = tx.ExecContext(ctx, `
		INSERT INTO alert_rules
			(site_id, kind, threshold, window_hours, recipients, slack_webhook_url, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id, kind) DO UPDATE SET
			threshold = excluded.threshold,
			window_hours = excluded.window_hours,
			recipients = excluded.recipients,
			slack_webhook_url = excluded.slack_webhook_url,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
	`, rule.SiteID, rule.Kind, rule.Threshold, rule.WindowHours, recipients,
		rule.SlackWebhookURL, boolToInt(rule.Enabled), now, now)
	if err != nil {
		return fmt.Errorf("reports: save alert rule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reports: save alert rule: %w", err)
	}

	return nil
}

// ownerMutation verifies a site's owner inside the transaction it returns, so
// the protected mutation and a site transfer serialise on system.db's writer
// rather than both authorising from stale reads.
func (s *Store) ownerMutation(ctx context.Context, siteID, expectedOwnerTeamID int64) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reports: begin owner mutation: %w", err)
	}
	var ownerTeamID int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?
	`, siteID).Scan(&ownerTeamID)
	if errors.Is(err, sql.ErrNoRows) || ownerTeamID != expectedOwnerTeamID {
		tx.Rollback() //nolint:errcheck // transaction cannot be reused
		return nil, ErrSiteOwnerChanged
	}
	if err != nil {
		tx.Rollback() //nolint:errcheck // transaction cannot be reused
		return nil, fmt.Errorf("reports: verify site owner: %w", err)
	}

	return tx, nil
}

// ErrSiteOwnerChanged means a transfer committed after request authorization.
var ErrSiteOwnerChanged = errors.New("reports: the site owner changed; reload and try again")

// EnabledAlertRules lists every live alert across the install, newest site
// first. The alert job walks it because an alert is evaluated against live
// traffic and there is no per-site schedule to filter by.
func (s *Store) EnabledAlertRules(ctx context.Context) ([]AlertRule, error) {
	return s.alertRules(ctx, `WHERE alert_rules.enabled = 1`)
}

// AlertRulesFor lists one site's alerts, disabled ones included, for the
// settings screen.
func (s *Store) AlertRulesFor(ctx context.Context, siteID int64) ([]AlertRule, error) {
	return s.alertRules(ctx, `WHERE alert_rules.site_id = ?`, siteID)
}

// alertRules is the shared read. The predicate is a literal from this file
// rather than anything a caller supplied — nothing in this package ever
// concatenates a value into SQL.
func (s *Store) alertRules(ctx context.Context, where string, args ...any) ([]AlertRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, site_id, kind, threshold, window_hours, recipients, slack_webhook_url, enabled
		FROM alert_rules `+where+` ORDER BY site_id, kind`, args...)
	if err != nil {
		return nil, fmt.Errorf("reports: list alert rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AlertRule

	for rows.Next() {
		var (
			rule       AlertRule
			recipients string
			enabled    int
		)

		if err := rows.Scan(&rule.ID, &rule.SiteID, &rule.Kind, &rule.Threshold, &rule.WindowHours,
			&recipients, &rule.SlackWebhookURL, &enabled); err != nil {
			return nil, fmt.Errorf("reports: list alert rules: %w", err)
		}

		rule.Recipients = decodeRecipients(recipients)
		rule.Enabled = enabled != 0
		out = append(out, rule)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: list alert rules: %w", err)
	}

	return out, nil
}

// ClaimPeriod leases one scheduled period and returns its unsent destinations.
// A completed period is never claimable; a pending period becomes claimable
// when its lease expires, which is what recovers a process killed mid-send.
func (s *Store) ClaimPeriod(ctx context.Context, siteID int64, kind, periodKey string,
	targets []DestinationTarget) (DeliveryClaim, bool, error) {
	if periodKey == "" {
		return DeliveryClaim{}, false, fmt.Errorf("reports: a scheduled report needs a period key")
	}

	tx, err := s.beginDelivery(ctx)
	if err != nil {
		return DeliveryClaim{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	now := s.now()
	token := uuid.NewString()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO notification_claims
			(site_id, kind, period_key, state, lease_token, lease_until, created_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, siteID, kind, periodKey, token, now.Add(s.leaseDuration()).Unix(), now.Unix())
	if err != nil {
		return DeliveryClaim{}, false, fmt.Errorf("reports: create period delivery: %w", err)
	}

	created, _ := result.RowsAffected()
	claim, state, leaseUntil, err := s.periodClaim(ctx, tx, siteID, kind, periodKey)
	if err != nil {
		return DeliveryClaim{}, false, err
	}

	if created > 0 {
		claim.Token = token
		if err := s.addDestinations(ctx, tx, claim.ID, targets); err != nil {
			return DeliveryClaim{}, false, err
		}
	} else {
		if state == "completed" || leaseUntil > now.Unix() {
			return DeliveryClaim{}, false, nil
		}

		result, err := tx.ExecContext(ctx, `
			UPDATE notification_claims SET lease_token = ?, lease_until = ?
			WHERE id = ? AND state = 'pending' AND lease_until <= ?
		`, token, now.Add(s.leaseDuration()).Unix(), claim.ID, now.Unix())
		if err != nil {
			return DeliveryClaim{}, false, fmt.Errorf("reports: recover period delivery: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return DeliveryClaim{}, false, nil
		}
		claim.Token = token
	}

	claim.Destinations, err = pendingDestinations(ctx, tx, claim.ID)
	if err != nil {
		return DeliveryClaim{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return DeliveryClaim{}, false, fmt.Errorf("reports: commit period claim: %w", err)
	}

	return claim, true, nil
}

// ClaimAlert leases an unfinished alert for this rule or atomically allocates a
// new site-wide rate-limit slot. Taking the write lock before counting makes it
// impossible for concurrent rules to both observe the last free slot.
func (s *Store) ClaimAlert(ctx context.Context, siteID int64, kind string,
	targets []DestinationTarget) (DeliveryClaim, bool, int, error) {
	return s.ClaimAlertSnapshot(ctx, siteID, kind, targets, "")
}

// ClaimAlertSnapshot leases an alert and stores the rendering input beside its
// destination snapshot. A retry can therefore finish after the rule is disabled
// or its live condition clears without taking a second, contradictory reading.
func (s *Store) ClaimAlertSnapshot(ctx context.Context, siteID int64, kind string,
	targets []DestinationTarget, payload string) (DeliveryClaim, bool, int, error) {
	tx, err := s.beginDelivery(ctx)
	if err != nil {
		return DeliveryClaim{}, false, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	now := s.now()
	token := uuid.NewString()

	claim, leaseUntil, createdAt, found, err := s.pendingAlert(ctx, tx, siteID, kind)
	if err != nil {
		return DeliveryClaim{}, false, 0, err
	}

	if found {
		windowStart := now.Add(-RateWindow)
		used, countErr := s.alertSlots(ctx, tx, siteID, windowStart)
		if countErr != nil {
			return DeliveryClaim{}, false, 0, countErr
		}
		if leaseUntil > now.Unix() {
			return DeliveryClaim{}, false, used, nil
		}

		// A claim stranded for more than one rate window no longer occupies a
		// current slot. Recovering it moves that slot into the current window,
		// after checking the cap under the same write reservation.
		if createdAt < windowStart.Unix() {
			if used >= MaxAlertsPerDay {
				return DeliveryClaim{}, false, used, nil
			}
			createdAt = now.Unix()
		}

		result, err := tx.ExecContext(ctx, `
			UPDATE notification_claims SET lease_token = ?, lease_until = ?, created_at = ?
			WHERE id = ? AND state = 'pending' AND lease_until <= ?
		`, token, now.Add(s.leaseDuration()).Unix(), createdAt, claim.ID, now.Unix())
		if err != nil {
			return DeliveryClaim{}, false, 0, fmt.Errorf("reports: recover alert delivery: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return DeliveryClaim{}, false, 0, nil
		}

		claim.Token = token
		claim.Destinations, err = pendingDestinations(ctx, tx, claim.ID)
		if err != nil {
			return DeliveryClaim{}, false, 0, err
		}

		if err := tx.Commit(); err != nil {
			return DeliveryClaim{}, false, 0, fmt.Errorf("reports: commit recovered alert: %w", err)
		}

		return claim, true, 0, nil
	}

	used, err := s.alertSlots(ctx, tx, siteID, now.Add(-RateWindow))
	if err != nil {
		return DeliveryClaim{}, false, 0, err
	}
	if used >= MaxAlertsPerDay {
		return DeliveryClaim{}, false, used, nil
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO notification_claims
			(site_id, kind, period_key, state, lease_token, lease_until, payload, created_at)
		VALUES (?, ?, '', 'pending', ?, ?, ?, ?)
	`, siteID, kind, token, now.Add(s.leaseDuration()).Unix(), payload, now.Unix())
	if err != nil {
		return DeliveryClaim{}, false, 0, fmt.Errorf("reports: allocate alert slot: %w", err)
	}

	claim.ID, err = result.LastInsertId()
	if err != nil {
		return DeliveryClaim{}, false, 0, fmt.Errorf("reports: identify alert slot: %w", err)
	}
	claim.Token = token
	claim.SiteID = siteID
	claim.Kind = kind
	claim.Payload = payload

	if err := s.addDestinations(ctx, tx, claim.ID, targets); err != nil {
		return DeliveryClaim{}, false, 0, err
	}
	claim.Destinations, err = pendingDestinations(ctx, tx, claim.ID)
	if err != nil {
		return DeliveryClaim{}, false, 0, err
	}

	if err := tx.Commit(); err != nil {
		return DeliveryClaim{}, false, 0, fmt.Errorf("reports: commit alert slot: %w", err)
	}

	return claim, true, used, nil
}

// ClaimPendingAlert reclaims the oldest expired snapshotted alert regardless of
// whether its current rule is enabled or still firing. Live leases are skipped,
// preventing a second worker from sending while the first is active.
func (s *Store) ClaimPendingAlert(ctx context.Context) (DeliveryClaim, bool, error) {
	return s.ClaimPendingAlertAfter(ctx, 0)
}

// ClaimPendingAlertAfter reclaims the next eligible snapshot after a bounded
// run cursor. Rate-blocked old sites are filtered in SQL so they cannot starve
// later sites, and a failed claim is attempted at most once in one run.
func (s *Store) ClaimPendingAlertAfter(ctx context.Context, afterID int64) (DeliveryClaim, bool, error) {
	tx, err := s.beginDelivery(ctx)
	if err != nil {
		return DeliveryClaim{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	now := s.now()
	var claim DeliveryClaim
	var createdAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT candidate.id, candidate.site_id, candidate.kind, candidate.period_key,
		       candidate.payload, candidate.created_at
		FROM notification_claims AS candidate
		WHERE candidate.id > ? AND candidate.state = 'pending'
		  AND candidate.period_key = '' AND candidate.payload <> ''
		  AND candidate.lease_until <= ?
		  AND (
		      candidate.created_at >= ? OR
		      (SELECT COUNT(*) FROM notification_claims AS recent
		       WHERE recent.site_id = candidate.site_id
		         AND recent.kind IN ('spike', 'drop')
		         AND recent.created_at >= ?) < ?
		  )
		ORDER BY candidate.id LIMIT 1
	`, afterID, now.Unix(), now.Add(-RateWindow).Unix(), now.Add(-RateWindow).Unix(),
		MaxAlertsPerDay).Scan(&claim.ID, &claim.SiteID, &claim.Kind, &claim.PeriodKey, &claim.Payload, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryClaim{}, false, nil
	}
	if err != nil {
		return DeliveryClaim{}, false, fmt.Errorf("reports: read pending alert snapshot: %w", err)
	}

	windowStart := now.Add(-RateWindow)
	used, err := s.alertSlots(ctx, tx, claim.SiteID, windowStart)
	if err != nil {
		return DeliveryClaim{}, false, err
	}
	if createdAt < windowStart.Unix() {
		if used >= MaxAlertsPerDay {
			return DeliveryClaim{}, false, nil
		}
		createdAt = now.Unix()
	}

	claim.Token = uuid.NewString()
	result, err := tx.ExecContext(ctx, `
		UPDATE notification_claims SET lease_token = ?, lease_until = ?, created_at = ?
		WHERE id = ? AND state = 'pending' AND lease_until <= ?
	`, claim.Token, now.Add(s.leaseDuration()).Unix(), createdAt, claim.ID, now.Unix())
	if err != nil {
		return DeliveryClaim{}, false, fmt.Errorf("reports: reclaim pending alert snapshot: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return DeliveryClaim{}, false, nil
	}

	claim.Destinations, err = pendingDestinations(ctx, tx, claim.ID)
	if err != nil {
		return DeliveryClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryClaim{}, false, fmt.Errorf("reports: commit pending alert snapshot: %w", err)
	}

	return claim, true, nil
}

// HasRecoverablePendingAlerts reports whether any expired snapshot is eligible
// under the current per-site rate window. Blocked rows do not hold live
// evaluation hostage, but failed or over-budget eligible rows defer it.
func (s *Store) HasRecoverablePendingAlerts(ctx context.Context) (bool, error) {
	now := s.now()
	var pending bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM notification_claims AS candidate
			WHERE candidate.state = 'pending' AND candidate.period_key = ''
			  AND candidate.payload <> '' AND candidate.lease_until <= ?
			  AND (
			      candidate.created_at >= ? OR
			      (SELECT COUNT(*) FROM notification_claims AS recent
			       WHERE recent.site_id = candidate.site_id
			         AND recent.kind IN ('spike', 'drop')
			         AND recent.created_at >= ?) < ?
			  )
		)
	`, now.Unix(), now.Add(-RateWindow).Unix(), now.Add(-RateWindow).Unix(),
		MaxAlertsPerDay).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("reports: inspect pending alert snapshots: %w", err)
	}

	return pending, nil
}

// ReleaseDelivery makes a failed claim immediately retryable while preserving
// every destination already marked sent.
func (s *Store) ReleaseDelivery(ctx context.Context, claim DeliveryClaim) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notification_claims SET lease_token = '', lease_until = ?
		WHERE id = ? AND state = 'pending' AND lease_token = ?
	`, s.now().Unix(), claim.ID, claim.Token)
	if err != nil {
		return fmt.Errorf("reports: release delivery: %w", err)
	}

	return nil
}

// RenewDelivery extends a live claim before and during slow external sends.
// The token predicate prevents a stale worker from reviving a claim another
// worker has already recovered.
func (s *Store) RenewDelivery(ctx context.Context, claim DeliveryClaim) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE notification_claims SET lease_until = ?
		WHERE id = ? AND state = 'pending' AND lease_token = ?
	`, s.now().Add(s.leaseDuration()).Unix(), claim.ID, claim.Token)
	if err != nil {
		return fmt.Errorf("reports: renew delivery: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrDeliveryLeaseLost
	}

	return nil
}

// MarkDestinationSent persists one successful external side effect before the
// notifier attempts the next endpoint.
func (s *Store) MarkDestinationSent(ctx context.Context, claim DeliveryClaim, destinationID int64) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE notification_destinations SET state = 'sent', sent_at = ?
		WHERE id = ? AND notification_id = ? AND state = 'pending'
		  AND EXISTS (
			SELECT 1 FROM notification_claims
			WHERE id = ? AND state = 'pending' AND lease_token = ?
		  )
	`, s.now().Unix(), destinationID, claim.ID, claim.ID, claim.Token)
	if err != nil {
		return fmt.Errorf("reports: mark destination sent: %w", err)
	}

	if affected, _ := result.RowsAffected(); affected > 0 {
		return nil
	}

	var state string
	if err := s.db.QueryRowContext(ctx, `
		SELECT state FROM notification_destinations
		WHERE id = ? AND notification_id = ?
	`, destinationID, claim.ID).Scan(&state); err != nil {
		return fmt.Errorf("reports: verify destination state: %w", err)
	}
	if state == "sent" {
		return nil
	}

	return ErrDeliveryLeaseLost
}

// CompleteDelivery closes a claim only after every destination is durable and
// appends the customer-facing sent ledger in the same transaction.
func (s *Store) CompleteDelivery(ctx context.Context, claim DeliveryClaim) (int, error) {
	tx, err := s.beginDelivery(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	var pending, sent int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE state = 'pending'), COUNT(*) FILTER (WHERE state = 'sent')
		FROM notification_destinations WHERE notification_id = ?
	`, claim.ID).Scan(&pending, &sent); err != nil {
		return 0, fmt.Errorf("reports: count delivery destinations: %w", err)
	}
	if pending > 0 {
		return 0, fmt.Errorf("reports: complete delivery with %d destinations still pending", pending)
	}

	now := s.now().Unix()
	result, err := tx.ExecContext(ctx, `
		UPDATE notification_claims
		SET state = 'completed', lease_token = '', lease_until = 0, recipients = ?, completed_at = ?
		WHERE id = ? AND state = 'pending' AND lease_token = ?
	`, sent, now, claim.ID, claim.Token)
	if err != nil {
		return 0, fmt.Errorf("reports: complete delivery: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return 0, ErrDeliveryLeaseLost
	}

	if claim.PeriodKey == "" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO notifications_sent (site_id, kind, period_key, recipients, sent_at)
			VALUES (?, ?, '', ?, ?)
		`, claim.SiteID, claim.Kind, sent, now)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO notifications_sent (site_id, kind, period_key, recipients, sent_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (site_id, kind, period_key) WHERE period_key <> '' DO UPDATE SET
				recipients = excluded.recipients,
				sent_at = excluded.sent_at
		`, claim.SiteID, claim.Kind, claim.PeriodKey, sent, now)
	}
	if err != nil {
		return 0, fmt.Errorf("reports: record completed delivery: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reports: commit completed delivery: %w", err)
	}

	return sent, nil
}

// AlertsSentSince counts completed alerts for diagnostics. Rate-limit
// allocation deliberately counts pending claims too and happens in ClaimAlert.
func (s *Store) AlertsSentSince(ctx context.Context, siteID int64, since time.Time) (int, error) {
	var count int

	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notification_claims
		WHERE site_id = ? AND created_at >= ? AND state = 'completed'
		  AND kind IN ('spike', 'drop')
	`, siteID, since.Unix()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("reports: count alerts: %w", err)
	}

	return count, nil
}

// beginDelivery starts a transaction and takes SQLite's write reservation
// before any count or lease read. That turns alert slot allocation into one
// serial operation even when the database handle has several connections.
func (s *Store) beginDelivery(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reports: begin delivery: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE notification_claims SET lease_until = lease_until WHERE id = -1
	`); err != nil {
		tx.Rollback() //nolint:errcheck // preserving the original lock error
		return nil, fmt.Errorf("reports: lock delivery state: %w", err)
	}

	return tx, nil
}

// periodClaim reads the row protected by the period's unique index.
func (s *Store) periodClaim(ctx context.Context, tx *sql.Tx, siteID int64,
	kind, periodKey string) (DeliveryClaim, string, int64, error) {
	var claim DeliveryClaim
	var state string
	var leaseUntil int64

	err := tx.QueryRowContext(ctx, `
		SELECT id, site_id, kind, period_key, state, lease_until
		FROM notification_claims
		WHERE site_id = ? AND kind = ? AND period_key = ?
	`, siteID, kind, periodKey).Scan(&claim.ID, &claim.SiteID, &claim.Kind,
		&claim.PeriodKey, &state, &leaseUntil)
	if err != nil {
		return DeliveryClaim{}, "", 0, fmt.Errorf("reports: read period claim: %w", err)
	}

	return claim, state, leaseUntil, nil
}

// pendingAlert returns the oldest unfinished alert for one rule. A retry must
// finish this claim before a fresh alert can consume another site-wide slot.
func (s *Store) pendingAlert(ctx context.Context, tx *sql.Tx, siteID int64,
	kind string) (DeliveryClaim, int64, int64, bool, error) {
	var claim DeliveryClaim
	var leaseUntil, createdAt int64

	err := tx.QueryRowContext(ctx, `
		SELECT id, site_id, kind, period_key, payload, lease_until, created_at
		FROM notification_claims
		WHERE site_id = ? AND kind = ? AND period_key = '' AND state = 'pending'
		ORDER BY id LIMIT 1
	`, siteID, kind).Scan(&claim.ID, &claim.SiteID, &claim.Kind, &claim.PeriodKey, &claim.Payload, &leaseUntil, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryClaim{}, 0, 0, false, nil
	}
	if err != nil {
		return DeliveryClaim{}, 0, 0, false, fmt.Errorf("reports: read pending alert: %w", err)
	}

	return claim, leaseUntil, createdAt, true, nil
}

// alertSlots counts every completed or recoverable alert in the rolling
// window. It is only called while beginDelivery's write reservation is held.
func (s *Store) alertSlots(ctx context.Context, tx *sql.Tx, siteID int64, since time.Time) (int, error) {
	var count int

	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM notification_claims
		WHERE site_id = ? AND created_at >= ? AND kind IN ('spike', 'drop')
	`, siteID, since.Unix()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("reports: count alert slots: %w", err)
	}

	return count, nil
}

// addDestinations snapshots and deduplicates the configured endpoints.
func (s *Store) addDestinations(ctx context.Context, tx *sql.Tx, claimID int64,
	targets []DestinationTarget) error {
	seen := map[string]bool{}

	for _, target := range targets {
		target.Target = strings.TrimSpace(target.Target)
		if target.Target == "" {
			continue
		}
		if target.Channel != ChannelEmail && target.Channel != ChannelSlack {
			return fmt.Errorf("reports: %q is not a delivery channel", target.Channel)
		}

		keyTarget := target.Target
		if target.Channel == ChannelEmail {
			keyTarget = strings.ToLower(keyTarget)
		}
		key := target.Channel + ":" + keyTarget
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(seen) > MaxRecipients+1 {
			return fmt.Errorf("reports: a notification may have at most %d email recipients and one Slack destination", MaxRecipients)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_destinations
				(notification_id, destination_key, channel, target, state)
			VALUES (?, ?, ?, ?, 'pending')
		`, claimID, key, target.Channel, target.Target); err != nil {
			return fmt.Errorf("reports: add delivery destination: %w", err)
		}
	}

	return nil
}

// pendingDestinations lists only work whose external side effect has not been
// confirmed. Successfully sent destinations therefore disappear from retries.
func pendingDestinations(ctx context.Context, tx *sql.Tx, claimID int64) ([]Destination, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, channel, target
		FROM notification_destinations
		WHERE notification_id = ? AND state = 'pending'
		ORDER BY id
	`, claimID)
	if err != nil {
		return nil, fmt.Errorf("reports: list pending destinations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var destinations []Destination
	for rows.Next() {
		var destination Destination
		if err := rows.Scan(&destination.ID, &destination.Channel, &destination.Target); err != nil {
			return nil, fmt.Errorf("reports: list pending destinations: %w", err)
		}
		destinations = append(destinations, destination)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: list pending destinations: %w", err)
	}

	return destinations, nil
}

// encodeRecipients validates and stores the address list. An address that does
// not parse is refused at save time rather than at send time, because a
// settings screen can tell somebody they mistyped and a background job three
// days later cannot.
func encodeRecipients(recipients []string) (string, error) {
	cleaned := make([]string, 0, len(recipients))
	seen := map[string]bool{}

	for _, address := range recipients {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}

		if _, err := mail.ParseAddress(address); err != nil {
			return "", fmt.Errorf("reports: %q is not a valid email address", address)
		}
		key := strings.ToLower(address)
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(cleaned) >= MaxRecipients {
			return "", fmt.Errorf("reports: at most %d email recipients are allowed", MaxRecipients)
		}

		cleaned = append(cleaned, address)
	}

	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return "", fmt.Errorf("reports: encode recipients: %w", err)
	}

	return string(encoded), nil
}

// decodeRecipients reads the stored list, treating anything unreadable as
// nobody. A corrupt list must not become "send to everything in the string",
// and an empty list is a report that is skipped and said so rather than a
// message with no recipients that an SMTP server rejects.
func decodeRecipients(raw string) []string {
	var recipients []string

	if err := json.Unmarshal([]byte(raw), &recipients); err != nil {
		return nil
	}
	if len(recipients) > MaxRecipients {
		return recipients[:MaxRecipients]
	}

	return recipients
}

// boolToInt converts for SQLite, which has no boolean type.
func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

// Delivery is one line of the ledger, for the settings screen.
type Delivery struct {
	Kind       string
	PeriodKey  string
	Recipients int
	SentAt     int64
}

// Deliveries lists what has been sent for a site, newest first. It is shown to
// the customer because "the report did not arrive" and "the report was never
// sent" are different problems with different fixes, and nothing else in the
// system can tell them apart.
func (s *Store) Deliveries(ctx context.Context, siteID int64, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, period_key, recipients, sent_at
		FROM notifications_sent
		WHERE site_id = ?
		ORDER BY sent_at DESC, id DESC
		LIMIT ?
	`, siteID, limit)
	if err != nil {
		return nil, fmt.Errorf("reports: list deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Delivery

	for rows.Next() {
		var delivery Delivery

		if err := rows.Scan(&delivery.Kind, &delivery.PeriodKey, &delivery.Recipients, &delivery.SentAt); err != nil {
			return nil, fmt.Errorf("reports: list deliveries: %w", err)
		}

		out = append(out, delivery)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: list deliveries: %w", err)
	}

	return out, nil
}
