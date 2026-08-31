//
// build.js
// Bundles and minifies the browser scripts, then refuses to ship one that is too big.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { build } from "esbuild";
import { gzipSync } from "node:zlib";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

// The two bundles, each with its own hard budget in bytes over the wire.
//
// Everything the tracker does has to fit in the first one: a script that grows
// without a ceiling is a script that eventually costs a customer their Core Web
// Vitals score, and there is no natural moment to start caring. Failing the
// build is the only enforcement that works.
//
// The vitals collector is a second bundle rather than a feature of the first
// precisely because of that ceiling — there is no room in the core budget, and
// raising it would make every site pay for a measurement most of them have not
// asked for. Its own budget is smaller because it does one thing.
//
// The second copy of each is inside the Go package that serves it. `go:embed`
// cannot reach outside its own directory, and the alternative — a Makefile step
// that copies the file — is a step that gets skipped, leaving the binary serving
// a bundle nobody can find the source of. Writing both here means the two can
// never disagree.
const BUNDLES = [
	{
		name: "tracker",
		entry: join(here, "src", "index.js"),
		out: join(here, "dist", "feasible.js"),
		embedded: join(here, "..", "internal", "tracker", "assets", "feasible.js"),
		budget: 3 * 1024,
	},
	{
		name: "vitals",
		entry: join(here, "src", "vitals.js"),
		out: join(here, "dist", "feasible.vitals.js"),
		embedded: join(here, "..", "internal", "tracker", "assets", "feasible.vitals.js"),
		budget: 1024,
	},
];

// The banner is the only comment that survives minification. The licence has to
// travel with the file: it is served from our origin onto other people's pages,
// where nothing else identifies it.
const BANNER = "/*! feasible.lol tracker | AGPL-3.0-or-later */";

// compile produces one bundle and its embedded copy.
//
// One artefact per job rather than a variant per feature is a deliberate
// reversal of how the incumbent ships. Their combinatorics — a hash build, a
// manual build, an exclusions build, an outbound-links build — is exactly where
// their bugs lived, because every feature worked alone and the pairs were never
// tested. The split here is not a feature switch: the vitals collector is a
// different script doing a different job, and it talks to the core one through
// the same public function a customer's own code uses.
async function compile(bundle) {
	mkdirSync(dirname(bundle.out), { recursive: true });

	await build({
		entryPoints: [bundle.entry],
		bundle: true,
		minify: true,
		format: "iife",
		// The floor is set by what the scripts rely on: fetch with keepalive,
		// ResizeObserver, requestSubmit, PerformanceObserver with buffered
		// entries. Targeting anything older only adds transpiled bytes for
		// browsers that could not run it anyway.
		target: ["es2020"],
		banner: { js: BANNER },
		legalComments: "none",
		outfile: bundle.out,
	});

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
