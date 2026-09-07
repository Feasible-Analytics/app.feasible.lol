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

import type { FunnelReport, FunnelReportStep, Goal, JourneyAnchor } from "../api/types";
import { FunnelChart, PanelFrame, anchorKey, behaviorCaveat, behaviorEnabled, blockHeight, filterAnchors, goalFilter, goalsFooter, goalsPrompt, hiddenGoalsNote } from "./GoalsCard";

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
				"dashboard.behavior.funnels.steps_one": "{count}-step funnel",
				"dashboard.behavior.funnels.steps_other": "{count}-step funnel",
				"dashboard.behavior.funnels.allows_between": "Other activity allowed between steps",
				"dashboard.behavior.funnels.consecutive_only": "Consecutive steps only",
				"dashboard.behavior.funnels.overall": "{rate} completed",
				"dashboard.behavior.funnels.continued": "{rate} continued",
				"dashboard.behavior.funnels.dropoff": "{count} dropped · {rate}",
				"dashboard.behavior.funnels.dropped": "{rate} dropped",
				"dashboard.behavior.funnels.nobody_here": "Nobody reached this step",
				"dashboard.behavior.funnels.completed_here": "Completed",
				"dashboard.behavior.funnels.visitors_one": "{count} visitor",
				"dashboard.behavior.funnels.visitors_other": "{count} visitors",
				"dashboard.behavior.funnels.not_to_scale": "Too small to draw.",
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

/** funnelStep supplies every wire field so each test below changes only the one
 * number it is about. */
function funnelStep(position: number, visitors: number, overrides: Partial<FunnelReportStep> = {}): FunnelReportStep {
	return {
		position,
		label: `Step ${position}`,
		goal: configuredGoal({ id: position }),
		visitors,
		visits: visitors,
		drop_off: 0,
		drop_off_rate: 0,
		conversion_rate: 0,
		...overrides,
	};
}

/** funnelMarkup draws the chart the way the panel does. */
function funnelMarkup(steps: FunnelReportStep[], strict = false): string {
	const report = {
		funnel: { id: 1, site_id: 9, name: "Checkout", strict_order: strict, steps: [] },
		steps,
	} as unknown as FunnelReport;

	return renderToStaticMarkup(createElement(FunnelChart, { report }));
}

/** columns returns the <li> markup of the wide layout, which is the first list
 * the chart renders. */
function columns(markup: string): string[] {
	const grid = markup.slice(markup.indexOf("<ol"), markup.indexOf("</ol>"));

	return grid.split("<li").slice(1);
}

test("a funnel renders one column per step, each carrying its own figures", () => {
	const markup = funnelMarkup([
		funnelStep(1, 314, { label: "Visit /", conversion_rate: 100 }),
		funnelStep(2, 27, { label: "Opened Signup", conversion_rate: 8.6, drop_off: 287, drop_off_rate: 91.4 }),
		funnelStep(3, 10, { label: "Newsletter Signup", conversion_rate: 3.2, drop_off: 17, drop_off_rate: 63 }),
		funnelStep(4, 2, { label: "Purchased Course", conversion_rate: 0.6, drop_off: 8, drop_off_rate: 80 }),
	]);

	const cells = columns(markup);

	assert.equal(cells.length, 4);

	for (const [index, want] of [
		["Visit /", "100%", "314 visitors"],
		["Opened Signup", "8.6%", "27 visitors"],
		["Newsletter Signup", "3.2%", "10 visitors"],
		["Purchased Course", "0.6%", "2 visitors"],
	].entries()) {
		for (const text of want) {
			assert.ok(cells[index].includes(text), `column ${index + 1} is missing ${text}: ${cells[index]}`);
		}
	}
});

test("a step too small to draw is held at a minimum height and marked", () => {
	// Drawn to scale this is under two pixels of the chart, which reads as an
	// empty box rather than as a number.
	assert.deepEqual(blockHeight(1, 1000), { height: 4, toScale: false });

	// Above the floor nothing is touched.
	assert.deepEqual(blockHeight(500, 1000), { height: 50, toScale: true });

	const markup = funnelMarkup([
		funnelStep(1, 1000, { conversion_rate: 100 }),
		funnelStep(2, 1, { conversion_rate: 0.1, drop_off: 999, drop_off_rate: 99.9 }),
	]);

	const [tall, tiny] = columns(markup);

	assert.ok(tall.includes('data-scale="true"'), tall);
	assert.ok(tiny.includes('data-scale="broken"'), tiny);
	assert.ok(tiny.includes("Too small to draw."), "the floored block must say it is not to scale");
});

test("a step nobody reached is a column showing zero, not a gap", () => {
	const markup = funnelMarkup([
		funnelStep(1, 40, { conversion_rate: 100 }),
		funnelStep(2, 0, { conversion_rate: 0, drop_off: 40, drop_off_rate: 100 }),
	]);

	const cells = columns(markup);

	assert.equal(cells.length, 2);
	assert.ok(cells[1].includes("0 visitors"), cells[1]);
	assert.ok(cells[1].includes("0%"), cells[1]);

	// "100% continued" out of nothing is arithmetic, not a fact about anybody.
	assert.ok(!cells[1].includes("continued"), cells[1]);

	// Nothing is not "too small to see" — a step nobody reached draws no block.
	assert.deepEqual(blockHeight(0, 40), { height: 0, toScale: true });
});

test("eight steps render eight columns and no label is emptied", () => {
	const steps = Array.from({ length: 8 }, (_, i) =>
		funnelStep(i + 1, 100 - i * 10, { label: `Step number ${i + 1}`, conversion_rate: 100 - i * 10 }),
	);

	const cells = columns(funnelMarkup(steps));

	assert.equal(cells.length, 8);

	for (const [index, cell] of cells.entries()) {
		assert.ok(cell.includes(`Step number ${index + 1}`), cell);
	}
});

test("the last column says the funnel completed rather than a continue rate", () => {
	const cells = columns(
		funnelMarkup([
			funnelStep(1, 100, { conversion_rate: 100 }),
			funnelStep(2, 25, { conversion_rate: 25, drop_off: 75, drop_off_rate: 75 }),
		]),
	);

	assert.ok(cells[0].includes("25% continued"), cells[0]);
	assert.ok(cells[0].includes("75% dropped"), cells[0]);
	assert.ok(!cells[1].includes("continued"), cells[1]);
	assert.ok(cells[1].includes("Completed"), cells[1]);
});

test("the chart is an ordered list with one item per step", () => {
	const markup = funnelMarkup([
		funnelStep(1, 10, { conversion_rate: 100 }),
		funnelStep(2, 5, { conversion_rate: 50, drop_off: 5, drop_off_rate: 50 }),
	]);

	// Two lists: the columns, and the rows a narrow screen gets instead. Only
	// one is in the layout at a time, so neither is read twice.
	assert.equal(markup.split("<ol").length - 1, 2);
	assert.equal(markup.split("<li").length - 1, 4);
	assert.ok(markup.includes("sm:hidden"), "the narrow-screen rows are missing");
	assert.ok(markup.includes("hidden gap-2 sm:grid"), "the wide-screen columns are missing");
});

test("the funnel header names the step count and the matching mode", () => {
	const steps = [funnelStep(1, 10, { conversion_rate: 100 }), funnelStep(2, 1, { conversion_rate: 10 })];

	const loose = funnelMarkup(steps);
	assert.ok(loose.includes("2-step funnel"), loose.slice(0, 400));
	assert.ok(loose.includes("Other activity allowed between steps"), loose.slice(0, 400));
	assert.ok(loose.includes("10% completed"), loose.slice(0, 400));

	assert.ok(funnelMarkup(steps, true).includes("Consecutive steps only"));
});
