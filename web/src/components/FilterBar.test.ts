//
// FilterBar.test.ts
// The value picker's search contract.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import { matchingRows, searchesLocally, suggestionsRequest, suggestionsSettled } from "./FilterBar";

/** row is one line of a suggestions answer, carrying only the value. */
function row(value: string) {
	return { dimensions: [value] };
}

/** country is the label renderer for the one dimension whose stored code and
 * rendered name share no letters, which is what makes a server search useless. */
function country(code: string): string {
	return { US: "United States", CA: "Canada", GB: "United Kingdom" }[code] ?? code;
}

test("an empty search asks for the busiest values and nothing else", () => {
	const body = suggestionsRequest("visit:entry_page", "28d", "");

	assert.deepEqual(body.dimensions, ["visit:entry_page"]);
	assert.equal(body.filters, undefined, "an unfiltered request must not carry an empty filter");
});

test("typing narrows the same dimension it is breaking down", () => {
	// The dimension appears twice on purpose: the filter has to name the same
	// field as the breakdown, or the list narrows by the wrong thing.
	const body = suggestionsRequest("visit:entry_page", "28d", "best-stocks");

	assert.deepEqual(body.dimensions, ["visit:entry_page"]);
	assert.deepEqual(body.filters, [
		["contains", "visit:entry_page", ["best-stocks"], { case_sensitive: false }],
	]);
});

test("the request keeps a custom property at event grain", () => {
	// A custom property has no session column. Asking for visitors alone lets
	// the planner choose session grain, which then refuses the breakdown and
	// answers the picker with an error where its values should be.
	const body = suggestionsRequest("event:props:plan", "28d", "");

	assert.ok(body.metrics.includes("events"), "an event-scoped metric must force event grain");
	assert.equal(body.metrics[0], "visitors", "the count beside each value is still visitors");
});

test("a changed search is a changed request, which is what re-runs the query", () => {
	// useStats keys its cache on the serialized body, so two searches that
	// produced the same request would silently reuse the first answer.
	const first = JSON.stringify(suggestionsRequest("event:page", "28d", "blog"));
	const second = JSON.stringify(suggestionsRequest("event:page", "28d", "blogs"));
	const cleared = JSON.stringify(suggestionsRequest("event:page", "28d", ""));

	assert.notEqual(first, second);
	assert.notEqual(first, cleared);
});

test("a dimension stored as a code is searched here, not by the server", () => {
	assert.equal(searchesLocally("visit:country"), true);
	assert.equal(searchesLocally("visit:region"), true);
	assert.equal(searchesLocally("visit:language"), true);
	assert.equal(searchesLocally("visit:entry_page"), false);
	assert.equal(searchesLocally("event:page"), false);

	// Sending "united" to the server would search the column, which holds "US",
	// so the request carries no filter and fetches the whole set instead.
	const body = suggestionsRequest("visit:country", "28d", "united");

	assert.equal(body.filters, undefined);
	assert.ok(
		(body.pagination?.limit ?? 0) > (suggestionsRequest("event:page", "28d", "").pagination?.limit ?? 0),
		"a locally searched list has to arrive whole or the search only finds the first page",
	);
});

test("a local search matches the name the reader is reading, and the code", () => {
	const rows = [row("US"), row("CA"), row("GB")];

	assert.deepEqual(matchingRows(rows, "visit:country", "united", country), [row("US"), row("GB")]);
	assert.deepEqual(matchingRows(rows, "visit:country", "CANAD", country), [row("CA")]);

	// Typing the code is a reasonable thing to do at a list of country names.
	assert.deepEqual(matchingRows(rows, "visit:country", "gb", country), [row("GB")]);

	assert.deepEqual(matchingRows(rows, "visit:country", "zzz", country), []);
	assert.deepEqual(matchingRows(rows, "visit:country", "", country), rows);

	// A server-searched dimension is already narrowed and must not be narrowed
	// twice: the rows it returns match the code, not the label.
	assert.deepEqual(matchingRows([row("/blog")], "event:page", "united", country), [row("/blog")]);
});

test("values are marked out of date until they answer the box above them", () => {
	// Nothing typed, nothing in flight: the busiest values are the answer.
	assert.equal(suggestionsSettled("event:page", "", "", false), true);

	// Still inside the debounce. The rows on screen were fetched for the empty
	// search and cannot contain what is being typed.
	assert.equal(suggestionsSettled("event:page", "blog", "", false), false);

	// Debounce done, query in flight. This is the seconds-long window a large
	// site spends showing its busiest pages under a search for something else.
	assert.equal(suggestionsSettled("event:page", "blog", "blog", true), false);

	assert.equal(suggestionsSettled("event:page", "blog", "blog", false), true);

	// The search is trimmed on its way to the server, so trailing space is not
	// a difference and must not leave the list looking permanently stale.
	assert.equal(suggestionsSettled("event:page", "blog ", "blog", false), true);

	// A locally searched dimension narrows on the keystroke, so it is never
	// waiting on anything and must never be dimmed.
	assert.equal(suggestionsSettled("visit:country", "united", "", false), true);
});
