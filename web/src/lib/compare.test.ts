//
// compare.test.ts
// The comparison arithmetic, including the cases that make a comparison lie.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { StatsResponse } from "../api/types";
import { changePercent, comparisonSeries, previousBucketLabel, series } from "./compare";

/** response builds the shape the engine returns for a time series with a
 *  comparison attached: one row per bucket that had traffic, the earlier
 *  period's figures hung off each row, and the full bucket list in meta. */
function response(
	labels: string[],
	rows: { bucket: string; value: number; earlier?: number }[],
): StatsResponse {
	return {
		results: rows.map((row) => ({
			metrics: [row.value],
			dimensions: [row.bucket],
			comparison: row.earlier === undefined ? undefined : { metrics: [row.earlier], change: [null] },
		})),
		meta: {
			time_labels: labels,
			present_index: null,
			interval: "day",
			sample_rate: 1,
			sources: ["raw"],
		},
		query: {
			site_ids: [1],
			metrics: ["visitors"],
			dimensions: ["time"],
			date_range: ["2026-08-24", "2026-08-27"],
			date_range_preset: "custom",
			timezone: "UTC",
		},
	};
}

test("a change is a percentage of the earlier figure", () => {
	assert.equal(changePercent(150, 100), 50);
	assert.equal(changePercent(50, 100), -50);
	assert.equal(changePercent(100, 100), 0);
});

test("there is no change from nothing", () => {
	// The engine returns null here for the same reason: a rise from zero has no
	// percentage, and printing 100% or ∞ puts a made-up number beside real ones.
	assert.equal(changePercent(10, 0), null);
	assert.equal(changePercent(0, 0), null);
});

test("a fall to zero is a real minus one hundred per cent", () => {
	assert.equal(changePercent(0, 40), -100);
});

test("a change is trimmed to the same three places the engine trims to", () => {
	// 66.66666… is not more accurate than 66.667, and the extra digits are noise
	// in every response body and every assertion made against one.
	assert.equal(changePercent(200, 120), 66.667);
});

test("no change never renders as negative zero", () => {
	assert.ok(!Object.is(changePercent(-0, 100), -0));
});

test("a bucket with no row is a gap, not a zero", () => {
	const data = response(
		["2026-08-24", "2026-08-25", "2026-08-26"],
		[
			{ bucket: "2026-08-24", value: 10 },
			{ bucket: "2026-08-26", value: 30 },
		],
	);

	assert.deepEqual(series(data), [10, null, 30]);
});

test("the overlay is indexed by the current period's buckets", () => {
	// The engine matches the two periods positionally, because they have
	// different dates by definition. Reading the overlay by the earlier period's
	// own dates would slide it sideways by however far apart the periods are.
	const data = response(
		["2026-08-24", "2026-08-25", "2026-08-26"],
		[
			{ bucket: "2026-08-24", value: 10, earlier: 8 },
			{ bucket: "2026-08-25", value: 20, earlier: 25 },
			{ bucket: "2026-08-26", value: 30, earlier: 30 },
		],
	);

	assert.deepEqual(comparisonSeries(data), [8, 25, 30]);
	assert.deepEqual(series(data), [10, 20, 30]);
});

test("a bucket the earlier period never reached leaves a gap in the overlay", () => {
	const data = response(
		["2026-08-24", "2026-08-25"],
		[
			{ bucket: "2026-08-24", value: 10, earlier: 8 },
			{ bucket: "2026-08-25", value: 20 },
		],
	);

	assert.deepEqual(comparisonSeries(data), [8, null]);
});

test("a response with no comparison has an empty overlay", () => {
	const data = response(["2026-08-24"], [{ bucket: "2026-08-24", value: 10 }]);

	assert.deepEqual(comparisonSeries(data), [null]);
	assert.deepEqual(comparisonSeries(null), []);
});

test("the earlier bucket a position is compared against is calendar arithmetic", () => {
	const bounds = ["2026-08-03T00:00:00-07:00", "2026-08-10T00:00:00-07:00"];

	assert.equal(previousBucketLabel(bounds, "day", 0), "2026-08-03");
	assert.equal(previousBucketLabel(bounds, "day", 6), "2026-08-09");
	assert.equal(previousBucketLabel(bounds, "week", 2), "2026-08-17");
});

test("a day step crosses a month and a year boundary", () => {
	assert.equal(previousBucketLabel(["2026-12-30T00:00:00Z"], "day", 3), "2027-01-02");
	assert.equal(previousBucketLabel(["2028-02-28T00:00:00Z"], "day", 1), "2028-02-29");
});

test("a month step counts months, not thirty-day blocks", () => {
	assert.equal(previousBucketLabel(["2025-11-01T00:00:00Z"], "month", 0), "2025-11");
	assert.equal(previousBucketLabel(["2025-11-01T00:00:00Z"], "month", 3), "2026-02");
});

test("an hourly comparison shows no date at all", () => {
	// The wall clock repeats an hour twice a year, so a label stepped from the
	// window's start would be an hour out for half a day. A tooltip that is
	// confidently wrong is worse than one that only shows the number.
	assert.equal(previousBucketLabel(["2026-08-03T00:00:00Z"], "hour", 5), "");
	assert.equal(previousBucketLabel(["2026-08-03T00:00:00Z"], "minute", 5), "");
});

test("a missing comparison window names no bucket", () => {
	assert.equal(previousBucketLabel(undefined, "day", 0), "");
	assert.equal(previousBucketLabel([], "day", 0), "");
	assert.equal(previousBucketLabel(["not-a-date"], "day", 0), "");
});
