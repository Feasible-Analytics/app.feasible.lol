//
// url.test.ts
// Shareable dashboard route state, including behavior analysis selections.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import { href, parse } from "./url";

globalThis.document = { getElementById: () => null } as unknown as Document;

test("behavior analysis selections survive a dashboard URL round trip", () => {
	const url = new URL("https://example.test/dashboard/site.test?analysis=explore&property=plan&funnel=12&explore_type=goal&explore_value=7&explore_label=Checkout&explore_direction=backward&explore_grouping=prefix&explore_trail=%5B%7B%22type%22%3A%22page%22%2C%22value%22%3A%22%2Fpricing%22%7D%5D");
	const state = parse(url);

	assert.deepEqual(state.behavior, {
		tab: "explore",
		property: "plan",
		funnel: 12,
		exploreAnchor: { type: "goal", value: "7", label: "Checkout" },
		exploreDirection: "backward",
		exploreGrouping: "prefix",
		exploreTrail: [{ type: "page", value: "/pricing", label: undefined }],
	});

	assert.deepEqual(parse(new URL(`https://example.test${href(state)}`)).behavior, state.behavior);
});

test("default and damaged behavior selections degrade to the Goals tab", () => {
	const state = parse(new URL("https://example.test/dashboard/site.test?analysis=unknown&funnel=-2&explore_type=script&explore_value=x&explore_trail=broken"));

	assert.deepEqual(state.behavior, {
		tab: "goals",
		property: "",
		funnel: 0,
		exploreAnchor: null,
		exploreDirection: "forward",
		exploreGrouping: "exact",
		exploreTrail: [],
	});
	assert.equal(href(state), "/dashboard/site.test");
});
