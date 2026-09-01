//
// notify.go
// The two jobs: send what is due, and alert on what just changed.
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
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
)

// The job kinds and the queue they run on. Notifications get their own queue so
// that a slow mail server cannot delay a maintenance job, and so a stuck report
// is visible as a backlog on one named queue rather than as general slowness.
const (
	Queue = "notifications"

	KindScheduleJob = "reports.schedule"
	KindAlertJob    = "reports.alerts"
)

// ScheduleEvery and AlertEvery are how often each job runs.
//
// The scheduler runs hourly because that is the resolution a local midnight
// needs: every timezone offset in use is a whole number of quarter hours, so an
// hourly tick lands inside every site's first local hour of the day exactly
// once. Alerts run more often because "ten people are on your site right now"
// stops being useful within the hour.
const (
	ScheduleEvery = time.Hour
	AlertEvery    = 10 * time.Minute

	// ScheduleLookback recovers a missed weekly or monthly boundary after a
	// prolonged outage without creating an unbounded historical queue.
	ScheduleLookback = 32 * 24 * time.Hour

	// MaxPendingAlertRecoveries bounds one job's outage catch-up. Each claim is
	// one already-allocated alert, and the next ten-minute job continues where
	// this one stopped.
	MaxPendingAlertRecoveries = 100
)

// SiteLookup resolves a site id to everything a report needs. It is a function
// rather than a table read at construction so that a site added five minutes
// ago is reportable without restarting the process.
type SiteLookup func(ctx context.Context, siteID int64) (SiteRef, error)

// IdempotentMailSender is implemented by providers that accept a stable
// idempotency key. Basic SMTP senders do not, so delivery remains explicitly
// at-least-once across a crash after provider acceptance and before our durable
// destination acknowledgement.
type IdempotentMailSender interface {
	SendIdempotent(ctx context.Context, message mail.Message, idempotencyKey string) (mail.Result, error)
}

// IdempotentSlackPoster is the equivalent optional provider capability for a
// chat destination.
type IdempotentSlackPoster interface {
	PostIdempotent(ctx context.Context, webhookURL, text, idempotencyKey string) error
}

// Notifier owns both jobs.
type Notifier struct {
	Store  *Store
	Source StatsSource
	Sites  SiteLookup
	Slack  SlackPoster
	Log    *logger.Logger

	// Mail is the process's one mailer. It is the shared Sender rather than a
	// transport of this package's own because the guarantees live in there:
	// every body is wrapped below the SMTP line limit before it goes out, and a
	// relay that declined the message is an error rather than a send that
	// returned nil and delivered nothing. The envelope sender belongs to the
	// mailer too, so a process has one From address rather than two that can
	// disagree.
	Mail mail.Sender

	// BaseURL is what a dashboard link is built from. It is required: an email
	// whose only call to action is a broken link is the failure this whole
	// package's strict templating exists to prevent, so a missing base URL is
	// caught when the notifier is built rather than in somebody's inbox.
	BaseURL string

	// Now is the clock both jobs run against.
	Now func() time.Time
}

// now reads the injected clock, falling back to the real one.
func (n *Notifier) now() time.Time {
	if n.Now == nil {
		return time.Now().UTC()
	}

	return n.Now().UTC()
}

// Validate rejects a notifier that cannot produce a complete message.
func (n *Notifier) Validate() error {
	if n.Store == nil || n.Source == nil || n.Sites == nil {
		return errors.New("reports: the notifier needs a store, a stats source and a site lookup")
	}

	if strings.TrimSpace(n.BaseURL) == "" {
		return errors.New("reports: the notifier needs a base URL to build dashboard links from")
	}

	return nil
}

// dashboardURL builds the link a report points at.
func (n *Notifier) dashboardURL(domain string) string {
	return strings.TrimRight(n.BaseURL, "/") + "/dashboard/" + url.PathEscape(domain)
}

// Register attaches both jobs to the runner and their ticks to the cron.
//
// Both handlers go through jobs.Reporting, which is what turns "sent nothing
// and did not say why" into a failure on the row rather than a completed job
// nobody looks at again.
func (n *Notifier) Register(runner *jobs.Runner, cron *jobs.Cron) {
	runner.Register(Queue, KindScheduleJob, jobs.Reporting(n.Log, n.RunSchedule))
	runner.Register(Queue, KindAlertJob, jobs.Reporting(n.Log, n.RunAlerts))

	cron.AddCatchUp(Queue, KindScheduleJob, ScheduleEvery, ScheduleLookback)
	cron.Add(Queue, KindAlertJob, AlertEvery)
}

// RunSchedule is the hourly job: work out which sites just crossed a local
// period boundary, and send those reports.
//
// Every return path says what happened. A run that sent nothing comes back with
// a reason — "no sites are subscribed", "nothing was due this hour" — because a
// job that reports success while doing nothing is indistinguishable from a job
// that is working, and that is exactly how an incumbent's notifier failed
// silently for months.
func (n *Notifier) RunSchedule(ctx context.Context, job jobs.Job) (jobs.Outcome, error) {
	if err := n.Validate(); err != nil {
		return jobs.Outcome{}, err
	}

	sites, err := n.Store.ScheduledSites(ctx)
	if err != nil {
		return jobs.Outcome{}, err
	}

	if len(sites) == 0 {
		return jobs.Nothing("no sites have a weekly or monthly report configured"), nil
	}

	due := DueAt(n.scheduleTime(job), sites)
	if len(due) == 0 {
		return jobs.Nothing(fmt.Sprintf("%d subscribed sites, none crossed a local period boundary this hour", len(sites))), nil
	}

	outcome := jobs.Outcome{}
	var failures []string

	for _, report := range due {
		sent, reason, err := n.sendDue(ctx, report)
		if err != nil {
			// One site's failure must not stop the others: a report is a
			// per-customer promise, and a bad webhook on one account is no
			// reason for another account to miss its week.
			failures = append(failures, fmt.Sprintf("%s %s: %v", report.Domain, report.Kind, err))

			if n.Log != nil {
				n.Log.Error("a scheduled report failed",
					"domain", report.Domain, "kind", report.Kind, "period", report.PeriodKey, "error", err)
			}

			continue
		}

		if sent {
			outcome.Handled++
			continue
		}

		outcome.Skipped++
		outcome.Note = reason
	}

	if len(failures) > 0 {
		return outcome, fmt.Errorf("reports: %d of %d scheduled reports failed: %s",
			len(failures), len(due), strings.Join(failures, "; "))
	}

	if outcome.Handled == 0 {
		outcome.Note = fmt.Sprintf("%d reports were due and every one was already sent or had no recipients", len(due))
	}

	return outcome, nil
}

// scheduleTime returns the recurring bucket captured on the job. Falling back
// to the notifier clock preserves direct calls and old rows created before the
// bucket argument existed. A stale-job retry must use the captured instant or
// a local-midnight report stops being due before its delivery lease recovers.
func (n *Notifier) scheduleTime(job jobs.Job) time.Time {
	var args jobs.PeriodicArgs

	if len(job.Args) > 0 && json.Unmarshal(job.Args, &args) == nil && args.ScheduledAt != 0 {
		return time.Unix(args.ScheduledAt, 0).UTC()
	}

	return n.now()
}

// sendDue delivers one scheduled report. It returns whether anything went out
// and, when nothing did, why.
func (n *Notifier) sendDue(ctx context.Context, due Due) (bool, string, error) {
	subscription, err := n.Store.SubscriptionFor(ctx, due.SiteID, due.Kind)
	if err != nil {
		return false, "", err
	}

	if !subscription.Enabled {
		return false, due.Domain + ": the subscription is switched off", nil
	}

	if len(subscription.Recipients) == 0 && subscription.SlackWebhookURL == "" {
		return false, due.Domain + ": the subscription has no recipients", nil
	}

	// The period and its destinations are claimed before anything is rendered,
	// so two processes that both decide Monday has arrived produce one logical
	// delivery between them. A retry receives only destinations not yet sent.
	claim, claimed, err := n.Store.ClaimPeriod(ctx, due.SiteID, due.Kind, due.PeriodKey,
		deliveryTargets(subscription.Recipients, subscription.SlackWebhookURL))
	if err != nil {
		return false, "", err
	}

	if !claimed {
		return false, due.Domain + ": " + due.PeriodKey + " has already been sent", nil
	}

	rendered, dashboardURL, err := n.renderReport(ctx, due)
	if err != nil {
		if releaseErr := n.Store.ReleaseDelivery(ctx, claim); releaseErr != nil && n.Log != nil {
			n.Log.Error("a claimed report period could not be released",
				"domain", due.Domain, "period", due.PeriodKey, "error", releaseErr)
		}

		return false, "", err
	}

	if _, err := n.deliverClaim(ctx, rendered, claim, dashboardURL, "report_"+due.Kind); err != nil {
		if releaseErr := n.Store.ReleaseDelivery(ctx, claim); releaseErr != nil && n.Log != nil {
			n.Log.Error("a claimed report period could not be released",
				"domain", due.Domain, "period", due.PeriodKey, "error", releaseErr)
		}

		return false, "", err
	}

	return true, "", nil
}

// renderReport reads and renders one period without producing an external side
// effect. Delivery is separate so each successful destination can be persisted
// before the next one is attempted.
func (n *Notifier) renderReport(ctx context.Context, due Due) (Rendered, string, error) {
	site, err := n.Sites(ctx, due.SiteID)
	if err != nil {
		return Rendered{}, "", err
	}

	snapshot, err := n.Source.Period(ctx, site, due.From, due.To)
	if err != nil {
		return Rendered{}, "", err
	}

	report := Report{
		Domain:       site.Domain,
		Kind:         due.Kind,
		PeriodLabel:  due.Label(),
		DashboardURL: n.dashboardURL(site.Domain),
		Figures:      snapshot.Figures,
		TopPages:     snapshot.TopPages,
		TopSources:   snapshot.TopSources,
		Countries:    snapshot.Countries,
		GeneratedAt:  n.now(),
	}

	// A period with no traffic at all is much more often a broken tracker than
	// a quiet week, and saying so in the report is the cheapest support ticket
	// we will ever avoid.
	if snapshot.Visitors == 0 {
		report.Note = "No visitors were recorded in this period. If that is unexpected, the ingestion health panel will say what happened to the events."
	}

	rendered, err := RenderReport(report)
	if err != nil {
		return Rendered{}, "", err
	}

	return rendered, report.DashboardURL, nil
}

// deliveryTargets turns report settings into the durable destination snapshot
// stored with a claim.
func deliveryTargets(recipients []string, webhookURL string) []DestinationTarget {
	targets := make([]DestinationTarget, 0, len(recipients)+1)

	for _, recipient := range recipients {
		targets = append(targets, DestinationTarget{Channel: ChannelEmail, Target: recipient})
	}
	if webhookURL != "" {
		targets = append(targets, DestinationTarget{Channel: ChannelSlack, Target: webhookURL})
	}

	return targets
}

// deliverClaim sends only the claim's pending destinations and marks each one
// immediately after success. A later destination failure therefore retries
// only what remains, then completion appends the sent ledger atomically.
//
// The residual guarantee is at-least-once: if a provider accepts a send and
// this process dies before MarkDestinationSent commits, a later worker sends
// it again. Providers with an idempotent interface receive a stable key and can
// collapse that replay; claiming exactly-once without provider participation
// would be false.
func (n *Notifier) deliverClaim(ctx context.Context, rendered Rendered, claim DeliveryClaim,
	dashboardURL, tag string) (int, error) {
	for _, destination := range claim.Destinations {
		key := fmt.Sprintf("feasible-notification-%d-destination-%d", claim.ID, destination.ID)
		err := n.withLeaseHeartbeat(ctx, claim, func(sendCtx context.Context) error {
			switch destination.Channel {
			case ChannelEmail:
				if n.Mail == nil {
					return errors.New("reports: no mailer is configured")
				}
				message := rendered.Message(destination.Target, tag)
				if sender, ok := n.Mail.(IdempotentMailSender); ok {
					_, err := sender.SendIdempotent(sendCtx, message, key)
					return err
				}
				_, err := n.Mail.Send(sendCtx, message)
				return err

			case ChannelSlack:
				if n.Slack == nil {
					return errors.New("reports: no Slack poster is configured")
				}
				message := SlackText(rendered, dashboardURL)
				if poster, ok := n.Slack.(IdempotentSlackPoster); ok {
					return poster.PostIdempotent(sendCtx, destination.Target, message, key)
				}
				return n.Slack.Post(sendCtx, destination.Target, message)

			default:
				return fmt.Errorf("reports: %q is not a delivery channel", destination.Channel)
			}
		})
		if err != nil {
			return 0, err
		}

		if err := n.Store.MarkDestinationSent(ctx, claim, destination.ID); err != nil {
			return 0, err
		}
	}

	return n.Store.CompleteDelivery(ctx, claim)
}

// withLeaseHeartbeat keeps a claim live for the entire provider call. Renewal
// failure cancels a context-aware provider and prevents this worker from
// acknowledging a send under a lease it no longer owns.
func (n *Notifier) withLeaseHeartbeat(ctx context.Context, claim DeliveryClaim, send func(context.Context) error) error {
	if err := n.Store.RenewDelivery(ctx, claim); err != nil {
		return err
	}

	interval := n.Store.leaseDuration() / 3
	if interval <= 0 {
		interval = time.Second
	}

	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				done <- nil
				return
			case <-sendCtx.Done():
				done <- sendCtx.Err()
				return
			case <-ticker.C:
				if err := n.Store.RenewDelivery(sendCtx, claim); err != nil {
					cancel()
					done <- err
					return
				}
			}
		}
	}()

	sendErr := send(sendCtx)
	close(stop)
	heartbeatErr := <-done
	if sendErr != nil {
		return sendErr
	}
	if heartbeatErr != nil && !errors.Is(heartbeatErr, context.Canceled) {
		return heartbeatErr
	}

	return nil
}

// deliver sends one rendering to every configured destination and reports how
// many it reached. Email and Slack are independent: a revoked webhook must not
// stop the email, and a bounced address must not stop the channel.
//
// One message goes out per recipient rather than one message with a recipient
// list. That is what the shared mailer takes, and it is also the honest shape:
// a relay that refuses one address should not cost the other four their report,
// and a single failed send names the address it failed for.
func (n *Notifier) deliver(ctx context.Context, rendered Rendered, recipients []string,
	webhookURL, dashboardURL, tag string) (int, error) {
	delivered := 0

	if len(recipients) > 0 && n.Mail == nil {
		return 0, errors.New("reports: no mailer is configured")
	}

	for _, recipient := range recipients {
		if _, err := n.Mail.Send(ctx, rendered.Message(recipient, tag)); err != nil {
			return delivered, err
		}

		delivered++
	}

	if webhookURL != "" {
		if n.Slack == nil {
			return delivered, errors.New("reports: no Slack poster is configured")
		}

		if err := n.Slack.Post(ctx, webhookURL, SlackText(rendered, dashboardURL)); err != nil {
			return delivered, err
		}

		delivered++
	}

	return delivered, nil
}

// RunAlerts evaluates every enabled spike and drop rule.
//
// The rate limit is applied here rather than at the rule, because the condition
// an alert watches for stays true for as long as the incident lasts. Without
// the cap a two-day outage sends a drop alert every ten minutes — three hundred
// messages — and the first thing the recipient does is add a filter, which is
// how a useful feature becomes a permanently ignored one.
func (n *Notifier) RunAlerts(ctx context.Context, job jobs.Job) (jobs.Outcome, error) {
	if err := n.Validate(); err != nil {
		return jobs.Outcome{}, err
	}

	outcome := jobs.Outcome{}
	var failures []string
	triggered, suppressed := 0, 0
	recovered := map[string]bool{}
	lastClaimID := int64(0)

	// Finish snapshotted work before reading current rules. A condition may have
	// cleared or an operator may have disabled the rule after email succeeded and
	// before Slack did; neither change retracts the already-created notification.
	for attempts := 0; attempts < MaxPendingAlertRecoveries; attempts++ {
		claim, claimed, err := n.Store.ClaimPendingAlertAfter(ctx, lastClaimID)
		if err != nil {
			failures = append(failures, err.Error())
			break
		}
		if !claimed {
			break
		}
		lastClaimID = claim.ID

		recovered[alertDeliveryKey(claim.SiteID, claim.Kind)] = true
		var alert Alert
		if err := json.Unmarshal([]byte(claim.Payload), &alert); err != nil {
			_ = n.Store.ReleaseDelivery(ctx, claim)
			failures = append(failures, fmt.Sprintf("site %d %s snapshot: %v", claim.SiteID, claim.Kind, err))
			continue
		}

		rendered, err := RenderAlert(alert)
		if err != nil {
			_ = n.Store.ReleaseDelivery(ctx, claim)
			failures = append(failures, fmt.Sprintf("site %d %s snapshot: %v", claim.SiteID, claim.Kind, err))
			continue
		}
		delivered, err := n.deliverClaim(ctx, rendered, claim, alert.DashboardURL, "alert_"+claim.Kind)
		if err != nil {
			_ = n.Store.ReleaseDelivery(ctx, claim)
			failures = append(failures, fmt.Sprintf("site %d %s snapshot: %v", claim.SiteID, claim.Kind, err))
			continue
		}
		if delivered == 0 {
			outcome.Skipped++
		} else {
			outcome.Handled++
		}
	}

	pending, err := n.Store.HasRecoverablePendingAlerts(ctx)
	if err != nil {
		failures = append(failures, err.Error())
	}
	if pending || len(failures) > 0 {
		if len(failures) > 0 {
			return outcome, fmt.Errorf("reports: %d pending alert recoveries failed: %s",
				len(failures), strings.Join(failures, "; "))
		}
		outcome.Note = fmt.Sprintf("recovered %d pending alerts; more recovery work remains", outcome.Handled)

		return outcome, nil
	}

	rules, err := n.Store.EnabledAlertRules(ctx)
	if err != nil {
		return outcome, err
	}
	if len(rules) == 0 && outcome.Handled == 0 && len(failures) == 0 {
		return jobs.Nothing("no spike or drop alerts are configured"), nil
	}

	for _, rule := range rules {
		if recovered[alertDeliveryKey(rule.SiteID, rule.Kind)] {
			outcome.Skipped++
			continue
		}
		site, err := n.Sites(ctx, rule.SiteID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("site %d: %v", rule.SiteID, err))
			continue
		}

		alert, fired, err := n.evaluate(ctx, site, rule)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", site.Domain, rule.Kind, err))
			continue
		}

		if !fired {
			outcome.Skipped++
			continue
		}

		triggered++

		targets := deliveryTargets(rule.Recipients, rule.SlackWebhookURL)
		if len(targets) == 0 {
			outcome.Skipped++
			continue
		}

		payload, err := json.Marshal(alert)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", site.Domain, rule.Kind, err))
			continue
		}

		claim, claimed, already, err := n.Store.ClaimAlertSnapshot(ctx, rule.SiteID, rule.Kind, targets, string(payload))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: %v", site.Domain, rule.Kind, err))
			continue
		}

		if !claimed {
			if already >= MaxAlertsPerDay {
				suppressed++
			}
			outcome.Skipped++

			if n.Log != nil {
				n.Log.Info("an alert delivery was already leased or suppressed by the rate limit",
					"domain", site.Domain, "kind", rule.Kind, "slots_today", already, "limit", MaxAlertsPerDay)
			}

			continue
		}

		rendered, err := RenderAlert(alert)
		if err != nil {
			_ = n.Store.ReleaseDelivery(ctx, claim)
			failures = append(failures, fmt.Sprintf("%s %s: %v", site.Domain, rule.Kind, err))
			continue
		}

		delivered, err := n.deliverClaim(ctx, rendered, claim, alert.DashboardURL, "alert_"+rule.Kind)
		if err != nil {
			_ = n.Store.ReleaseDelivery(ctx, claim)
			failures = append(failures, fmt.Sprintf("%s %s: %v", site.Domain, rule.Kind, err))
			continue
		}

		if delivered == 0 {
			outcome.Skipped++
			continue
		}

		outcome.Handled++
	}

	if len(failures) > 0 {
		return outcome, fmt.Errorf("reports: %d alert rules failed: %s", len(failures), strings.Join(failures, "; "))
	}

	if outcome.Handled == 0 {
		outcome.Note = fmt.Sprintf("%d rules evaluated, %d tripped a threshold, %d suppressed by the rate limit",
			len(rules), triggered, suppressed)
	}

	return outcome, nil
}

// alertDeliveryKey identifies one rule without exposing a site id as an
// external metric or log label. It is process-local bookkeeping for one run.
func alertDeliveryKey(siteID int64, kind string) string {
	return fmt.Sprintf("%d:%s", siteID, kind)
}

// evaluate decides whether one rule has tripped and builds the alert it would
// send. Building the alert here rather than at the send site means the numbers
// in the message are the ones the decision was made on, not a second reading
// taken a moment later.
func (n *Notifier) evaluate(ctx context.Context, site SiteRef, rule AlertRule) (Alert, bool, error) {
	alert := Alert{
		Domain:       site.Domain,
		Kind:         rule.Kind,
		Threshold:    rule.Threshold,
		DashboardURL: n.dashboardURL(site.Domain),
		TriggeredAt:  n.now(),
	}

	switch rule.Kind {
	case KindSpike:
		current, err := n.Source.CurrentVisitors(ctx, site)
		if err != nil {
			return Alert{}, false, err
		}

		alert.Observed = current

		if current < rule.Threshold {
			return alert, false, nil
		}

		alert.Headline = fmt.Sprintf("%d visitors are on the site right now", current)
		alert.Detail = fmt.Sprintf("That is at or above the spike threshold of %d. Something is sending you traffic — the sources report will say what.", rule.Threshold)

		return alert, true, nil

	case KindDrop:
		hours := rule.WindowHours
		if hours <= 0 {
			hours = DefaultDropWindowHours
		}

		visitors, err := n.Source.VisitorsInLastHours(ctx, site, hours)
		if err != nil {
			return Alert{}, false, err
		}

		alert.Observed = visitors

		if visitors >= rule.Threshold {
			return alert, false, nil
		}

		alert.Headline = fmt.Sprintf("Only %d unique visitors in the last %d hours", visitors, hours)
		alert.Detail = fmt.Sprintf("That is below the drop threshold of %d. If the site is normally busier than this, check the ingestion health panel — a drop to zero is usually a tracker or proxy problem rather than a traffic one.", rule.Threshold)

		return alert, true, nil
	}

	return Alert{}, false, fmt.Errorf("reports: %q is not a known alert kind", rule.Kind)
}

// SendNow renders and sends one site's report immediately, ignoring the
// schedule and the delivery ledger. It is what the "send a test report" button
// calls, and it exists because the only way to know an email renders and
// arrives is to make one arrive.
func (n *Notifier) SendNow(ctx context.Context, siteID int64, kind string, recipients []string) (Rendered, error) {
	if err := n.Validate(); err != nil {
		return Rendered{}, err
	}

	site, err := n.Sites(ctx, siteID)
	if err != nil {
		return Rendered{}, err
	}

	location := site.Location()
	now := n.now().In(location)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)

	from := to.AddDate(0, 0, -7)
	if kind == KindMonthly {
		from = to.AddDate(0, -1, 0)
	}

	due := Due{
		SiteID:   siteID,
		Domain:   site.Domain,
		Kind:     kind,
		From:     from,
		To:       to,
		Location: location,
	}

	snapshot, err := n.Source.Period(ctx, site, from, to)
	if err != nil {
		return Rendered{}, err
	}

	rendered, err := RenderReport(Report{
		Domain:       site.Domain,
		Kind:         kind,
		PeriodLabel:  due.Label(),
		DashboardURL: n.dashboardURL(site.Domain),
		Figures:      snapshot.Figures,
		TopPages:     snapshot.TopPages,
		TopSources:   snapshot.TopSources,
		Countries:    snapshot.Countries,
		GeneratedAt:  n.now(),
	})
	if err != nil {
		return Rendered{}, err
	}

	if len(recipients) > 0 {
		if _, err := n.deliver(ctx, rendered, recipients, "", n.dashboardURL(site.Domain), "report_preview"); err != nil {
			return rendered, err
		}
	}

	return rendered, nil
}

// ControlSiteLookup reads a site's identity out of control.db. It is a
// constructor rather than a method so the notifier holds a function and can be
// tested with a fixed one.
func ControlSiteLookup(db *sql.DB) SiteLookup {
	return func(ctx context.Context, siteID int64) (SiteRef, error) {
		var site SiteRef

		err := db.QueryRowContext(ctx, `
			SELECT id, account_id, domain, timezone FROM sites WHERE id = ?
		`, siteID).Scan(&site.SiteID, &site.AccountID, &site.Domain, &site.Timezone)

		if errors.Is(err, sql.ErrNoRows) {
			return SiteRef{}, fmt.Errorf("reports: site %d does not exist", siteID)
		}
		if err != nil {
			return SiteRef{}, fmt.Errorf("reports: read site %d: %w", siteID, err)
		}

		return site, nil
	}
}
