//
// explain_test.go
// The tool worth getting right, checked on both the arithmetic and the answer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"math"
	"strings"
	"testing"
)

// TestAttributeRanksByMovementNotBySize is the arithmetic that decides what an
// assistant tells somebody about their business, tested without a database, a
// clock or a query engine.
func TestAttributeRanksByMovementNotBySize(t *testing.T) {
	current := map[string]float64{"Google": 950, "Twitter": 40, "Reddit": 30}
	previous := map[string]float64{"Google": 1000, "Twitter": 10, "Newsletter": 200}

	// Total fell from 1210 to 1020.
	movers := attribute(current, previous, nil, -190)

	if len(movers) != 4 {
		t.Fatalf("got %d movers, want one per value in either period", len(movers))
	}

	// Newsletter is the answer: it is not the largest group in either period and
	// it does not appear in the current one at all, but it moved the most.
	if movers[0].Value != "Newsletter" {
		t.Fatalf("top mover = %q, want Newsletter — ranking by size rather than by movement is the mistake this test exists to catch", movers[0].Value)
	}

	if movers[0].Delta != -200 {
		t.Errorf("Newsletter delta = %v, want -200", movers[0].Delta)
	}

	// It accounts for more than the whole net fall, because other groups grew at
	// the same time. Reporting that honestly is more useful than capping it at
	// 100 and hiding the offsetting growth.
	if math.Abs(movers[0].SharePct) <= 100 {
		t.Errorf("Newsletter share = %v%%, want more than the whole net change", movers[0].SharePct)
	}

	// The sign is the direction. A group carrying more than the whole of a fall
	// has to read as a fall, not as growth.
	if movers[0].SharePct > 0 {
		t.Errorf("Newsletter share = %v%%, want it negative like the change it caused", movers[0].SharePct)
	}

	// A group that did not move is not a mover; padding the answer with rows
	// whose only content is "this stayed the same" makes it harder to read.
	for _, mover := range movers {
		if mover.Delta == 0 {
			t.Errorf("%q is listed with no movement", mover.Value)
		}
	}
}

// TestAttributeHandlesAFlatTotal checks the case where the net change is nothing
// but the mix churned. Dividing by a zero net change would give infinities;
// measuring against the gross movement instead answers the question that is
// actually interesting.
func TestAttributeHandlesAFlatTotal(t *testing.T) {
	current := map[string]float64{"Google": 500, "Twitter": 500}
	previous := map[string]float64{"Google": 900, "Twitter": 100}

	movers := attribute(current, previous, nil, 0)

	if len(movers) != 2 {
		t.Fatalf("got %d movers", len(movers))
	}

	for _, mover := range movers {
		if math.IsNaN(mover.SharePct) || math.IsInf(mover.SharePct, 0) {
			t.Fatalf("%q has a share of %v", mover.Value, mover.SharePct)
		}

		if math.Abs(mover.SharePct) != 50 {
			t.Errorf("%q share = %v%%, want half the gross movement", mover.Value, mover.SharePct)
		}
	}
}

// TestAttributeIsStableAcrossRuns checks that two runs of the same tool produce
// the same ordering. An assistant comparing two answers should not see a diff
// that is only map iteration order.
func TestAttributeIsStableAcrossRuns(t *testing.T) {
	current := map[string]float64{"a": 10, "b": 10, "c": 10, "d": 10, "e": 10}
	previous := map[string]float64{"a": 20, "b": 20, "c": 20, "d": 20, "e": 20}

	first := attribute(current, previous, nil, -50)

	for run := 0; run < 20; run++ {
		again := attribute(current, previous, nil, -50)

		for i := range first {
			if first[i].Value != again[i].Value {
				t.Fatalf("run %d ordered %q where the first run had %q", run, again[i].Value, first[i].Value)
			}
		}
	}
}

// TestPatternNamesTheShape checks the classifier the findings are built on.
func TestPatternNamesTheShape(t *testing.T) {
	concentrated := attribute(
		map[string]float64{"Google": 100, "Twitter": 50},
		map[string]float64{"Google": 200, "Twitter": 50},
		nil, -100)

	if patternOf(concentrated, nil, nil) != "concentrated" {
		t.Errorf("one group carrying the whole change was called %q", patternOf(concentrated, nil, nil))
	}

	// Ten pages each losing a tenth of the change. No single page explains
	// anything, which is the shape a broken snippet makes rather than a change
	// in the audience.
	current := map[string]float64{}
	previous := map[string]float64{}

	for i := 0; i < 10; i++ {
		page := string(rune('a' + i))
		current[page] = 90
		previous[page] = 100
	}

	broad := attribute(current, previous, nil, -100)

	if patternOf(broad, current, previous) != "broad" {
		t.Errorf("an evenly spread change was called %q", patternOf(broad, current, previous))
	}
}

// TestExplainFindsTheSourceThatStopped is the whole tool against the real
// engine.
//
// The fixture has a newsletter that sent traffic last week and none this week.
// It is invisible to any breakdown of the current period alone, so an
// implementation that only looks forward would miss the one thing a person would
// have spotted immediately.
func TestExplainFindsTheSourceThatStopped(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "explain_traffic_change",
		`{"site_id":"example.com","date_range":"7d","compare":"previous_period"}`)
	if answer.IsError {
		t.Fatalf("explain_traffic_change failed: %s", answer.Content[0].Text)
	}

	var explained explanation
	structured(t, answer, &explained)

	if explained.Current != currentVisitors || explained.Previous != previousVisitors {
		t.Fatalf("headline = %v against %v, want %d against %d",
			explained.Current, explained.Previous, currentVisitors, previousVisitors)
	}

	if explained.Delta != currentVisitors-previousVisitors {
		t.Errorf("delta = %v", explained.Delta)
	}

	// The source breakdown has to name Newsletter, with the right numbers.
	var found bool

	for _, driver := range explained.Drivers {
		if driver.Dimension != "visit:source" {
			continue
		}

		for _, mover := range driver.Movers {
			if mover.Value == "Newsletter" {
				found = true

				if mover.Previous != 4 || mover.Current != 0 {
					t.Errorf("Newsletter = %v now against %v before, want 0 against 4", mover.Current, mover.Previous)
				}
			}
		}
	}

	if !found {
		t.Fatalf("the source that stopped is not in the answer: %+v", explained.Drivers)
	}

	// And it has to be said in words, because that is what somebody reads.
	if !strings.Contains(strings.Join(explained.Findings, " "), "Newsletter") {
		t.Errorf("the findings do not mention the source that stopped: %v", explained.Findings)
	}

	if !strings.Contains(strings.Join(explained.Findings, " "), "stopped entirely") {
		t.Errorf("a source that went to zero must be called out as such: %v", explained.Findings)
	}

	if explained.Summary == "" {
		t.Fatal("no summary was written")
	}

	if answer.Content[0].Text != explained.Summary {
		t.Error("the text a model reads should be the summary")
	}
}

// TestExplainCoversTheDimensionsAnAnalystWouldCheck guards the sweep. A tool
// that only breaks down by source answers "which campaign" and never "which
// country blocked us".
func TestExplainCoversTheDimensionsAnAnalystWouldCheck(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "explain_traffic_change", `{"site_id":"example.com","date_range":"7d"}`)

	var explained explanation
	structured(t, answer, &explained)

	seen := map[string]bool{}
	for _, driver := range explained.Drivers {
		seen[driver.Dimension] = true

		// A dimension that could not be measured has to say so rather than
		// disappearing: a missing breakdown with no explanation reads as a
		// dimension with no movement.
		if driver.Note == "" && driver.Pattern == "" && len(driver.Movers) > 0 {
			t.Errorf("%s reported movers with no pattern", driver.Dimension)
		}
	}

	for _, expected := range explainDimensions {
		if !seen[expected] {
			t.Errorf("%s was not checked", expected)
		}
	}
}

// TestExplainIgnoresDimensionsWhereEverythingIsUnset checks the noise filter.
//
// A dimension whose every visit is unset trivially accounts for the whole change
// — one group, all of it — and without this it would score a perfect hundred
// percent and sort above the dimension that actually names the cause.
func TestExplainIgnoresDimensionsWhereEverythingIsUnset(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "explain_traffic_change", `{"site_id":"example.com","date_range":"7d"}`)

	var explained explanation
	structured(t, answer, &explained)

	// The fixture sets a source and a page and nothing else, so every other
	// dimension is entirely unset.
	if explained.Drivers[0].Dimension != "visit:source" && explained.Drivers[0].Dimension != "event:page" {
		t.Fatalf("the top driver is %q, want the dimension that actually moved", explained.Drivers[0].Dimension)
	}

	for _, driver := range explained.Drivers {
		if driver.Pattern != "unset" {
			continue
		}

		if driver.ExplainsPct != 0 {
			t.Errorf("%s explains %v%% while being entirely unset", driver.Dimension, driver.ExplainsPct)
		}

		if driver.Note == "" {
			t.Errorf("%s was dropped with no explanation", driver.Dimension)
		}
	}

	for _, finding := range explained.Findings {
		if strings.Contains(finding, "(not set)") {
			t.Errorf("a finding names an unset value, which explains nothing: %q", finding)
		}
	}
}

// TestExplainReportsHeadlineMetricsBesideTheDrivingOne checks the context that
// changes the story. A fall in visitors with a flat bounce rate is a different
// thing from one where the bounce rate doubled.
func TestExplainReportsHeadlineMetricsBesideTheDrivingOne(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "explain_traffic_change", `{"site_id":"example.com","date_range":"7d"}`)

	var explained explanation
	structured(t, answer, &explained)

	for _, metric := range headlineMetrics {
		if _, present := explained.Headline[metric]; !present {
			t.Errorf("the headline is missing %s", metric)
		}
	}

	if explained.Headline["visitors"].Current != currentVisitors {
		t.Errorf("headline visitors = %v", explained.Headline["visitors"].Current)
	}
}

// TestExplainRefusesAllTime checks the range that has nothing before it. Running
// twenty queries to produce a table of zeroes would be worse than saying so.
func TestExplainRefusesAllTime(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "explain_traffic_change", `{"site_id":"example.com","date_range":"all"}`)

	if !answer.IsError {
		t.Fatal("explaining all of time was accepted")
	}

	if !strings.Contains(answer.Content[0].Text, "earlier period") {
		t.Errorf("the refusal does not say why: %q", answer.Content[0].Text)
	}
}

// TestExplainRejectsBadArguments checks the parameter validation on the tool
// with the most of it.
func TestExplainRejectsBadArguments(t *testing.T) {
	f := newFixture(t)

	cases := map[string]string{
		"a site this key cannot see":   `{"site_id":"notyours.com"}`,
		"a metric that does not exist": `{"site_id":"example.com","metric":"vistors"}`,
		"a comparison nobody has":      `{"site_id":"example.com","compare":"last_tuesday"}`,
		"an invented argument":         `{"site_id":"example.com","why":"please"}`,
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if answer := f.tool(t, "explain_traffic_change", args); !answer.IsError {
				t.Fatal("the call succeeded")
			}
		})
	}
}
