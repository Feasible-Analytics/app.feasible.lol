//
// interval.test.ts
// Regression tests for which bucket widths a range offers.
//
// Created: 2026-09-10
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { IntervalPref } from "./interval";
import { INTERVALS, effectiveInterval, intervalChoices, nextInterval } from "./interval";

/** days is a resolved range that long, in the wire's own format. */
function days(count: number): string[] {
	const end = Date.parse("2026-09-10T00:00:00Z");

	return [new Date(end - count * 86_400_000).toISOString(), new Date(end).toISOString()];
}

test("a year is offered days, weeks and months but never hours", () => {
	// 365 days of hourly buckets is 8,760 points. Nobody can read it and it is
	// the slowest query the engine can be given, so it is not on the menu.
	assert.deepEqual(intervalChoices(days(365)), ["auto", "day", "week", "month"]);
});

test("a week is offered hours and days and nothing wider", () => {
	// One week bucket is not a graph, and a month bucket is less than one.
	assert.deepEqual(intervalChoices(days(7)), ["auto", "hour", "day"]);
});

test("a month of days is offered hours, days and weeks", () => {
	assert.deepEqual(intervalChoices(days(30)), ["auto", "hour", "day", "week"]);
});

test("a range with one sensible width offers no choice at all", () => {
	// A single day can only be drawn hourly. A menu of one row is a control
	// that changes nothing, so the caller is told there is nothing to show.
	assert.deepEqual(intervalChoices(days(1)), []);
});

test("an unknown range trusts what the reader already chose", () => {
	// The range is undefined for the moment between the first paint and the
	// first answer. Narrowing the offer there would throw away a stored
	// preference and refetch the graph for nothing.
	for (const range of [undefined, [], ["2026-09-10T00:00:00Z"], ["nonsense", "also nonsense"]]) {
		assert.deepEqual(intervalChoices(range), [...INTERVALS], JSON.stringify(range));
	}
});

test("a width this range cannot draw falls back to auto without being forgotten", () => {
	// Pick hourly on a week, move to a year: the request must not ask for 8,760
	// buckets. Going back to the week has to bring the choice back, which is
	// why the preference itself is left alone.
	const year = intervalChoices(days(365));
	const week = intervalChoices(days(7));

	assert.equal(effectiveInterval("hour", year), "auto");
	assert.equal(effectiveInterval("hour", week), "hour");
	assert.equal(effectiveInterval("month", year), "month");
});

test("the i key only ever lands on a width the menu is showing", () => {
	const choices = intervalChoices(days(365));

	let at: IntervalPref = "auto";
	const walked: IntervalPref[] = [];

	for (let step = 0; step < choices.length; step++) {
		at = nextInterval(at, choices);
		walked.push(at);
	}

	assert.deepEqual(walked, ["day", "week", "month", "auto"], "the cycle covers the offer and closes");

	// A stale preference joins the cycle where auto sits rather than jumping to
	// wherever the width it names would have been.
	assert.equal(nextInterval("hour", choices), "day");

	// A range with no choice to make has nowhere to step to.
	assert.equal(nextInterval("week", []), "auto");
});
