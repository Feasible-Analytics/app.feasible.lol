//
// SearchConsoleCard.test.ts
// The number formats and the three caveats the search card is built on.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import type { SearchReport } from "../api/types";

// The catalogue is read once from the page, so it is stubbed before any test
// asks for a string. It is the real English file rather than a hand-written
// copy: a stub with its own strings drifts out of step with the catalogue the
// card actually reads.
const messages = JSON.parse(
	readFileSync(new URL("../../../internal/i18n/locales/en/dashboard.json", import.meta.url), "utf8"),
) as Record<string, string>;

globalThis.document = {
	getElementById: () => ({ textContent: JSON.stringify({ locale: "en", messages }) }),
} as unknown as Document;

const { formatCTR, formatPosition, headingId, rowLabel, searchCaveat, tabLabelId, totalsNote } = await import("./SearchConsoleCard");

/** report builds a whole answer so each test changes only the field it means to. */
function report(overrides: Partial<SearchReport> = {}): SearchReport {
	return {
		status: "ready",
		dimension: "query",
		rows: [],
		totals: { value: "", clicks: 120, impressions: 4800, ctr: 0.025, position: 12.34 },
		...overrides,
	};
}

test("a click-through rate keeps the decimal that separates two keywords", () => {
	// Whole percentages round 2.5% and 3.4% to the same number, and those are
	// exactly the two keywords somebody is trying to tell apart.
	assert.equal(formatCTR(0.025), "2.5%");
	assert.equal(formatCTR(0.034), "3.4%");
	assert.equal(formatCTR(0), "0.0%");
	assert.equal(formatCTR(1), "100.0%");
});

test("a row with no rank shows a dash rather than inventing first place", () => {
	assert.equal(formatPosition(0), "—");
	assert.equal(formatPosition(-1), "—");
	assert.equal(formatPosition(12.34), "12.3");
	assert.equal(formatPosition(1), "1.0");
});

test("the totals strip is withheld when there is nothing to total", () => {
	assert.equal(totalsNote(report({ totals: { value: "", clicks: 0, impressions: 0, ctr: 0, position: 0 } })), undefined);

	const note = totalsNote(report());

	assert.ok(note?.includes("2.5%"), `the rate is missing from ${note}`);
	assert.ok(note?.includes("12.3"), `the position is missing from ${note}`);
});

test("the caveats always cover the three numbers that look like bugs", () => {
	const notes = searchCaveat(report());

	// The delay, the totals that cannot match, and the filters that do not
	// apply. Each one is a number a reader would otherwise report as broken.
	assert.equal(notes.length, 3);
	assert.ok(notes.some((note) => note.includes("behind")), "nothing explains the empty recent days");
	assert.ok(notes.some((note) => note.includes("hides")), "nothing explains rows that do not add up");
	assert.ok(notes.some((note) => note.includes("filters")), "nothing explains that the dashboard filters are ignored");
});

test("the caveats name the last published day and the property when we know them", () => {
	const notes = searchCaveat(report({ updated_through: "2026-09-05", property: "sc-domain:example.com" }));

	assert.equal(notes.length, 5);
	assert.ok(notes[0]?.includes("2026"), `the published-through date is missing from ${notes[0]}`);
	assert.ok(notes.some((note) => note.includes("sc-domain:example.com")), "the property is not named");
});

test("a caveat list survives having no report yet", () => {
	assert.equal(searchCaveat(null).length, 3);
});

test("a country row reads the same as it does on the locations card", () => {
	// The value on the wire is the alpha-2 code the Go side converted Google's
	// three-letter one into, so the browser's own region names resolve it.
	assert.equal(rowLabel("country", "US"), "United States");
	assert.equal(rowLabel("query", "buy widgets"), "buy widgets");

	// Search Console's own placeholder for traffic it cannot place has no name,
	// so the row keeps the code rather than going blank.
	assert.equal(rowLabel("country", "zzz"), "zzz");
});

test("every grouping names its own column and its own tab", () => {
	for (const dimension of ["query", "page", "country", "device"] as const) {
		assert.ok(messages[headingId(dimension)], `no column heading for ${dimension}`);
		assert.ok(messages[tabLabelId(dimension)], `no tab label for ${dimension}`);
	}
});
