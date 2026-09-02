//
// format.test.ts
// Dashboard number and calendar formatting regressions.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { compact } from "./format";

test("compact counts keep one useful decimal above ten thousand", () => {
	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				locale: "en",
				messages: {
					"dashboard.format.compact.thousand": "{value}k",
					"dashboard.format.compact.million": "{value}M",
					"dashboard.format.compact.billion": "{value}B",
				},
			}),
		}),
	} as unknown as Document;

	assert.equal(compact(20_300), "20.3k");
	assert.equal(compact(22_400), "22.4k");
	assert.equal(compact(44_000), "44k");
});
