//
// feasible.test.js
// Everything the client promises, proved against a real http server on an ephemeral port.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createRequire } from "node:module";

import {
	FeasibleClient,
	FeasibleApiError,
	FeasibleValidationError,
	createClient,
	visitorFromNodeRequest,
	visitorFromWebRequest,
} from "../src/index.js";

// startServer runs a real listener on an ephemeral port and records what it
// received. It is a real server rather than a stubbed fetch because the content
// type, the two forwarded headers and the retry behaviour are exactly what
// these tests exist to check, and a stub would let a bug in any of them through.
async function startServer(handler) {
	const requests = [];

	const server = createServer((req, res) => {
		let body = "";

		req.on("data", (chunk) => {
			body += chunk;
		});

		req.on("end", () => {
			requests.push({ body, headers: req.headers, method: req.method, url: req.url });
			handler(req, res, requests.length);
		});
	});

	await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));

	return {
		host: `http://127.0.0.1:${server.address().port}`,
		requests,
		// Keep-alive is what makes the client fast and what makes a naive
		// close() hang, so open sockets are dropped before closing.
		async close() {
			if (server.closeAllConnections) server.closeAllConnections();
			await new Promise((resolve) => server.close(resolve));
		},
	};
}

// accepted answers every request the way the ingest endpoint does.
function accepted(_req, res) {
	res.writeHead(202).end();
}

// client builds a client pointed at a test server, with a backoff short enough
// that the retry tests run in milliseconds rather than seconds.
function client(host, overrides = {}) {
	return new FeasibleClient({
		domain: "example.com",
		host,
		baseBackoff: 1,
		maxBackoff: 2,
		...overrides,
	});
}

// visitor is the pair every event needs, spelled out once.
const visitor = { clientIp: "203.0.113.9", userAgent: "Mozilla/5.0 (Macintosh)" };

test("a pageview sends three keys, text/plain, and the two forwarded headers", async () => {
	const server = await startServer(accepted);

	try {
		const result = await client(server.host).pageview({ url: "https://example.com/pricing", ...visitor });

		assert.equal(result.statusCode, 202);
		assert.equal(result.attempts, 1);
		assert.equal(result.dropReason, "");
		assert.equal(result.skipped, false);

		assert.equal(server.requests.length, 1);

		const sent = server.requests[0];

		assert.deepEqual(Object.keys(JSON.parse(sent.body)).sort(), ["d", "n", "u"]);
		assert.deepEqual(JSON.parse(sent.body), {
			n: "pageview",
			u: "https://example.com/pricing",
			d: "example.com",
		});

		assert.equal(sent.headers["content-type"], "text/plain");
		assert.equal(sent.headers["x-forwarded-for"], "203.0.113.9");
		assert.equal(sent.headers["user-agent"], "Mozilla/5.0 (Macintosh)");
	} finally {
		await server.close();
	}
});

test("a custom event carries props, revenue and the attribution overrides", async () => {
	const server = await startServer(accepted);

	try {
		await client(server.host).track("Purchase", {
			url: "https://example.com/checkout",
			...visitor,
			props: { plan: "annual", seats: 4 },
			revenue: { amount: 99.5, currency: "usd" },
			title: "Checkout",
			referrer: "https://news.example/post",
			interactive: false,
			attribution: {
				referrer: "https://news.example/post",
				utmSource: "newsletter",
				utmMedium: "email",
				utmCampaign: "spring",
				utmContent: "cta-a",
				utmTerm: "analytics",
			},
		});

		const body = JSON.parse(server.requests[0].body);

		assert.deepEqual(Object.keys(body).sort(), [
			"$",
			"d",
			"i",
			"n",
			"p",
			"r",
			"referrer",
			"t",
			"u",
			"utm_campaign",
			"utm_content",
			"utm_medium",
			"utm_source",
			"utm_term",
		]);

		assert.deepEqual(body.p, { plan: "annual", seats: 4 });
		assert.deepEqual(body.$, { amount: 99.5, currency: "usd" });
		assert.equal(body.i, false);
		assert.equal(body.utm_source, "newsletter");
	} finally {
		await server.close();
	}
});

test("it refuses to send without a client IP or a User-Agent", async () => {
	const server = await startServer(accepted);

	const cases = [
		{
			name: "missing client IP",
			event: { url: "https://example.com/", userAgent: "curl/8.4.0" },
			field: "event.clientIp",
			code: "missing_client_ip",
		},
		{
			name: "blank client IP",
			event: { url: "https://example.com/", clientIp: "   ", userAgent: "curl/8.4.0" },
			field: "event.clientIp",
			code: "missing_client_ip",
		},
		{
			name: "missing User-Agent",
			event: { url: "https://example.com/", clientIp: "203.0.113.9" },
			field: "event.userAgent",
			code: "missing_user_agent",
		},
		{
			name: "missing url",
			event: { ...visitor },
			field: "event.url",
			code: "missing_url",
		},
	];

	try {
		const sender = client(server.host);

		for (const item of cases) {
			await assert.rejects(
				() => sender.pageview(item.event),
				(error) => {
					assert.ok(error instanceof FeasibleValidationError, `${item.name}: wrong error type`);
					assert.equal(error.field, item.field);
					assert.equal(error.code, item.code);
					assert.ok(error.reason.length > 0, "a validation error must say why the field matters");
					return true;
				},
				item.name,
			);
		}

		await assert.rejects(
			() => sender.send({ url: "https://example.com/", ...visitor }),
			(error) => error instanceof FeasibleValidationError && error.code === "missing_name",
		);

		assert.equal(server.requests.length, 0, "a refused event must never reach the wire");
	} finally {
		await server.close();
	}
});

test("the constructor refuses a client with no domain", () => {
	assert.throws(
		() => createClient({}),
		(error) => error instanceof FeasibleValidationError && error.code === "missing_domain",
	);
});

test("no-op mode sends nothing, succeeds, and records the events", async () => {
	const server = await startServer(accepted);

	try {
		for (const source of ["flag", "environment"]) {
			if (source === "environment") process.env.FEASIBLE_DISABLED = "1";

			const sender = client(server.host, { disabled: source === "flag" ? true : undefined });

			assert.equal(sender.disabled, true, `${source}: client should be in no-op mode`);

			const result = await sender.track("Signup", { url: "https://example.com/join", ...visitor });

			assert.deepEqual(result, { statusCode: 0, dropReason: "", attempts: 0, skipped: true });

			const recorded = sender.recorded();

			assert.equal(recorded.length, 1);
			assert.equal(recorded[0].name, "Signup");
			assert.equal(recorded[0].clientIp, "203.0.113.9");

			sender.reset();
			assert.equal(sender.recorded().length, 0);

			delete process.env.FEASIBLE_DISABLED;
		}

		assert.equal(server.requests.length, 0, "no-op mode must not send anything");
	} finally {
		delete process.env.FEASIBLE_DISABLED;
		await server.close();
	}
});

test("no-op mode still refuses a missing client IP", async () => {
	const sender = client("https://example.invalid", { disabled: true });

	await assert.rejects(
		() => sender.pageview({ url: "https://example.com/", userAgent: "curl/8.4.0" }),
		(error) => error instanceof FeasibleValidationError && error.code === "missing_client_ip",
	);
});

test("a 5xx is retried with backoff until it succeeds", async () => {
	const server = await startServer((req, res, count) => {
		if (count < 3) return res.writeHead(500).end("nope");
		res.writeHead(202).end();
	});

	try {
		const result = await client(server.host).pageview({ url: "https://example.com/", ...visitor });

		assert.equal(result.attempts, 3);
		assert.equal(server.requests.length, 3);
	} finally {
		await server.close();
	}
});

test("a 429 that never clears ends in an api error naming the attempts", async () => {
	const server = await startServer((req, res) => {
		res.writeHead(429, { "retry-after": "1" }).end("slow down");
	});

	try {
		await assert.rejects(
			() => client(server.host).pageview({ url: "https://example.com/", ...visitor }),
			(error) => {
				assert.ok(error instanceof FeasibleApiError);
				assert.equal(error.statusCode, 429);
				assert.equal(error.attempts, 3);
				return true;
			},
		);

		assert.equal(server.requests.length, 3);
	} finally {
		await server.close();
	}
});

test("a 400 is not retried and keeps the server's own sentence", async () => {
	const server = await startServer((req, res) => {
		res
			.writeHead(400, { "content-type": "text/plain" })
			.end("this request arrived from a datacentre address with no X-Forwarded-For");
	});

	try {
		await assert.rejects(
			() => client(server.host).pageview({ url: "https://example.com/", ...visitor }),
			(error) => {
				assert.ok(error instanceof FeasibleApiError);
				assert.equal(error.statusCode, 400);
				assert.match(error.body, /datacentre address/);
				return true;
			},
		);

		assert.equal(server.requests.length, 1, "a 400 must not be retried");
	} finally {
		await server.close();
	}
});

test("a 202 carrying a drop reason is reported, not retried and not an error", async () => {
	const server = await startServer((req, res) => {
		res.writeHead(202, { "x-feasible-dropped": "bot:datacenter" }).end();
	});

	try {
		const result = await client(server.host).pageview({ url: "https://example.com/", ...visitor });

		assert.equal(result.statusCode, 202);
		assert.equal(result.dropReason, "bot:datacenter");
		assert.equal(server.requests.length, 1, "a classification must not be retried");
	} finally {
		await server.close();
	}
});

test("a transport failure is retried and reports the last attempt", async () => {
	const server = await startServer(accepted);
	const host = server.host;
	await server.close();

	await assert.rejects(
		() => client(host, { attempts: 2 }).pageview({ url: "https://example.com/", ...visitor }),
		(error) => {
			assert.match(error.message, /attempt 2/);
			return true;
		},
	);
});

test("debug returns the server's derived event and sends the debug header", async () => {
	const server = await startServer((req, res) => {
		if (req.headers["x-debug-request"] !== "true") return res.writeHead(202).end();

		res
			.writeHead(200, { "content-type": "application/json" })
			.end(JSON.stringify({ site_id: 1, client_ip_source: "x-forwarded-for" }));
	});

	try {
		const derived = await client(server.host).debug({ name: "pageview", url: "https://example.com/", ...visitor });

		assert.equal(derived.client_ip_source, "x-forwarded-for");
		assert.equal(server.requests[0].headers["x-debug-request"], "true");
	} finally {
		await server.close();
	}
});

test("visitorFromNodeRequest follows the server's own precedence", async () => {
	const seen = [];

	const server = await startServer((req, res) => {
		seen.push(visitorFromNodeRequest(req));
		res.writeHead(204).end();
	});

	try {
		await fetch(`${server.host}/`, {
			headers: {
				"cf-connecting-ip": "198.51.100.5",
				"x-forwarded-for": "192.0.2.5, 10.0.0.7",
				"user-agent": "Mozilla/5.0 (X11)",
			},
		});

		await fetch(`${server.host}/`, {
			headers: { "x-forwarded-for": "192.0.2.5, 10.0.0.7", "user-agent": "Mozilla/5.0 (X11)" },
		});

		await fetch(`${server.host}/`, { headers: { "user-agent": "Mozilla/5.0 (X11)" } });

		assert.deepEqual(seen[0], { clientIp: "198.51.100.5", userAgent: "Mozilla/5.0 (X11)" });
		assert.deepEqual(seen[1], { clientIp: "192.0.2.5", userAgent: "Mozilla/5.0 (X11)" });
		assert.equal(seen[2].clientIp, "127.0.0.1", "the socket address is the fallback");
	} finally {
		await server.close();
	}
});

test("visitorFromWebRequest reads a WHATWG Request", () => {
	const cloudflare = new Request("https://example.com/", {
		headers: {
			"cf-connecting-ip": "198.51.100.5",
			"x-forwarded-for": "192.0.2.5",
			"user-agent": "Mozilla/5.0 (iPhone)",
		},
	});

	assert.deepEqual(visitorFromWebRequest(cloudflare), {
		clientIp: "198.51.100.5",
		userAgent: "Mozilla/5.0 (iPhone)",
	});

	const forwarded = new Request("https://example.com/", {
		headers: { "x-forwarded-for": "192.0.2.5, 10.0.0.7", "user-agent": "Mozilla/5.0 (iPhone)" },
	});

	assert.deepEqual(visitorFromWebRequest(forwarded), {
		clientIp: "192.0.2.5",
		userAgent: "Mozilla/5.0 (iPhone)",
	});
});

test("what the helper read is what the request carries", async () => {
	const server = await startServer(accepted);

	try {
		const inbound = new Request("https://example.com/pricing", {
			headers: { "x-forwarded-for": "192.0.2.5, 10.0.0.7", "user-agent": "Mozilla/5.0 (iPhone)" },
		});

		await client(server.host).pageview({
			url: "https://example.com/pricing",
			...visitorFromWebRequest(inbound),
		});

		assert.equal(server.requests[0].headers["x-forwarded-for"], "192.0.2.5");
		assert.equal(server.requests[0].headers["user-agent"], "Mozilla/5.0 (iPhone)");
	} finally {
		await server.close();
	}
});

test("the require entry exposes the same surface as the import entry", async () => {
	const require = createRequire(import.meta.url);
	const cjs = require("../src/index.cjs");

	assert.equal(typeof cjs.FeasibleClient, "function");
	assert.equal(cjs.FeasibleClient, FeasibleClient);
	assert.equal(cjs.visitorFromNodeRequest, visitorFromNodeRequest);
	assert.equal(cjs.createClient, createClient);
	assert.equal(cjs.FeasibleValidationError, FeasibleValidationError);
});

test("the exports map, main, module and types all point at files that exist", async () => {
	const require = createRequire(import.meta.url);
	const { existsSync } = await import("node:fs");
	const { fileURLToPath } = await import("node:url");
	const { dirname, join } = await import("node:path");

	const root = join(dirname(fileURLToPath(import.meta.url)), "..");
	const manifest = require("../package.json");

	const paths = [
		manifest.main,
		manifest.module,
		manifest.types,
		manifest.exports["."].types,
		manifest.exports["."].import,
		manifest.exports["."].require,
		manifest.exports["."].default,
	];

	for (const path of paths) {
		assert.ok(existsSync(join(root, path)), `${path} is declared in package.json but not shipped`);
	}

	assert.equal(manifest.sideEffects, false);
	assert.equal(manifest.engines.node, ">=18");
	assert.deepEqual(Object.keys(manifest.exports["."]), ["types", "import", "require", "default"]);
});
