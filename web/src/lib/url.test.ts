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
	// The damaged behavior parameters are gone. The range travels on every URL
	// now, so that a stored period can never be mistaken for an absent one.
	assert.equal(href(state), "/dashboard/site.test?period=28d&compare=previous_period");
});

test("an explicit period in the URL beats the remembered one", () => {
	// The guarantee a shared link depends on: whatever this reader last chose
	// for themselves, a link that names a period shows the period it names.
	const state = parse(new URL("https://example.test/dashboard/site.test?period=day&compare=off"));

	assert.equal(state.preset, "day");
	assert.equal(state.compare, "off");
});

test("a URL with no range falls back to the documented defaults", () => {
	// Storage is unavailable under the test runner, which is the same path a
	// private window and a blocked-storage browser take. The dashboard has to
	// open on something rather than throw.
	const state = parse(new URL("https://example.test/dashboard/site.test"));

	assert.equal(state.preset, "28d");
	assert.equal(state.compare, "previous_period");
});

test("the period survives a round trip through the URL", () => {
	// The reload half of remembering a selector. Writing the default out is what
	// makes a deliberate choice of it readable as a choice.
	const chosen = parse(new URL("https://example.test/dashboard/site.test?period=day"));
	const roundTripped = parse(new URL("https://example.test" + href(chosen)));

	assert.equal(roundTripped.preset, "day");
});

test("a malformed percent-encoding in the path opens a dashboard rather than throwing", () => {
	const state = parse(new URL("https://example.test/dashboard/%C0%80"));

	assert.equal(state.domain, "%C0%80");
	assert.equal(state.preset, "28d");
});
