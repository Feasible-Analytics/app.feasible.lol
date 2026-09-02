//
// test.mjs
// Bundles the unit tests with esbuild, then hands them to node --test.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { build } from "esbuild";
import { spawnSync } from "node:child_process";
import { globSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const OUT = join(here, ".test-build");

// The bundle step exists because Node's own TypeScript support strips types but
// does not resolve a bundler-style import — `./filters` with no extension — and
// rewriting every import in the source to satisfy the test runner would be the
// tail wagging the dog. esbuild is already a dependency and already knows how
// to resolve exactly what the real bundle resolves, so the code under test is
// resolved the same way the shipped code is.
const tests = globSync("src/**/*.test.ts", { cwd: here }).map((file) => join(here, file));

if (tests.length === 0) {
	console.error("no test files found under web/src");
	process.exit(1);
}

rmSync(OUT, { recursive: true, force: true });

await build({
	entryPoints: tests,
	outdir: OUT,
	outExtension: { ".js": ".mjs" },
	bundle: true,
	platform: "node",
	format: "esm",
	target: "node22",
	// Packages stay external because Node resolves node_modules itself at run
	// time. Bundling them would copy React into every test file that renders a
	// component, for nothing.
	packages: "external",
	logLevel: "warning",
});

// The bundled files are named rather than the directory: node --test treats a
// directory argument as a file to execute, not as a tree to walk.
const bundled = globSync("**/*.test.mjs", { cwd: OUT }).map((file) => join(OUT, file));

const run = spawnSync(process.execPath, ["--test", ...bundled], { stdio: "inherit", cwd: here });

process.exit(run.status ?? 1);
