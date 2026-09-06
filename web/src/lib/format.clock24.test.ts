//
// format.clock24.test.ts
// The 24-hour dial: the clock every reader had before the preference existed.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The dial has one test file per cycle — this one and format.clock12.test.ts —
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
			hour_cycle: "24",
			messages: {
				"dashboard.format.time": "{hour}:{minute}",
				"dashboard.format.hour": "{hour}:00",
				"dashboard.format.date_time": "{date}, {hour}:{minute}",
				"dashboard.format.date_hour": "{date}, {hour}:00",
				"dashboard.format.date_long": "{month} {day}, {year}",
			},
		}),
	}),
} as unknown as Document;

// The point of these is that adding a preference changed nothing for the reader
// who never touches it. Every one of these strings is what the graph printed
// before the setting existed.
test("the axis is unchanged for a 24-hour reader", () => {
	assert.equal(bucketShort("2026-09-04 00:00", "hour"), "0:00");
	assert.equal(bucketShort("2026-09-04 12:00", "hour"), "12:00");
	assert.equal(bucketShort("2026-09-04 13:00", "hour"), "13:00");
	assert.equal(bucketShort("2026-09-04 14:35", "minute"), "14:35");
	assert.equal(bucketShort("2026-09-04 00:05", "minute"), "0:05");
});

test("the tooltip is unchanged for a 24-hour reader", () => {
	assert.equal(bucketLong("2026-09-04 13:00", "hour"), "Sep 4, 2026, 13:00");
	assert.equal(bucketLong("2026-09-04 14:35", "minute"), "Sep 4, 2026, 14:35");
	assert.equal(bucketLong("2026-09-04 00:00", "hour"), "Sep 4, 2026, 0:00");
});

// No meridiem may leak onto a 24-hour clock, whichever bucket it is.
test("no meridiem appears anywhere on a 24-hour dial", () => {
	for (const label of ["2026-09-04 00:00", "2026-09-04 12:00", "2026-09-04 23:59"]) {
		for (const interval of ["hour", "minute", "day"] as const) {
			assert.doesNotMatch(bucketShort(label, interval), /\b(AM|PM)\b/);
			assert.doesNotMatch(bucketLong(label, interval), /\b(AM|PM)\b/);
		}
	}
});
