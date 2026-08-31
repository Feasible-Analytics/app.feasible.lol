//
// package.test.js
// The manifest guard: the fields a bundler resolves must point at files that exist and agree.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import test from "node:test";
import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const manifest = require("../package.json");

test("every path the manifest declares is a file that ships", () => {
	const declared = [
		manifest.main,
		manifest.module,
		manifest.types,
		manifest.exports["."].types,
		manifest.exports["."].import,
		manifest.exports["."].require,
		manifest.exports["."].default,
		...manifest.files.filter((entry) => entry.startsWith("dist/")),
	];

	for (const path of declared) {
		assert.ok(existsSync(join(root, path)), `${path} is declared in package.json but is not there`);
	}
});

test("the exports map is ordered and pointed the way a bundler needs", () => {
	// "types" must come first: conditions are matched in order, so a types
	// condition after import or require is never reached and TypeScript falls
	// back to guessing.
	assert.deepEqual(Object.keys(manifest.exports["."]), ["types", "import", "require", "default"]);

	assert.equal(manifest.exports["."].types, "./dist/index.d.ts");
	assert.equal(manifest.exports["."].import, "./dist/index.mjs");
	assert.equal(manifest.exports["."].require, "./dist/index.cjs");

	// main is the CommonJS build and module is the ESM one. Crossing them is
	// how a package ends up with `require()` handed an ES module and a bundler
	// handed CommonJS.
	assert.equal(manifest.main, "./dist/index.cjs");
	assert.equal(manifest.module, "./dist/index.mjs");
	assert.equal(manifest.types, "./dist/index.d.ts");

	assert.equal(manifest.sideEffects, false);
	assert.equal(manifest.license, "MIT");
	assert.equal(manifest.engines.node, ">=18");
});

test("the two hand-written builds expose the same API", async () => {
	const esm = await import("../dist/index.mjs");
	const cjs = require("../dist/index.cjs");

	const names = ["init", "track", "pageview", "enable", "disable", "isEnabled"];

	for (const name of names) {
		assert.equal(typeof esm[name], "function", `the ESM build is missing ${name}`);
		assert.equal(typeof cjs[name], "function", `the CommonJS build is missing ${name}`);
	}

	assert.deepEqual(Object.keys(esm).sort(), [...names, "default"].sort());
	assert.deepEqual(Object.keys(cjs).sort(), [...names, "default"].sort());
});

test("the script is loaded from the host, never bundled into this package", async () => {
	const { readFile } = await import("node:fs/promises");

	for (const build of ["dist/index.mjs", "dist/index.cjs"]) {
		const source = await readFile(join(root, build), "utf8");

		// A build that grew past a few kilobytes is a build that has swallowed
		// the tracker script, which is the one thing this package must not do:
		// the script is AGPL and served by the analytics host, and a stale copy
		// vendored into an npm package is a bug nobody can fix from here.
		assert.ok(source.length < 12000, `${build} is too large to be a loader`);
		assert.match(source, /js\/script\.js/);
	}
});
