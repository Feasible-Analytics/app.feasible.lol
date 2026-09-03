//
// period.test.ts
// Stepping a period is calendar arithmetic, and calendar arithmetic is where
// off-by-one-day bugs live.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { PERIODS, addDays, addMonths, canStep, dayCount, lastDayOfMonth, step, windowOf, yesterday } from "./period";

const TODAY = "2026-08-30";

test("a fixed-day preset is the window ending today", () => {
	assert.deepEqual(windowOf("day", "", "", TODAY), { from: "2026-08-30", to: "2026-08-30" });
	assert.deepEqual(windowOf("7d", "", "", TODAY), { from: "2026-08-24", to: "2026-08-30" });
	assert.deepEqual(windowOf("28d", "", "", TODAY), { from: "2026-08-03", to: "2026-08-30" });
});

test("a calendar preset is whole months", () => {
	assert.deepEqual(windowOf("month", "", "", TODAY), { from: "2026-08-01", to: "2026-08-31" });
	assert.deepEqual(windowOf("last_month", "", "", TODAY), { from: "2026-07-01", to: "2026-07-31" });
	assert.deepEqual(windowOf("year", "", "", TODAY), { from: "2026-01-01", to: "2026-12-31" });
});

test("a period with nothing before it does not step", () => {
	// All time has no earlier period, and the two live windows are about now by
	// definition — silently turning one into a fixed half hour in the past is
	// not what an arrow key should do.
	for (const preset of ["all", "realtime", "5m", "24h"] as const) {
		assert.equal(windowOf(preset, "", "", TODAY), null, preset);
		assert.equal(step(preset, "", "", TODAY, -1), null, preset);
	}
});

test("a day-spanned period steps by its own length", () => {
	assert.deepEqual(step("7d", "", "", TODAY, -1), { from: "2026-08-17", to: "2026-08-23" });
	assert.deepEqual(step("7d", "", "", TODAY, 1), { from: "2026-08-31", to: "2026-09-06" });
});

test("the steps either side of a period do not overlap or leave a gap", () => {
	const back = step("28d", "", "", TODAY, -1);

	assert.ok(back);
	assert.equal(addDays(back.to, 1), "2026-08-03");
	assert.equal(dayCount(back.from, back.to), 28);
});

test("a month-spanned period lands on whole months, not on thirty-day blocks", () => {
	assert.deepEqual(step("month", "", "", TODAY, -1), { from: "2026-07-01", to: "2026-07-31" });
	assert.deepEqual(step("month", "", "", "2026-03-15", -1), { from: "2026-02-01", to: "2026-02-28" });
	assert.deepEqual(step("year", "", "", TODAY, -1), { from: "2025-01-01", to: "2025-12-31" });
});

test("a custom range steps by its own number of days", () => {
	assert.deepEqual(step("28d", "2026-08-10", "2026-08-12", TODAY, -1), { from: "2026-08-07", to: "2026-08-09" });
	assert.deepEqual(step("28d", "2026-08-10", "2026-08-12", TODAY, 1), { from: "2026-08-13", to: "2026-08-15" });
});

test("stepping crosses a year boundary", () => {
	assert.deepEqual(step("day", "2026-01-01", "2026-01-01", TODAY, -1), { from: "2025-12-31", to: "2025-12-31" });
});

test("a month step clamps rather than spilling into the next month", () => {
	// 31 January minus a month is 28 February, not 3 March.
	assert.equal(addMonths("2026-01-31", 1), "2026-02-28");
	assert.equal(addMonths("2028-01-31", 1), "2028-02-29");
});

test("the last day of a month knows about leap years", () => {
	assert.equal(lastDayOfMonth("2026-02-10"), "2026-02-28");
	assert.equal(lastDayOfMonth("2028-02-10"), "2028-02-29");
});

test("the arrows refuse the periods that have nowhere to go", () => {
	// All time has nothing before it, and the live views are about now by
	// definition. An arrow that appears to work and does nothing is worse than
	// one that says it cannot.
	for (const preset of ["all", "realtime", "5m", "24h"] as const) {
		for (const direction of [-1, 1] as const) {
			assert.equal(
				canStep(preset, "", "", "2026-09-03", direction),
				false,
				`${preset} claimed it could step ${direction}`,
			);
		}
	}
});

test("the forward arrow refuses to walk into the future", () => {
	// A window already ending today has nothing ahead of it but an empty graph.
	assert.equal(canStep("28d", "", "", "2026-09-03", 1), false);
	assert.equal(canStep("day", "", "", "2026-09-03", 1), false);

	// Backwards from the same window is always fine.
	assert.equal(canStep("28d", "", "", "2026-09-03", -1), true);
	assert.equal(canStep("day", "", "", "2026-09-03", -1), true);

	// A window wholly in the past moves both ways.
	assert.equal(canStep("28d", "2026-01-01", "2026-01-28", "2026-09-03", 1), true);
	assert.equal(canStep("28d", "2026-01-01", "2026-01-28", "2026-09-03", -1), true);
});

test("a step that would end exactly today is allowed", () => {
	// The boundary case: seven days ending on the 27th steps forward to a
	// window ending on the 3rd, which is today and therefore real data.
	assert.equal(canStep("7d", "2026-08-21", "2026-08-27", "2026-09-03", 1), true);
});

test("yesterday is one day, computed once", () => {
	assert.deepEqual(yesterday("2026-09-03"), { from: "2026-09-02", to: "2026-09-02" });

	// Across a month boundary, which is where two implementations drift.
	assert.deepEqual(yesterday("2026-03-01"), { from: "2026-02-28", to: "2026-02-28" });
	assert.deepEqual(yesterday("2024-03-01"), { from: "2024-02-29", to: "2024-02-29" });
});

test("every period has a key, and no key is used twice", () => {
	// The menu prints these keys and the keyboard handler reads them from the
	// same table. A duplicate would be a key that jumps to whichever row was
	// written first, with the menu advertising the other.
	const keys = PERIODS.map((period) => period.key);

	assert.equal(new Set(keys).size, keys.length, `duplicate period key in ${keys.join(",")}`);

	for (const period of PERIODS) {
		assert.match(period.key, /^[a-z]$/, `${period.id} has the key ${JSON.stringify(period.key)}`);
		assert.ok(period.labelId.startsWith("dashboard."), `${period.id} has no catalogue label`);
	}
});
