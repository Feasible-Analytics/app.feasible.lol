//
// GoalsCard.test.ts
// Pure behavior contracts behind dashboard conversion interactions.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import type { Goal, JourneyAnchor } from "../api/types";
import { PanelFrame, anchorKey, behaviorCaveat, behaviorEnabled, filterAnchors, goalFilter, goalsPrompt, hiddenGoalsNote } from "./GoalsCard";

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
				"dashboard.goals.hidden": "Showing {shown} of {configured} goals. The rest had no conversions in this period.",
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
	for (const [tab, caveat] of [
		["goals", "Unique conversions count each visitor once."],
		["funnels", "Steps are measured against the first step."],
	] as const) {
		const [first, second, ...rest] = behaviorCaveat(tab, "2026-09-02T12:00:00Z");

		assert.equal(first, caveat);
		assert.deepEqual(rest, []);
		assert.match(
			second ?? "",
			/^Reporting starts \w+ \d+, \d+, when this configuration became measurable\.$/,
			"the date must be substituted, not left as its placeholder",
		);
	}
});

test("an unparseable reporting start date is shown as it arrived", () => {
	// A timestamp we cannot read is a server bug, and quoting it back is what
	// lets somebody report it. Hiding it behind a dash loses the evidence.
	const [, second] = behaviorCaveat("goals", "not-a-date");

	assert.equal(second, "Reporting starts not-a-date, when this configuration became measurable.");
});

/** goalRow is one line of a goals report, carrying only the two fields the
 * empty-state decision reads. */
function goalRow(conversions: number, automatic = false) {
	return { total_conversions: conversions, goal: configuredGoal({ is_automatic: automatic }) };
}

test("a period in which nothing converted says which kind of nothing it is", () => {
	// Nothing set up at all: send them to configure a goal.
	assert.equal(goalsPrompt([]), "unconfigured");

	// Only the goals we provisioned, none fired: they are armed, not missing.
	assert.equal(goalsPrompt([goalRow(0, true), goalRow(0, true)]), "automatic_only");

	// A goal of their own is configured and did not fire. Telling them to
	// configure a goal would read as though the one they made was lost.
	assert.equal(goalsPrompt([goalRow(0, true), goalRow(0)]), "none_converted");
});

test("one converted goal is enough to render the table", () => {
	assert.equal(goalsPrompt([goalRow(3)]), "rows");

	// A single conversion among a wall of zeroes is exactly the case the tab
	// hides rows for, so it must reach the table rather than an empty state.
	assert.equal(goalsPrompt([goalRow(0, true), goalRow(0), goalRow(1)]), "rows");
});

test("the hidden-goals note counts what the table is not showing", () => {
	assert.equal(
		hiddenGoalsNote(1, 9),
		"Showing 1 of 9 goals. The rest had no conversions in this period.",
	);
});

test("no note is written when every configured goal is on screen", () => {
	assert.equal(hiddenGoalsNote(9, 9), undefined);
});

test("the note sits in the footer beside the link, not above the table", () => {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			note: hiddenGoalsNote(1, 9),
			footer: createElement("a", { href: "/settings" }, "Manage goals \u2192"),
			children: createElement("table", null, "rows"),
		}),
	);

	const footer = markup.slice(markup.indexOf("<footer"));

	assert.match(footer, /Showing 1 of 9 goals/, "the note belongs in the footer");
	assert.doesNotMatch(markup.slice(0, markup.indexOf("<footer")), /Showing 1 of 9 goals/,
		"nothing may push the table down with the note");
	assert.ok(footer.indexOf("Showing 1 of 9") < footer.indexOf("Manage goals"),
		"the note is left of the link");
});

test("exactly one link to the settings page renders beside the note", () => {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			note: hiddenGoalsNote(1, 9),
			footer: createElement("a", { href: "/settings" }, "Manage goals \u2192"),
			children: createElement("table", null, "rows"),
		}),
	);

	assert.equal(markup.split('href="/settings"').length - 1, 1);
});

test("a footer with nothing hidden holds only the link", () => {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			note: hiddenGoalsNote(9, 9),
			footer: createElement("a", { href: "/settings" }, "Manage goals \u2192"),
			children: createElement("table", null, "rows"),
		}),
	);

	const footer = markup.slice(markup.indexOf("<footer"));

	assert.doesNotMatch(footer, /Showing/);
	assert.match(footer, /Manage goals/);
});

test("the note wraps rather than truncating on a narrow card", () => {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			note: hiddenGoalsNote(1, 9),
			footer: createElement("a", { href: "/settings" }, "Manage goals \u2192"),
			children: createElement("table", null, "rows"),
		}),
	);

	const footer = markup.slice(markup.indexOf("<footer"));

	assert.doesNotMatch(footer, /truncate|whitespace-nowrap|text-ellipsis/);
	assert.match(footer, /min-h-\[42px\]/, "the strip keeps its reserved height");
});

test("panels with nothing to note render the footer they always did", () => {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			footer: createElement("a", { href: "/settings" }, "Manage properties \u2192"),
			children: createElement("div", null, "values"),
		}),
	);

	assert.match(markup, /<footer[^>]*>.*Manage properties/s);
	assert.doesNotMatch(markup, /Showing/);
});
