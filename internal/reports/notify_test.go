//
// notify_test.go
// The two jobs, end to end, against fixed numbers.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
)

// fakeSource answers with numbers a test sets, so the interesting behaviour —
// when a notifier fires and when it does not — is tested without a database
// full of events.
type fakeSource struct {
	visitors int
	current  int
	rolling  int
	err      error
}

// Period returns a fixed snapshot.
func (f *fakeSource) Period(context.Context, SiteRef, time.Time, time.Time) (Snapshot, error) {
	if f.err != nil {
		return Snapshot{}, f.err
	}

	return Snapshot{
		Visitors: f.visitors,
		Figures: []Figure{
			{Label: "Unique visitors", Value: "1,234", Change: "+3%", Direction: "up"},
		},
		TopPages:   []Entry{{Label: "/", Value: "900"}},
		TopSources: []Entry{{Label: "Direct", Value: "700"}},
		Countries:  []Entry{{Label: "United Kingdom", Value: "500"}},
	}, nil
}

// CurrentVisitors returns the live count a spike alert reads.
func (f *fakeSource) CurrentVisitors(context.Context, SiteRef) (int, error) {
	return f.current, f.err
}

// VisitorsInLastHours returns the rolling count a drop alert reads.
func (f *fakeSource) VisitorsInLastHours(context.Context, SiteRef, int) (int, error) {
	return f.rolling, f.err
}

// captureTransport records what would have been sent. It stands in for the
// process's mailer, so it answers with the same Result the real one does.
type captureTransport struct {
	mu       sync.Mutex
	messages []mail.Message
	err      error
	failures map[string]int
}

// slowTransport holds a provider call open long enough to cross several short
// test leases.
type slowTransport struct {
	started chan struct{}
	delay   time.Duration
	once    sync.Once
}

// Send blocks until the configured delay or cancellation.
func (s *slowTransport) Send(ctx context.Context, _ mail.Message) (mail.Result, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return mail.Result{}, ctx.Err()
	case <-time.After(s.delay):
		return mail.Result{Transport: "slow", Accepted: true}, nil
	}
}

// acceptedThenFailedTransport simulates the residual at-least-once window: the
// provider accepted the first call but the process did not durably acknowledge
// it. Its idempotent method records the stable key a real provider can collapse.
type acceptedThenFailedTransport struct {
	keys  []string
	calls int
}

// Send satisfies the base mail interface; deliverClaim should prefer the
// idempotent extension below.
func (a *acceptedThenFailedTransport) Send(context.Context, mail.Message) (mail.Result, error) {
	return mail.Result{}, errors.New("non-idempotent send should not be called")
}

// SendIdempotent accepts then reports a failure once, succeeding on replay.
func (a *acceptedThenFailedTransport) SendIdempotent(_ context.Context, _ mail.Message, key string) (mail.Result, error) {
	a.keys = append(a.keys, key)
	a.calls++
	if a.calls == 1 {
		return mail.Result{Transport: "capture", Accepted: true}, errors.New("process died before acknowledgement")
	}

	return mail.Result{Transport: "capture", Accepted: true}, nil
}

// Send records one message.
func (c *captureTransport) Send(_ context.Context, message mail.Message) (mail.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err != nil {
		return mail.Result{Transport: "capture", Detail: c.err.Error()}, c.err
	}
	if c.failures[message.To] > 0 {
		c.failures[message.To]--
		return mail.Result{Transport: "capture", Detail: "recipient refused"}, errors.New("recipient refused")
	}

	c.messages = append(c.messages, message)

	return mail.Result{Transport: "capture", Accepted: true}, nil
}

// count reports how many messages were captured.
func (c *captureTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.messages)
}

// countTo reports how many successful messages one destination received.
func (c *captureTransport) countTo(address string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for _, message := range c.messages {
		if message.To == address {
			count++
		}
	}

	return count
}

// countSubject reports how many successful messages include text in the subject.
func (c *captureTransport) countSubject(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for _, message := range c.messages {
		if strings.Contains(message.Subject, text) {
			count++
		}
	}

	return count
}

// TestSlowFanoutRenewsItsLease proves another worker cannot reclaim a live
// destination while the provider call runs longer than the original lease.
func TestSlowFanoutRenewsItsLease(t *testing.T) {
	f := newStoreFixture(t)
	f.store.Now = func() time.Time { return time.Now().UTC() }
	f.store.Lease = 3 * time.Second
	claim, claimed, err := f.store.ClaimPeriod(context.Background(), f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}

	transport := &slowTransport{started: make(chan struct{}), delay: 5 * time.Second}
	notifier := &Notifier{Store: f.store, Mail: transport}
	done := make(chan error, 1)
	go func() {
		_, err := notifier.deliverClaim(context.Background(), Rendered{Subject: "Report", Text: "body"}, claim, "", "report")
		done <- err
	}()
	<-transport.started
	time.Sleep(3500 * time.Millisecond)

	_, reclaimed, err := f.store.ClaimPeriod(context.Background(), f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Fatal("a live slow delivery was reclaimed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestProviderAcceptanceBeforeAcknowledgementIsAtLeastOnce documents the
// unavoidable replay and verifies provider-capable sends receive one stable
// idempotency key across that retry.
func TestProviderAcceptanceBeforeAcknowledgementIsAtLeastOnce(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	claim, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}

	transport := &acceptedThenFailedTransport{}
	notifier := &Notifier{Store: f.store, Mail: transport}
	if _, err := notifier.deliverClaim(ctx, Rendered{Subject: "Report", Text: "body"}, claim, "", "report"); err == nil {
		t.Fatal("simulated post-acceptance crash reported success")
	}
	if err := f.store.ReleaseDelivery(ctx, claim); err != nil {
		t.Fatal(err)
	}

	retry, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil || !claimed {
		t.Fatalf("retry claim = %v, %v", claimed, err)
	}
	if _, err := notifier.deliverClaim(ctx, Rendered{Subject: "Report", Text: "body"}, retry, "", "report"); err != nil {
		t.Fatal(err)
	}
	if len(transport.keys) != 2 || transport.keys[0] != transport.keys[1] {
		t.Fatalf("retry idempotency keys = %v, want one stable key", transport.keys)
	}
}

// capturePoster records what would have been posted to Slack.
type capturePoster struct {
	posts []string
	err   error
}

// Post records one message.
func (c *capturePoster) Post(_ context.Context, _, text string) error {
	if c.err != nil {
		return c.err
	}

	c.posts = append(c.posts, text)

	return nil
}

// notifierFixture wires a notifier over the store fixture.
type notifierFixture struct {
	*storeFixture

	notifier *Notifier
	source   *fakeSource
	mail     *captureTransport
	slack    *capturePoster
}

// newNotifier builds the whole thing.
func newNotifier(t *testing.T) *notifierFixture {
	t.Helper()

	base := newStoreFixture(t)

	f := &notifierFixture{
		storeFixture: base,
		source:       &fakeSource{visitors: 1234, current: 2, rolling: 40},
		mail:         &captureTransport{failures: map[string]int{}},
		slack:        &capturePoster{},
	}

	f.notifier = &Notifier{
		Store:   base.store,
		Source:  f.source,
		Sites:   SystemSiteLookup(base.db),
		Mail:    f.mail,
		Slack:   f.slack,
		BaseURL: "https://feasible.lol",
		Now:     func() time.Time { return base.now },
	}

	return f
}

// TestTheSchedulerSaysWhyItDidNothing is the third failure mode from the issue.
//
// A notifier job that did nothing must not record success. Every path out of
// the scheduler carries a reason, so a job that has silently stopped sending is
// visible as a note rather than as an absence of email nobody notices for weeks.
func TestTheSchedulerSaysWhyItDidNothing(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	// No subscriptions at all.
	outcome, err := f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if err := outcome.Validate(); err != nil {
		t.Fatalf("the scheduler reported success without a reason: %v", err)
	}

	if !strings.Contains(outcome.Note, "no sites") {
		t.Fatalf("the note does not explain the no-op: %q", outcome.Note)
	}

	// A subscription, but nothing due this hour.
	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Recipients: []string{"anna@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	outcome, err = f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if err := outcome.Validate(); err != nil {
		t.Fatalf("a run with nothing due reported success without a reason: %v", err)
	}

	if !strings.Contains(outcome.Note, "local period boundary") {
		t.Fatalf("the note does not explain the no-op: %q", outcome.Note)
	}
}

// TestAWeeklyReportGoesOutAtTheLocalBoundary drives the whole path.
func TestAWeeklyReportGoesOutAtTheLocalBoundary(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID:          f.siteA,
		Kind:            KindWeekly,
		Recipients:      []string{"anna@example.com", "sam@example.com"},
		SlackWebhookURL: "https://hooks.example.com/abc",
		Enabled:         true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 04:05 UTC on Monday 3 August 2026 is 00:05 in New York.
	f.now = time.Date(2026, 8, 3, 4, 5, 0, 0, time.UTC)

	outcome, err := f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if outcome.Handled != 1 {
		t.Fatalf("the scheduler handled %d reports, want 1 (note %q)", outcome.Handled, outcome.Note)
	}

	// One message per recipient. The shared mailer addresses one person at a
	// time, which is also what keeps a relay refusing one address from costing
	// the other subscriber their report.
	if f.mail.count() != 2 {
		t.Fatalf("%d emails were sent, want one per recipient (2)", f.mail.count())
	}

	if len(f.slack.posts) != 1 {
		t.Fatalf("%d Slack messages were posted, want 1", len(f.slack.posts))
	}

	addressed := map[string]bool{}
	for _, message := range f.mail.messages {
		addressed[message.To] = true

		if longest := mail.LongestLine(message.HTML); longest >= mail.MaxLineLength {
			t.Fatalf("the sent report has a %d-octet line", longest)
		}
	}

	for _, want := range []string{"anna@example.com", "sam@example.com"} {
		if !addressed[want] {
			t.Fatalf("%s did not receive the report", want)
		}
	}

	// Running again in the same hour must not send a second copy — the period
	// claim is what makes every process in a deployment safe to run this on.
	second, err := f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if second.Handled != 0 || f.mail.count() != 2 {
		t.Fatalf("the report was sent twice: handled=%d emails=%d", second.Handled, f.mail.count())
	}

	if err := second.Validate(); err != nil {
		t.Fatalf("the duplicate-suppressing run reported success without a reason: %v", err)
	}
}

// TestAFailedSendReleasesThePeriodSoTheRetryWorks checks that one SMTP hiccup
// does not cost a site its whole week's report.
func TestAFailedSendReleasesThePeriodSoTheRetryWorks(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Recipients: []string{"anna@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.now = time.Date(2026, 8, 3, 4, 5, 0, 0, time.UTC)
	f.mail.err = errors.New("the mail server said no")

	if _, err := f.notifier.RunSchedule(ctx, jobs.Job{}); err == nil {
		t.Fatal("a failed send reported no error")
	}

	f.mail.err = nil

	outcome, err := f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if outcome.Handled != 1 {
		t.Fatalf("the retry sent %d reports, want 1", outcome.Handled)
	}
}

// TestPartialReportRetrySkipsSuccessfulDestinations checks durable per-endpoint
// idempotency across email and Slack. The first address succeeds before the
// second fails; the retry sends only the second address and webhook.
func TestPartialReportRetrySkipsSuccessfulDestinations(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly,
		Recipients:      []string{"anna@example.com", "sam@example.com"},
		SlackWebhookURL: "https://hooks.example.com/abc",
		Enabled:         true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.now = time.Date(2026, 8, 3, 4, 5, 0, 0, time.UTC)
	f.mail.failures["sam@example.com"] = 1

	if _, err := f.notifier.RunSchedule(ctx, jobs.Job{}); err == nil {
		t.Fatal("the partial email failure was not reported")
	}
	if f.mail.countTo("anna@example.com") != 1 || f.mail.countTo("sam@example.com") != 0 {
		t.Fatalf("first attempt messages = %+v", f.mail.messages)
	}
	if len(f.slack.posts) != 0 {
		t.Fatal("Slack ran after an earlier destination failed")
	}

	outcome, err := f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if outcome.Handled != 1 {
		t.Fatalf("retry outcome = %+v", outcome)
	}
	if f.mail.countTo("anna@example.com") != 1 {
		t.Fatal("the retry duplicated the email that already succeeded")
	}
	if f.mail.countTo("sam@example.com") != 1 || len(f.slack.posts) != 1 {
		t.Fatalf("retry did not finish pending destinations: emails=%+v Slack=%d", f.mail.messages, len(f.slack.posts))
	}
}

// TestCrashedScheduledDeliveryUsesItsOriginalBucket checks both recovery
// layers together. The delivery lease expires after local midnight, while the
// retried job still evaluates the bucket captured before the crash.
func TestCrashedScheduledDeliveryUsesItsOriginalBucket(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Recipients: []string{"anna@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.now = time.Date(2026, 8, 3, 4, 5, 0, 0, time.UTC)
	due := DueAt(f.now, []ScheduledSite{{
		SiteID: f.siteA, Domain: "acme.example", Timezone: "America/New_York", Weekly: true,
	}})
	if len(due) != 1 {
		t.Fatalf("due reports = %+v", due)
	}

	first, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, due[0].PeriodKey,
		deliveryTargets([]string{"anna@example.com"}, ""))
	if err != nil || !claimed {
		t.Fatalf("crashed claim: claimed=%v err=%v", claimed, err)
	}

	f.now = f.now.Add(DeliveryLease + time.Minute)
	job := jobs.Job{Args: []byte(`{"scheduled_at":1785729600}`)}
	outcome, err := f.notifier.RunSchedule(ctx, job)
	if err != nil {
		t.Fatalf("recovered run: %v", err)
	}
	if outcome.Handled != 1 || f.mail.count() != 1 {
		t.Fatalf("recovered outcome=%+v emails=%d original_claim=%d", outcome, f.mail.count(), first.ID)
	}
}

// TestASubscriptionWithNoRecipientsIsSkippedWithAReason checks that a report
// nobody would receive does not silently claim its period.
func TestASubscriptionWithNoRecipientsIsSkippedWithAReason(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.now = time.Date(2026, 8, 3, 4, 5, 0, 0, time.UTC)

	outcome, err := f.notifier.RunSchedule(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if outcome.Handled != 0 || outcome.Skipped != 1 {
		t.Fatalf("outcome = %+v, want one skip", outcome)
	}

	if !strings.Contains(outcome.Note, "no recipients") {
		t.Fatalf("the note does not say why: %q", outcome.Note)
	}
}

// TestASpikeAlertFiresAtItsThreshold checks the default of ten current
// visitors, and the boundary in both directions.
func TestASpikeAlertFiresAtItsThreshold(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveAlertRule(ctx, AlertRule{
		SiteID: f.siteA, Kind: KindSpike, Threshold: DefaultSpikeThreshold,
		Recipients: []string{"ops@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.source.current = DefaultSpikeThreshold - 1

	outcome, err := f.notifier.RunAlerts(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if outcome.Handled != 0 {
		t.Fatalf("the alert fired below its threshold")
	}

	if err := outcome.Validate(); err != nil {
		t.Fatalf("a quiet alert run reported success without a reason: %v", err)
	}

	f.source.current = DefaultSpikeThreshold

	outcome, err = f.notifier.RunAlerts(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if outcome.Handled != 1 {
		t.Fatalf("the alert did not fire at its threshold: %+v", outcome)
	}

	if f.mail.count() != 1 {
		t.Fatalf("%d alert emails were sent", f.mail.count())
	}

	if !strings.Contains(f.mail.messages[0].HTML, "https://feasible.lol/dashboard/acme.example") {
		t.Fatal("the alert has no dashboard link in it")
	}
}

// TestADropAlertFiresBelowItsThreshold checks the other rule, whose real
// question is "has my tracking stopped".
func TestADropAlertFiresBelowItsThreshold(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveAlertRule(ctx, AlertRule{
		SiteID: f.siteA, Kind: KindDrop, Threshold: DefaultDropThreshold,
		WindowHours: DefaultDropWindowHours, Recipients: []string{"ops@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.source.rolling = 1

	if outcome, _ := f.notifier.RunAlerts(ctx, jobs.Job{}); outcome.Handled != 0 {
		t.Fatal("the drop alert fired at its threshold rather than below it")
	}

	f.source.rolling = 0

	outcome, err := f.notifier.RunAlerts(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if outcome.Handled != 1 {
		t.Fatalf("the drop alert did not fire at zero visitors: %+v", outcome)
	}

	if !strings.Contains(f.mail.messages[0].HTML, "health panel") {
		t.Fatal("the drop alert does not point at the health panel, which is where the answer is")
	}
}

// TestTheRateLimitStopsAnIncidentBecomingAFlood runs the alert job repeatedly
// against a condition that stays true, which is exactly what an outage looks
// like.
func TestTheRateLimitStopsAnIncidentBecomingAFlood(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	if err := f.store.SaveAlertRule(ctx, AlertRule{
		SiteID: f.siteA, Kind: KindDrop, Threshold: 1, WindowHours: 12,
		Recipients: []string{"ops@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	f.source.rolling = 0

	// Twelve hours of the job running every ten minutes.
	for tick := 0; tick < 72; tick++ {
		if _, err := f.notifier.RunAlerts(ctx, jobs.Job{}); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}

		f.now = f.now.Add(AlertEvery)
	}

	if sent := f.mail.count(); sent > MaxAlertsPerDay {
		t.Fatalf("a twelve-hour outage sent %d alerts, want at most %d", sent, MaxAlertsPerDay)
	}

	if f.mail.count() == 0 {
		t.Fatal("a twelve-hour outage sent no alert at all")
	}
}

// TestOneBrokenSiteDoesNotStopTheOthers checks that a per-customer promise is
// kept per customer.
func TestOneBrokenSiteDoesNotStopTheOthers(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	for _, site := range []int64{f.siteA, f.siteB} {
		if err := f.store.SaveAlertRule(ctx, AlertRule{
			SiteID: site, Kind: KindSpike, Threshold: 1,
			Recipients: []string{"ops@example.com"}, Enabled: true,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	f.source.current = 100
	f.slack.err = errors.New("the webhook was revoked")

	// Only site A has a webhook, so only site A's delivery fails.
	if err := f.store.SaveAlertRule(ctx, AlertRule{
		SiteID: f.siteA, Kind: KindSpike, Threshold: 1,
		Recipients: []string{"ops@example.com"}, SlackWebhookURL: "https://hooks.example.com/dead", Enabled: true,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	outcome, err := f.notifier.RunAlerts(ctx, jobs.Job{})

	if err == nil {
		t.Fatal("a failed webhook was not reported")
	}

	if outcome.Handled != 1 {
		t.Fatalf("the other site's alert did not go out: %+v", outcome)
	}
	if sent := f.mail.countSubject("acme.example"); sent != 1 {
		t.Fatalf("site A sent %d successful emails before its Slack failure, want 1", sent)
	}

	// Recovering the webhook resumes site A's pending destination without
	// repeating its already successful email.
	f.slack.err = nil
	if _, err := f.notifier.RunAlerts(ctx, jobs.Job{}); err != nil {
		t.Fatalf("retry alerts: %v", err)
	}
	if sent := f.mail.countSubject("acme.example"); sent != 1 {
		t.Fatalf("site A resent its successful email during retry: %d sends", sent)
	}
	if len(f.slack.posts) != 1 {
		t.Fatalf("site A posted %d Slack alerts after recovery, want 1", len(f.slack.posts))
	}
}

// TestPendingAlertRecoveryDrainsInBoundedBatchesBeforeLiveEvaluation proves a
// backlog larger than one run's budget is neither capped forever nor mixed with
// newly evaluated rules before durable recovery drains.
func TestPendingAlertRecoveryDrainsInBoundedBatchesBeforeLiveEvaluation(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()
	for index := 0; index < MaxPendingAlertRecoveries+5; index++ {
		siteID := f.site(t, fmt.Sprintf("backlog-%03d.example", index), "UTC")
		insertPendingAlertSnapshot(t, f, siteID, f.now.Unix(), fmt.Sprintf("ops-%03d@example.com", index))
	}

	outcome, err := f.notifier.RunAlerts(ctx, jobs.Job{})
	if err != nil {
		t.Fatalf("first recovery batch: %v", err)
	}
	if outcome.Handled != MaxPendingAlertRecoveries || f.mail.count() != MaxPendingAlertRecoveries ||
		!strings.Contains(outcome.Note, "more recovery work remains") {
		t.Fatalf("first batch outcome/mail = %+v/%d", outcome, f.mail.count())
	}
	if _, err := f.notifier.RunAlerts(ctx, jobs.Job{}); err != nil {
		t.Fatalf("second recovery batch: %v", err)
	}
	if f.mail.count() != MaxPendingAlertRecoveries+5 {
		t.Fatalf("two batches delivered %d snapshots, want %d", f.mail.count(), MaxPendingAlertRecoveries+5)
	}
}

// TestRateBlockedOldestPendingAlertDoesNotStarveLaterSites proves the recovery
// selector skips an old site whose current rate window is full.
func TestRateBlockedOldestPendingAlertDoesNotStarveLaterSites(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()
	old := f.now.Add(-2 * RateWindow).Unix()
	insertPendingAlertSnapshot(t, f, f.siteA, old, "blocked@example.com")
	for index := 0; index < MaxAlertsPerDay; index++ {
		if _, err := f.db.Exec(`
			INSERT INTO notification_claims
				(site_id, kind, state, recipients, created_at, completed_at)
			VALUES (?, 'drop', 'completed', 1, ?, ?)
		`, f.siteA, f.now.Unix(), f.now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	insertPendingAlertSnapshot(t, f, f.siteB, old+1, "later@example.com")

	outcome, err := f.notifier.RunAlerts(ctx, jobs.Job{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Handled != 1 || f.mail.countTo("later@example.com") != 1 ||
		f.mail.countTo("blocked@example.com") != 0 {
		t.Fatalf("blocked-oldest outcome/later/blocked = %+v/%d/%d", outcome,
			f.mail.countTo("later@example.com"), f.mail.countTo("blocked@example.com"))
	}
}

// insertPendingAlertSnapshot writes the exact durable state left by a worker
// that crashed after snapshotting destinations but before delivery.
func insertPendingAlertSnapshot(t *testing.T, f *notifierFixture, siteID, createdAt int64, recipient string) {
	t.Helper()
	payload, err := json.Marshal(Alert{
		Domain: "recovery.example", Kind: KindSpike, Headline: "Traffic spike",
		Detail: "Recovery", Threshold: 10, Observed: 20,
		DashboardURL: "https://feasible.lol/dashboard/recovery.example", TriggeredAt: f.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.db.Exec(`
		INSERT INTO notification_claims (site_id, kind, payload, created_at)
		VALUES (?, 'spike', ?, ?)
	`, siteID, string(payload), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	claimID, _ := result.LastInsertId()
	if _, err := f.db.Exec(`
		INSERT INTO notification_destinations (notification_id, destination_key, channel, target)
		VALUES (?, ?, 'email', ?)
	`, claimID, "email:"+recipient, recipient); err != nil {
		t.Fatal(err)
	}
}

// TestCrashedAlertSnapshotFinishesAfterRuleStateChanges proves recovery happens
// before live evaluation. A snapshotted Slack destination remains owed after a
// crash even when the condition clears or the operator disables the rule.
func TestCrashedAlertSnapshotFinishesAfterRuleStateChanges(t *testing.T) {
	for _, changed := range []string{"condition cleared", "rule disabled"} {
		t.Run(changed, func(t *testing.T) {
			f := newNotifier(t)
			ctx := context.Background()
			rule := AlertRule{
				SiteID: f.siteA, Kind: KindSpike, Threshold: 10,
				Recipients: []string{"ops@example.test"}, SlackWebhookURL: "https://hooks.example.test/alert", Enabled: true,
			}
			if err := f.store.SaveAlertRule(ctx, rule); err != nil {
				t.Fatal(err)
			}
			alert := Alert{
				Domain: "acme.example", Kind: KindSpike, Headline: "20 visitors are on the site right now",
				Detail: "The snapshotted threshold was exceeded.", Threshold: 10, Observed: 20,
				DashboardURL: "https://feasible.lol/dashboard/acme.example", TriggeredAt: f.now,
			}
			payload, err := json.Marshal(alert)
			if err != nil {
				t.Fatal(err)
			}
			claim, claimed, _, err := f.store.ClaimAlertSnapshot(ctx, f.siteA, KindSpike,
				deliveryTargets(rule.Recipients, rule.SlackWebhookURL), string(payload))
			if err != nil || !claimed {
				t.Fatalf("initial snapshot claim = %v, %v", claimed, err)
			}
			for _, destination := range claim.Destinations {
				if destination.Channel == ChannelEmail {
					if err := f.store.MarkDestinationSent(ctx, claim, destination.ID); err != nil {
						t.Fatal(err)
					}
				}
			}

			// The process dies without releasing or completing the claim. Recovery
			// starts only after its durable lease expires.
			f.now = f.now.Add(DeliveryLease + time.Second)
			if changed == "condition cleared" {
				f.source.current = 0
			} else {
				rule.Enabled = false
				if err := f.store.SaveAlertRule(ctx, rule); err != nil {
					t.Fatal(err)
				}
			}

			outcome, err := f.notifier.RunAlerts(ctx, jobs.Job{})
			if err != nil {
				t.Fatalf("recover alert: %v", err)
			}
			if outcome.Handled != 1 || f.mail.count() != 0 || len(f.slack.posts) != 1 {
				t.Fatalf("recovered outcome/mail/Slack = %+v/%d/%d, want handled 1/0/1",
					outcome, f.mail.count(), len(f.slack.posts))
			}
			var state string
			if err := f.db.QueryRow(`SELECT state FROM notification_claims WHERE id = ?`, claim.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "completed" {
				t.Fatalf("recovered claim state = %q, want completed", state)
			}
		})
	}
}

// TestANotifierWithNoBaseURLRefusesToRun checks the guard that stops an email
// shipping with a dead link — the exact bug the strict templating exists for,
// caught one layer earlier.
func TestANotifierWithNoBaseURLRefusesToRun(t *testing.T) {
	f := newNotifier(t)
	f.notifier.BaseURL = ""

	if _, err := f.notifier.RunSchedule(context.Background(), jobs.Job{}); err == nil {
		t.Fatal("a notifier with no base URL ran")
	}
}

// TestSendNowIgnoresTheScheduleAndTheLedger checks the "send one now" button.
func TestSendNowIgnoresTheScheduleAndTheLedger(t *testing.T) {
	f := newNotifier(t)
	ctx := context.Background()

	rendered, err := f.notifier.SendNow(ctx, f.siteA, KindWeekly, []string{"anna@example.com"})
	if err != nil {
		t.Fatalf("send now: %v", err)
	}

	if f.mail.count() != 1 {
		t.Fatalf("%d emails were sent", f.mail.count())
	}

	if !strings.Contains(rendered.Subject, "acme.example") {
		t.Fatalf("the subject does not name the site: %q", rendered.Subject)
	}

	// It must not have consumed the week's slot: the scheduled send still has
	// to go out at the local boundary.
	_, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if !claimed {
		t.Fatal("sending one now consumed the scheduled period")
	}
}
