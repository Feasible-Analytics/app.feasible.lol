//
// specialgoals.test.ts
// Which filtered goal takes over the first behavior tab, and which does not.
//
// Created: 2026-09-11
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import type { Filter, Goal } from "../api/types";
import { SPECIAL_GOALS, filteredGoalID, specialGoal } from "./specialgoals";

/** automatic builds one of the goals we provision, so a test changes only the
 * field it is about. */
function automatic(overrides: Partial<Goal> = {}): Goal {
	return {
		id: 17,
		site_id: 3,
		kind: "event",
		display_name: "Outbound link clicks",
		event_name: "Outbound Link: Click",
		is_revenue: false,
		is_automatic: true,
		created_at: 0,
		...overrides,
	};
}

test("each automatic goal breaks down by what its own event carries", () => {
	const byEvent = new Map(SPECIAL_GOALS.map((entry) => [entry.event, entry]));

	// The link and the file are on the event as a property; the form and the
	// missing page are only ever the page the event fired on.
	assert.equal(byEvent.get("Outbound Link: Click")?.dimension, "event:props:url");
	assert.equal(byEvent.get("File Download")?.dimension, "event:props:url");
	assert.equal(byEvent.get("Form: Submission")?.dimension, "event:page");
	assert.equal(byEvent.get("404")?.dimension, "event:page");
	assert.equal(byEvent.size, SPECIAL_GOALS.length);
});

test("a goal is special only when we created it", () => {
	assert.equal(specialGoal(automatic())?.event, "Outbound Link: Click");

	// Somebody else's goal of the same name sends whatever properties they
	// chose, so it gets the ordinary goals table rather than a `url` column
	// that would be empty.
	assert.equal(specialGoal(automatic({ is_automatic: false })), undefined);
});

test("a goal matching pages is never special, whatever it is called", () => {
	assert.equal(
		specialGoal(automatic({ kind: "page", event_name: undefined, page_pattern: "/404" })),
		undefined,
	);
});

test("an unrecognised goal leaves the goals table alone", () => {
	assert.equal(specialGoal(automatic({ event_name: "Signup" })), undefined);
	assert.equal(specialGoal(undefined), undefined);
});

test("one goal filter names a goal, and anything else names none", () => {
	const goal = (values: string[]): Filter => ["is", "event:goal", values, { case_sensitive: true }];

	assert.equal(filteredGoalID([goal(["17"])]), "17");

	// Other filters travel alongside it and change nothing about which goal is
	// being read.
	assert.equal(filteredGoalID([["is", "visit:country", ["US"]], goal(["17"])]), "17");

	// Two goals at once is a question about neither of them in particular.
	assert.equal(filteredGoalID([goal(["17", "18"])]), "");
	assert.equal(filteredGoalID([goal(["17"]), goal(["18"])]), "");

	// Excluding a goal is not reading it.
	assert.equal(filteredGoalID([["is_not", "event:goal", ["17"]]]), "");

	assert.equal(filteredGoalID([]), "");
});
