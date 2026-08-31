//
// build.js
// Bundles and minifies the tracker, then refuses to ship one that is too big.
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

// The hard budget, in bytes over the wire. Everything the tracker does has to
// fit here: a script that grows without a ceiling is a script that eventually
// costs a customer their Core Web Vitals score, and there is no natural moment
// to start caring. Failing the build is the only enforcement that works.
const BUDGET = 3 * 1024;

const OUT = join(here, "dist", "feasible.js");

// The second copy, inside the Go package that serves it. `go:embed` cannot
// reach outside its own directory, and the alternative — a Makefile step that
// copies the file — is a step that gets skipped, leaving the binary serving a
// bundle nobody can find the source of. Writing both here means the two can
// never disagree.
const EMBEDDED = join(here, "..", "internal", "tracker", "assets", "feasible.js");

// The banner is the only comment that survives minification. The licence has to
// travel with the file: it is served from our origin onto other people's pages,
// where nothing else identifies it.
const BANNER = "/*! feasible.lol tracker | AGPL-3.0-or-later */";

// compile produces the single bundle both delivery modes are served from.
//
// One artefact rather than a variant per feature is a deliberate reversal of
// how the incumbent ships. Their combinatorics — a hash build, a manual build,
// an exclusions build, an outbound-links build — is exactly where their bugs
// lived, because every feature worked alone and the pairs were never tested.
async function compile() {
	mkdirSync(dirname(OUT), { recursive: true });

	await build({
		entryPoints: [join(here, "src", "index.js")],
		bundle: true,
		minify: true,
		format: "iife",
		// The floor is set by what the tracker relies on: fetch with keepalive,
		// ResizeObserver, requestSubmit. Targeting anything older only adds
		// transpiled bytes for browsers that could not run it anyway.
		target: ["es2020"],
		banner: { js: BANNER },
		legalComments: "none",
		outfile: OUT,
	});

	mkdirSync(dirname(EMBEDDED), { recursive: true });
	writeFileSync(EMBEDDED, readFileSync(OUT));
}

// report prints the size and returns whether it is within budget.
function report() {
	const raw = readFileSync(OUT);
	const gzipped = gzipSync(raw, { level: 9 });

	const ok = gzipped.length <= BUDGET;
	const verdict = ok ? "ok" : "OVER BUDGET";

	console.log(
		`tracker: ${raw.length} bytes raw, ${gzipped.length} bytes gzipped ` +
			`(budget ${BUDGET}) — ${verdict}`,
	);

	return ok;
}

// The --check flag skips the build and only measures, so that `make test` can
// enforce the budget on a machine with no Node modules installed beyond this
// one having run once.
if (!process.argv.includes("--check")) await compile();

if (!report()) process.exit(1);
