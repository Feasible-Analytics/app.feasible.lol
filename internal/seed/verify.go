//
// verify.go
// Reading the generated data back and asserting it has the shape it was meant to.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/geo"
)

// minimumForChecks is the number of events below which the distribution checks
// are reported but not enforced. A hundred pageviews cannot be power-law
// distributed in any meaningful sense, and failing a tiny run for it would make
// the checks noise rather than signal.
const minimumForChecks = 5_000

// Report is the shape of what was generated, measured by querying it back. It
// is measured rather than counted during generation on purpose: what matters is
// what a query sees, and the only way to know that is to ask one.
type Report struct {
	Events    int64
	Pageviews int64
	Sessions  int64
	Visitors  int64

	Sites     []SiteReport
	Databases []DatabaseSize
	Checks    []Check
}

// SiteReport is one site's numbers.
type SiteReport struct {
	Domain    string
	Events    int64
	Pageviews int64
	Sessions  int64
	Visitors  int64

	DistinctPages     int64
	DistinctSources   int64
	DistinctCountries int64
	DistinctAgents    int64

	// TopPageShare is the fraction of pageviews the ten busiest pages take. It
	// is the single number that says whether the data is power-law or uniform:
	// uniform over two thousand pages would put it at half a per cent.
	TopPageShare float64

	BounceRate      float64
	SinglePageShare float64
	LongestSession  int64
	ViewsPerVisit   float64

	// ZeroDays counts days in range with no events at all, and SpikeRatio is
	// the busiest day against the median one.
	ZeroDays   int
	SpikeRatio float64
}

// DatabaseSize is one file on disk, including its write-ahead log — a database
// measured without its WAL is measured before it has finished being written.
type DatabaseSize struct {
	Path  string
	Bytes int64
}

// Check is one assertion about the generated data.
type Check struct {
	Name     string
	OK       bool
	Detail   string
	Enforced bool
}

// measure reads every seeded account back and builds the report. It runs
// against the reader pool rather than the writer, which is also a first
// measurement of what a dashboard query costs on the data that was just
// written.
func measure(ctx context.Context, dataDir string, runs []*accountRun, start time.Time, days int) (Report, error) {
	var report Report

	for _, run := range runs {
		for _, site := range run.seeded.Sites {
			if !site.Fixture.Traffic {
				continue
			}

			siteReport, err := measureSite(ctx, run.account.Reader(), site, start, days)
			if err != nil {
				return report, err
			}

			report.Events += siteReport.Events
			report.Pageviews += siteReport.Pageviews
			report.Sessions += siteReport.Sessions
			report.Visitors += siteReport.Visitors
			report.Sites = append(report.Sites, siteReport)
		}
	}

	sizes, err := databaseSizes(dataDir, runs)
	if err != nil {
		return report, err
	}
	report.Databases = sizes

	checks, err := runChecks(ctx, runs, report, days)
	if err != nil {
		return report, err
	}
	report.Checks = checks

	return report, nil
}

// measureSite runs the numbers for one site. The queries are written the way a
// dashboard query has to be written — resolve the dimension id once, then scan
// on the indexed columns — because the shape report is also the first real
// measurement of what a query costs against the data that was just generated.
func measureSite(ctx context.Context, db *sql.DB, site *seededSite, start time.Time, days int) (SiteReport, error) {
	report := SiteReport{Domain: site.Fixture.Domain}

	var pageview int64
	if err := db.QueryRowContext(ctx, "SELECT id FROM dim_event_name WHERE value = 'pageview'").Scan(&pageview); err != nil && err != sql.ErrNoRows {
		return report, fmt.Errorf("seed verify: pageview id: %w", err)
	}

	row := db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(name_id = ?), 0),
		       COUNT(DISTINCT pathname_id),
		       COUNT(DISTINCT source_id),
		       COUNT(DISTINCT country_id)
		FROM events WHERE site_id = ?`, pageview, site.ID)

	if err := row.Scan(&report.Events, &report.Pageviews,
		&report.DistinctPages, &report.DistinctSources, &report.DistinctCountries); err != nil {
		return report, fmt.Errorf("seed verify: events: %w", err)
	}

	// Browser and operating system pairs, counted over the distinct pairs
	// rather than over a string built per row — at a million rows the
	// concatenation costs more than the rest of the report put together.
	row = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (SELECT DISTINCT browser_id, os_id FROM events WHERE site_id = ?)`, site.ID)

	if err := row.Scan(&report.DistinctAgents); err != nil {
		return report, fmt.Errorf("seed verify: dimensions: %w", err)
	}

	// Visitors are counted over sessions rather than events: every event belongs
	// to a visit and a visit has one visitor, so the answer is the same over a
	// third of the rows.
	row = db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(DISTINCT user_id),
		       COALESCE(AVG(is_bounce), 0),
		       COALESCE(AVG(CASE WHEN pageviews <= 1 THEN 1.0 ELSE 0.0 END), 0),
		       COALESCE(MAX(pageviews), 0),
		       COALESCE(AVG(pageviews), 0)
		FROM sessions WHERE site_id = ?`, site.ID)

	if err := row.Scan(&report.Sessions, &report.Visitors, &report.BounceRate, &report.SinglePageShare, &report.LongestSession, &report.ViewsPerVisit); err != nil {
		return report, fmt.Errorf("seed verify: sessions: %w", err)
	}

	share, err := topPageShare(ctx, db, site.ID, pageview)
	if err != nil {
		return report, err
	}
	report.TopPageShare = share

	zeros, spike, err := dailyShape(ctx, db, site.ID, start, days)
	if err != nil {
		return report, err
	}
	report.ZeroDays, report.SpikeRatio = zeros, spike

	return report, nil
}

// topPageShare is what fraction of a site's pageviews its ten busiest pages
// take. It is the assertion that the data is not uniform, which is the whole
// difference between a seed that tells you something about production and one
// that does not.
func topPageShare(ctx context.Context, db *sql.DB, siteID, pageview int64) (float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COUNT(*) AS hits
		FROM events
		WHERE site_id = ? AND name_id = ?
		GROUP BY pathname_id
		ORDER BY hits DESC`, siteID, pageview)
	if err != nil {
		return 0, fmt.Errorf("seed verify: page share: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var top, total float64

	for i := 0; rows.Next(); i++ {
		var hits float64
		if err := rows.Scan(&hits); err != nil {
			return 0, fmt.Errorf("seed verify: page share: %w", err)
		}

		total += hits
		if i < 10 {
			top += hits
		}
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("seed verify: page share: %w", err)
	}

	if total == 0 {
		return 0, nil
	}

	return top / total, nil
}

// dailyShape counts the days with no traffic and measures the busiest day
// against the median one. Both are deliberate features of the data: a gap in
// the graph is not a zero, and a spike is what an alert has to fire on.
func dailyShape(ctx context.Context, db *sql.DB, siteID int64, start time.Time, days int) (int, float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT timestamp / 86400 AS day, COUNT(*)
		FROM events
		WHERE site_id = ?
		GROUP BY day`, siteID)
	if err != nil {
		return 0, 0, fmt.Errorf("seed verify: daily shape: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[int64]int64{}

	for rows.Next() {
		var day, count int64
		if err := rows.Scan(&day, &count); err != nil {
			return 0, 0, fmt.Errorf("seed verify: daily shape: %w", err)
		}
		counts[day] = count
	}

	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("seed verify: daily shape: %w", err)
	}

	var (
		zeros    int
		ordered  []int64
		firstDay = start.Unix() / 86400
	)

	for i := 0; i < days; i++ {
		count := counts[firstDay+int64(i)]
		if count == 0 {
			zeros++
			continue
		}
		ordered = append(ordered, count)
	}

	if len(ordered) == 0 {
		return zeros, 0, nil
	}

	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	median := float64(ordered[len(ordered)/2])
	if median == 0 {
		return zeros, 0, nil
	}

	return zeros, float64(ordered[len(ordered)-1]) / median, nil
}

// databaseSizes measures every file the run wrote, WAL included. The number
// people want after a seed is "how big did that get", and a database measured
// without its write-ahead log answers a different question.
func databaseSizes(dataDir string, runs []*accountRun) ([]DatabaseSize, error) {
	paths := []string{filepath.Join(dataDir, config.SystemDatabaseName)}

	for _, run := range runs {
		paths = append(paths, accounts.Path(dataDir, run.account.ID))
	}

	sizes := make([]DatabaseSize, 0, len(paths))

	for _, path := range paths {
		var total int64

		for _, suffix := range []string{"", "-wal"} {
			info, err := os.Stat(path + suffix)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("seed verify: %w", err)
			}

			total += info.Size()
		}

		sizes = append(sizes, DatabaseSize{Path: path, Bytes: total})
	}

	return sizes, nil
}

// runChecks asserts that the data has the properties the generator claims. They
// are checks rather than comments because a distribution that quietly went
// uniform — one exponent changed, one sampler rewired — produces a database
// that looks fine and measures nothing.
func runChecks(ctx context.Context, runs []*accountRun, report Report, days int) ([]Check, error) {
	enforced := report.Events >= minimumForChecks

	var checks []Check

	add := func(name string, ok bool, enforce bool, format string, args ...any) {
		checks = append(checks, Check{
			Name:     name,
			OK:       ok,
			Detail:   fmt.Sprintf(format, args...),
			Enforced: enforce && enforced,
		})
	}

	// The head of the page distribution. Uniform over two thousand pages would
	// put ten of them at half a per cent between them.
	worst, worstDomain := 1.0, ""
	for _, site := range report.Sites {
		if site.Pageviews > 0 && site.TopPageShare < worst {
			worst, worstDomain = site.TopPageShare, site.Domain
		}
	}
	// Uniform over two thousand pages would put ten of them at half a per cent
	// between them; a catalogue that collapsed to a handful of pages would put
	// them at everything. Both are wrong in the same way — the data stops
	// saying anything about production — so the check has two sides.
	add("pages follow a power law", worst >= 0.30 && worst <= 0.80, true,
		"the ten busiest pages take %.0f%% of pageviews on %s", worst*100, worstDomain)

	// Bounce rate. Zero means the session fold never saw a second pageview and
	// a hundred means it never saw a first one; both mean the fold was never
	// exercised at all.
	lowest, highest := 1.0, 0.0
	for _, site := range report.Sites {
		if site.Sessions == 0 {
			continue
		}
		lowest = minFloat(lowest, site.BounceRate)
		highest = maxFloat(highest, site.BounceRate)
	}
	add("bounce rate is neither 0% nor 100%", lowest > 0.15 && highest < 0.95, true,
		"between %.0f%% and %.0f%% across the seeded sites", lowest*100, highest*100)

	// Session lengths. Sixty per cent single-pageview with a tail past twenty is
	// the distribution the specification asks for.
	longest := int64(0)
	singles := 0.0
	for _, site := range report.Sites {
		longest = maxInt64(longest, site.LongestSession)
		singles = maxFloat(singles, site.SinglePageShare)
	}
	add("sessions have a realistic length spread", longest >= 20 && singles > 0.35 && singles < 0.85, true,
		"the longest visit is %d pageviews and %.0f%% are single-pageview", longest, singles*100)

	// The gap and the spike are measured on the busiest site rather than across
	// all of them. A small site can have a quiet day by chance, and a check that
	// accepted that would pass on a run where the deliberate gap was missing.
	if len(report.Sites) > 0 {
		primary := report.Sites[0]

		if days >= 12 {
			add("a day with no traffic exists", primary.ZeroDays >= 1, true,
				"%d day(s) with no events on %s", primary.ZeroDays, primary.Domain)
		}

		if days >= 8 {
			add("a traffic spike exists", primary.SpikeRatio >= 2.5, true,
				"the busiest day is %.1fx the median day on %s", primary.SpikeRatio, primary.Domain)
		}
	}

	for _, run := range runs {
		found, err := oddities(ctx, run)
		if err != nil {
			return nil, err
		}

		for _, check := range found {
			checks = append(checks, Check{Name: check.Name, OK: check.OK, Detail: check.Detail, Enforced: check.Enforced && enforced})
		}
	}

	return checks, nil
}

// oddities looks for the deliberately strange rows in one account. Every one of
// them is a case a report gets wrong the first time it meets it, and every one
// of them is absent from data that was only sampled.
func oddities(ctx context.Context, run *accountRun) ([]Check, error) {
	db := run.account.Reader()

	var checks []Check

	count := func(query string, args ...any) (int64, error) {
		var found int64
		if err := db.QueryRowContext(ctx, query, args...).Scan(&found); err != nil {
			return 0, fmt.Errorf("seed verify: %w", err)
		}

		return found, nil
	}

	singles, err := count(`
		SELECT COUNT(*) FROM (
			SELECT pathname_id FROM events
			WHERE name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
			GROUP BY pathname_id HAVING COUNT(*) = 1
		)`)
	if err != nil {
		return nil, err
	}

	currencies, err := count("SELECT COUNT(DISTINCT revenue_currency) FROM event_details WHERE revenue_currency IS NOT NULL")
	if err != nil {
		return nil, err
	}

	// The event carrying the property cap is found by the name of its
	// thirtieth property rather than by counting keys, so the check does not
	// depend on the JSON extension being compiled in.
	capped, err := count(`SELECT COUNT(*) FROM event_details WHERE props LIKE '%"dimension_30"%'`)
	if err != nil {
		return nil, err
	}

	vpn, err := count(`
		SELECT COUNT(*) FROM events
		JOIN dim_country country ON country.id = events.country_id
		WHERE country.value = ?`, geo.AnonymousVPNCountry)
	if err != nil {
		return nil, err
	}

	unknownHost, err := count(`
		SELECT COUNT(*) FROM events
		JOIN dim_hostname hostname ON hostname.id = events.hostname_id
		WHERE hostname.value = ?`, unvalidatedHostname)
	if err != nil {
		return nil, err
	}

	if run.seeded.Fixture.State == stateActive {
		checks = append(checks,
			Check{Name: "a page has exactly one pageview", OK: singles >= 1, Detail: fmt.Sprintf("%d page(s) with a single pageview", singles), Enforced: true},
			Check{Name: "revenue arrives in three currencies", OK: currencies >= 3, Detail: fmt.Sprintf("%d distinct currencies", currencies), Enforced: true},
			Check{Name: "an event carries the property cap", OK: capped >= 1, Detail: fmt.Sprintf("%d event(s) with thirty properties", capped), Enforced: true},
			Check{Name: "a visitor is bucketed as Anonymous VPN Service", OK: vpn >= 1, Detail: fmt.Sprintf("%d event(s) from a VPN exit", vpn), Enforced: true},
			Check{Name: "unvalidated hostname traffic is rejected", OK: unknownHost == 0, Detail: fmt.Sprintf("%d stored event(s) from %s", unknownHost, unvalidatedHostname), Enforced: true},
		)

		// The empty state. A site with no rows at all is what every report card
		// has to render before it has anything to say.
		for _, site := range run.seeded.Sites {
			if site.Fixture.Traffic {
				continue
			}

			empty, err := count("SELECT COUNT(*) FROM events WHERE site_id = ?", site.ID)
			if err != nil {
				return nil, err
			}

			checks = append(checks, Check{
				Name:     "a site has no data at all",
				OK:       empty == 0,
				Detail:   fmt.Sprintf("%s has %d events", site.Fixture.Domain, empty),
				Enforced: true,
			})
		}
	}

	if run.seeded.Fixture.State == stateDormant {
		// A dormant account stops partway through the history rather than
		// having no history, which is the difference between an account that
		// lapsed and one that never sent anything.
		events, err := count("SELECT COUNT(*) FROM events")
		if err != nil {
			return nil, err
		}

		checks = append(checks, Check{
			Name:     "a dormant account stopped receiving traffic",
			OK:       events > 0,
			Detail:   fmt.Sprintf("%s has %d events and is past its ingestion deadline", run.seeded.Fixture.Name, events),
			Enforced: true,
		})
	}

	if run.seeded.Fixture.State == stateLocked {
		events, err := count("SELECT COUNT(*) FROM events")
		if err != nil {
			return nil, err
		}

		checks = append(checks, Check{
			Name:     "a locked account still has data",
			OK:       events > 0,
			Detail:   fmt.Sprintf("%s has %d events with a cancelled subscription", run.seeded.Fixture.Name, events),
			Enforced: true,
		})
	}

	return checks, nil
}

// Failed reports the checks that were enforced and did not pass. A seed that
// silently produced the wrong shape is worse than one that failed, because
// every measurement taken against it is wrong in a way nobody can see.
func (r Report) Failed() []Check {
	var failed []Check

	for _, check := range r.Checks {
		if check.Enforced && !check.OK {
			failed = append(failed, check)
		}
	}

	return failed
}

// Write prints the report. It is the whole output of a seed run: what was
// generated, how big it is, and whether it has the shape it was supposed to.
func (r Report) Write(out io.Writer) {
	fmt.Fprintf(out, "\n%-28s %10s %10s %10s %10s %8s %8s %7s\n",
		"site", "events", "pageviews", "visits", "visitors", "top-10", "bounce", "v/visit")

	for _, site := range r.Sites {
		fmt.Fprintf(out, "%-28s %10d %10d %10d %10d %7.0f%% %7.0f%% %7.2f\n",
			site.Domain, site.Events, site.Pageviews, site.Sessions, site.Visitors,
			site.TopPageShare*100, site.BounceRate*100, site.ViewsPerVisit)
	}

	fmt.Fprintf(out, "\n%-28s %10s %10s %10s %10s %8s %8s %7s\n",
		"site", "pages", "sources", "countries", "browsers", "1-page", "longest", "spike")

	for _, site := range r.Sites {
		fmt.Fprintf(out, "%-28s %10d %10d %10d %10d %7.0f%% %8d %6.1fx\n",
			site.Domain, site.DistinctPages, site.DistinctSources, site.DistinctCountries,
			site.DistinctAgents, site.SinglePageShare*100, site.LongestSession, site.SpikeRatio)
	}

	fmt.Fprintln(out, "\ndatabases")
	for _, database := range r.Databases {
		fmt.Fprintf(out, "  %-52s %8.1f MB\n", database.Path, float64(database.Bytes)/(1<<20))
	}

	fmt.Fprintln(out, "\nshape")
	for _, check := range r.Checks {
		mark := "ok  "
		if !check.OK {
			mark = "FAIL"
			if !check.Enforced {
				mark = "note"
			}
		}

		fmt.Fprintf(out, "  %s %-46s %s\n", mark, check.Name, check.Detail)
	}
}

// The small numeric helpers. They are here rather than inline so the report
// code reads as arithmetic about traffic instead of as three-line comparisons.
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}

	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}

	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}

	return b
}
