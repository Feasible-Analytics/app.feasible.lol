//
// MainGraph.test.ts
// Weekly annotation markers on the graph's real buckets.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import type { Annotation } from "../api/types";
import { metricAxisValue } from "../lib/format";
import {
	annotationTooltipReducer,
	Bars,
	ComparisonSwatch,
	barRects,
	barWidth,
	comparisonShape,
	bucketAt,
	bucketX,
	placeMarkers,
	visibleAnnotationTooltip,
	type AnnotationTooltipState,
} from "./MainGraph";
import { GRAPHABLE, TILE_METRICS } from "./TopStats";

/** annotation builds the complete wire shape around one local date. */
function annotation(id: number, shownOn: string): Annotation {
	return {
		id,
		site_id: 1,
		shown_on: shownOn,
		body: `Note ${id}`,
		author_user_id: 1,
		author_name: "Anna",
		created_at: 1,
		updated_at: 1,
	};
}

test("weekly markers bucket every day through the graph's Monday start", () => {
	const labels = ["2026-08-24", "2026-08-31"];
	const markers = placeMarkers(
		[
			annotation(1, "2026-08-24"),
			annotation(2, "2026-08-26"),
			annotation(3, "2026-08-30"),
			annotation(4, "2026-08-31"),
		],
		labels,
		"week",
	);

	assert.deepEqual(
		markers.map((marker) => ({ index: marker.index, ids: marker.notes.map((note) => note.id) })),
		[
			{ index: 0, ids: [1, 2, 3] },
			{ index: 1, ids: [4] },
		],
	);
});

test("a weekly marker crosses month and year boundaries by calendar date", () => {
	const markers = placeMarkers(
		[annotation(1, "2027-01-03"), annotation(2, "2027-01-04")],
		["2026-12-28", "2027-01-04"],
		"week",
	);

	assert.deepEqual(markers.map((marker) => marker.index), [0, 1]);
});

test("weekly markers stay on current buckets while a comparison is present", () => {
	// Comparison rows are aligned positionally under these current labels. An
	// annotation from the current Wednesday therefore stays at current index 1;
	// it must not be shifted to a date from the earlier comparison window.
	const markers = placeMarkers(
		[annotation(1, "2026-09-02")],
		["2026-08-24", "2026-08-31", "2026-09-07"],
		"week",
	);

	assert.equal(markers.length, 1);
	assert.equal(markers[0]?.index, 1);
});

test("daily and monthly marker matching is unchanged", () => {
	assert.equal(placeMarkers([annotation(1, "2026-08-26")], ["2026-08-26 00:00:00"], "day")[0]?.index, 0);
	assert.equal(placeMarkers([annotation(2, "2026-08-26")], ["2026-08"], "month")[0]?.index, 0);
});

test("tap and click toggle one annotation tooltip without sticky touch hover", () => {
	let state: AnnotationTooltipState = { hovered: null, focused: null, pinned: null };
	state = annotationTooltipReducer(state, { type: "pointer-enter", index: 1, pointerType: "touch" });
	assert.equal(visibleAnnotationTooltip(state), null);

	state = annotationTooltipReducer(state, { type: "toggle", index: 1 });
	assert.equal(visibleAnnotationTooltip(state), 1);
	state = annotationTooltipReducer(state, { type: "pointer-enter", index: 1, pointerType: "mouse" });
	state = annotationTooltipReducer(state, { type: "focus", index: 1 });
	state = annotationTooltipReducer(state, { type: "toggle", index: 1 });
	assert.equal(visibleAnnotationTooltip(state), null);
});

test("hover and keyboard focus remain transient when no marker is pinned", () => {
	let state: AnnotationTooltipState = { hovered: null, focused: null, pinned: null };
	state = annotationTooltipReducer(state, { type: "pointer-enter", index: 2, pointerType: "mouse" });
	assert.equal(visibleAnnotationTooltip(state), 2);
	state = annotationTooltipReducer(state, { type: "pointer-leave", index: 2 });
	state = annotationTooltipReducer(state, { type: "focus", index: 3 });
	assert.equal(visibleAnnotationTooltip(state), 3);
	state = annotationTooltipReducer(state, { type: "blur", index: 3 });
	assert.equal(visibleAnnotationTooltip(state), null);
});

test("Escape and an outside pointer dismiss every tooltip interaction", () => {
	for (const reason of ["escape", "outside"] as const) {
		let state: AnnotationTooltipState = { hovered: 1, focused: 1, pinned: 1 };
		state = annotationTooltipReducer(state, { type: reason });
		assert.equal(visibleAnnotationTooltip(state), null, reason);
	}
});

test("every headline metric can drive the main graph", () => {
	assert.deepEqual([...GRAPHABLE], TILE_METRICS);
});

test("engagement graph axes retain their units", () => {
	assert.equal(metricAxisValue("views_per_visit", 2.5), "2.5");
	assert.equal(metricAxisValue("bounce_rate", 57.25), "57.25%");
});

// The plot the scale tests measure against. 400 pixels over four buckets makes
// every slot a round hundred, so a wrong answer is a readable number rather than
// a rounding argument.
const PLOT = 400;
const BUCKETS = 4;

test("a line reaches both edges of the plot and bars sit inside their own slots", () => {
	assert.equal(bucketX("line", 0, PLOT, BUCKETS), 60);
	assert.equal(bucketX("line", BUCKETS - 1, PLOT, BUCKETS), 460);

	// Half a slot in from each edge, which is what stops the first and last bar
	// hanging off the plot.
	assert.equal(bucketX("bar", 0, PLOT, BUCKETS), 110);
	assert.equal(bucketX("bar", BUCKETS - 1, PLOT, BUCKETS), 410);
});

test("one bucket is centred under either shape", () => {
	assert.equal(bucketX("line", 0, PLOT, 1), 260);
	assert.equal(bucketX("bar", 0, PLOT, 1), 260);
});

test("a pointer anywhere in a bar's slot picks that bar", () => {
	assert.equal(bucketAt("bar", 60, PLOT, BUCKETS), 0);
	assert.equal(bucketAt("bar", 159, PLOT, BUCKETS), 0);
	assert.equal(bucketAt("bar", 160, PLOT, BUCKETS), 1);
	assert.equal(bucketAt("bar", 459, PLOT, BUCKETS), 3);
});

test("a pointer on a line picks the nearest point rather than a slot", () => {
	// Two thirds of the way towards the second point, which a slot would still
	// call the first bucket.
	assert.equal(bucketAt("line", 60 + 90, PLOT, BUCKETS), 1);
	assert.equal(bucketAt("line", 60 + 60, PLOT, BUCKETS), 0);
});

test("a pointer past the end of the plot is over no bucket at all", () => {
	// A bar's slot has hard edges, so one pixel outside is already outside.
	assert.equal(bucketAt("bar", 59, PLOT, BUCKETS), null);
	assert.equal(bucketAt("bar", 461, PLOT, BUCKETS), null);

	// A point has no edges, so it keeps half a step of tolerance on each side and
	// only clears past that. Anything tighter would make the first and last
	// buckets of every chart the two hardest to hover.
	assert.equal(bucketAt("line", 59, PLOT, BUCKETS), 0);
	assert.equal(bucketAt("line", 60 - 70, PLOT, BUCKETS), null);
	assert.equal(bucketAt("line", 460 + 70, PLOT, BUCKETS), null);
});

test("an empty chart is over no bucket wherever the pointer is", () => {
	for (const shape of ["line", "bar"] as const) assert.equal(bucketAt(shape, 200, PLOT, 0), null, shape);
});

test("bars keep a gap at every range and never grow into a slab", () => {
	// A year of daily buckets: the bar is thin but still drawn, because a bar
	// rounded away is indistinguishable from a bucket with no data.
	assert.ok(barWidth(PLOT, 365) >= 1);

	// A normal range: narrower than its slot, so consecutive bars stay apart.
	assert.ok(barWidth(PLOT, 10) < PLOT / 10);

	// A three-day range: capped, rather than three slabs filling the card.
	assert.equal(barWidth(PLOT, 3), 56);
});

test("a comparison splits the slot without changing a single-series chart", () => {
	// One series is exactly what it was: the default argument is what keeps
	// every existing caller and every existing width unchanged.
	assert.equal(barWidth(PLOT, 10, 1), barWidth(PLOT, 10));

	// Two series share the slot, so each bar is half as wide and the pair
	// occupies what one bar used to.
	assert.equal(barWidth(PLOT, 10, 2), barWidth(PLOT, 10) / 2);

	// A bar is as close to the 1px floor as its share of the slot allows. At 365
	// paired buckets the share is under a pixel, and a sliver inside the slot
	// beats a full pixel spilling over the bucket beside it.
	assert.equal(barWidth(PLOT, 365, 2), PLOT / 365 / 2);
	assert.equal(barWidth(PLOT, 365, 1), 1);

	// The pair always fits inside its slot, at every bucket count. That is what
	// keeps both bars under the hover highlight and off the bucket beside them,
	// however thin the chart gets.
	for (const buckets of [3, 12, 24, 100, 365, 1000]) {
		assert.ok(barWidth(PLOT, buckets, 2) * 2 <= PLOT / buckets + 1e-9, `${buckets} buckets`);
	}
});

test("a compared bucket draws two bars, one either side of its centre", () => {
	const rects = barRects(10, 4, true, 100, 12);

	assert.deepEqual(rects, [
		{ series: "current", value: 10, x: 88, width: 12 },
		{ series: "earlier", value: 4, x: 100, width: 12 },
	]);

	// The pair is centred on the x the markers and the hover highlight use, so
	// one bucket is still one slot.
	assert.equal((rects[0]!.x + rects[1]!.x + rects[1]!.width) / 2, 100);
});

test("an uncompared bucket draws one bar, centred, exactly as before", () => {
	assert.deepEqual(barRects(10, null, false, 100, 12), [
		{ series: "current", value: 10, x: 94, width: 12 },
	]);
});

test("a missing value leaves its half of the slot empty", () => {
	// The current bar does not drift right into the space the earlier one would
	// have taken: two buckets are only comparable by eye if a bar stays put.
	assert.deepEqual(barRects(10, null, true, 100, 12), [
		{ series: "current", value: 10, x: 88, width: 12 },
	]);

	assert.deepEqual(barRects(null, 4, true, 100, 12), [
		{ series: "earlier", value: 4, x: 100, width: 12 },
	]);

	// Nothing at all draws nothing at all.
	assert.deepEqual(barRects(null, null, true, 100, 12), []);
});

/** drawn renders the bar layer on its own. MainGraph measures its container
 * before it draws anything, so outside a browser the component itself only ever
 * produces its loading state. */
function drawn(comparing: boolean, previous: (number | null)[] = [40, null, 60]) {
	return renderToStaticMarkup(
		createElement(Bars, {
			points: [10, 20, 30],
			previous,
			labels: ["a", "b", "c"],
			comparing,
			present: 2,
			bar: 12,
			x: (index: number) => 100 + index * 40,
			y: (value: number) => 200 - value,
			axis: 200,
		}),
	);
}

test("a compared bar chart draws two rects per bucket, in two colours", () => {
	const markup = drawn(true);

	// fill, not any mention: the in-progress bar strokes in the accent colour too.
	assert.equal(markup.match(/fill="var\(--fs-accent\)"/g)?.length, 3, "one accent bar per bucket");
	assert.equal(markup.match(/fill="var\(--fs-faint\)"/g)?.length, 2, "one neutral bar per bucket that has one");
	assert.doesNotMatch(markup, /stroke-dasharray="3 3"/, "the dashed comparison line has no place in bar mode");
});

test("an uncompared bar chart draws exactly what it always did", () => {
	const markup = drawn(false);

	assert.equal(markup.match(/fill="var\(--fs-accent\)"/g)?.length, 3);
	assert.doesNotMatch(markup, /fill="var\(--fs-faint\)"/, "there is no earlier period to draw");
});

test("only the current bar is drawn hollow", () => {
	// present is bucket 2, which has both a current and an earlier value. The
	// earlier period has no bucket still filling up, so it is solid.
	const markup = drawn(true, [40, 50, 60]);
	const buckets = markup.split("<g>").slice(1);
	const pending = buckets[2]!;

	assert.match(pending, /var\(--fs-accent\)[^>]*fill-opacity="0.3"/, "the in-progress current bar is hollow");
	assert.doesNotMatch(
		pending.slice(pending.indexOf("--fs-faint")),
		/fill-opacity="0.3"/,
		"the earlier bar is solid",
	);
});

test("the legend swatch is the mark the chart actually draws", () => {
	assert.match(renderToStaticMarkup(createElement(ComparisonSwatch, { chart: "bar" })), /<rect[^>]*fill="var\(--fs-faint\)"/);
	assert.doesNotMatch(renderToStaticMarkup(createElement(ComparisonSwatch, { chart: "bar" })), /stroke-dasharray/);

	assert.match(renderToStaticMarkup(createElement(ComparisonSwatch, { chart: "line" })), /stroke-dasharray="3 3"/);
});

test("only a line chart draws the comparison as a line, and only a bar chart pairs", () => {
	assert.equal(comparisonShape("bar", true, true), "paired");
	assert.equal(comparisonShape("line", true, true), "line");

	// A bar chart never draws the dashed overlay, which is the whole change.
	assert.notEqual(comparisonShape("bar", true, true), "line");
});

test("a comparison that is on but not yet answered draws nothing", () => {
	// The response is held while the next one loads. Halving every bar on the
	// setting alone would shift them left and snap them back a moment later.
	assert.equal(comparisonShape("bar", true, false), "none");
	assert.equal(comparisonShape("line", true, false), "none");

	assert.equal(comparisonShape("bar", false, true), "none");
	assert.equal(comparisonShape("line", false, true), "none");
});
