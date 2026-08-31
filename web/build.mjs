//
// build.mjs
// Bundles the dashboard and writes it straight into the Go package that embeds it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { build, context } from "esbuild";
import { execFileSync } from "node:child_process";
import { copyFileSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

// The output lives inside the Go package rather than in a local dist/ because
// `go:embed` cannot reach outside its own directory. Writing it here means the
// bundle the binary serves and the bundle the build produced can never be two
// different files.
const OUT = join(here, "..", "internal", "dashboard", "assets");

// The filenames are stable on purpose. Cache busting is done by the Go handler,
// which hashes each file at start-up and appends the digest as a query string —
// hashed filenames would churn the repository on every build for no gain, since
// the compiled output is committed.
const JS = join(OUT, "app.js");
const CSS = join(OUT, "app.css");

const BANNER = "/*! feasible.lol dashboard | AGPL-3.0-or-later */";

const watching = process.argv.includes("--watch");

// options is the one esbuild configuration both the single build and the watch
// build run, so a bundle produced during development cannot differ from the one
// that ships.
const options = {
	entryPoints: [join(here, "src", "main.tsx")],
	bundle: true,
	format: "iife",
	// The dashboard sits behind a login and is cached, so bytes are cheap here
	// in a way they never are in the tracker. Minifying anyway keeps the parse
	// cost down on the slow laptops this is actually read on.
	minify: !watching,
	sourcemap: watching ? "inline" : false,
	// Evergreen browsers only. Nobody administers an analytics account from a
	// browser that cannot run this, and transpiling further would only add
	// bytes for a population of zero.
	target: ["es2022", "chrome111", "firefox115", "safari16"],
	jsx: "automatic",
	banner: { js: BANNER },
	legalComments: "none",
	define: { "process.env.NODE_ENV": watching ? '"development"' : '"production"' },
	outfile: JS,
	logLevel: "info",
};

// styles runs the Tailwind compiler over the same sources esbuild just read.
// It is a separate process rather than an esbuild plugin because Tailwind v4
// ships its own CLI and scanner, and wrapping it would mean re-implementing the
// class extraction that is the whole product.
function styles() {
	const cli = join(here, "node_modules", "@tailwindcss", "cli", "dist", "index.mjs");
	const args = [cli, "--input", join(here, "src", "styles.css"), "--output", CSS];

	if (!watching) args.push("--minify");

	execFileSync(process.execPath, args, { stdio: "inherit", cwd: here });
}

// shell copies the HTML the SPA boots from. It is a plain file rather than a
// template because the Go handler is what injects the bootstrap data, and a
// build-time template would put two rendering steps in front of one page.
function shell() {
	copyFileSync(join(here, "src", "index.html"), join(OUT, "index.html"));
}

// report prints what the binary is about to embed. Bundle size is not a budget
// here the way it is for the tracker, but an unexplained jump is still the
// first sign that a dependency arrived by accident.
function report() {
	const js = statSync(JS).size;
	const css = statSync(CSS).size;

	console.log(
		`dashboard: ${(js / 1024).toFixed(1)} KB js, ${(css / 1024).toFixed(1)} KB css, ` +
			`${((js + css) / 1024).toFixed(1)} KB total`,
	);
}

mkdirSync(OUT, { recursive: true });

if (watching) {
	const ctx = await context(options);
	await ctx.watch();
	styles();
	shell();
	console.log("watching web/src — ctrl-c to stop");
} else {
	await build(options);
	styles();
	shell();
	report();

	// A bundle that somehow came out empty would still embed and still serve,
	// producing a blank dashboard with a 200 and no error anywhere. Failing the
	// build is the only place that is cheap to notice.
	if (readFileSync(JS).length < 1024) {
		console.error("dashboard: the bundle is implausibly small — refusing to ship it");
		process.exit(1);
	}
}
