//
// server.js
// The fixture server the end-to-end suite loads pages from.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// Playwright starts this and kills it, so a test run never leaves a listener
// behind. It is deliberately not the product's own server: these tests are
// about the script's behaviour in a browser, and pointing them at a Go process
// with a database behind it would make a tracker bug and an ingest bug look the
// same.

import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { dirname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const port = Number(process.env.PORT || 19311);

// The bundle is served from the same origin as the pages, which is what makes
// the script's own endpoint defaulting — the origin of its script tag — the
// thing under test rather than something the fixtures work around.
const BUNDLE = join(here, "..", "dist", "feasible.js");

// The optional vitals collector, served from the same origin for the same
// reason. It is a second file rather than part of the bundle because the core
// script has no room left in its size budget.
const VITALS = join(here, "..", "dist", "feasible.vitals.js");

const TYPES = {
	".html": "text/html; charset=utf-8",
	".js": "application/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".pdf": "application/pdf",
};

// send writes one response with no caching at all. Every test reloads pages and
// expects the script it just built, so a cached anything is a test that passes
// against yesterday's bundle.
function send(res, status, type, body) {
	res.writeHead(status, {
		"Content-Type": type,
		"Cache-Control": "no-store",
		"Access-Control-Allow-Origin": "*",
	});
	res.end(body);
}

// The handler serves three things: the bundle, the fixture pages, and a 202 for
// any event that reaches it. The 202 matters because a test that does not
// intercept the endpoint should still see the script behave normally.
const server = createServer(async (req, res) => {
	const url = new URL(req.url, `http://localhost:${port}`);
	const path = url.pathname;

	if (path === "/api/event") {
		send(res, 202, "text/plain", "ok");
		return;
	}

	if (path === "/js/script.js") {
		send(res, 200, TYPES[".js"], await readFile(BUNDLE));
		return;
	}

	if (path === "/js/vitals.js") {
		send(res, 200, TYPES[".js"], await readFile(VITALS));
		return;
	}

	// A stand-in for a file download, so the extension matching has something
	// real to point at rather than a 404 the browser might handle differently.
	if (path === "/files/report.pdf") {
		send(res, 200, TYPES[".pdf"], "%PDF-1.4 test");
		return;
	}

	const name = normalize(path === "/" ? "/basic.html" : path).replace(/^(\.\.[/\\])+/, "");
	const file = join(here, "fixtures", name);

	if (!file.startsWith(join(here, "fixtures"))) {
		send(res, 403, "text/plain", "no");
		return;
	}

	try {
		const body = await readFile(file);
		const ext = name.slice(name.lastIndexOf("."));
		send(res, 200, TYPES[ext] || "text/plain", body);
	} catch {
		// A 404 still has to be an HTML page, because several fixtures navigate
		// to a path that does not exist on purpose and the script has to keep
		// running on the page it lands on.
		send(res, 404, TYPES[".html"], "<!doctype html><title>404</title><h1>Not found</h1>");
	}
});

server.listen(port, "127.0.0.1", () => {
	console.log(`fixture server listening on http://127.0.0.1:${port}`);
});
