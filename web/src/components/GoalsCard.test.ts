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
import { anchorKey, behaviorCaveat, behaviorEnabled, filterAnchors, goalFilter } from "./GoalsCard";

// The catalogue is read once from the page, so it is stubbed before any test
// asks for a string rather than inside the one test that needs it.
globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			locale: "en",
			messages: {
				"dashboard.behavior.goals.caveat": "Unique conversions count each visitor once.",
				"dashboard.behavior.funnels.caveat": "Steps are measured against the first step.",
				"dashboard.behavior.partial": "Reporting starts {from}, when this configuration became measurable.",
			},
		}),
	}),
} as unknown as Document;

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

test("anchor keys cannot collide across pages, events and goals", () => {
	assert.equal(anchorKey({ type: "page", value: "/pricing", label: "Pricing" }), "page:/pricing");
	assert.equal(anchorKey({ type: "event", value: "7" }), "event:7");
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

test("the help bubble carries only the tab caveat when a report is complete", () => {
	assert.deepEqual(behaviorCaveat("goals"), ["Unique conversions count each visitor once."]);
	assert.deepEqual(behaviorCaveat("funnels"), ["Steps are measured against the first step."]);
});

test("a partial report appends its reporting start date as a second paragraph", () => {
	const paragraphs = behaviorCaveat("goals", "2026-09-02T00:00:00Z");

	assert.equal(paragraphs.length, 2);
	assert.equal(paragraphs[0], "Unique conversions count each visitor once.");
	assert.match(paragraphs[1], /^Reporting starts .+, when this configuration became measurable\.$/);
	assert.ok(paragraphs[1].includes("2026"), "the date must be substituted, not left as its placeholder");
});
