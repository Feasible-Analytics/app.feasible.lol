//
// reports.test.ts
// Regression tests for response enrichments and report dimensions.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { CARDS, DEVICES, PAGES, breakdownValueIndex, dimensionsOf } from "./reports";

test("Top Pages keeps captured titles outside its grouping dimensions", () => {
	const pages = PAGES.tabs[0];
	assert.ok(pages);

	assert.deepEqual(dimensionsOf(pages), ["event:page"]);
	assert.deepEqual(dimensionsOf(pages, "visit:country"), ["event:page", "visit:country"]);
	assert.equal(breakdownValueIndex(pages), 1);
	assert.equal(pages.companion?.enrichment, "page_title");
});

test("reports without a companion keep their existing dimension order", () => {
	const entries = PAGES.tabs[1];
	assert.ok(entries);

	assert.deepEqual(dimensionsOf(entries, "visit:country"), ["visit:entry_page", "visit:country"]);
	assert.equal(breakdownValueIndex(entries), 1);
});

test("Languages shares the Devices card and leaves four half-width reports", () => {
	assert.equal(DEVICES.tabs.at(-1)?.dimension, "visit:language");
	assert.deepEqual(CARDS.map((card) => card.id), ["sources", "pages", "locations", "devices"]);
});
