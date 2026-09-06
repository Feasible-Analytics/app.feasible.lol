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
import { PanelFrame, anchorKey, behaviorCaveat, behaviorEnabled, filterAnchors, goalFilter, goalsFooter, goalsPrompt, hiddenGoalsNote } from "./GoalsCard";

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

// The footer strip a Goals card carries, and the frame that draws it. The note
// is the only thing telling a reader that rows are missing, so both what is
// decided and what is drawn are pinned.

const HIDDEN = "Showing 1 of 9 goals. The rest had no conversions in this period.";

/** footerMarkup draws PanelFrame the way GoalsPanel does, so the assertions
 * below are about the strip and not about React. */
function footerMarkup(strip: { note?: string; manageURL?: string }, label = "Manage goals"): string {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			note: strip.note,
			footer: strip.manageURL ? createElement("a", { href: strip.manageURL, className: "shrink-0" }, `${label} \u2192`) : undefined,
			children: createElement("table", null, "rows"),
		}),
	);

	const at = markup.indexOf("<footer");

	return at < 0 ? "" : markup.slice(at);
}

test("the hidden-goals note counts what the table is not showing", () => {
	assert.equal(hiddenGoalsNote(1, 9), HIDDEN);
});

test("no note is written when every configured goal is on screen", () => {
	assert.equal(hiddenGoalsNote(9, 9), undefined);
});

test("a reader who cannot manage goals is still told that rows are missing", () => {
	const strip = goalsFooter("rows", 1, 9, undefined);

	assert.equal(strip.note, HIDDEN);
	assert.equal(strip.manageURL, undefined);
	assert.match(footerMarkup(strip), /Showing 1 of 9 goals/,
		"a public dashboard or a viewer has no settings link, and must not lose the note with it");
});

test("the empty states carry no footer at all", () => {
	assert.deepEqual(goalsFooter("unconfigured", 0, 0, "/settings"), {});
	assert.deepEqual(goalsFooter("none_converted", 0, 9, "/settings"), {});
	assert.equal(footerMarkup(goalsFooter("none_converted", 0, 9, "/settings")), "");
});

test("the note sits in the footer, left of the link", () => {
	const footer = footerMarkup(goalsFooter("rows", 1, 9, "/settings"));

	assert.match(footer, /Showing 1 of 9 goals/);
	assert.ok(footer.indexOf("Showing 1 of 9") < footer.indexOf("Manage goals"), "the note comes first");
	assert.match(footer, /class="mr-auto[^"]*"[^>]*>Showing 1 of 9/,
		"the note carries the auto margin — it is what pushes the link right, and what keeps a lone link left");
});

test("nothing but the footer link renders when no goal is hidden", () => {
	const footer = footerMarkup(goalsFooter("rows", 9, 9, "/settings"));

	assert.doesNotMatch(footer, /Showing/);
	assert.doesNotMatch(footer, /mr-auto|justify-between/,
		"a footer holding only a link must be laid out exactly as it was before the note existed");
	assert.match(footer, /Manage goals/);
});

test("the properties and funnels footers are byte-identical to the goals one without a note", () => {
	const properties = footerMarkup({ manageURL: "/settings" }, "Manage properties");
	const goals = footerMarkup(goalsFooter("rows", 9, 9, "/settings"), "Manage properties");

	assert.equal(properties, goals);
	assert.equal(
		properties,
		'<footer class="flex min-h-[42px] shrink-0 items-center gap-4 border-t border-line px-4 py-1.5 sm:px-5">'
			+ '<a href="/settings" class="shrink-0">Manage properties \u2192</a></footer></div>',
	);
});

test("exactly one link to the settings page renders in the rows state", () => {
	const markup = renderToStaticMarkup(
		createElement(PanelFrame, {
			note: goalsFooter("rows", 1, 9, "/settings").note,
			footer: createElement("a", { href: "/settings" }, "Manage goals \u2192"),
			children: createElement("table", null, "rows"),
		}),
	);

	assert.equal(markup.split('href="/settings"').length - 1, 1);
});

test("the note wraps rather than truncating on a narrow card", () => {
	const footer = footerMarkup(goalsFooter("rows", 1, 9, "/settings"));

	assert.doesNotMatch(footer, /truncate|whitespace-nowrap|text-ellipsis/);
	assert.match(footer, /min-h-\[42px\]/, "the strip keeps its reserved height and grows to fit");
	assert.match(footer, /class="shrink-0"/, "the link does not get squeezed by a long note");
});
