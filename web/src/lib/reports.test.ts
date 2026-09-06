//
// reports.test.ts
// Regression tests for response enrichments and report dimensions.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { PAGES, dimensionsOf, noticesOf } from "./reports";

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
	// Splitting them is a reasonable thing to want, and should be a decision
	// rather than two labels quietly disagreeing.
	assert.equal(PAGES.titleId, PAGES.tabs[0]?.labelId);
});

test("one reinterpreted filter reads as one sentence, not one per metric", () => {
	const sentence = "Your page filter was applied to the page each visit started on.";

	assert.deepEqual(
		noticesOf({
			metric_warnings: {
				visitors: { code: "entry_scoped", warning: sentence },
				visits: { code: "entry_scoped", warning: sentence },
				bounce_rate: { code: "entry_scoped", warning: sentence },
			},
		}),
		[sentence],
	);
});

test("an answer with nothing to say about it produces no notices", () => {
	assert.deepEqual(noticesOf(undefined), []);
	assert.deepEqual(noticesOf({}), []);
	assert.deepEqual(noticesOf({ metric_warnings: {} }), []);
});
