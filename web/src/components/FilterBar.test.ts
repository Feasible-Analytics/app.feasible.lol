//
// FilterBar.test.ts
// The value picker's search contract.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import { suggestionsRequest } from "./FilterBar";

test("an empty search asks for the busiest values and nothing else", () => {
	const body = suggestionsRequest("visit:entry_page", "28d", "");

	assert.deepEqual(body.dimensions, ["visit:entry_page"]);
	assert.equal(body.filters, undefined, "an unfiltered request must not carry an empty filter");
	assert.ok((body.pagination?.limit ?? 0) > 0, "the list is capped, which is why the search exists");
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

test("the search is case-insensitive on every filterable dimension", () => {
	for (const dimension of ["event:page", "visit:entry_page", "visit:source", "visit:referrer"]) {
		const body = suggestionsRequest(dimension, "28d", "Blog");
		const filter = body.filters?.[0];

		assert.equal(filter?.[0], "contains", `${dimension} must search rather than match exactly`);
		assert.equal(filter?.[1], dimension);
		assert.deepEqual(filter?.[3], { case_sensitive: false }, `${dimension} must ignore case`);
	}
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
