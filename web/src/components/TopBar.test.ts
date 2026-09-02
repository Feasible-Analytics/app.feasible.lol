//
// TopBar.test.ts
// Regression tests for the live number's query contract.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { Filter } from "../api/types";
import type { UrlState } from "../lib/url";
import { currentVisitorsRequest, periodLabel } from "./TopBar";

test("the current visitors number always requests an exact answer", () => {
	const filter: Filter = ["is", "visit:country", ["US"]];
	const request = currentVisitorsRequest([filter]);

	assert.equal(request.exact, true);
	assert.deepEqual(request.filters?.[0], filter);
	assert.deepEqual(request.filters?.at(-1), [
		"is_not",
		"event:name",
		["engagement"],
		{ case_sensitive: true },
	]);
});

test("custom dates are friendly on the period button", () => {
	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				locale: "en",
				messages: {
					"dashboard.format.date_long": "{month} {day}, {year}",
					"dashboard.format.range": "{from} – {to}",
				},
			}),
		}),
	} as unknown as Document;

	const state: UrlState = {
		domain: "example.com",
		preset: "28d",
		from: "2026-09-01",
		to: "2026-09-01",
		compare: "off",
		filters: [],
		labels: {},
		drawer: null,
		behavior: {
			tab: "goals",
			property: "",
			funnel: 0,
			exploreAnchor: null,
			exploreDirection: "forward",
			exploreGrouping: "exact",
			exploreTrail: [],
		},
	};

	assert.equal(periodLabel(state), "Sep 1, 2026");
});
