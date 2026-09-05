//
// client.test.ts
// Capability propagation on dashboard API requests.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test, { before } from "node:test";

import { funnelReport, goalsReport, journeyReport, properties, propertyReport, query } from "./client";
import type { Filter } from "./types";

const capability = "unguessable-shared-link";

// Every test here reads the same shared-link bootstrap, so it is installed
// before any of them rather than inside the first: the client parses the page
// once, and a test that ran alone would otherwise see no capability at all.
before(() => {
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
});

/** TestQueryCarriesSharedCapability proves filter and period requests cannot
 * drop the bearer that limits a shared dashboard to its validated link. */
test("every stats query carries the shared-link capability", async () => {
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

/** Dashboard queries include migrated history by default, and an explicit
 * choice is preserved. Live windows need no exception here: the engine refuses
 * imports on them for every caller, so the browser no longer carries a rule it
 * could forget. */
test("dashboard queries include imports unless the caller says otherwise", async () => {
	const presented: Record<string, unknown>[] = [];
	globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
		presented.push(JSON.parse(String(init?.body)) as Record<string, unknown>);

		return Response.json({ results: [], meta: {}, query: {} });
	}) as typeof fetch;

	await query("example.com", { metrics: ["visitors"], date_range: "28d" });
	await query("example.com", { metrics: ["visitors"], date_range: "realtime" });
	await query("example.com", { metrics: ["visitors"], date_range: "all", include: { imports: false } });

	assert.deepEqual(presented[0]?.include, { imports: true });
	assert.deepEqual(presented[1]?.include, { imports: true });
	assert.deepEqual(presented[2]?.include, { imports: false });
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

	assert.equal(presented, capability);
	assert.match(decodeURIComponent(requested), /date_range="28d"/);
	assert.match(decodeURIComponent(requested), /event:page/);
});

test("enabled property discovery carries the dashboard capability", async () => {
	let requested = "";
	let presented = "";
	globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
		requested = String(input);
		presented = new Headers(init?.headers).get("X-Feasible-Share") ?? "";
		return Response.json({ properties: [{ id: 1, site_id: 1, name: "plan", scope: "event", created_at: 0 }] });
	}) as typeof fetch;

	const found = await properties("example.com");

	assert.equal(requested, "/api/sites/example.com/properties");
	assert.equal(presented, capability);
	assert.equal(found[0]?.name, "plan");
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
	const journey = await journeyReport("example.com", { type: "goal", value: "7" }, "backward", "prefix", [], dashboard);

	assert.deepEqual(presented, [capability, capability, capability]);
	assert.match(requested[0] ?? "", /properties\/plan name\/report/);
	assert.match(requested[1] ?? "", /funnels\/42\/report/);
	assert.match(requested[2] ?? "", /anchor_type=goal/);
	assert.match(requested[2] ?? "", /direction=backward/);
	assert.match(requested[2] ?? "", /grouping=prefix/);
	assert.deepEqual(journey.steps, []);
	assert.deepEqual(journey.trail, []);
	for (const url of requested) {
		assert.match(url, /date_range="28d"/);
		assert.match(url, /visit:country/);
		assert.match(url, /exact=true/);
	}
});
