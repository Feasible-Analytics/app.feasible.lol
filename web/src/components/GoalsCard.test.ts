//
// GoalsCard.test.ts
// Pure behavior contracts behind dashboard conversion interactions.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import type { Goal, JourneyAnchor } from "../api/types";
import { anchorKey, conversionSettingsURL, extendJourneyTrail, goalFilter } from "./GoalsCard";

/** configuredGoal supplies all wire fields so each test changes only the goal
 * behavior it intends to exercise. */
function configuredGoal(overrides: Partial<Goal> = {}): Goal {
	return {
		id: 42,
		site_id: 9,
		kind: "event",
		display_name: "Paid signup",
		event_name: "Signup",
		is_revenue: false,
		is_automatic: false,
		created_at: 0,
		...overrides,
	};
}

test("goal rows filter through the exact goal definition", () => {
	assert.deepEqual(goalFilter(configuredGoal()), {
		operator: "is",
		dimension: "event:goal",
		values: ["42"],
	});

	assert.deepEqual(goalFilter(configuredGoal({ kind: "scroll", event_name: undefined, scroll_depth: 75 })), {
		operator: "is",
		dimension: "event:goal",
		values: ["42"],
	});
});

test("conversion settings links tolerate trailing slashes and missing navigation", () => {
	assert.equal(conversionSettingsURL("/sites/example.com/settings/"), "/sites/example.com/settings/conversions");
	assert.equal(conversionSettingsURL("/sites/example.com/settings"), "/sites/example.com/settings/conversions");
	assert.equal(conversionSettingsURL(undefined), undefined);
});

test("journey continuation keeps typed anchors and does not mutate its trail", () => {
	const page: JourneyAnchor = { type: "page", value: "/pricing", label: "Pricing" };
	const event: JourneyAnchor = { type: "event", value: "Signup" };
	const trail = [page];
	const next = extendJourneyTrail(trail, event);

	assert.deepEqual(trail, [page]);
	assert.deepEqual(next, [page, event]);
	assert.equal(anchorKey(page), "page:/pricing");
	assert.equal(anchorKey({ type: "goal", value: "7" }), "goal:7");
});
