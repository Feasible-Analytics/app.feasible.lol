//
// labels.test.ts
// Human-readable dimension labels must tolerate visitor-owned values.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { languageName } from "./labels";

test("an invalid stored language tag falls back without blanking the dashboard", () => {
	assert.equal(languageName("root"), "root");
});
