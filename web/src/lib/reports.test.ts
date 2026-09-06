//
// reports.test.ts
// Regression tests for response enrichments and report dimensions.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { PAGES, SOURCES, dimensionsOf } from "./reports";

test("the Pages card keeps captured titles outside its grouping dimensions", () => {
	const pages = PAGES.tabs[0];
	assert.ok(pages);

	assert.deepEqual(dimensionsOf(pages), ["event:page"]);
	assert.deepEqual(dimensionsOf(pages, "visit:country"), ["event:page", "visit:country"]);
	assert.equal(pages.companion?.enrichment, "page_title");
});

test("reports without a companion keep their existing dimension order", () => {
	const entries = PAGES.tabs[1];
	assert.ok(entries);

	assert.deepEqual(dimensionsOf(entries, "visit:country"), ["visit:entry_page", "visit:country"]);
});

test("the Pages heading and its first tab are one string", () => {
	// One id is what makes renaming this card a one-line change. Splitting them
	// is a reasonable thing to want, and should be a decision rather than a
	// surprise found by reading two labels that no longer agree.
	assert.equal(PAGES.titleId, PAGES.tabs[0]?.labelId);

	// Sources is the other shape on purpose: its first tab is Channels, so its
	// heading is its own string and renaming the card leaves the tabs alone.
	assert.notEqual(SOURCES.titleId, SOURCES.tabs[0]?.labelId);
});
