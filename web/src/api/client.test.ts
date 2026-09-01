//
// client.test.ts
// Capability propagation on dashboard API requests.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import { query } from "./client";

/** TestQueryCarriesSharedCapability proves filter and period requests cannot
 * drop the bearer that limits a shared dashboard to its validated link. */
test("every stats query carries the shared-link capability", async () => {
	const capability = "unguessable-shared-link";

	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				sites: ["example.com"],
				locale: "en",
				messages: {},
				shared: {
					mode: "share",
					base: "/share/" + capability,
					domain: "example.com",
					capability,
					embed: false,
					storage: true,
				},
			}),
		}),
	} as unknown as Document;

	let presented = "";
	globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
		presented = new Headers(init?.headers).get("X-Feasible-Share") ?? "";

		return new Response(JSON.stringify({ results: [], meta: {}, query: {} }), {
			status: 200,
			headers: { "Content-Type": "application/json" },
		});
	}) as typeof fetch;

	await query("example.com", {
		metrics: ["visitors"],
		date_range: "28d",
		filters: [["event:page", "is", ["/pricing"]]],
	});

	assert.equal(presented, capability);
});
