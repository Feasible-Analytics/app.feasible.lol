//
// filters.test.ts
// The URL encoding is a contract, so it is tested like one.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { FilterState } from "./filters";
import {
	aliasOf,
	decodeFilter,
	decodeLabel,
	dimensionLabel,
	dimensionOf,
	encodeFilter,
	encodeLabel,
	pillMessage,
	readFilters,
	readLabels,
	remove,
	toApi,
	toggle,
	writeFilters,
} from "./filters";

/** roundTrip encodes a filter and reads it straight back, which is the only
 *  property the encoding actually has to have. */
function roundTrip(filter: FilterState): FilterState | null {
	return decodeFilter(encodeFilter(filter));
}

test("a filter survives the round trip", () => {
	const filter: FilterState = { operator: "is", dimension: "visit:country", values: ["DE"] };

	assert.equal(encodeFilter(filter), "is,country,DE");
	assert.deepEqual(roundTrip(filter), filter);
});

test("the short alias in the URL expands to the API dimension", () => {
	assert.equal(aliasOf("event:page"), "page");
	assert.equal(aliasOf("visit:source"), "source");
	assert.equal(aliasOf("visit:screen"), "screen");

	assert.equal(dimensionOf("page"), "event:page");
	assert.equal(dimensionOf("source"), "visit:source");
	assert.equal(dimensionOf("screen"), "visit:screen");
});

test("a dimension with no alias travels spelled out", () => {
	const filter: FilterState = { operator: "is", dimension: "event:props:plan", values: ["pro"] };

	assert.equal(encodeFilter(filter), "is,event:props:plan,pro");
	assert.deepEqual(roundTrip(filter), filter);
});

test("several values in one filter round trip as an OR", () => {
	const filter: FilterState = { operator: "is", dimension: "visit:country", values: ["DE", "FR", "GB"] };

	assert.equal(encodeFilter(filter), "is,country,DE,FR,GB");
	assert.deepEqual(roundTrip(filter), filter);
});

test("a comma inside a value does not become a separator", () => {
	const filter: FilterState = { operator: "is", dimension: "event:page", values: ["/a,b", "/c"] };

	assert.equal(encodeFilter(filter), "is,page,/a\\,b,/c");
	assert.deepEqual(roundTrip(filter), filter);
});

test("a backslash inside a value survives, escapes and all", () => {
	const filter: FilterState = { operator: "matches", dimension: "event:page", values: ["^/docs\\d+,$"] };

	assert.deepEqual(roundTrip(filter), filter);
});

test("the empty value is a real filter, not a missing one", () => {
	// Direct traffic is stored as the empty string, so "source is (none)" has to
	// be expressible or the biggest row on the sources card cannot be clicked.
	const filter: FilterState = { operator: "is", dimension: "visit:source", values: [""] };

	assert.equal(encodeFilter(filter), "is,source,");
	assert.deepEqual(roundTrip(filter), filter);
});

test("a damaged filter is dropped rather than thrown", () => {
	assert.equal(decodeFilter(""), null);
	assert.equal(decodeFilter("is"), null);
	assert.equal(decodeFilter("is,country"), null);
	assert.equal(decodeFilter("sounds_like,country,DE"), null);
	assert.equal(decodeFilter("is,,DE"), null);
});

test("only the operators the engine implements are accepted", () => {
	for (const operator of ["is", "is_not", "contains", "contains_not", "matches", "matches_not"]) {
		assert.ok(decodeFilter(`${operator},country,DE`), `${operator} must decode`);
	}

	for (const operator of ["starts_with", "gt", "between", "has_done"]) {
		assert.equal(decodeFilter(`${operator},country,DE`), null, `${operator} must not decode`);
	}
});

test("a label pair round trips, separators and all", () => {
	assert.equal(encodeLabel("DE", "Germany"), "DE,Germany");
	assert.deepEqual(decodeLabel("DE,Germany"), ["DE", "Germany"]);
	assert.deepEqual(decodeLabel(encodeLabel("x,y", "Comma, Inc")), ["x,y", "Comma, Inc"]);
	assert.equal(decodeLabel("DE"), null);
});

test("a whole query string round trips through the parameters", () => {
	const filters: FilterState[] = [
		{ operator: "is", dimension: "visit:country", values: ["DE"] },
		{ operator: "contains", dimension: "event:page", values: ["/blog"] },
	];

	const params = new URLSearchParams();
	writeFilters(params, filters, { DE: "Germany", US: "United States" });

	// Only the labels a filter actually references are written; carrying the
	// rest would grow the URL forever as somebody clicks around.
	assert.equal(params.toString(), "f=is%2Ccountry%2CDE&f=contains%2Cpage%2C%2Fblog&l=DE%2CGermany");

	const read = new URLSearchParams(params.toString());
	assert.deepEqual(readFilters(read), filters);
	assert.deepEqual(readLabels(read), { DE: "Germany" });
});

test("more than the engine's ceiling of filters is truncated, not sent", () => {
	const params = new URLSearchParams();
	for (let i = 0; i < 50; i++) params.append("f", `is,country,C${i}`);

	assert.equal(readFilters(params).length, 32);
});

test("the wire form is the positional array the engine reads", () => {
	const filters: FilterState[] = [{ operator: "is_not", dimension: "visit:device", values: ["Mobile"] }];

	assert.deepEqual(toApi(filters), [["is_not", "visit:device", ["Mobile"], { case_sensitive: true }]]);
});

test("clicking the same row twice removes the filter it added", () => {
	const filter: FilterState = { operator: "is", dimension: "visit:country", values: ["DE"] };

	assert.deepEqual(toggle([], filter), [filter]);
	assert.deepEqual(toggle([filter], filter), []);
});

test("clicking a different row of the same report replaces rather than stacks", () => {
	// Two "is" filters on one dimension can never both be true, so stacking them
	// would empty the dashboard and read as the filter being broken.
	const germany: FilterState = { operator: "is", dimension: "visit:country", values: ["DE"] };
	const france: FilterState = { operator: "is", dimension: "visit:country", values: ["FR"] };
	const mobile: FilterState = { operator: "is", dimension: "visit:device", values: ["Mobile"] };

	assert.deepEqual(toggle([germany, mobile], france), [mobile, france]);
});

test("removing a pill takes only that one", () => {
	const germany: FilterState = { operator: "is", dimension: "visit:country", values: ["DE"] };
	const mobile: FilterState = { operator: "is", dimension: "visit:device", values: ["Mobile"] };

	assert.deepEqual(remove([germany, mobile], 0), [mobile]);
});

test("a pill is one whole sentence per operator, not glued-together fragments", () => {
	// Word order is not universal. A translator handed a dimension, a verb and a
	// value separately cannot put them in the order their own grammar needs, so
	// each operator names a complete sentence with placeholders in it.
	const one: FilterState = { operator: "is", dimension: "visit:country", values: ["DE"] };
	assert.equal(pillMessage(one), "dashboard.filter.pill.is");

	const negated: FilterState = { operator: "contains_not", dimension: "event:page", values: ["/admin"] };
	assert.equal(pillMessage(negated), "dashboard.filter.pill.contains_not");
});

test("several values pick the counted form of the same sentence", () => {
	const several: FilterState = { operator: "is", dimension: "visit:country", values: ["DE", "FR", "GB"] };

	assert.equal(pillMessage(several), "dashboard.filter.pill.is_any");
});

test("a dimension is named by a message id, and a custom property by itself", () => {
	// A property name is whatever the site's own code sent. Inventing a message
	// id for it would be inventing one per customer.
	assert.deepEqual(dimensionLabel("visit:country"), { id: "dashboard.dimension.country" });
	assert.deepEqual(dimensionLabel("event:props:plan"), { text: "plan" });
});
