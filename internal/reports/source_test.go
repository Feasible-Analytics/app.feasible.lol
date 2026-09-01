//
// source_test.go
// Exact report and alert queries above the automatic sampling threshold.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// querySourceFixture holds one closed report period and one live alert period
// whose fact counts both exceed the deliberately low sampling threshold.
type querySourceFixture struct {
	account *accounts.Account
	source  *QuerySource
	site    SiteRef
	now     time.Time
}

// newQuerySourceFixture opens a real migrated account and writes traffic on
// both sides of the report/alert boundary through the production schema.
func newQuerySourceFixture(t *testing.T) *querySourceFixture {
	t.Helper()

	ctx := context.Background()
	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(ctx, 1)
	if err != nil {
		t.Fatalf("open account: %v", err)
	}

	// Resolve the dimensions through the same cache ingest uses so the report
	// proves its formatted breakdowns as well as aggregate completion.
	dimensionID := func(dimension intern.Dimension, value string) int64 {
		id, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatalf("intern %s: %v", dimension, err)
		}

		return id
	}

	pageviewID := dimensionID(intern.EventName, "pageview")
	pageID := dimensionID(intern.Pathname, "/pricing")
	sourceID := dimensionID(intern.Source, "newsletter")
	countryID := dimensionID(intern.Country, "US")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 10, 10, 0, 0, time.UTC),
		now.Add(-20 * time.Minute),
		now.Add(-10 * time.Minute),
	}

	for index, timestamp := range timestamps {
		id := int64(index + 1)
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (
				id, site_id, user_id, started_at, last_seen_at, duration,
				is_bounce, pageviews, events, entry_page_id, exit_page_id,
				source_id, country_id
			) VALUES (?, 1, ?, ?, ?, 0, 1, 1, 1, ?, ?, ?, ?)`,
			id, id, timestamp.Unix(), timestamp.Unix(), pageID, pageID, sourceID, countryID); err != nil {
			t.Fatalf("insert session %d: %v", id, err)
		}

		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO events (
				id, site_id, timestamp, name_id, user_id, session_id,
				pathname_id, source_id, country_id
			) VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?)`,
			id, timestamp.Unix(), pageviewID, id, id, pageID, sourceID, countryID); err != nil {
			t.Fatalf("insert event %d: %v", id, err)
		}
	}

	// Put the event facts outside the automatic sample's low bucket prefix. If
	// the page breakdown stops requesting exact execution, it returns no row
	// instead of accidentally matching the exact total in this tiny fixture.
	if _, err := account.Writer().ExecContext(ctx, "UPDATE event_sampling SET bucket = 1023"); err != nil {
		t.Fatalf("move event sampling buckets: %v", err)
	}

	source := NewQuerySource(manager)
	source.Now = func() time.Time { return now }
	source.SampleThreshold = 1

	return &querySourceFixture{
		account: account,
		source:  source,
		site:    SiteRef{SiteID: 1, AccountID: 1, Domain: "example.test", Timezone: "UTC"},
		now:     now,
	}
}

// requireSamplingNeedsExact proves the control query really crossed the low
// automatic threshold and was rejected because visitor sampling is unsafe.
func requireSamplingNeedsExact(t *testing.T, fixture *querySourceFixture, dateRange query.DateRange) {
	t.Helper()

	engine := query.New(fixture.account.Reader())
	engine.Now = func() time.Time { return fixture.now }
	engine.SampleThreshold = 1

	_, err := engine.Run(context.Background(), query.Query{
		SiteIDs:   []int64{fixture.site.SiteID},
		Metrics:   []string{"visitors"},
		DateRange: dateRange,
		Timezone:  fixture.site.Timezone,
	})
	var queryError *query.Error
	if !errors.As(err, &queryError) || queryError.Code != "sampling_requires_exact" {
		t.Fatalf("non-exact visitor control error = %v, want sampling_requires_exact", err)
	}
}

// TestQuerySourcePeriodForcesExactAboveSamplingThreshold proves both the
// aggregate and each top-list query finish with undisclosed sampling disabled.
func TestQuerySourcePeriodForcesExactAboveSamplingThreshold(t *testing.T) {
	fixture := newQuerySourceFixture(t)
	from := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	dateRange := query.DateRange{
		Preset:   query.RangeCustom,
		Start:    from,
		End:      from,
		DateOnly: true,
	}
	requireSamplingNeedsExact(t, fixture, dateRange)

	snapshot, err := fixture.source.Period(context.Background(), fixture.site, from, to)
	if err != nil {
		t.Fatalf("exact report period: %v", err)
	}
	if snapshot.Visitors != 2 {
		t.Fatalf("report visitors = %d, want 2", snapshot.Visitors)
	}

	for name, entries := range map[string][]Entry{
		"pages":     snapshot.TopPages,
		"sources":   snapshot.TopSources,
		"countries": snapshot.Countries,
	} {
		if len(entries) != 1 || entries[0].Value != "2" {
			t.Errorf("exact report %s = %+v, want one entry with value 2", name, entries)
		}
	}
}

// TestQuerySourceAlertsForceExactAboveSamplingThreshold proves both rolling
// alert windows return exact visitors instead of the visitor-sampling error.
func TestQuerySourceAlertsForceExactAboveSamplingThreshold(t *testing.T) {
	fixture := newQuerySourceFixture(t)
	requireSamplingNeedsExact(t, fixture, query.DateRange{Preset: query.RangeRealtime})

	current, err := fixture.source.CurrentVisitors(context.Background(), fixture.site)
	if err != nil {
		t.Fatalf("exact current visitors: %v", err)
	}
	if current != 2 {
		t.Fatalf("current visitors = %d, want 2", current)
	}

	rollingRange := query.DateRange{
		Preset: query.RangeCustom,
		Start:  fixture.now.Add(-time.Hour),
		End:    fixture.now,
	}
	requireSamplingNeedsExact(t, fixture, rollingRange)

	rolling, err := fixture.source.VisitorsInLastHours(context.Background(), fixture.site, 1)
	if err != nil {
		t.Fatalf("exact rolling visitors: %v", err)
	}
	if rolling != 2 {
		t.Fatalf("rolling visitors = %d, want 2", rolling)
	}
}
