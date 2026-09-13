//
// labels.test.ts
// Human-readable dimension labels must tolerate visitor-owned values.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { flagFor, languageName } from "./labels";

test("an invalid stored language tag falls back without blanking the dashboard", () => {
	assert.equal(languageName("root"), "root");
});

test("a city row flies the flag the server attached, having none of its own", () => {
	// "Salem" is a bare name on the row, so without the enrichment there is
	// nothing to read a country from and the row leads with a blank.
	assert.equal(flagFor("visit:city", "Salem"), "");
	assert.equal(flagFor("visit:city", "Salem", "US"), "\u{1F1FA}\u{1F1F8}");
});

test("an enriched country beats the row's own value, so a region cannot fly two flags", () => {
	assert.equal(flagFor("visit:region", "US-CA"), "\u{1F1FA}\u{1F1F8}");
	assert.equal(flagFor("visit:region", "US-CA", "GB"), "\u{1F1EC}\u{1F1E7}");
});
