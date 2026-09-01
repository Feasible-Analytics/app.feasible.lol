//
// build.js
// Bundles and minifies the browser scripts, then refuses to ship one that is too big.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { build } from "esbuild";
import { minify } from "terser";
import { gzipSync } from "node:zlib";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

// The tracker has a hard budget in bytes over the wire.
//
// Everything the tracker does has to fit in this artifact: a script that grows
// without a ceiling is a script that eventually costs a customer their Core Web
// Vitals score, and there is no natural moment to start caring. Failing the
// build is the only enforcement that works.
//
// The second copy is inside the Go package that serves it. `go:embed`
// cannot reach outside its own directory, and the alternative — a Makefile step
// that copies the file — is a step that gets skipped, leaving the binary serving
// a bundle nobody can find the source of. Writing both here means the copies can
// never disagree.
const BUNDLES = [
	{
		name: "tracker base",
		entry: join(here, "src", "index.js"),
		out: join(here, "dist", "feasible.js"),
		embedded: join(here, "..", "internal", "tracker", "assets", "feasible.js"),
		// Stable client ids plus current-policy enforcement on every live and
		// persisted send add a small fixed cost. Keep the planned 3.25 KiB
		// post-feature ceiling hard so privacy does not become an excuse for
		// unbounded bundle growth.
		budget: 13 * 256,
		format: "iife",
		external: ["./vitals.js"],
	},
	{
		name: "Web Vitals optional module",
		entry: join(here, "src", "vitals.js"),
		out: join(here, "dist", "vitals.js"),
		embedded: join(here, "..", "internal", "tracker", "assets", "vitals.js"),
		budget: 6 * 1024,
		format: "esm",
	},
];

// The banner is the only comment that survives minification. The licence has to
// travel with the file: it is served from our origin onto other people's pages,
// where nothing else identifies it.
const BANNER = "/*! feasible.lol tracker | AGPL-3.0-or-later */";

// compile produces one independently budgeted bundle and its embedded copy.
//
// Esbuild owns module resolution and syntax lowering. Terser then spends a few
// extra compression passes on the already bundled program; that deterministic
// second pass is what keeps the always-loaded base below its wire-size ceiling.
async function compile(bundle) {
	mkdirSync(dirname(bundle.out), { recursive: true });

	await build({
		entryPoints: [bundle.entry],
		bundle: true,
		minify: false,
		format: bundle.format,
		external: bundle.external,
		// The floor is set by what the scripts rely on: fetch with keepalive,
		// ResizeObserver, requestSubmit, PerformanceObserver with buffered
		// entries. Targeting anything older only adds transpiled bytes for
		// browsers that could not run it anyway.
		target: ["es2020"],
		banner: { js: BANNER },
		legalComments: "none",
		outfile: bundle.out,
	});

	const bundled = readFileSync(bundle.out, "utf8");
	const minified = await minify(bundled, {
		compress: { passes: 5, toplevel: true, unsafe: true },
		mangle: { toplevel: true },
		module: bundle.format === "esm",
		format: { comments: /^!/ },
	});
	if (!minified.code) throw new Error(`${bundle.name} minification produced no code`);
	writeFileSync(bundle.out, `${minified.code}\n`);

	mkdirSync(dirname(bundle.embedded), { recursive: true });
	writeFileSync(bundle.embedded, readFileSync(bundle.out));
}

// report prints one bundle's size and returns whether it is within budget.
function report(bundle) {
	const raw = readFileSync(bundle.out);
	const gzipped = gzipSync(raw, { level: 9 });

	const ok = gzipped.length <= bundle.budget;
	const verdict = ok ? "ok" : "OVER BUDGET";

	console.log(
		`${bundle.name}: ${raw.length} bytes raw, ${gzipped.length} bytes gzipped ` +
			`(budget ${bundle.budget}) — ${verdict}`,
	);

	return ok;
}

// The --check flag skips the build and only measures, so that `make test` can
// enforce the budgets on a machine with no Node modules installed beyond this
// one having run once.
const checkOnly = process.argv.includes("--check");

let ok = true;

for (const bundle of BUNDLES) {
	if (!checkOnly) await compile(bundle);
	if (!report(bundle)) ok = false;
}

if (!ok) process.exit(1);
