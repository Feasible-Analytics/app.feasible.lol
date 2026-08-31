//
// traffic.go
// The shape of the traffic: the week, the day, the spike, and the gap.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"time"
)

// hourCurves are the twenty-four hourly weights, in UTC. Without a curve the
// hourly roll-ups are tested against flat buckets that look nothing like a real
// day, and the first time anyone sees a realistic one is in production.
//
// The three curves differ because the audiences do: a marketing site peaks
// during European and American working hours, a blog peaks in the evening and
// at the weekend, and a documentation site is read at a desk.
var hourCurves = map[siteKind][24]float64{
	kindMarketing: {
		0.25, 0.18, 0.14, 0.12, 0.13, 0.18, 0.30, 0.50,
		0.75, 1.00, 1.15, 1.20, 1.15, 1.20, 1.35, 1.40,
		1.30, 1.10, 0.90, 0.75, 0.62, 0.50, 0.40, 0.30,
	},
	kindBlog: {
		0.35, 0.28, 0.22, 0.18, 0.16, 0.20, 0.30, 0.45,
		0.65, 0.85, 0.95, 1.00, 1.05, 1.10, 1.20, 1.30,
		1.35, 1.30, 1.25, 1.20, 1.10, 0.90, 0.65, 0.45,
	},
	kindDocs: {
		0.18, 0.12, 0.09, 0.08, 0.09, 0.14, 0.28, 0.55,
		0.90, 1.25, 1.45, 1.50, 1.30, 1.35, 1.50, 1.45,
		1.25, 0.95, 0.70, 0.52, 0.40, 0.32, 0.26, 0.20,
	},
	kindShop: {
		0.30, 0.22, 0.16, 0.13, 0.14, 0.20, 0.34, 0.52,
		0.72, 0.92, 1.05, 1.12, 1.10, 1.15, 1.25, 1.32,
		1.35, 1.30, 1.25, 1.20, 1.05, 0.85, 0.62, 0.44,
	},
}

// weekendFactors are how much of a weekday each site kind keeps at the weekend.
// A documentation site loses two thirds of its traffic on a Saturday and a blog
// barely notices, and a seed that treated them alike would make the
// week-over-week comparison card meaningless.
var weekendFactors = map[siteKind][2]float64{
	kindMarketing: {0.58, 0.52}, // Saturday, Sunday
	kindBlog:      {0.95, 1.05},
	kindDocs:      {0.34, 0.30},
	kindShop:      {0.78, 0.86},
}

// growthPerDay is the compounding trend applied across the whole run. Real
// traffic is never flat, and a flat series makes every comparison card in the
// product read "0.0%", which is indistinguishable from a broken one.
const growthPerDay = 1.006

// spikeFactor is how much the busiest day multiplies by. A post lands somewhere
// with an audience and the day is four times normal — this is what a spike
// alert has to fire on and what a broken y-axis looks like.
const spikeFactor = 4.0

// spikeSpillover is the smaller bump the same day gives the other sites, from
// people who arrived for one thing and looked at another.
const spikeSpillover = 1.6

// hourly returns the hour weights for a site kind, falling back to the
// marketing curve for a kind with no curve of its own.
func hourly(kind siteKind) [24]float64 {
	if curve, ok := hourCurves[kind]; ok {
		return curve
	}

	return hourCurves[kindMarketing]
}

// weekFactor is what one weekday does to a site's volume. Saturday and Sunday
// are separate numbers because they are genuinely different days on every site
// anyone has ever measured.
func weekFactor(day time.Time, kind siteKind) float64 {
	factors, ok := weekendFactors[kind]
	if !ok {
		factors = weekendFactors[kindMarketing]
	}

	switch day.Weekday() {
	case time.Saturday:
		return factors[0]
	case time.Sunday:
		return factors[1]
	case time.Monday:
		return 1.04
	case time.Tuesday:
		return 1.08
	case time.Wednesday:
		return 1.06
	case time.Thursday:
		return 1.02
	default:
		return 0.94
	}
}

// gapDay is the index of the day with no traffic at all, or -1 for a run too
// short to spare one. A day with zero rows is a gap in the graph rather than a
// zero, and it is the case a chart library gets wrong: it either interpolates
// straight through it or drops the axis entirely.
func gapDay(days int) int {
	if days < 12 {
		return -1
	}

	return days - 9
}

// spikeDay is the index of the day that goes several times normal. It sits near
// the end so it lands inside the default seven- and thirty-day dashboard
// ranges, which is where anyone would look for it.
func spikeDay(days int) int {
	if days < 8 {
		return -1
	}

	return days - 4
}

// dayFactor is one site's volume multiplier for one day: the week, the trend,
// the spike and the gap, multiplied together. Keeping them as one function is
// what makes the shape of a run readable in one place instead of spread across
// the loop that uses it.
func dayFactor(index, days int, day time.Time, kind siteKind, primary bool) float64 {
	if index == gapDay(days) {
		return 0
	}

	factor := weekFactor(day, kind)

	// The trend is anchored at the end of the run rather than the start, so
	// "the last thirty days" is always the busy part whatever length was asked
	// for.
	for i := index; i < days-1; i++ {
		factor /= growthPerDay
	}

	if index == spikeDay(days) {
		if primary {
			factor *= spikeFactor
		} else {
			factor *= spikeSpillover
		}
	}

	return factor
}

// dayFraction is how much of a day has happened. It is 1 for every day but the
// last, which is only complete if the run ends at midnight — and a seeded
// "today" that already holds a full day's traffic makes the today card wrong in
// a way that is hard to spot.
func dayFraction(dayStart, now time.Time, kind siteKind) float64 {
	if !now.Before(dayStart.Add(24 * time.Hour)) {
		return 1
	}
	if !now.After(dayStart) {
		return 0
	}

	curve := hourly(kind)

	var elapsed, total float64
	for hour := 0; hour < 24; hour++ {
		total += curve[hour]
	}

	full := int(now.Sub(dayStart) / time.Hour)
	for hour := 0; hour < full && hour < 24; hour++ {
		elapsed += curve[hour]
	}

	// The partial hour is counted in proportion, so a run started at half past
	// the hour does not lose or gain a whole hour's traffic.
	if full < 24 {
		part := float64(now.Sub(dayStart)%time.Hour) / float64(time.Hour)
		elapsed += curve[full] * part
	}

	return elapsed / total
}

// allocate turns one total into a pageview budget per day per site. The
// arithmetic is a running cumulative rather than a rounding of each cell,
// because rounding two thousand cells independently loses or gains a few
// hundred pageviews and the number printed at the end of a run has to be the
// number that was asked for.
func allocate(total int64, days int, start, now time.Time, sites []siteFixture) [][]int64 {
	weights := make([][]float64, days)
	sum := 0.0

	for index := 0; index < days; index++ {
		day := start.AddDate(0, 0, index)
		weights[index] = make([]float64, len(sites))

		for i, site := range sites {
			weight := site.Weight *
				dayFactor(index, days, day, site.Kind, i == 0) *
				dayFraction(day, now, site.Kind)

			weights[index][i] = weight
			sum += weight
		}
	}

	budget := make([][]int64, days)
	for index := range budget {
		budget[index] = make([]int64, len(sites))
	}

	if sum <= 0 {
		return budget
	}

	var (
		running   float64
		allocated int64
	)

	for index := 0; index < days; index++ {
		for i := range sites {
			running += weights[index][i]

			// The cell gets whatever the cumulative target has moved past,
			// which makes the fractions carry forward instead of vanishing.
			target := int64(float64(total)*running/sum + 0.5)
			budget[index][i] = target - allocated
			allocated = target
		}
	}

	return budget
}

// sessionLengths is the distribution of pageviews per visit. It is built once
// and sampled per session: sixty per cent of visits are a single pageview, and
// the tail reaches thirty. Without the head the bounce rate is nothing like a
// real one; without the tail the session fold is never asked to do anything.
func sessionLengths() *chooser {
	return newChooser(zipfTail(maxSessionPageviews, singlePageviewShare, sessionLengthExponen))
}

// dwell is how long a visitor spends on a page before the next one, in seconds.
// It is drawn from a rough log-normal rather than a uniform range because time
// on page is heavily skewed — most people move on in seconds and a few read the
// whole thing — and a uniform dwell makes every duration average identical.
func dwell(u float64) int64 {
	switch {
	case u < 0.30:
		return 3 + int64(u*40)
	case u < 0.70:
		return 15 + int64((u-0.30)*220)
	case u < 0.93:
		return 60 + int64((u-0.70)*900)
	default:
		return 300 + int64((u-0.93)*8000)
	}
}
