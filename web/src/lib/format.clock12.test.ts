//
// format.clock12.test.ts
// The 12-hour dial, including the two boundaries everybody gets wrong.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The dial has one test file per cycle — this one and format.clock24.test.ts —
// because the catalogue is read once per module instance and cached. The runner
// builds and runs each test file as its own bundle, so a file is the only unit
// that can pin one bootstrap; two dials in one file would both render on
// whichever one the first test happened to install.

import assert from "node:assert/strict";
import { test } from "node:test";

import { bucketLong, bucketShort } from "./format";

globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			locale: "en",
			hour_cycle: "12",
			messages: {
				"dashboard.format.time_12": "{hour}:{minute} {meridiem}",
				"dashboard.format.hour_12": "{hour} {meridiem}",
				"dashboard.format.date_time_12": "{date}, {hour}:{minute} {meridiem}",
				"dashboard.format.date_hour_12": "{date}, {hour} {meridiem}",
				"dashboard.format.date_long": "{month} {day}, {year}",
				"dashboard.format.meridiem_am": "AM",
				"dashboard.format.meridiem_pm": "PM",
			},
		}),
	}),
} as unknown as Document;

// Midnight and noon are the whole reason this has a test. `h % 12` on its own
// prints them as "0 AM" and "0 PM", which look plausible enough to ship and are
// wrong on every graph a 12-hour reader opens.
test("the axis renders hourly buckets on a 12-hour dial", () => {
	assert.equal(bucketShort("2026-09-04 00:00", "hour"), "12 AM");
	assert.equal(bucketShort("2026-09-04 12:00", "hour"), "12 PM");
	assert.equal(bucketShort("2026-09-04 13:00", "hour"), "1 PM");
	assert.equal(bucketShort("2026-09-04 14:00", "hour"), "2 PM");
	assert.equal(bucketShort("2026-09-04 23:00", "hour"), "11 PM");
});

test("the axis renders minute buckets on a 12-hour dial", () => {
	assert.equal(bucketShort("2026-09-04 00:05", "minute"), "12:05 AM");
	assert.equal(bucketShort("2026-09-04 12:05", "minute"), "12:05 PM");
	assert.equal(bucketShort("2026-09-04 14:35", "minute"), "2:35 PM");

	// The minute keeps its leading zero while the hour loses one. A clock that
	// reads "2:5 PM" is the other half of this arithmetic going wrong.
	assert.equal(bucketShort("2026-09-04 23:09", "minute"), "11:09 PM");
});

test("the tooltip keeps the date and adds the meridiem", () => {
	assert.equal(bucketLong("2026-09-04 14:00", "hour"), "Sep 4, 2026, 2 PM");
	assert.equal(bucketLong("2026-09-04 14:35", "minute"), "Sep 4, 2026, 2:35 PM");
	assert.equal(bucketLong("2026-09-04 00:00", "hour"), "Sep 4, 2026, 12 AM");
	assert.equal(bucketLong("2026-09-04 12:00", "hour"), "Sep 4, 2026, 12 PM");
});

// A bucket with no clock component must not grow a meridiem: a day, a week and
// a month are dates, and "Sep 4 AM" is nonsense.
test("date-only buckets are untouched by the dial", () => {
	assert.equal(bucketLong("2026-09-04 00:00", "day"), "Sep 4, 2026");
});
