//
// MainGraph.test.ts
// Weekly annotation markers on the graph's real buckets.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { Annotation } from "../api/types";
import { metricAxisValue } from "../lib/format";
import {
	annotationTooltipReducer,
	barWidth,
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
