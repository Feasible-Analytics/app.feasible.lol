//
// weights_test.go
// The distributions really do have the concentration the specification asks for.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"math"
	"testing"
)

// TestConcentrationTargets is the specification of this package written as a
// test. Every exponent in the catalogue was chosen to put a named share of the
// traffic in the head of its distribution, and an exponent that drifted would
// produce data that still looks plausible and measures nothing like production.
func TestConcentrationTargets(t *testing.T) {
	places, placeWeights := placeCatalog()

	for _, item := range []struct {
		name  string
		what  *chooser
		top   int
		least float64
		most  float64
	}{
		// Pages are sampled through two distributions — the entry page of a
		// visit and every page after it — so the head of the page catalogue on
		// its own sits below the fifty per cent the two produce together.
		{name: "pages", what: newChooser(zipf(distinctPages, pageExponent)), top: 10, least: 0.30, most: 0.55},
		{name: "sources", what: newChooser(zipf(200, sourceExponent)), top: 5, least: 0.60, most: 0.80},
		{name: "browser and os pairs", what: newChooser(zipf(len(agentCatalog), agentExponent)), top: 5, least: 0.85, most: 0.95},
	} {
		if share := item.what.share(item.top); share < item.least || share > item.most {
			t.Errorf("the top %d %s take %.0f%% of traffic, want between %.0f%% and %.0f%%",
				item.top, item.name, share*100, item.least*100, item.most*100)
		}
	}

	// The place catalogue splits each country's weight across its cities, so
	// the top ten *places* are not the top ten countries. What the target is
	// about is countries, and this is where that is checked.
	if share := countryShare(places, placeWeights, 10); share < 0.72 || share > 0.88 {
		t.Errorf("the top ten countries take %.0f%% of traffic, want about 80%%", share*100)
	}
}

// countryShare adds up the weight of the busiest n countries.
func countryShare(places []place, weights []float64, n int) float64 {
	byCountry := map[string]float64{}
	total := 0.0

	for i, item := range places {
		byCountry[item.Country] += weights[i]
		total += weights[i]
	}

	// The countries are ranked by weight, which is the order the catalogue is
	// written in — but reading it back rather than assuming it is what makes
	// this a check rather than a restatement.
	var ordered []float64
	for _, weight := range byCountry {
		ordered = append(ordered, weight)
	}

	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j] > ordered[i] {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}

	head := 0.0
	for i := 0; i < n && i < len(ordered); i++ {
		head += ordered[i]
	}

	return head / total
}

// TestSessionLengthsMatchTheTarget checks the head and the tail of the visit
// length distribution. Without the head the bounce rate is nothing like a real
// one; without the tail the session fold is never asked to do anything.
func TestSessionLengthsMatchTheTarget(t *testing.T) {
	lengths := sessionLengths()

	if lengths.len() != maxSessionPageviews {
		t.Fatalf("the distribution reaches %d pageviews, want %d", lengths.len(), maxSessionPageviews)
	}

	single := lengths.share(1)
	if math.Abs(single-singlePageviewShare) > 0.02 {
		t.Errorf("%.0f%% of visits are a single pageview, want about %.0f%%", single*100, singlePageviewShare*100)
	}

	// A visit of twenty pageviews has to be rare and possible. Rare enough that
	// it does not distort the averages, possible enough that a seeded dataset
	// contains some.
	tail := 1 - lengths.share(19)
	if tail <= 0 || tail > 0.02 {
		t.Errorf("%.4f of visits reach twenty pageviews, want a thin but present tail", tail)
	}
}

// TestChooserFollowsItsWeights checks the sampler itself, since every
// distribution in the package is only as good as this one function.
func TestChooserFollowsItsWeights(t *testing.T) {
	chooser := newChooser([]float64{1, 1, 2})

	for _, item := range []struct {
		draw float64
		want int
	}{
		{draw: 0, want: 0},
		{draw: 0.24, want: 0},
		{draw: 0.26, want: 1},
		{draw: 0.60, want: 2},
		{draw: 0.999999, want: 2},
	} {
		if got := chooser.pick(item.draw); got != item.want {
			t.Errorf("a draw of %.6f picked %d, want %d", item.draw, got, item.want)
		}
	}

	// An empty catalogue must not divide by zero or panic. It can only come
	// from a data file, and failing a whole run over one would be the wrong
	// trade.
	if got := newChooser(nil).pick(0.5); got != 0 {
		t.Errorf("an empty distribution picked %d, want 0", got)
	}
}
