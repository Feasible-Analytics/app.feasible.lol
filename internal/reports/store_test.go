//
// store_test.go
// Subscriptions, alert rules, the period claim and the rate limiter.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// storeFixture is a system database with one team and two sites.
type storeFixture struct {
	db     *sql.DB
	store  *Store
	now    time.Time
	teamID int64
	siteA  int64
	siteB  int64
}

// newStoreFixture builds and seeds the database.
func newStoreFixture(t *testing.T) *storeFixture {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &storeFixture{db: db, now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	f.store = NewStore(db)
	f.store.Now = func() time.Time { return f.now }

	result, err := db.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', ?, ?)`,
		f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}
	f.teamID, _ = result.LastInsertId()

	f.siteA = f.site(t, "acme.example", "America/New_York")
	f.siteB = f.site(t, "beta.example", "Asia/Tokyo")

	return f
}

// site inserts a site and returns its id.
func (f *storeFixture) site(t *testing.T, domain, timezone string) int64 {
	t.Helper()

	result, err := f.db.Exec(`
		INSERT INTO sites (account_id, domain, timezone, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
	`, f.teamID, domain, timezone, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}

	id, _ := result.LastInsertId()

	return id
}

// testDestinations gives every claimed notification one endpoint to finish.
func testDestinations() []DestinationTarget {
	return []DestinationTarget{{Channel: ChannelEmail, Target: "ops@example.com"}}
}

// completeClaim marks every endpoint sent and closes the logical notification.
func completeClaim(t *testing.T, store *Store, claim DeliveryClaim) {
	t.Helper()

	for _, destination := range claim.Destinations {
		if err := store.MarkDestinationSent(context.Background(), claim, destination.ID); err != nil {
			t.Fatalf("mark destination: %v", err)
		}
	}

	if _, err := store.CompleteDelivery(context.Background(), claim); err != nil {
		t.Fatalf("complete delivery: %v", err)
	}
}

// TestAtMostTwoAlertsPerSitePerDay is the acceptance criterion.
//
// The condition an alert watches for stays true for as long as the incident
// lasts, so without the cap a two-day outage sends a message every ten minutes —
// three hundred of them — and the first thing the recipient does is add a filter.
func TestAtMostTwoAlertsPerSitePerDay(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	for i := 0; i < MaxAlertsPerDay; i++ {
		claim, claimed, used, err := f.store.ClaimAlert(ctx, f.siteA, KindSpike, testDestinations())
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if !claimed {
			t.Fatalf("alert %d was suppressed with only %d slots used", i, used)
		}
		completeClaim(t, f.store, claim)

		f.now = f.now.Add(10 * time.Minute)
	}

	_, claimed, used, err := f.store.ClaimAlert(ctx, f.siteA, KindSpike, testDestinations())
	if err != nil {
		t.Fatalf("claim over cap: %v", err)
	}
	if claimed {
		t.Fatal("a third alert was allocated a slot")
	}
	if used != MaxAlertsPerDay {
		t.Fatalf("the limiter counted %d slots, want %d", used, MaxAlertsPerDay)
	}
}

// TestRecipientFanoutIsBoundedAndDeduplicated rejects an oversized audience at
// configuration time and keeps duplicate spellings from consuming slots.
func TestRecipientFanoutIsBoundedAndDeduplicated(t *testing.T) {
	f := newStoreFixture(t)
	recipients := make([]string, 0, MaxRecipients+1)
	for index := 0; index <= MaxRecipients; index++ {
		recipients = append(recipients, fmt.Sprintf("person-%d@example.test", index))
	}
	if err := f.store.SaveSubscription(context.Background(), Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Recipients: recipients, Enabled: true,
	}); err == nil {
		t.Fatalf("saved %d recipients, want a bound at %d", len(recipients), MaxRecipients)
	}

	if err := f.store.SaveSubscription(context.Background(), Subscription{
		SiteID: f.siteA, Kind: KindWeekly,
		Recipients: []string{"Ops@example.test", "ops@example.test"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	subscription, err := f.store.SubscriptionFor(context.Background(), f.siteA, KindWeekly)
	if err != nil {
		t.Fatal(err)
	}
	if len(subscription.Recipients) != 1 {
		t.Fatalf("duplicate recipients persisted as %v", subscription.Recipients)
	}
}

// TestTheRateLimitIsPerSite checks that one noisy site does not silence another.
func TestTheRateLimitIsPerSite(t *testing.T) {
	f := newStoreFixture(t)

	for i := 0; i < MaxAlertsPerDay; i++ {
		claim, claimed, _, err := f.store.ClaimAlert(context.Background(), f.siteA, KindDrop, testDestinations())
		if err != nil || !claimed {
			t.Fatalf("claim site A %d: claimed=%v err=%v", i, claimed, err)
		}
		completeClaim(t, f.store, claim)
	}

	_, claimed, _, err := f.store.ClaimAlert(context.Background(), f.siteB, KindDrop, testDestinations())
	if err != nil {
		t.Fatalf("claim site B: %v", err)
	}
	if !claimed {
		t.Fatal("one site's alerts silenced another site")
	}
}

// TestTheRateWindowRollsForward checks that the budget refills, so an incident
// that lasts three days still produces two alerts a day rather than two ever.
func TestTheRateWindowRollsForward(t *testing.T) {
	f := newStoreFixture(t)

	for i := 0; i < MaxAlertsPerDay; i++ {
		claim, claimed, _, err := f.store.ClaimAlert(context.Background(), f.siteA, KindDrop, testDestinations())
		if err != nil || !claimed {
			t.Fatalf("claim %d: claimed=%v err=%v", i, claimed, err)
		}
		completeClaim(t, f.store, claim)
	}

	if _, claimed, _, _ := f.store.ClaimAlert(context.Background(), f.siteA, KindDrop, testDestinations()); claimed {
		t.Fatal("the limit did not apply")
	}

	f.now = f.now.Add(RateWindow + time.Minute)

	if _, claimed, used, _ := f.store.ClaimAlert(context.Background(), f.siteA, KindDrop, testDestinations()); !claimed {
		t.Fatalf("the budget did not refill after the window: %d still counted", used)
	}
}

// TestScheduledReportsDoNotConsumeTheAlertBudget is the decision spelled out.
//
// Reports are already limited by their own schedule — at most one weekly and
// one monthly per site per day — and letting them eat the alert budget would
// mean a site whose report went out this morning is silent through an outage
// this afternoon.
func TestScheduledReportsDoNotConsumeTheAlertBudget(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	for _, kind := range []string{KindWeekly, KindMonthly} {
		claim, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, kind, "2026-W35", testDestinations())
		if err != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", kind, claimed, err)
		}
		completeClaim(t, f.store, claim)
	}

	_, claimed, used, err := f.store.ClaimAlert(ctx, f.siteA, KindSpike, testDestinations())
	if err != nil {
		t.Fatalf("claim alert: %v", err)
	}
	if !claimed || used != 0 {
		t.Fatalf("two scheduled reports consumed %d of the alert budget", used)
	}
}

// TestAPeriodCanOnlyBeClaimedOnce is what makes the hourly job safe to run on
// every process in a deployment: they all decide Monday has arrived within the
// same minute, and one email goes out.
func TestAPeriodCanOnlyBeClaimedOnce(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	first, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	_, second, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}

	if !claimed || second {
		t.Fatalf("claims returned %v then %v, want true then false", claimed, second)
	}
	completeClaim(t, f.store, first)
}

// TestAReleasedClaimRetriesOnlyUnsentDestinations checks both halves of partial
// failure recovery: the period is available again and the successful address
// is absent from the next lease.
func TestAReleasedClaimRetriesOnlyUnsentDestinations(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()
	targets := []DestinationTarget{
		{Channel: ChannelEmail, Target: "first@example.com"},
		{Channel: ChannelSlack, Target: "https://hooks.example.com/one"},
	}

	first, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", targets)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := f.store.MarkDestinationSent(ctx, first, first.Destinations[0].ID); err != nil {
		t.Fatalf("mark first destination: %v", err)
	}
	if err := f.store.ReleaseDelivery(ctx, first); err != nil {
		t.Fatalf("release: %v", err)
	}

	again, reclaimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", targets)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !reclaimed {
		t.Fatal("a released period could not be claimed again")
	}
	if len(again.Destinations) != 1 || again.Destinations[0].Channel != ChannelSlack {
		t.Fatalf("retry destinations = %+v, want only Slack", again.Destinations)
	}
	completeClaim(t, f.store, again)
}

// TestAnExpiredLeaseRecoversAfterACrash checks that no cleanup callback is
// required. Advancing the durable clock past the lease is enough for another
// process to resume the same period and its pending destinations.
func TestAnExpiredLeaseRecoversAfterACrash(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	first, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}

	f.now = f.now.Add(DeliveryLease + time.Second)

	recovered, claimed, err := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations())
	if err != nil || !claimed {
		t.Fatalf("recover claim: claimed=%v err=%v", claimed, err)
	}
	if recovered.ID != first.ID {
		t.Fatalf("recovered claim %d, want original %d", recovered.ID, first.ID)
	}
	completeClaim(t, f.store, recovered)

	if _, again, _ := f.store.ClaimPeriod(ctx, f.siteA, KindWeekly, "2026-W35", testDestinations()); again {
		t.Fatal("a completed period was claimed again")
	}
}

// TestAnOldRecoveredAlertConsumesACurrentSlot checks a long outage around the
// notifier itself. Delivering a claim created more than a day ago must count
// against today's cap, or its delayed send plus two fresh alerts makes three.
func TestAnOldRecoveredAlertConsumesACurrentSlot(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	if _, claimed, _, err := f.store.ClaimAlert(ctx, f.siteA, KindSpike, testDestinations()); err != nil || !claimed {
		t.Fatalf("claim old alert: claimed=%v err=%v", claimed, err)
	}

	f.now = f.now.Add(RateWindow + DeliveryLease + time.Second)

	current, claimed, _, err := f.store.ClaimAlert(ctx, f.siteA, KindDrop, testDestinations())
	if err != nil || !claimed {
		t.Fatalf("claim current alert: claimed=%v err=%v", claimed, err)
	}
	completeClaim(t, f.store, current)

	recovered, claimed, _, err := f.store.ClaimAlert(ctx, f.siteA, KindSpike, testDestinations())
	if err != nil || !claimed {
		t.Fatalf("recover old alert: claimed=%v err=%v", claimed, err)
	}
	completeClaim(t, f.store, recovered)

	if _, claimed, used, err := f.store.ClaimAlert(ctx, f.siteA, KindDrop, testDestinations()); err != nil {
		t.Fatalf("claim over cap: %v", err)
	} else if claimed || used != MaxAlertsPerDay {
		t.Fatalf("old recovery left claimed=%v with %d slots used, want false and %d", claimed, used, MaxAlertsPerDay)
	}
}

// TestConcurrentAlertClaimsCannotExceedTheCap drives both alert kinds through
// separate goroutines. The count and insertion share one write transaction, so
// no pair can both observe the final free slot.
func TestConcurrentAlertClaimsCannotExceedTheCap(t *testing.T) {
	f := newStoreFixture(t)

	var wait sync.WaitGroup
	errs := make(chan error, 16)

	for worker := 0; worker < 16; worker++ {
		wait.Add(1)

		go func(worker int) {
			defer wait.Done()

			kind := KindSpike
			if worker%2 == 1 {
				kind = KindDrop
			}

			_, _, _, err := f.store.ClaimAlert(context.Background(), f.siteA, kind, testDestinations())
			if err != nil {
				errs <- err
			}
		}(worker)
	}

	wait.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent claim: %v", err)
	}

	var claims int
	if err := f.db.QueryRow(`
		SELECT COUNT(*) FROM notification_claims
		WHERE site_id = ? AND kind IN ('spike', 'drop')
	`, f.siteA).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims > MaxAlertsPerDay {
		t.Fatalf("concurrent workers allocated %d alert slots, cap is %d", claims, MaxAlertsPerDay)
	}
}

// TestSubscriptionsAreUpsertedNotDuplicated checks the settings screen's save.
func TestSubscriptionsAreUpsertedNotDuplicated(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	for _, recipients := range [][]string{{"a@example.com"}, {"a@example.com", "b@example.com"}} {
		if err := f.store.SaveSubscription(ctx, Subscription{
			SiteID: f.siteA, Kind: KindWeekly, Recipients: recipients, Enabled: true,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	subscriptions, err := f.store.Subscriptions(ctx, f.siteA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(subscriptions) != 1 {
		t.Fatalf("saving twice produced %d subscriptions", len(subscriptions))
	}

	if len(subscriptions[0].Recipients) != 2 {
		t.Fatalf("the second save did not replace the recipients: %+v", subscriptions[0].Recipients)
	}
}

// TestABadAddressIsRefusedAtSaveTime checks that a typo is caught by a screen
// that can say so rather than by a background job three days later.
func TestABadAddressIsRefusedAtSaveTime(t *testing.T) {
	f := newStoreFixture(t)

	err := f.store.SaveSubscription(context.Background(), Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Recipients: []string{"not an address"}, Enabled: true,
	})

	if err == nil {
		t.Fatal("an unparseable address was saved")
	}
}

// TestScheduledSitesReportsBothKinds checks the query the hourly job walks.
func TestScheduledSitesReportsBothKinds(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly, Recipients: []string{"a@example.com"}, Enabled: true,
	}); err != nil {
		t.Fatalf("save weekly: %v", err)
	}

	if err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindMonthly, Recipients: []string{"a@example.com"}, Enabled: false,
	}); err != nil {
		t.Fatalf("save monthly: %v", err)
	}

	sites, err := f.store.ScheduledSites(ctx)
	if err != nil {
		t.Fatalf("scheduled sites: %v", err)
	}

	if len(sites) != 1 {
		t.Fatalf("%d sites are scheduled, want 1", len(sites))
	}

	if !sites[0].Weekly || sites[0].Monthly {
		t.Fatalf("the flags are wrong: %+v", sites[0])
	}

	if sites[0].Timezone != "America/New_York" {
		t.Fatalf("the timezone is %q, want the site's own", sites[0].Timezone)
	}
}

// TestAlertDefaultsMatchTheIssue pins the two numbers the product promises.
func TestAlertDefaultsMatchTheIssue(t *testing.T) {
	if DefaultSpikeThreshold != 10 {
		t.Errorf("the spike default is %d, want 10 current visitors", DefaultSpikeThreshold)
	}

	if DefaultDropThreshold != 1 {
		t.Errorf("the drop default is %d, want 1 unique visitor", DefaultDropThreshold)
	}

	if DefaultDropWindowHours != 12 {
		t.Errorf("the drop window is %d hours, want 12", DefaultDropWindowHours)
	}
}

// TestSavingAnAlertWithNoThresholdUsesTheDefault checks that an empty form
// field produces a working alert rather than one that fires on every event.
func TestSavingAnAlertWithNoThresholdUsesTheDefault(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	for kind, want := range map[string]int{KindSpike: DefaultSpikeThreshold, KindDrop: DefaultDropThreshold} {
		if err := f.store.SaveAlertRule(ctx, AlertRule{SiteID: f.siteA, Kind: kind, Enabled: true}); err != nil {
			t.Fatalf("save %s: %v", kind, err)
		}

		rules, err := f.store.AlertRulesFor(ctx, f.siteA)
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		for _, rule := range rules {
			if rule.Kind == kind && rule.Threshold != want {
				t.Errorf("%s threshold defaulted to %d, want %d", kind, rule.Threshold, want)
			}
		}
	}
}

// TestReadingAMissingSubscriptionIsNotAnError checks the sentinel the notifier
// branches on.
func TestReadingAMissingSubscriptionIsNotAnError(t *testing.T) {
	f := newStoreFixture(t)

	if _, err := f.store.SubscriptionFor(context.Background(), f.siteA, KindWeekly); !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("SubscriptionFor on an unconfigured site = %v, want ErrNoSubscription", err)
	}
}
