//
// unsubscribe_test.go
// That a link removes one address, and a forged one removes none.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
)

// unsubscriber builds one over a temporary system database, with a real sealer.
func unsubscriber(t *testing.T) (*Unsubscriber, *storeFixture) {
	t.Helper()

	sealer, err := auth.NewSealer(make([]byte, auth.KeySize))
	if err != nil {
		t.Fatal(err)
	}

	f := newStoreFixture(t)

	return &Unsubscriber{Store: f.store, Sealer: sealer, BaseURL: "https://app.feasible.lol"}, f
}

// save puts one subscription on the fixture's first site.
func (f *storeFixture) save(t *testing.T, kind string, recipients ...string) {
	t.Helper()

	if err := f.store.SaveSubscription(context.Background(), Subscription{
		SiteID:     f.siteA,
		Kind:       kind,
		Recipients: recipients,
		Enabled:    true,
	}); err != nil {
		t.Fatal(err)
	}
}

// token mints a link and hands back just the token.
func (u *Unsubscriber) token(t *testing.T, who Unsubscribed) string {
	t.Helper()

	link := u.Link(who)
	if link == "" {
		t.Fatal("no link was minted")
	}

	return strings.TrimPrefix(link, u.BaseURL+UnsubscribePath)
}

// TestATokenResolvesBackToOneAddressOnOneList is the whole mechanism: what goes
// into the link is what comes out of it.
func TestATokenResolvesBackToOneAddressOnOneList(t *testing.T) {
	u, f := unsubscriber(t)

	want := Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "anna@example.com"}

	got, ok := u.read(u.token(t, want))
	if !ok {
		t.Fatal("the token did not open")
	}

	if got != want {
		t.Errorf("the token opened to %+v, want %+v", got, want)
	}
}

// TestTwoRecipientsGetDifferentTokens keeps one person's link from removing
// somebody else.
func TestTwoRecipientsGetDifferentTokens(t *testing.T) {
	u, f := unsubscriber(t)

	who := Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly}

	who.Address = "anna@example.com"
	first := u.Link(who)

	who.Address = "ben@example.com"
	second := u.Link(who)

	if first == second {
		t.Fatal("two recipients were given the same link")
	}

	for _, link := range []string{first, second} {
		if strings.Contains(link, "@") || strings.Contains(link, "example.com") {
			t.Errorf("the address is readable in the link: %s", link)
		}
	}
}

// TestAForgedTokenRemovesNothing is what stops the endpoint being a way to
// empty somebody's recipient list.
func TestAForgedTokenRemovesNothing(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	f.save(t, KindWeekly, "anna@example.com", "ben@example.com")

	token := u.token(t, Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "anna@example.com"})

	for name, forged := range map[string]string{
		"empty":     "",
		"nonsense":  "not-a-token",
		"truncated": token[:len(token)-4],
		"flipped":   "A" + token[1:],
	} {
		if err := u.Remove(ctx, forged); err != nil {
			t.Errorf("%s token: %v", name, err)
		}

		subscription, err := u.Store.SubscriptionFor(ctx, f.siteA, KindWeekly)
		if err != nil {
			t.Fatal(err)
		}

		if len(subscription.Recipients) != 2 {
			t.Fatalf("%s token removed somebody: %q", name, subscription.Recipients)
		}
	}
}

// TestUnsubscribingRemovesOnlyThatAddress keeps the report going out to
// everybody else.
func TestUnsubscribingRemovesOnlyThatAddress(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	f.save(t, KindWeekly, "anna@example.com", "ben@example.com")
	f.save(t, KindMonthly, "anna@example.com")

	// Upper case, because an address is compared case-insensitively everywhere
	// else here and a mail client may quote it back differently.
	token := u.token(t, Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "ANNA@example.com"})

	if err := u.Remove(ctx, token); err != nil {
		t.Fatal(err)
	}

	weekly, err := u.Store.SubscriptionFor(ctx, f.siteA, KindWeekly)
	if err != nil {
		t.Fatal(err)
	}

	if len(weekly.Recipients) != 1 || weekly.Recipients[0] != "ben@example.com" {
		t.Errorf("the weekly recipients are %q, want only ben", weekly.Recipients)
	}

	if !weekly.Enabled {
		t.Error("the subscription itself was disabled")
	}

	// The monthly report is a different list, and was not asked about.
	monthly, err := u.Store.SubscriptionFor(ctx, f.siteA, KindMonthly)
	if err != nil {
		t.Fatal(err)
	}

	if len(monthly.Recipients) != 1 {
		t.Errorf("the monthly recipients are %q, want anna still on it", monthly.Recipients)
	}
}

// TestASecondClickStillReportsSuccess is what a person who clicked twice sees.
func TestASecondClickStillReportsSuccess(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	f.save(t, KindWeekly, "anna@example.com")

	token := u.token(t, Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "anna@example.com"})

	for i := range 2 {
		if err := u.Remove(ctx, token); err != nil {
			t.Fatalf("click %d: %v", i+1, err)
		}
	}
}

// TestDescribeNamesTheSiteAndTheList is what the page tells somebody before
// they press the button.
func TestDescribeNamesTheSiteAndTheList(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	f.save(t, KindWeekly, "anna@example.com")

	token := u.token(t, Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "anna@example.com"})

	site, list, ok := u.Describe(ctx, token)
	if !ok {
		t.Fatal("a real token was not recognised")
	}

	if site != "acme.example" {
		t.Errorf("the site is %q", site)
	}

	if !strings.Contains(list, KindWeekly) || !strings.Contains(list, ListReport) {
		t.Errorf("the list is %q, want it to name the kind and the list", list)
	}

	if _, _, ok := u.Describe(ctx, "not-a-token"); ok {
		t.Error("a forged token was described as real")
	}
}

// TestNoSealerMintsNoLink keeps a self-hosted install with no application key
// sending its reports rather than failing to render them.
func TestNoSealerMintsNoLink(t *testing.T) {
	var none *Unsubscriber

	if link := none.Link(Unsubscribed{List: ListReport, SiteID: 1, Address: "a@example.com"}); link != "" {
		t.Errorf("a nil unsubscriber minted %q", link)
	}

	empty := &Unsubscriber{}
	if link := empty.Link(Unsubscribed{List: ListReport, SiteID: 1, Address: "a@example.com"}); link != "" {
		t.Errorf("an unconfigured unsubscriber minted %q", link)
	}
}

// TestAClaimIsRefusedUnlessItIsExactlyWhatWeWrote is defence in depth around
// the one value that chooses a table name.
func TestAClaimIsRefusedUnlessItIsExactlyWhatWeWrote(t *testing.T) {
	for name, plaintext := range map[string]string{
		"empty":           "",
		"three parts":     "report\x001\x00anna@example.com",
		"five parts":      "report\x001\x00weekly\x00anna@example.com\x00extra",
		"an unknown list": "digest\x001\x00weekly\x00anna@example.com",
		"a table name":    "alert_rules\x001\x00weekly\x00anna@example.com",
		"no site":         "report\x00\x00weekly\x00anna@example.com",
		"a site of zero":  "report\x000\x00weekly\x00anna@example.com",
		"a negative site": "report\x00-1\x00weekly\x00anna@example.com",
		"no kind":         "report\x001\x00\x00anna@example.com",
		"no address":      "report\x001\x00weekly\x00",
		"not a number":    "report\x00one\x00weekly\x00anna@example.com",
		"a different sep": "report|1|weekly|anna@example.com",
	} {
		if _, ok := parseClaim(plaintext); ok {
			t.Errorf("%s was accepted: %q", name, plaintext)
		}
	}

	for name, plaintext := range map[string]string{
		"a report": "report\x0042\x00weekly\x00anna@example.com",
		"an alert": "alert\x0042\x00spike\x00anna@example.com",
	} {
		if _, ok := parseClaim(plaintext); !ok {
			t.Errorf("%s was refused: %q", name, plaintext)
		}
	}
}

// TestAnAlertRecipientCanLeaveToo covers the other table end to end, since the
// list chooses which one every query runs against.
func TestAnAlertRecipientCanLeaveToo(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	if err := u.Store.SaveAlertRule(ctx, AlertRule{
		SiteID:      f.siteA,
		Kind:        KindSpike,
		Threshold:   10,
		WindowHours: 1,
		Recipients:  []string{"anna@example.com", "ben@example.com"},
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}

	token := u.token(t, Unsubscribed{List: ListAlert, SiteID: f.siteA, Kind: KindSpike, Address: "anna@example.com"})

	site, list, ok := u.Describe(ctx, token)
	if !ok || site != "acme.example" || list != "alert_spike" {
		t.Fatalf("describe = %q/%q/%v", site, list, ok)
	}

	if err := u.Remove(ctx, token); err != nil {
		t.Fatal(err)
	}

	rules, err := u.Store.AlertRulesFor(ctx, f.siteA)
	if err != nil {
		t.Fatal(err)
	}

	if len(rules) != 1 || len(rules[0].Recipients) != 1 || rules[0].Recipients[0] != "ben@example.com" {
		t.Fatalf("the rule's recipients are %+v", rules)
	}
}

// TestALinkMintedBeforeTheListWasTidiedStillWorks is why the address is matched
// on its mailbox: these links are clicked months later, and an owner who
// rewrites "Anna <anna@…>" to "anna@…" must not silently break them.
func TestALinkMintedBeforeTheListWasTidiedStillWorks(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	f.save(t, KindWeekly, "Anna <anna@example.com>", "ben@example.com")

	token := u.token(t, Unsubscribed{
		List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "Anna <anna@example.com>",
	})

	f.save(t, KindWeekly, "anna@example.com", "ben@example.com")

	if err := u.Remove(ctx, token); err != nil {
		t.Fatal(err)
	}

	subscription, err := u.Store.SubscriptionFor(ctx, f.siteA, KindWeekly)
	if err != nil {
		t.Fatal(err)
	}

	if len(subscription.Recipients) != 1 || subscription.Recipients[0] != "ben@example.com" {
		t.Errorf("the recipients are %q", subscription.Recipients)
	}
}

// TestADatabaseFailureIsNotReportedAsSuccess is the never-fail-silently rule on
// the one path where failing silently sends the next report to somebody who
// believes they left.
func TestADatabaseFailureIsNotReportedAsSuccess(t *testing.T) {
	u, f := unsubscriber(t)
	ctx := context.Background()

	f.save(t, KindWeekly, "anna@example.com")

	token := u.token(t, Unsubscribed{List: ListReport, SiteID: f.siteA, Kind: KindWeekly, Address: "anna@example.com"})

	if _, err := f.db.Exec("ALTER TABLE report_subscriptions RENAME COLUMN recipients TO gone"); err != nil {
		t.Fatal(err)
	}

	if err := u.Remove(ctx, token); err == nil {
		t.Error("a broken database was reported as a successful unsubscribe")
	}
}
