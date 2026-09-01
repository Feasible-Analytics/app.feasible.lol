//
// table_test.go
// The table decider, and the combinations it refuses.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import "testing"

// planFor decides a query or fails the test.
func planFor(t *testing.T, q Query) *plan {
	t.Helper()

	q.Normalise()

	decided, err := decide(&q)
	if err != nil {
		t.Fatalf("decide failed: %v", err)
	}

	return decided
}

// TestDeciderPicksOneTableWhereItCan checks that a query which only needs one
// fact table reads exactly one.
func TestDeciderPicksOneTableWhereItCan(t *testing.T) {
	events := planFor(t, Query{SiteIDs: []int64{1}, Metrics: []string{"pageviews", "visitors"}})
	if events.Primary != tableEvents || events.HasSecondary {
		t.Errorf("hit-grain metrics should read events alone, got %+v", events)
	}

	sessions := planFor(t, Query{SiteIDs: []int64{1}, Metrics: []string{"bounce_rate", "visitors"}})
	if sessions.Primary != tableSessions || sessions.HasSecondary {
		t.Errorf("visit-grain metrics should read sessions alone, got %+v", sessions)
	}

	if sessions.MetricTable["visitors"] != tableSessions {
		t.Error("a metric that counts the same on either table should be counted on the one already being read")
	}
}

// TestDeciderReadsBothTablesWhenItMust checks the mixed case.
func TestDeciderReadsBothTablesWhenItMust(t *testing.T) {
	mixed := planFor(t, Query{SiteIDs: []int64{1}, Metrics: []string{"pageviews", "bounce_rate"}})

	if mixed.Primary != tableEvents || !mixed.HasSecondary || mixed.Secondary != tableSessions {
		t.Fatalf("a mixed query should read events first and sessions second, got %+v", mixed)
	}

	if mixed.MetricTable["pageviews"] != tableEvents || mixed.MetricTable["bounce_rate"] != tableSessions {
		t.Errorf("metrics were assigned to %+v", mixed.MetricTable)
	}
}

// TestVisitsAreCountedWhereTheQueryAlreadyIs checks that asking for visits does
// not drag the sessions table into an event-scoped breakdown. On events a visit
// is a distinct session id, which is the same number counted in place.
func TestVisitsAreCountedWhereTheQueryAlreadyIs(t *testing.T) {
	decided := planFor(t, Query{
		SiteIDs:    []int64{1},
		Metrics:    []string{"pageviews", "visits"},
		Dimensions: []string{"event:page"},
	})

	if decided.HasSecondary {
		t.Errorf("visits beside pageviews should not open the sessions table, got %+v", decided)
	}
}

// TestSessionMetricUnderAPageBreakdownIsEntryScoped checks that the decider
// records the re-scoping rather than letting it happen silently.
func TestSessionMetricUnderAPageBreakdownIsEntryScoped(t *testing.T) {
	decided := planFor(t, Query{
		SiteIDs:    []int64{1},
		Metrics:    []string{"bounce_rate"},
		Dimensions: []string{"event:page"},
	})

	if !decided.SessionsEntryScoped {
		t.Error("a bounce rate per page is measured over entrances and must be recorded as such")
	}
}

// TestDeciderRefusesTheCombinationsWithNoAnswer is the guard rail. Each of
// these has no correctly-scoped answer, and answering anyway is how a wrong
// number ends up on a dashboard with nothing to flag it.
func TestDeciderRefusesTheCombinationsWithNoAnswer(t *testing.T) {
	cases := []struct {
		name  string
		query Query
	}{
		{
			name: "bounce rate per custom event name",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"bounce_rate"},
				Dimensions: []string{"event:name"}},
		},
		{
			name: "visit duration per property",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"visit_duration"},
				Dimensions: []string{"event:props:plan"}},
		},
		{
			name:  "conversion rate with nothing to convert",
			query: Query{SiteIDs: []int64{1}, Metrics: []string{"conversion_rate"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.query
			q.Normalise()

			if _, err := decide(&q); err == nil {
				t.Fatal("want a refusal")
			}
		})
	}
}

// TestEntryScopedDimensionsStillCompose checks that the page dimensions which
// do have a visit-grain analogue are allowed through.
func TestEntryScopedDimensionsStillCompose(t *testing.T) {
	for _, name := range []string{"event:page", "event:hostname", "event:page_title", "visit:source", "time:day"} {
		q := Query{SiteIDs: []int64{1}, Metrics: []string{"bounce_rate"}, Dimensions: []string{name}}
		q.Normalise()

		if _, err := decide(&q); err != nil {
			t.Errorf("a bounce rate per %s should be answerable: %v", name, err)
		}
	}
}

// TestSessionPropertyAggregateUsesSessions checks that a visit-scoped numeric
// property does not open the events table merely because aggregates use the
// special-metric execution path.
func TestSessionPropertyAggregateUsesSessions(t *testing.T) {
	q := Query{SiteIDs: []int64{1}, Metrics: []string{"sum(event:props:price)"}}
	q.Normalise()

	decided, err := decideScoped(&q, map[string]string{"price": propScopeSession})
	if err != nil {
		t.Fatal(err)
	}

	if decided.Primary != tableSessions || decided.HasSecondary {
		t.Fatalf("session property aggregate should scan sessions alone, got %+v", decided)
	}
}

// TestCompositesAreAnchoredOnEvents checks that a composite metric never ends
// up joined against session-grain group keys, which would silently return
// zeros.
func TestCompositesAreAnchoredOnEvents(t *testing.T) {
	decided := planFor(t, Query{
		SiteIDs:    []int64{1},
		Metrics:    []string{"scroll_depth"},
		Dimensions: []string{"visit:entry_page"},
	})

	if decided.Primary != tableEvents {
		t.Errorf("a composite metric must be counted from the events table, got %v", decided.Primary.name())
	}
}
