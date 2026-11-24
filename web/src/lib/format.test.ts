//
// format.test.ts
// Dashboard number and calendar formatting regressions.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { compact, daysLabel, rangeLabel } from "./format";

// The catalogue is read once per module instance, so every test in this file
// shares this one bootstrap.
globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			locale: "en",
			messages: {
				"dashboard.format.compact.thousand": "{value}k",
				"dashboard.format.compact.million": "{value}M",
				"dashboard.format.compact.billion": "{value}B",
				"dashboard.format.date_long": "{month} {day}, {year}",
				"dashboard.format.range": "{from} – {to}",
			},
		}),
	}),
} as unknown as Document;

test("compact counts keep one useful decimal above ten thousand", () => {
	assert.equal(compact(20_300), "20.3k");
	assert.equal(compact(22_400), "22.4k");
	assert.equal(compact(44_000), "44k");
});

// The bounds below are what the query API returned for each period on Nov 24,
// 2026 at 08:09 in America/Los_Angeles. The end bound is exclusive.
test("a custom range ending in the past ends on its own last day", () => {
	assert.equal(rangeLabel(["2026-09-19T00:00:00-07:00", "2026-10-17T00:00:00-07:00"]), "Sep 19, 2026 – Oct 16, 2026");
	assert.equal(rangeLabel(["2026-08-22T00:00:00-07:00", "2026-09-19T00:00:00-07:00"]), "Aug 22, 2026 – Sep 18, 2026");
});

test("presets that run to today end today, not tomorrow", () => {
	assert.equal(rangeLabel(["2026-11-18T00:00:00-08:00", "2026-11-25T00:00:00-08:00"]), "Nov 18, 2026 – Nov 24, 2026");
	assert.equal(rangeLabel(["2026-10-28T00:00:00-07:00", "2026-11-25T00:00:00-08:00"]), "Oct 28, 2026 – Nov 24, 2026");
	assert.equal(rangeLabel(["2026-11-01T00:00:00-07:00", "2026-12-01T00:00:00-08:00"]), "Nov 1, 2026 – Nov 30, 2026");
});

test("last month ends on the last day of that month", () => {
	assert.equal(rangeLabel(["2026-10-01T00:00:00-07:00", "2026-11-01T00:00:00-07:00"]), "Oct 1, 2026 – Oct 31, 2026");
	assert.equal(rangeLabel(["2026-09-01T00:00:00-07:00", "2026-10-01T00:00:00-07:00"]), "Sep 1, 2026 – Sep 30, 2026");
	assert.equal(rangeLabel(["2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"]), "Feb 1, 2026 – Feb 28, 2026");
	assert.equal(rangeLabel(["2025-12-01T00:00:00Z", "2026-01-01T00:00:00Z"]), "Dec 1, 2025 – Dec 31, 2025");
});

test("a comparison cut to the elapsed time keeps its partial last day", () => {
	assert.equal(rangeLabel(["2026-09-30T00:00:00-07:00", "2026-10-27T09:09:27-07:00"]), "Sep 30, 2026 – Oct 27, 2026");
	assert.equal(rangeLabel(["2026-11-11T00:00:00-08:00", "2026-11-17T08:09:28-08:00"]), "Nov 11, 2026 – Nov 17, 2026");
	assert.equal(rangeLabel(["2026-10-01T00:00:00-07:00", "2026-10-24T09:09:29-07:00"]), "Oct 1, 2026 – Oct 24, 2026");
});

test("a single day prints one date", () => {
	assert.equal(rangeLabel(["2026-11-24T00:00:00-08:00", "2026-11-25T00:00:00-08:00"]), "Nov 24, 2026");
	assert.equal(rangeLabel(["2026-11-23T00:00:00-08:00", "2026-11-23T08:09:29-08:00"]), "Nov 23, 2026");
	assert.equal(rangeLabel(["2026-10-16T00:00:00-07:00", "2026-10-17T00:00:00-07:00"]), "Oct 16, 2026");
	assert.equal(rangeLabel(["2026-10-15T00:00:00-07:00", "2026-10-16T00:00:00-07:00"]), "Oct 15, 2026");
});

test("a rolling 24 hours spans the two days it touches", () => {
	assert.equal(rangeLabel(["2026-11-23T08:09:29-08:00", "2026-11-24T08:09:30-08:00"]), "Nov 23, 2026 – Nov 24, 2026");
});

test("a window with no length is about its first day", () => {
	assert.equal(rangeLabel(["2026-11-24T00:00:00-08:00", "2026-11-24T00:00:00-08:00"]), "Nov 24, 2026");
});

test("an incomplete pair of bounds prints nothing", () => {
	assert.equal(rangeLabel(undefined), "");
	assert.equal(rangeLabel([]), "");
	assert.equal(rangeLabel(["2026-11-24T00:00:00-08:00"]), "");
	assert.equal(rangeLabel(["", "2026-11-25T00:00:00-08:00"]), "");
});

test("the label is the site's days in any reader's timezone", () => {
	const was = process.env.TZ;

	try {
		for (const zone of ["Pacific/Kiritimati", "Pacific/Pago_Pago", "UTC"]) {
			process.env.TZ = zone;

			assert.equal(rangeLabel(["2026-10-01T00:00:00-07:00", "2026-11-01T00:00:00-07:00"]), "Oct 1, 2026 – Oct 31, 2026", zone);
			assert.equal(rangeLabel(["2026-11-01T00:00:00+14:00", "2026-11-02T00:00:00+14:00"]), "Nov 1, 2026", zone);
			assert.equal(rangeLabel(["2026-03-01T00:00:00-11:00", "2026-03-02T00:00:00-11:00"]), "Mar 1, 2026", zone);
		}
	} finally {
		if (was === undefined) delete process.env.TZ;
		else process.env.TZ = was;
	}
});

test("inclusive days print as given", () => {
	assert.equal(daysLabel("2026-10-01", "2026-10-31"), "Oct 1, 2026 – Oct 31, 2026");
	assert.equal(daysLabel("2026-11-23", "2026-11-23"), "Nov 23, 2026");
	assert.equal(daysLabel("", "2026-11-23"), "");
});
