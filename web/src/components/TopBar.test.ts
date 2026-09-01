//
// TopBar.test.ts
// Regression tests for the live number's query contract.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { Filter } from "../api/types";
import { currentVisitorsRequest } from "./TopBar";

test("the current visitors number always requests an exact answer", () => {
	const filter: Filter = ["is", "visit:country", ["US"]];
	const request = currentVisitorsRequest([filter]);

	assert.equal(request.exact, true);
	assert.deepEqual(request.filters?.[0], filter);
	assert.deepEqual(request.filters?.at(-1), [
		"is_not",
		"event:name",
		["engagement"],
		{ case_sensitive: true },
	]);
});
