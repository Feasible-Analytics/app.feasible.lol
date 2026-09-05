//
// SitePicker.test.ts
// The site search, which is the only decision the picker makes on its own.
//
// Created: 2026-09-04
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { SEARCH_THRESHOLD, matchSites } from "./SitePicker";

const SITES = ["cloudmanic.com", "harbor.my", "herdrplus.com", "options.cafe", "Skyclerk.com"];

test("an empty search is every site, in the order it was given", () => {
	assert.deepEqual(matchSites(SITES, ""), SITES);
	assert.deepEqual(matchSites(SITES, "   "), SITES);
});

test("the search matches anywhere in the domain, not just the start", () => {
	// "cafe" is the end of one domain and "plus" the middle of another. An
	// agency naming sites "client-acme.com" would find nothing under a
	// prefix-only match, which is the whole population this box exists for.
	assert.deepEqual(matchSites(SITES, "cafe"), ["options.cafe"]);
	assert.deepEqual(matchSites(SITES, "plus"), ["herdrplus.com"]);
});

test("the search ignores case in both directions", () => {
	assert.deepEqual(matchSites(SITES, "SKYCLERK"), ["Skyclerk.com"]);
	assert.deepEqual(matchSites(SITES, "harbor"), ["harbor.my"]);
});

test("several matches keep the list's own order rather than being ranked", () => {
	// Re-ranking as characters arrive moves rows under the cursor, which is how
	// somebody clicks the site they were not aiming at.
	assert.deepEqual(matchSites(SITES, "h"), ["harbor.my", "herdrplus.com"]);
});

test("no match is an empty list rather than the whole list", () => {
	assert.deepEqual(matchSites(SITES, "nothing-like-this"), []);
});

test("the search box appears only once the list is too long to read", () => {
	// A field to filter four rows only ever costs a keystroke.
	assert.ok(SEARCH_THRESHOLD > SITES.length, "five sites should not earn a search box");
	assert.ok(SEARCH_THRESHOLD <= 10, "a long list must get one before it needs scrolling");
});
