//
// overview_test.go
// The all-sites analytics read: per-site figures and how they roll up.
//
// Created: 2026-09-04
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// visit describes one seeded session and the pageviews inside it, which is
// everything the all-sites screen counts except goals.
type visit struct {
	siteID    int64
	user      int64
	startedAt int64
	duration  int64
	bounce    int64
	pageviews int
}

// seedVisits writes sessions and their pageview events into an account,
// interning the event name exactly as the ingest path does. Without the
// interned name the pageview metric has nothing to compare against and every
// count comes back zero.
func seedVisits(t *testing.T, manager *accounts.Manager, accountID int64, visits []visit) {
	t.Helper()

	ctx := context.Background()

	account, err := manager.Open(ctx, accountID)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}

	nameID, err := account.Intern.ID(ctx, intern.EventName, ingest.EventPageview)
	if err != nil {
		t.Fatalf("intern pageview: %v", err)
	}

	for i, v := range visits {
		session := int64(i + 1)

		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce, pageviews)
			VALUES (?,?,?,?,?,?,?,?)`,
			session, v.siteID, v.user, v.startedAt, v.startedAt+v.duration,
			v.duration, v.bounce, v.pageviews); err != nil {
			t.Fatalf("insert session: %v", err)
		}

		for hit := range v.pageviews {
			if _, err := account.Writer().ExecContext(ctx, `
				INSERT INTO events (site_id, timestamp, name_id, user_id, session_id)
				VALUES (?,?,?,?,?)`,
				v.siteID, v.startedAt+int64(hit), nameID, v.user, session); err != nil {
				t.Fatalf("insert event: %v", err)
			}
		}
	}
}

// TestOverviewRollsUpRatesFromCountsNotAverages is the number this screen is
// most likely to get quietly wrong.
//
// Two sites with very different traffic have a selection bounce rate of 25% and
// a selection visit length of 45 seconds. Averaging the two sites' own rates
// instead gives 50% and 30 seconds — both believable, both wrong, and neither
// reconcilable against any other screen in the product.
func TestOverviewRollsUpRatesFromCountsNotAverages(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-2 * time.Hour).Unix()

	quiet := &Site{ID: 1, AccountID: 1, Domain: "quiet.example", Timezone: "Etc/UTC"}
	busy := &Site{ID: 2, AccountID: 1, Domain: "busy.example", Timezone: "Etc/UTC"}

	seedVisits(t, manager, 1, []visit{
		{siteID: quiet.ID, user: 10, startedAt: earlier, duration: 0, bounce: 1, pageviews: 1},

		{siteID: busy.ID, user: 20, startedAt: earlier, duration: 60, bounce: 0, pageviews: 3},
		{siteID: busy.ID, user: 21, startedAt: earlier, duration: 60, bounce: 0, pageviews: 2},
		{siteID: busy.ID, user: 21, startedAt: earlier + 1, duration: 60, bounce: 0, pageviews: 2},
	})

	overview, err := traffic.Overview(ctx, []*Site{quiet, busy}, query.RangeLast7Days, now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if len(overview.Sites) != 2 {
		t.Fatalf("want a card per site, got %d", len(overview.Sites))
	}

	// The busy site's own figures first, so a failure below is a roll-up
	// problem rather than a reading problem.
	second := overview.Sites[1]
	if second.Visitors != 2 || second.Visits != 3 || second.Pageviews != 7 {
		t.Errorf("busy site = %d visitors, %d visits, %d pageviews; want 2, 3, 7",
			second.Visitors, second.Visits, second.Pageviews)
	}

	if rate := second.BounceRate(); rate != 0 {
		t.Errorf("busy site bounce rate = %.1f%%, want 0%%", rate)
	}

	totals := overview.Totals
	if totals.Visitors != 3 || totals.Visits != 4 || totals.Pageviews != 8 {
		t.Errorf("totals = %d visitors, %d visits, %d pageviews; want 3, 4, 8",
			totals.Visitors, totals.Visits, totals.Pageviews)
	}

	if rate := totals.BounceRate(); rate != 25 {
		t.Errorf("selection bounce rate = %.1f%%, want 25%% — an average of averages gives 50%%", rate)
	}

	if visit := totals.AverageVisit(); visit != 45 {
		t.Errorf("selection visit length = %ds, want 45s — an average of averages gives 30s", visit)
	}
}

// TestOverviewChartCoversEveryBucket checks the graph is drawn from the range
// rather than from the rows that happened to have traffic. A chart built from
// the rows alone closes up its own gaps, which turns a day the snippet was
// broken into a day that never existed.
func TestOverviewChartCoversEveryBucket(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	site := &Site{ID: 1, AccountID: 1, Domain: "example.com", Timezone: "Etc/UTC"}

	seedVisits(t, manager, 1, []visit{
		{siteID: site.ID, user: 10, startedAt: now.Add(-time.Hour).Unix(), duration: 30, pageviews: 2},
	})

	overview, err := traffic.Overview(ctx, []*Site{site}, query.RangeLast7Days, now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	card := overview.Sites[0]

	if len(card.VisitorSeries) != 7 || len(card.PageviewSeries) != 7 {
		t.Fatalf("series lengths = %d/%d, want a bucket a day for seven days",
			len(card.VisitorSeries), len(card.PageviewSeries))
	}

	// Today is the last bucket, and the six before it are empty rather than
	// absent.
	if last := card.VisitorSeries[6]; last != 1 {
		t.Errorf("today holds %d visitors, want 1", last)
	}

	if last := card.PageviewSeries[6]; last != 2 {
		t.Errorf("today holds %d pageviews, want 2", last)
	}

	for i, value := range card.VisitorSeries[:6] {
		if value != 0 {
			t.Errorf("bucket %d holds %d visitors, want an empty bucket", i, value)
		}
	}
}

// TestOverviewCountsOnlyConfiguredGoals checks the goal figure. A site with no
// goals reports zero without running a query, and a site with one counts only
// the events that match it.
func TestOverviewCountsOnlyConfiguredGoals(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	site := &Site{ID: 1, AccountID: 1, Domain: "example.com", Timezone: "Etc/UTC"}

	seedVisits(t, manager, 1, []visit{
		{siteID: site.ID, user: 10, startedAt: now.Add(-time.Hour).Unix(), duration: 30, pageviews: 2},
	})

	overview, err := traffic.Overview(ctx, []*Site{site}, query.RangeLast7Days, now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if got := overview.Sites[0].Goals; got != 0 {
		t.Fatalf("a site with no goals reports %d, want 0", got)
	}

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	signupID, err := account.Intern.ID(ctx, intern.EventName, "Signup")
	if err != nil {
		t.Fatal(err)
	}

	// Two Signup events and one unrelated event, so a goal figure that simply
	// counted every custom event would read three.
	otherID, err := account.Intern.ID(ctx, intern.EventName, "Newsletter")
	if err != nil {
		t.Fatal(err)
	}

	for _, nameID := range []int64{signupID, signupID, otherID} {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO events (site_id, timestamp, name_id, user_id, session_id)
			VALUES (?,?,?,?,?)`, site.ID, now.Add(-time.Hour).Unix(), nameID, 10, 1); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO goals (site_id, kind, event_name, created_at) VALUES (?, 'event', 'Signup', ?)`,
		site.ID, now.AddDate(0, 0, -30).Unix()); err != nil {
		t.Fatal(err)
	}

	overview, err = traffic.Overview(ctx, []*Site{site}, query.RangeLast7Days, now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if got := overview.Sites[0].Goals; got != 2 {
		t.Errorf("goal completions = %d, want 2 — the unrelated event must not count", got)
	}
}

// TestOverviewReadsTransferredHistory checks the screen still renders a site
// whose analytics live in a former owner's account, which is the state an
// ownership transfer leaves behind.
func TestOverviewReadsTransferredHistory(t *testing.T) {
	traffic, manager := newTestTraffic(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	when := now.Add(-time.Hour).Unix()

	here := &Site{ID: 1, AccountID: 1, TeamID: 1, Domain: "here.example", Timezone: "Etc/UTC"}
	moved := &Site{ID: 2, AccountID: 2, TeamID: 1, Domain: "moved.example", Timezone: "Etc/UTC"}

	seedVisits(t, manager, 1, []visit{{siteID: here.ID, user: 10, startedAt: when, pageviews: 1}})
	seedVisits(t, manager, 2, []visit{{siteID: moved.ID, user: 20, startedAt: when, pageviews: 4}})

	overview, err := traffic.Overview(ctx, []*Site{here, moved}, query.RangeLast7Days, now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if overview.Sites[0].Pageviews != 1 || overview.Sites[1].Pageviews != 4 {
		t.Fatalf("pageviews = %d/%d, want 1/4",
			overview.Sites[0].Pageviews, overview.Sites[1].Pageviews)
	}

	if overview.Totals.Pageviews != 5 {
		t.Errorf("totals = %d pageviews, want 5", overview.Totals.Pageviews)
	}
}

// TestValidOverviewPeriodFallsBackRatherThanFailing checks a stale or
// hand-edited URL shows the screen. A bookmark from before a period was renamed
// should open on the default, not on an error.
func TestValidOverviewPeriodFallsBackRatherThanFailing(t *testing.T) {
	if got := ValidOverviewPeriod(query.RangeLast28Days); got != query.RangeLast28Days {
		t.Errorf("an offered period must survive, got %q", got)
	}

	for _, bad := range []string{"", "nonsense", query.RangeAll, query.RangeRealtime} {
		if got := ValidOverviewPeriod(bad); got != DefaultOverviewPeriod {
			t.Errorf("period %q resolved to %q, want the default %q", bad, got, DefaultOverviewPeriod)
		}
	}
}

// TestSortOverviewByTrafficHoldsPinnedSitesFirst checks a pin still means "this
// one, every time" when the cards are ordered by traffic.
func TestSortOverviewByTrafficHoldsPinnedSitesFirst(t *testing.T) {
	cards := []*SiteOverview{
		{Site: &Site{ID: 1, Domain: "quiet.example"}, Numbers: Numbers{Visitors: 5}},
		{Site: &Site{ID: 2, Domain: "busy.example"}, Numbers: Numbers{Visitors: 900}},
		{Site: &Site{ID: 3, Domain: "pinned.example", PinnedAt: 1}, Numbers: Numbers{Visitors: 1}},
	}

	SortOverviewByTraffic(cards)

	if cards[0].Site.ID != 3 {
		t.Fatalf("the pinned site is %d, want it first", cards[0].Site.ID)
	}

	if cards[1].Site.ID != 2 || cards[2].Site.ID != 1 {
		t.Errorf("order after the pin = %d, %d; want the busiest first",
			cards[1].Site.ID, cards[2].Site.ID)
	}
}
