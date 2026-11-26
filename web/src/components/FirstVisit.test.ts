//
// FirstVisit.test.ts
// Tests what a site with no visits yet shows in place of its reports.
//
// Created: 2026-11-26
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { FIRST_VISIT_REQUEST, FirstVisit, firstVisitStatusURL } from "./FirstVisit";

// The bootstrap is read once per bundle, and the catalogue travels in it.
globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			sites: ["example.com"],
			locale: "en",
			version: "dev",
			messages: {
				"dashboard.waiting.title": "No visits yet",
				"dashboard.waiting.body": "{site} hasn't recorded a visit yet. Paste this snippet.",
				"dashboard.waiting.body_viewer": "{site} hasn't recorded a visit yet. Its reports show here later.",
				"dashboard.waiting.copy": "Copy snippet",
				"dashboard.waiting.install": "More ways to install",
				"dashboard.waiting.live": "This page checks every few seconds.",
			},
		}),
	}),
} as unknown as Document;

const SNIPPET = '<script defer src="https://app.example/js/abc.js"></script>';

test("an owner gets the snippet, a copy button and the install screen", () => {
	const markup = renderToStaticMarkup(
		createElement(FirstVisit, {
			waiting: { domain: "example.com", install_url: "/onboarding/7", snippet: SNIPPET },
			onArrived: () => {},
		}),
	);

	assert.match(markup, /No visits yet/);
	assert.match(markup, /example\.com hasn&#x27;t recorded a visit yet\. Paste this snippet\./);
	assert.match(markup, /&lt;script defer src=&quot;https:\/\/app\.example\/js\/abc\.js&quot;&gt;/);
	assert.match(markup, />Copy snippet<\/button>/);
	assert.match(markup, /<a href="\/onboarding\/7"[^>]*>More ways to install<\/a>/);
	assert.match(markup, /This page checks every few seconds\./);
});

test("a reader who cannot install the site gets the message and nothing to paste", () => {
	const markup = renderToStaticMarkup(
		createElement(FirstVisit, { waiting: { domain: "example.com" }, onArrived: () => {} }),
	);

	assert.match(markup, /Its reports show here later\./);
	assert.doesNotMatch(markup, /<pre/);
	assert.doesNotMatch(markup, /Copy snippet/);
	assert.doesNotMatch(markup, /\/onboarding\//);
	assert.match(markup, /This page checks every few seconds\./);
});

test("a reader who can install the site is told by the install screen's uncached endpoint", () => {
	assert.equal(firstVisitStatusURL({ domain: "example.com", install_url: "/onboarding/7", snippet: SNIPPET }), "/onboarding/7/status");
});

test("a reader who cannot install the site falls back to every visit the site has ever had", () => {
	assert.equal(firstVisitStatusURL({ domain: "example.com" }), "");
	assert.deepEqual(FIRST_VISIT_REQUEST, { metrics: ["visitors"], date_range: "all" });
});
