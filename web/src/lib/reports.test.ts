//
// reports.test.ts
// Regression tests for response enrichments and report dimensions.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { before, test } from "node:test";

import { CARDS, PAGES, SOURCES, dimensionsOf, groupsOf, noticesOf } from "./reports";

// The locale is read from the page once, so the stub is installed before any
// test asks for a formatter.
before(() => {
	globalThis.document = {
		getElementById: () => ({ textContent: JSON.stringify({ locale: "en", messages: {} }) }),
	} as unknown as Document;
});

test("the Sources card offers all five UTM tags behind one button", () => {
	const campaigns = groupsOf(SOURCES).find((group) => group.labelId === "dashboard.group.campaigns");
	assert.ok(campaigns);

	assert.deepEqual(
		campaigns.tabs.map((tab) => tab.dimension),
		[
			"visit:utm_source",
			"visit:utm_medium",
			"visit:utm_campaign",
			"visit:utm_content",
			"visit:utm_term",
		],
	);

	// Every one of them excludes the untagged bucket. Without it the 90-odd
	// percent of traffic carrying no tag is one row swamping the report.
	for (const tab of campaigns.tabs) {
		assert.deepEqual(tab.filters, [["is_not", tab.dimension, [""], { case_sensitive: true }]]);
	}
});

test("a group opens on its first report, and every tab belongs to exactly one group", () => {
	for (const card of CARDS) {
		const groups = groupsOf(card);

		for (const group of groups) {
			assert.equal(group.tab, group.tabs[0]);
		}

		// The header draws one button per group, so a tab that fell out of the
		// grouping would be a report nobody can reach.
		assert.equal(
			groups.reduce((count, group) => count + group.tabs.length, 0),
			card.tabs.length,
		);
	}
});

test("the Pages card keeps captured titles outside its grouping dimensions", () => {
	const pages = PAGES.tabs[0];
	assert.ok(pages);

	assert.deepEqual(dimensionsOf(pages), ["event:page"]);
	assert.deepEqual(dimensionsOf(pages, "visit:country"), ["event:page", "visit:country"]);
	assert.equal(pages.companion?.enrichment, "page_title");
});

test("reports without a companion keep their existing dimension order", () => {
	const entries = PAGES.tabs[1];
	assert.ok(entries);

	assert.deepEqual(dimensionsOf(entries, "visit:country"), ["visit:entry_page", "visit:country"]);
});

test("the Pages heading and its first tab are one string", () => {
	// Splitting them is a reasonable thing to want, and should be a decision
	// rather than two labels quietly disagreeing.
	assert.equal(PAGES.titleId, PAGES.tabs[0]?.labelId);
});

test("one reinterpreted filter reads as one paragraph naming every metric", () => {
	const sentence = "computed over the visits that entered on the matching page";

	assert.deepEqual(
		noticesOf(
			{
				metric_warnings: {
					bounce_rate: { code: "entry_scoped", warning: sentence },
					visit_duration: { code: "entry_scoped", warning: sentence },
					views_per_visit: { code: "entry_scoped", warning: sentence },
				},
			},
			(metric) => metric.replaceAll("_", " "),
		),
		// Intl supplies the separators, which is why the serial comma is there
		// in English and would not be in French.
		[`bounce rate, visit duration, and views per visit: ${sentence}`],
	);
});

test("a warning that is about the data rather than the question is left alone", () => {
	// Sampling has its own badge and its own explainer, and the per-metric
	// facts only read beside the metric they are about. Branching on the code
	// is what the codes are for.
	assert.deepEqual(
		noticesOf({
			metric_warnings: {
				visitors: { code: "sampled", warning: "read from 10% deterministic buckets" },
				pageviews: { code: "partial_bucket", warning: "the last bucket is still filling" },
			},
		}),
		[],
	);
});

test("two different reinterpretations are two paragraphs", () => {
	const notices = noticesOf(
		{
			metric_warnings: {
				bounce_rate: { code: "entry_scoped", warning: "entered on the matching page" },
				visit_duration: { code: "session_scoped", warning: "whole visits containing a matching event" },
				visitors: { code: "sampled", warning: "read from part of the data" },
			},
		},
		(metric) => metric,
	);

	assert.deepEqual(notices, [
		"bounce_rate: entered on the matching page",
		"visit_duration: whole visits containing a matching event",
	]);
});

test("an answer with nothing to say about it produces no notices", () => {
	assert.deepEqual(noticesOf(undefined), []);
	assert.deepEqual(noticesOf({}), []);
	assert.deepEqual(noticesOf({ metric_warnings: {} }), []);
});
