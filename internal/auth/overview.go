//
// overview.go
// The headline numbers behind the all-sites analytics screen.
//
// Created: 2026-09-04
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// OverviewPeriods is what the period control offers, in the order it lists
// them. They are the query engine's own preset names, so the screen and the
// dashboard resolve the same words to the same days.
var OverviewPeriods = []string{
	query.RangeDay,
	query.RangeLast7Days,
	query.RangeLast28Days,
	query.RangeLast91Days,
	query.RangeMonth,
	query.RangeLastMonth,
	query.RangeLast12Months,
}

// DefaultOverviewPeriod is what the screen opens on. Today is the question this
// screen is for — "what is happening across everything right now" — and it is
// also the cheapest range to answer for an account with fifty sites.
const DefaultOverviewPeriod = query.RangeDay

// overviewReaders is how many sites are read at once. It matches the per-
// database reader pool: more goroutines than connections would queue inside
// database/sql rather than get anything done sooner.
const overviewReaders = 4

// ValidOverviewPeriod maps whatever arrived in the URL onto a period we offer,
// falling back to the default rather than erroring. A bad preset in a
// hand-edited or stale bookmarked URL should show the screen, not a 400.
func ValidOverviewPeriod(period string) string {
	for _, candidate := range OverviewPeriods {
		if candidate == period {
			return period
		}
	}

	return DefaultOverviewPeriod
}

// Numbers is one column of headline figures — either one site's or the
// selection's.
//
// Bounces and Seconds are held rather than the two rates they produce, for the
// same reason the summary tables store a numerator and a denominator: an
// average of averages is not the average, and adding one site's bounce rate to
// another's is arithmetic nobody could reconcile.
type Numbers struct {
	// Current is visitors seen in the last five minutes, which is the same
	// window the dashboard calls "current visitors".
	Current int64

	Visitors  int64
	Visits    int64
	Pageviews int64

	// Goals is every event in the period that matched a configured goal. An
	// event matching two goals is counted once, because it is one thing that
	// happened.
	Goals int64

	Bounces int64
	Seconds int64
}

// Add folds one site's figures into a running selection total.
//
// Visitors and Current are summed, which counts somebody who visited two of
// your sites twice. There is no cheap way to tell that person from two people
// — a visitor id is derived per site by design — so the screen labels the
// figure "site visitors" rather than claiming a unique count it does not have.
func (n *Numbers) Add(other Numbers) {
	n.Current += other.Current
	n.Visitors += other.Visitors
	n.Visits += other.Visits
	n.Pageviews += other.Pageviews
	n.Goals += other.Goals
	n.Bounces += other.Bounces
	n.Seconds += other.Seconds
}

// BounceRate is bounced visits over all visits, as a percentage.
func (n Numbers) BounceRate() float64 {
	if n.Visits == 0 {
		return 0
	}

	return 100 * float64(n.Bounces) / float64(n.Visits)
}

// AverageVisit is the mean visit length in seconds, with bounces counted as
// zero-length visits — the same rule the visit_duration metric uses, so this
// figure and the dashboard's agree.
func (n Numbers) AverageVisit() int64 {
	if n.Visits == 0 {
		return 0
	}

	return int64(math.Round(float64(n.Seconds) / float64(n.Visits)))
}

// SiteOverview is one card on the all-sites screen.
type SiteOverview struct {
	Site *Site
	Numbers

	// VisitorSeries and PageviewSeries are the card's chart, oldest bucket
	// first and including the empty buckets. A chart handed only the buckets
	// that had traffic cannot tell a quiet Sunday from a broken snippet.
	VisitorSeries  []int64
	PageviewSeries []int64
}

// Overview is everything the all-sites screen draws.
type Overview struct {
	Sites  []*SiteOverview
	Totals Numbers

	// Period is the preset the figures were read over, after validation.
	Period string
}

// Overview reads the headline numbers for a list of sites.
//
// Every figure comes back through the query compiler rather than from SQL
// written for this screen. That costs a handful of small queries per site and
// buys the only thing that matters: a number here is counted by exactly the
// same code as the same number on the site's own dashboard, so the two can
// never disagree.
//
// The sites are grouped by their storage account first, because an ownership
// transfer makes "the team that owns this site" and "the database holding its
// history" two different facts, and one team's screen may need more than one
// database.
func (t *Traffic) Overview(ctx context.Context, list []*Site, period string, now time.Time) (*Overview, error) {
	period = ValidOverviewPeriod(period)

	out := &Overview{Period: period, Sites: make([]*SiteOverview, 0, len(list))}
	if len(list) == 0 {
		return out, nil
	}

	byAccount := map[int64][]*Site{}
	for _, site := range list {
		byAccount[site.AccountID] = append(byAccount[site.AccountID], site)
	}

	// The account ids are read in a fixed order so that a page rendered twice
	// from the same data is the same page. Map iteration is deliberately
	// random in Go, and a card list that reshuffles on refresh reads as a bug.
	accountIDs := make([]int64, 0, len(byAccount))
	for id := range byAccount {
		accountIDs = append(accountIDs, id)
	}

	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })

	found := map[int64]*SiteOverview{}

	for _, accountID := range accountIDs {
		read, err := t.overviewForAccount(ctx, accountID, byAccount[accountID], period, now)
		if err != nil {
			return nil, err
		}

		for _, card := range read {
			found[card.Site.ID] = card
		}
	}

	// The cards come out in the order the caller handed the sites over, which
	// is the order the sort control asked for.
	for _, site := range list {
		card, ok := found[site.ID]
		if !ok {
			card = &SiteOverview{Site: site}
		}

		out.Sites = append(out.Sites, card)
		out.Totals.Add(card.Numbers)
	}

	return out, nil
}

// overviewForAccount reads every card belonging to one analytics database.
func (t *Traffic) overviewForAccount(ctx context.Context, accountID int64, list []*Site, period string, now time.Time) ([]*SiteOverview, error) {
	lease, err := t.manager.Acquire(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("auth: overview: %w", err)
	}
	defer lease.Release() //nolint:errcheck // the numbers are more useful than an unlock error

	reader := lease.Account.Reader()

	siteIDs := make([]int64, 0, len(list))
	for _, site := range list {
		siteIDs = append(siteIDs, site.ID)
	}

	// One read for every site's goal ids, rather than one per card. The ids are
	// needed before the goal query can be built, so this is the one step that
	// cannot go inside the per-site worker.
	goalIDs, err := goals.IDsForSites(ctx, reader, siteIDs)
	if err != nil {
		return nil, fmt.Errorf("auth: overview: %w", err)
	}

	engine := query.New(reader)
	engine.Now = func() time.Time { return now }

	cards := make([]*SiteOverview, len(list))
	errs := make([]error, len(list))

	// Sites are read in parallel up to the reader pool's width. A fifty-site
	// agency is exactly who this screen is for, and reading their sites one
	// after another would make the page they open most the slowest one.
	var wait sync.WaitGroup
	slots := make(chan struct{}, overviewReaders)

	for i, site := range list {
		wait.Add(1)

		go func() {
			defer wait.Done()

			slots <- struct{}{}
			defer func() { <-slots }()

			card, err := readSiteOverview(ctx, engine, site, goalIDs[site.ID], period)
			cards[i], errs[i] = card, err
		}()
	}

	wait.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	return cards, nil
}

// readSiteOverview runs one site's three or four queries.
//
// They are separate requests rather than one because they genuinely ask
// different questions: a total over the period is not the sum of its buckets
// once distinct visitors are involved, "who is here now" is a different window
// entirely, and a goal figure needs a filter that the other three must not
// have.
func readSiteOverview(ctx context.Context, engine *query.Engine, site *Site, goalIDs []int64, period string) (*SiteOverview, error) {
	card := &SiteOverview{Site: site}

	base := query.Query{
		SiteIDs:   []int64{site.ID},
		Timezone:  site.Timezone,
		DateRange: query.DateRange{Preset: period},
	}

	totals := base
	totals.Metrics = []string{"visitors", "visits", "pageviews", "bounce_rate", "visit_duration"}

	result, err := engine.Run(ctx, totals)
	if err != nil {
		return nil, fmt.Errorf("auth: overview: site %d totals: %w", site.ID, err)
	}

	if len(result.Results) > 0 {
		row := result.Results[0]
		card.Visitors = whole(row.Metrics, 0)
		card.Visits = whole(row.Metrics, 1)
		card.Pageviews = whole(row.Metrics, 2)

		// The two rates are turned back into the counts they were made from, so
		// that adding this site to the selection total is a sum of counts
		// rather than an average of averages.
		card.Bounces = int64(math.Round(value(row.Metrics, 3) / 100 * float64(card.Visits)))
		card.Seconds = int64(math.Round(value(row.Metrics, 4) * float64(card.Visits)))
	}

	series := base
	series.Metrics = []string{"visitors", "pageviews"}
	series.Dimensions = []string{"time"}

	graph, err := engine.Run(ctx, series)
	if err != nil {
		return nil, fmt.Errorf("auth: overview: site %d graph: %w", site.ID, err)
	}

	card.VisitorSeries, card.PageviewSeries = plotSeries(graph)

	live := base
	live.Metrics = []string{"visitors"}
	live.DateRange = query.DateRange{Preset: query.RangeLast5Minutes}

	current, err := engine.Run(ctx, live)
	if err != nil {
		return nil, fmt.Errorf("auth: overview: site %d current: %w", site.ID, err)
	}

	if len(current.Results) > 0 {
		card.Current = whole(current.Results[0].Metrics, 0)
	}

	// A site with no goals configured needs no query: the answer is zero and
	// the filter would have nothing to compile.
	if len(goalIDs) == 0 {
		return card, nil
	}

	converted := base
	converted.Metrics = []string{"events"}
	converted.Filters = []query.Filter{{
		Operator:  query.OpIs,
		Dimension: "event:goal",
		Values:    goalValues(goalIDs),
	}}

	completions, err := engine.Run(ctx, converted)
	if err != nil {
		return nil, fmt.Errorf("auth: overview: site %d goals: %w", site.ID, err)
	}

	if len(completions.Results) > 0 {
		card.Goals = whole(completions.Results[0].Metrics, 0)
	}

	return card, nil
}

// plotSeries turns a time-grouped result into two series covering every bucket
// in the range, oldest first.
//
// The bucket list comes from the response's own labels rather than from the
// rows, because the rows only carry the buckets that had traffic and a chart
// drawn from those alone silently closes up its own gaps.
func plotSeries(result *query.Result) (visitors, pageviews []int64) {
	byBucket := make(map[string][]float64, len(result.Results))

	for _, row := range result.Results {
		if len(row.Dimensions) == 0 {
			continue
		}

		byBucket[row.Dimensions[0]] = row.Metrics
	}

	labels := result.Meta.TimeLabels

	visitors = make([]int64, 0, len(labels))
	pageviews = make([]int64, 0, len(labels))

	for _, label := range labels {
		row := byBucket[label]
		visitors = append(visitors, whole(row, 0))
		pageviews = append(pageviews, whole(row, 1))
	}

	return visitors, pageviews
}

// goalValues renders goal ids as the strings a filter carries.
func goalValues(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}

	return out
}

// value reads one metric out of a row, or zero when the row is shorter than the
// request asked for. A short row is not an error worth failing a page over: the
// missing figure renders as zero and every other number on the card stands.
func value(metrics []float64, index int) float64 {
	if index >= len(metrics) {
		return 0
	}

	return metrics[index]
}

// whole reads one metric as a count.
func whole(metrics []float64, index int) int64 {
	return int64(math.Round(value(metrics, index)))
}

// SortOverviewByTraffic reorders cards by visitors, busiest first, with pinned
// sites still held at the top — the same rule the list view's traffic sort
// follows, so the two screens do not disagree about what "most traffic" means.
func SortOverviewByTraffic(cards []*SiteOverview) {
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].Site.Pinned() != cards[j].Site.Pinned() {
			return cards[i].Site.Pinned()
		}

		return cards[i].Visitors > cards[j].Visitors
	})
}
