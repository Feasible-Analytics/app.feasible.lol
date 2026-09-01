//
// client.test.ts
// Capability propagation on dashboard API requests.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import { funnelReport, goalsReport, journeyReport, propertyReport, query } from "./client";
import type { Filter } from "./types";

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

test("the goals report carries capabilities, filters, and the selected period", async () => {
	let requested = "";
	let presented = "";
	globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
		requested = String(input);
		presented = new Headers(init?.headers).get("X-Feasible-Share") ?? "";

		return new Response(JSON.stringify({ rows: [], visitors: 0, visits: 0, from: "", to: "" }), {
			status: 200,
			headers: { "Content-Type": "application/json" },
		});
	}) as typeof fetch;

	await goalsReport("example.com", {
		dateRange: "28d",
		filters: [["is", "event:page", ["/pricing"]]],
	});

	assert.equal(presented, "unguessable-shared-link");
	assert.match(decodeURIComponent(requested), /date_range="28d"/);
	assert.match(decodeURIComponent(requested), /event:page/);
});

/** Behavior report clients share the dashboard range, filter, exactness, and
 * capability contract even though their resource selectors differ. */
test("property, funnel, and journey reports keep the dashboard query contract", async () => {
	const requested: string[] = [];
	const presented: string[] = [];
	globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
		requested.push(decodeURIComponent(String(input)));
		presented.push(new Headers(init?.headers).get("X-Feasible-Share") ?? "");

		const url = String(input);
		if (url.includes("/properties/")) return Response.json({ property: {}, rows: [], from: "", to: "" });
		if (url.includes("/funnels/")) return Response.json({ funnel: {}, steps: [], from: "", to: "", partial: false });

		return Response.json({
			anchor: { type: "page", value: "/pricing" },
			direction: "forward",
			next_pages: [],
			previous_pages: [],
			next_events: [],
			previous_events: [],
			views: 0,
			visitors: 0,
			from: "",
			to: "",
		});
	}) as typeof fetch;

	const dashboard = {
		dateRange: "28d" as const,
		filters: [["is", "visit:country", ["US"]] as Filter],
		exact: true,
	};

	await propertyReport("example.com", "plan name", dashboard);
	await funnelReport("example.com", 42, dashboard);
	const journey = await journeyReport("example.com", { type: "goal", value: "7" }, "backward", [], dashboard);

	assert.deepEqual(presented, ["unguessable-shared-link", "unguessable-shared-link", "unguessable-shared-link"]);
	assert.match(requested[0] ?? "", /properties\/plan name\/report/);
	assert.match(requested[1] ?? "", /funnels\/42\/report/);
	assert.match(requested[2] ?? "", /anchor_type=goal/);
	assert.match(requested[2] ?? "", /direction=backward/);
	assert.deepEqual(journey.steps, []);
	assert.deepEqual(journey.trail, []);
	for (const url of requested) {
		assert.match(url, /date_range="28d"/);
		assert.match(url, /visit:country/);
		assert.match(url, /exact=true/);
	}
});
