//
// vitals.spec.js
// The optional Core Web Vitals collector, in a real browser.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { expect, test } from "@playwright/test";

import { collect, named, settledCount } from "./helpers.js";

// hide puts the page in the state the collector reports from. Cumulative layout
// shift and interaction to next paint are not final until the visitor has
// stopped looking at the page, so nothing is sent before this.
async function hide(page) {
	await page.evaluate(() => {
		Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
		document.dispatchEvent(new Event("visibilitychange"));
	});
}

test.describe("Core Web Vitals", () => {
	test("reports the measurements once, when the page is hidden", async ({ page, browserName }) => {
		// The Performance API entry types this reads are Chromium's. Reporting
		// nothing elsewhere is the honest outcome and is what the collector
		// does: a browser missing an entry type contributes no measurement
		// rather than a zero that would drag an average down.
		test.skip(browserName !== "chromium", "the vitals entry types are Chromium-only");

		const state = await collect(page);

		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		// Nothing is sent on load. A report before the page is hidden would be
		// a smaller layout shift than the truth.
		expect(named(state, "Web Vitals")).toHaveLength(0);

		await page.click("#interact");

		// The interaction's entry is delivered on a later frame, so the report
		// has to be given a moment to have something to report.
		await page.waitForTimeout(300);

		await hide(page);

		const events = await settledCount(state, "Web Vitals", 1);
		expect(events).toHaveLength(1);

		const event = events[0];

		// Not interactive: a measurement is not something the visitor did, so
		// it must never end a bounce.
		expect(event.i).toBe(false);

		// The URL it was measured on travels with it, which is what makes the
		// numbers break down by page like every other event.
		expect(event.u).toContain("/vitals.html");

		// Every measurement is a numeric property, so it aggregates through the
		// same sum, average and percentile the query engine gives every other
		// custom property.
		expect(Object.keys(event.p).length).toBeGreaterThan(0);

		for (const [key, value] of Object.entries(event.p)) {
			expect(["lcp", "cls", "inp", "ttfb", "fcp"]).toContain(key);
			expect(typeof value).toBe("number");
			expect(Number.isFinite(value)).toBe(true);
			expect(value).toBeGreaterThanOrEqual(0);
		}

		// The interaction was held for eighty milliseconds on purpose, so a
		// collector that measured it reports something in that region.
		expect(event.p.inp).toBeGreaterThan(30);
	});

	test("reports once however many times the page is hidden", async ({ page, browserName }) => {
		test.skip(browserName !== "chromium", "the vitals entry types are Chromium-only");

		const state = await collect(page);

		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		await page.click("#interact");

		// Hidden, shown and hidden again is one page load. The measurements are
		// cumulative for the life of the document, so a second report would be
		// the same measurement counted as two.
		await hide(page);
		await page.evaluate(() => {
			Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
			document.dispatchEvent(new Event("visibilitychange"));
		});
		await hide(page);

		const events = await settledCount(state, "Web Vitals", 1);
		expect(events).toHaveLength(1);
	});

	test("a page without the second script sends no vitals at all", async ({ page }) => {
		const state = await collect(page);

		await page.goto("/basic.html");
		await settledCount(state, "pageview", 1);

		await hide(page);
		await page.waitForTimeout(500);

		expect(named(state, "Web Vitals")).toHaveLength(0);
	});
});
