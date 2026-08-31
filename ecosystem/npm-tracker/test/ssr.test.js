//
// ssr.test.js
// The regression guard: importing and calling this package with no DOM at all must not throw.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

// The import itself is the first assertion. A package that reads window,
// document, navigator or localStorage at module scope throws right here, in a
// plain Node process with no DOM — which is exactly what a server-side render
// is, and exactly the failure this file exists to prevent for good.
import * as esm from "../dist/index.mjs";

const require = createRequire(import.meta.url);
const cjs = require("../dist/index.cjs");

// There must genuinely be no DOM in this process, or the test proves nothing.
// It runs in its own file so no other test's fake globals can leak into it —
// node --test gives every file its own process.
test("this process really has no DOM", () => {
	assert.equal(typeof window, "undefined");
	assert.equal(typeof document, "undefined");
});

for (const [label, mod] of [
	["esm", esm],
	["cjs", cjs],
]) {
	test(`${label}: init() on a server is a no-op that returns a working stub`, async () => {
		const tracker = mod.init({ domain: "example.com" });

		assert.equal(typeof tracker.track, "function");
		assert.equal(typeof tracker.pageview, "function");

		assert.deepEqual(await tracker.track("Signup"), { sent: false, status: null });
		assert.deepEqual(await tracker.pageview(), { sent: false, status: null });

		assert.equal(tracker.enable(), false);
		assert.equal(tracker.disable(), false);
		assert.equal(tracker.isEnabled(), false);
	});

	test(`${label}: the top-level functions resolve rather than throw`, async () => {
		assert.deepEqual(await mod.track("Signup", { props: { plan: "annual" } }), {
			sent: false,
			status: null,
		});

		assert.deepEqual(await mod.pageview(), { sent: false, status: null });

		assert.equal(mod.enable(), false);
		assert.equal(mod.disable(), false);
		assert.equal(mod.isEnabled(), false);
	});

	test(`${label}: nothing was written to the global scope`, () => {
		assert.equal(typeof globalThis.window, "undefined");
		assert.equal(typeof globalThis.document, "undefined");
		assert.equal(typeof globalThis.feasible, "undefined");
	});
}
