//
// SitePicker.test.ts
// The site search, which is the only decision the picker makes on its own.
//
// Created: 2026-09-04
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { SEARCH_THRESHOLD, SitePicker, matchSites } from "./SitePicker";

// The catalogue is read from the page, so it is stubbed before any test renders
// the picker. It is the real English file rather than a copy, which cannot drift
// from what ships.
const messages = JSON.parse(
	readFileSync(new URL("../../../internal/i18n/locales/en/dashboard.json", import.meta.url), "utf8"),
) as Record<string, string>;

globalThis.document = {
	getElementById: () => ({ textContent: JSON.stringify({ locale: "en", messages }) }),
} as unknown as Document;

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

/** pickerMarkup draws the picker the way the top bar does. */
function pickerMarkup(current: string): string {
	return renderToStaticMarkup(
		createElement(SitePicker, { current, sites: SITES, onPick: () => {} }),
	);
}

test("the button says which site is selected, to a screen reader as well", () => {
	// An aria-label replaces the button's own text, so the label is the only
	// thing that can name the current site to a screen reader.
	const markup = pickerMarkup("stopoverpayingforanalytics.com");

	assert.match(markup, /aria-label="[^"]*stopoverpayingforanalytics\.com[^"]*"/);
});

test("a domain too long for the button is still readable on hover", () => {
	// A hostname can be 253 characters, so no width removes truncation. The
	// title is the only thing that makes the rest of one reachable.
	const long = "a-very-long-subdomain.of-an-even-longer-second-level-domain.example.com";

	assert.ok(pickerMarkup(long).includes(`title="${long}"`), "the label carries no title");
});

test("the button is capped at the phone width and widens from sm up", () => {
	// 288px is the whole content width of a small phone, and it is the width of
	// the menu this button opens — so the button is never wider than its list.
	const markup = pickerMarkup("harbor.my");

	assert.ok(markup.includes("max-w-52"), "the phone cap is gone");
	assert.ok(markup.includes("sm:max-w-72"), "the wide cap is missing");
});
