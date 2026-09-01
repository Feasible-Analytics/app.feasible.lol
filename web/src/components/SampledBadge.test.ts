//
// SampledBadge.test.ts
// Regression tests for shared-section exactness.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { renderToStaticMarkup } from "react-dom/server";

import { query, QueryError } from "../api/client";
import type { Sampling } from "../api/types";
import { exactResponsesReady, SampledBadge, samplingExplanation } from "./SampledBadge";

const sampled: Sampling = {
	rate: 0.1,
	reason: "automatic",
	estimated_rows: 10_000_000,
	threshold: 5_000_000,
	event_metrics: ["pageviews", "avg(event:props:lcp)"],
	session_metrics: ["bounce_rate"],
	mixed_metrics: [],
	sparse: false,
	zero_result: false,
	uncertainty: "sampling error is not quantified",
	scaled_metrics: ["pageviews"],
	direct_metrics: ["bounce_rate", "avg(event:props:lcp)"],
};

test("exact is hidden while either response is loading or still sampled", () => {
	assert.equal(exactResponsesReady(true, true, undefined, undefined), false);
	assert.equal(exactResponsesReady(true, false, undefined, sampled), false);
	assert.equal(exactResponsesReady(true, false, sampled, undefined), false);
});

test("exact is shown only after every response is exact", () => {
	assert.equal(exactResponsesReady(true, false, undefined, undefined), true);
	assert.equal(exactResponsesReady(false, false, undefined, undefined), false);
});

test("sampling copy distinguishes expanded totals from direct statistics", () => {
	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				locale: "en",
				messages: {
					"dashboard.format.compact.thousand": "{value}k",
					"dashboard.format.compact.million": "{value}M",
					"dashboard.format.compact.billion": "{value}B",
					"dashboard.sampled.automatic":
						"Estimated from {percent}% deterministic buckets at each metric's event or session row grain. Before applying that rate, the raw plan represented about {rows} fact-row reads. Additive totals are expanded; rates, averages, minima, maxima, and percentiles are calculated directly in sampled rows and may differ materially when values are skewed. No confidence interval is claimed.",
					"dashboard.sampled.sparse": "Sparse sample.",
					"dashboard.sampled.zero": "Zero sampled result.",
					"dashboard.sampled.exact_required": "Complete membership is required.",
					"dashboard.sampled.exact_action": "Show exact numbers",
				},
			}),
		}),
	} as unknown as Document;

	const explanation = samplingExplanation(sampled);
	assert.match(explanation, /10M fact-row reads/);
	assert.match(explanation, /Additive totals are expanded/);
	assert.match(explanation, /percentiles are calculated directly in sampled rows/);
	assert.match(explanation, /No confidence interval is claimed/);
	assert.doesNotMatch(explanation, /figures.*scaled back up/i);
});

test("sampling copy discloses sparse and all-zero results", () => {
	const explanation = samplingExplanation({ ...sampled, sparse: true, zero_result: true });
	assert.match(explanation, /Sparse sample/);
	assert.match(explanation, /Zero sampled result/);
});

test("a membership refusal renders an explicit exact action", () => {
	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				locale: "en",
				messages: {
					"dashboard.sampled.exact_required": "Complete membership is required.",
					"dashboard.sampled.exact_action": "Show exact numbers",
				},
			}),
		}),
	} as unknown as Document;

	const markup = renderToStaticMarkup(
		SampledBadge({ sampling: undefined, exact: false, exactFallback: true, onExact: () => undefined }),
	);
	assert.match(markup, /Complete membership is required/);
	assert.match(markup, /<button[^>]*>Show exact numbers<\/button>/);
});

test("the API exposes exact recovery without an automatic retry loop", async () => {
	let requests = 0;
	globalThis.fetch = (async () => {
		requests++;
		return new Response(
			JSON.stringify({ error: "complete membership required", code: "sampling_requires_exact" }),
			{ status: 400, headers: { "Content-Type": "application/json" } },
		);
	}) as typeof fetch;

	await assert.rejects(
		query("example.com", { metrics: ["visitors"], date_range: "7d" }),
		(error: unknown) => error instanceof QueryError && error.code === "sampling_requires_exact",
	);
	assert.equal(requests, 1);
});
