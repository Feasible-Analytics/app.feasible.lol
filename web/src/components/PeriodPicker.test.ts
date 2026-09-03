//
// PeriodPicker.test.ts
// The menu's tick, and the agreement between the period table and the overlay.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { PERIODS, addDays, today, yesterday } from "../lib/period";
import type { UrlState } from "../lib/url";
import { isCurrent } from "./PeriodPicker";
import { ACTION_KEYS } from "./Shortcuts";

/** at builds the smallest URL state the marking rule reads. */
function at(preset: string, from = "", to = ""): UrlState {
	return { preset, from, to } as UrlState;
}

/** row finds a period by id, so a renamed id fails loudly here rather than
 *  quietly matching nothing. */
function row(id: string) {
	const period = PERIODS.find((entry) => entry.id === id);

	assert.ok(period, `no period with the id ${id}`);

	return period;
}

test("a preset marks its own row and no other", () => {
	assert.equal(isCurrent(row("28d"), at("28d")), true);
	assert.equal(isCurrent(row("7d"), at("28d")), false);
	assert.equal(isCurrent(row("custom"), at("28d")), false);
});

test("exactly yesterday marks Yesterday, and any other pair of dates marks Custom range", () => {
	// Yesterday and a custom range are the same shape in the URL — two dates,
	// no preset — so the only thing separating them is which two dates.
	const day = yesterday(today());

	assert.equal(isCurrent(row("yesterday"), at("28d", day.from, day.to)), true);
	assert.equal(isCurrent(row("custom"), at("28d", day.from, day.to)), false);

	const older = addDays(today(), -30);

	assert.equal(isCurrent(row("custom"), at("28d", older, older)), true);
	assert.equal(isCurrent(row("yesterday"), at("28d", older, older)), false);

	// A range with dates never marks the preset row its URL still carries.
	assert.equal(isCurrent(row("28d"), at("28d", older, older)), false);
});

test("no period claims a key the handler has already spent", () => {
	// The `?` overlay prints both tables side by side. The handler answers the
	// actions first, so a period keyed `x` would be advertised and dead — and
	// nothing would say so, because each table is valid on its own.
	const reserved = new Set(ACTION_KEYS.flatMap((action) => action.reserves));

	for (const period of PERIODS) {
		assert.equal(reserved.has(period.key), false, `${period.id} claims the reserved key ${period.key}`);
	}
});

test("every row the overlay prints has a label to print", () => {
	for (const action of ACTION_KEYS) {
		assert.ok(action.key.length > 0, `${action.labelId} has no key to show`);
		assert.ok(action.labelId.startsWith("dashboard."), `${action.key} has no catalogue label`);
		assert.ok(action.reserves.length > 0, `${action.labelId} reserves nothing`);
	}
});
