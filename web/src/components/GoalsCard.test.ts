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
import { anchorKey, behaviorEnabled, extendJourneyTrail, filterAnchors, goalFilter } from "./GoalsCard";

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

test("journey anchor search is case-insensitive and preserves server order", () => {
	const anchors: JourneyAnchor[] = [
		{ type: "page", value: "/pricing", label: "Pricing" },
		{ type: "event", value: "Signup", label: "Newsletter signup" },
		{ type: "goal", value: "7", label: "Paid Signup" },
	];

	assert.deepEqual(filterAnchors(anchors, "SIGNUP"), [anchors[1], anchors[2]]);
	assert.equal(filterAnchors(anchors, "  "), anchors);
});

test("deep-linked behavior tabs load before the lazy card reaches the viewport", () => {
	assert.equal(behaviorEnabled("goals", false), false);
	assert.equal(behaviorEnabled("properties", false), true);
	assert.equal(behaviorEnabled("funnels", false), true);
	assert.equal(behaviorEnabled("explore", false), true);
	assert.equal(behaviorEnabled("goals", true), true);
});
