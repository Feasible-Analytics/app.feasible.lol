//
// compare.ts
// The arithmetic behind comparison mode.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Interval, StatsResponse } from "../api/types";
import { pad } from "./period";

/**
 * The engine does the counting; this file does the alignment.
 *
 * A comparison query comes back with the earlier period's numbers hung off the
 * rows of the current one, matched by *position* rather than by date — bucket
 * three of last week is what bucket three of this week is compared against,
 * because the two periods have different dates by definition. Everything here
 * exists to keep that positional match honest on the way to the screen.
 */

/** The comparison modes the engine resolves. `off` is this file's own state, not
 *  a mode the wire has: it means no comparison is requested at all. */
export type CompareMode = "off" | "previous_period" | "year_over_year";

/** The wording each mode gets, as message ids. Nothing in this file renders a
 *  string: it is arithmetic, and arithmetic that imported a catalogue could not
 *  be tested outside a browser. */
export const COMPARE_LABELS: Record<CompareMode, string> = {
	off: "dashboard.compare.off",
	previous_period: "dashboard.compare.previous_period",
	year_over_year: "dashboard.compare.year_over_year",
};

/**
 * changePercent is the percentage difference between two figures.
 *
 * It mirrors the engine's own rule exactly, including the null: there is no
 * meaningful percentage change from zero, and rendering one — 0%, or ∞, or a
 * bare 100% — puts a fabricated number on the page beside real ones. The client
 * needs its own copy because a figure it derived, such as a bucket total on the
 * graph, has no server-computed change to read.
 */
export function changePercent(current: number, previous: number): number | null {
	if (previous === 0) return null;

	const value = Math.round(((100 * (current - previous)) / previous) * 1000) / 1000;

	// Negative zero renders as "-0", which reads as a bug rather than as no
	// change at all.
	return value === 0 ? 0 : value;
}

/**
 * series reads one metric out of a time-series response as a bucket-aligned
 * array, with null for a bucket that has no row.
 *
 * Null is not zero. A bucket the engine returned no row for is a bucket in which
 * nothing was recorded, which on a live chart is a gap and on a broken tracker
 * is the only visible symptom. Drawing a confident zero for it is how a
 * dashboard lies with a straight line.
 */
export function series(response: StatsResponse | null, metricIndex = 0): (number | null)[] {
	const labels = response?.meta.time_labels ?? [];
	const values = new Map<string, number>();

	for (const row of response?.results ?? []) {
		const key = row.dimensions[0];
		if (key !== undefined) values.set(key, row.metrics[metricIndex] ?? 0);
	}

	return labels.map((label) => values.get(label) ?? null);
}

/**
 * comparisonSeries reads the earlier period's values off the same rows, in the
 * current period's bucket order.
 *
 * The overlay has to sit under the current line bucket for bucket, so it is
 * indexed by the current period's labels even though its numbers belong to
 * different dates. Reading it any other way — by the earlier period's own dates
 * — would slide the whole overlay sideways by however many days apart the two
 * periods are.
 */
export function comparisonSeries(response: StatsResponse | null, metricIndex = 0): (number | null)[] {
	const labels = response?.meta.time_labels ?? [];
	const values = new Map<string, number | null>();

	for (const row of response?.results ?? []) {
		const key = row.dimensions[0];
		if (key === undefined) continue;

		const earlier = row.comparison?.metrics[metricIndex];
		values.set(key, earlier === undefined ? null : earlier);
	}

	return labels.map((label) => values.get(label) ?? null);
}

/**
 * previousBucketLabel names the earlier bucket a position is compared against.
 *
 * It is derived here rather than returned by the engine because the response
 * carries the comparison window's bounds and the match is positional, so the
 * label is arithmetic on those bounds rather than data. The arithmetic is done
 * on the calendar fields with no timezone attached, exactly as the engine steps
 * its own buckets: a day is one calendar day long, not twenty-four hours, and
 * stepping in hours puts every bucket after a daylight saving change on the
 * wrong date.
 *
 * Hour and minute buckets return the empty string. Their wall clock repeats an
 * hour twice a year, so a label stepped from the window's start would be an hour
 * out for half a day — and a comparison tooltip that is confidently wrong is
 * worse than one that simply shows the number.
 */
export function previousBucketLabel(bounds: string[] | undefined, interval: Interval, index: number): string {
	const start = bounds?.[0];
	if (!start || index < 0) return "";

	const [year, month, day] = start.slice(0, 10).split("-").map(Number);
	if (!year || !month || !day) return "";

	switch (interval) {
		case "day":
			return isoDate(year, month, day + index);

		case "week":
			return isoDate(year, month, day + index * 7);

		case "month": {
			const total = (year * 12 + month - 1) + index;

			return `${pad(Math.floor(total / 12), 4)}-${pad((total % 12) + 1)}`;
		}

		default:
			return "";
	}
}

/** isoDate normalises a possibly out-of-range day into a calendar date. Date.UTC
 *  is used rather than a local Date because UTC has no daylight saving, so the
 *  arithmetic is pure calendar counting and cannot shift a date by an hour. */
function isoDate(year: number, month: number, day: number): string {
	const at = new Date(Date.UTC(year, month - 1, day));

	return `${pad(at.getUTCFullYear(), 4)}-${pad(at.getUTCMonth() + 1)}-${pad(at.getUTCDate())}`;
}
