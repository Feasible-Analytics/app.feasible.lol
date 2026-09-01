//
// vitals.spec.js
// The optional Core Web Vitals collector, in a real browser.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { expect, test } from "@playwright/test";

import { collect, named, settledCount, waitFor } from "./helpers.js";

// hide puts the page in the state the collector reports from. Cumulative layout
// shift and interaction to next paint are not final until the visitor has
// stopped looking at the page, so nothing is sent before this.
async function hide(page) {
	await page.evaluate(() => {
		Object.defineProperty(document, "visibilityState", {
			value: "hidden",
			configurable: true,
		});
		document.dispatchEvent(new Event("visibilitychange"));
	});
}

// show restores the synthetic visibility state before another lifecycle phase.
async function show(page) {
	await page.evaluate(() => {
		Object.defineProperty(document, "visibilityState", {
			value: "visible",
			configurable: true,
		});
		document.dispatchEvent(new Event("visibilitychange"));
	});
}

// nextPaint lets the interaction timing entry reach its defining paint without
// hiding behind a fixed sleep that could pass before or long after the browser
// actually produced the metric.
async function nextPaint(page) {
	await page.evaluate(
		() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
	);
}

// injectMetric puts one deterministic measurement into the generated module's
// real pending map. Browser-produced values cover lifecycle behavior below;
// this helper isolates route privacy from whether an experimental entry type is
// enabled in the Playwright build running the suite.
async function injectMetric(page, url, value, flush = true) {
	await page.evaluate(
		async ({ capturedURL, capturedValue, shouldFlush }) => {
			const vitals = await import("/js/vitals.js");
			vitals.__collectForTest({
				name: "LCP",
				id: `test-${capturedValue}`,
				value: capturedValue,
				navigationType: "soft-navigation",
				navigationId: capturedValue,
				navigationURL: capturedURL,
			});
			if (shouldFlush) vitals.__flushForTest();
		},
		{ capturedURL: url, capturedValue: value, shouldFlush: flush },
	);
}

// flushInjected finalizes metrics held by injectMetric after a route change.
async function flushInjected(page) {
	await page.evaluate(async () => {
		const vitals = await import("/js/vitals.js");
		vitals.__flushForTest();
	});
}

test.describe("Core Web Vitals", () => {
	test("reports the measurements once, when the page is hidden", async ({ page, browserName }) => {
		// This suite currently runs Chromium because it is the browser available
		// in CI. The maintained collector itself feature-detects every API.
		test.skip(browserName !== "chromium", "the vitals entry types are Chromium-only");

		const state = await collect(page);

		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		// Nothing is sent on load. A report before the page is hidden would be
		// a smaller layout shift than the truth.
		expect(named(state, "Web Vitals")).toHaveLength(0);

		await page.click("#interact");
		await nextPaint(page);
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
			expect(["lcp", "cls", "inp", "ttfb"]).toContain(key);
			expect(typeof value).toBe("number");
			expect(Number.isFinite(value)).toBe(true);
			expect(value).toBeGreaterThanOrEqual(0);
		}

		// The interaction was held for eighty milliseconds on purpose, so a
		// collector that measured it reports something in that region.
		expect(event.p.inp).toBeGreaterThan(30);
	});

	test("one embedded script loads the optional generated module", async ({ page }) => {
		const scripts = [];
		page.on("request", (request) => {
			if (request.resourceType() === "script") scripts.push(new URL(request.url()).pathname);
		});

		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		expect(scripts).toEqual(["/js/script.js", "/js/vitals.js"]);
	});

	test("reports once however many times the page is hidden", async ({ page, browserName }) => {
		test.skip(browserName !== "chromium", "the vitals entry types are Chromium-only");

		const state = await collect(page);

		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		await page.click("#interact");
		await nextPaint(page);

		// Hidden, shown and hidden again is one page load. The measurements are
		// cumulative for the life of the document, so a second report would be
		// the same measurement counted as two.
		await hide(page);
		await show(page);
		await hide(page);

		const events = await settledCount(state, "Web Vitals", 1);
		expect(events).toHaveLength(1);
	});

	test("reports an actual bfcache restore as a new observation", async ({ page, browserName }) => {
		test.skip(browserName !== "chromium", "the vitals entry types are Chromium-only");

		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);
		await page.click("#interact");
		await nextPaint(page);

		await Promise.all([page.waitForURL("**/vitals-away.html"), page.click("#away")]);
		await settledCount(state, "Web Vitals", 1);

		await page.goBack();
		const restored = await page.evaluate(() => window.__pageShows.at(-1) === true);
		test.skip(!restored, "Chromium did not retain this document in bfcache");

		await settledCount(state, "pageview", 2);
		await page.click("#interact");
		await nextPaint(page);
		await hide(page);

		const events = await settledCount(state, "Web Vitals", 2);
		expect(events).toHaveLength(2);
		expect(events[1].u).toContain("/vitals.html");
	});

	test("attributes actual SPA soft navigations once per route", async ({ page, browserName }) => {
		test.skip(browserName !== "chromium", "the vitals entry types are Chromium-only");

		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		const supported = await page.evaluate(
			() =>
				PerformanceObserver.supportedEntryTypes.includes("soft-navigation") &&
				typeof globalThis.PerformanceSoftNavigation?.prototype?.getLargestInteractionContentfulPaint === "function",
		);
		test.skip(!supported, "this Chromium build does not expose soft-navigation entries");

		await page.click("#interact");
		await page.click("#navigate");
		expect(page.url()).toContain("/vitals/next");
		await settledCount(state, "pageview", 2);
		await page.click("#interact");
		await nextPaint(page);

		await hide(page);
		const events = await settledCount(state, "Web Vitals", 2);
		const initial = events.filter((event) => event.u.endsWith("/vitals.html"));
		const next = events.filter((event) => event.u.endsWith("/vitals/next"));

		expect(initial).toHaveLength(1);
		expect(next).toHaveLength(1);
		expect(initial[0].p.inp).toBeGreaterThan(30);
	});

	test("an excluded captured route stays excluded after navigating to an allowed route", async ({ page }) => {
		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		await page.evaluate(() => history.pushState({}, "", "/vitals/private"));
		await injectMetric(page, page.url(), 911, false);
		await page.evaluate(() => history.pushState({}, "", "/vitals/allowed"));
		await flushInjected(page);
		await page.waitForTimeout(300);

		expect(named(state, "Web Vitals").some((event) => event.p?.lcp === 911)).toBe(false);
		expect(state.events.some((event) => event.u?.includes("/vitals/private"))).toBe(false);
	});

	test("an allowed captured route stays reportable after navigating to an excluded route", async ({ page }) => {
		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		const allowed = page.url();
		await injectMetric(page, allowed, 922, false);
		await page.evaluate(() => history.pushState({}, "", "/vitals/private"));
		await flushInjected(page);

		await waitFor(state, (events) => events.some((event) => event.p?.lcp === 922), "captured-route vital");
		const event = named(state, "Web Vitals").find((candidate) => candidate.p?.lcp === 922);
		expect(event.u).toBe(allowed);
	});

	test("public consent revocation cancels a pending measurement", async ({ page }) => {
		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		await injectMetric(page, page.url(), 933, false);
		await page.evaluate(() => {
			window.feasible.consent = false;
		});
		await flushInjected(page);
		await page.waitForTimeout(300);

		expect(named(state, "Web Vitals").some((event) => event.p?.lcp === 933)).toBe(false);
	});

	test("bootstrap consent revocation cancels a pending measurement", async ({ page }) => {
		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		await injectMetric(page, page.url(), 944, false);
		await page.evaluate(() => {
			window.__feasible.consent = false;
		});
		await flushInjected(page);
		await page.waitForTimeout(300);

		expect(named(state, "Web Vitals").some((event) => event.p?.lcp === 944)).toBe(false);
	});

	test("Do Not Track is re-evaluated before a pending measurement sends", async ({ page }) => {
		const state = await collect(page);
		await page.goto("/vitals.html");
		await settledCount(state, "pageview", 1);

		await injectMetric(page, page.url(), 955, false);
		await page.evaluate(() => {
			Object.defineProperty(navigator, "doNotTrack", { configurable: true, value: "1" });
		});
		await flushInjected(page);
		await page.waitForTimeout(300);

		expect(named(state, "Web Vitals").some((event) => event.p?.lcp === 955)).toBe(false);
	});

	test("a page without the optional mode sends no vitals at all", async ({ page }) => {
		const state = await collect(page);
		const scripts = [];
		page.on("request", (request) => {
			if (request.resourceType() === "script") scripts.push(new URL(request.url()).pathname);
		});

		await page.goto("/basic.html");
		await settledCount(state, "pageview", 1);

		await hide(page);
		await page.waitForTimeout(500);

		expect(named(state, "Web Vitals")).toHaveLength(0);
		expect(scripts).not.toContain("/js/vitals.js");
	});

	test("an explicit zero sampling rate does not download the optional module", async ({ page }) => {
		const scripts = [];
		page.on("request", (request) => {
			if (request.resourceType() === "script") scripts.push(new URL(request.url()).pathname);
		});
		await page.addInitScript(() => {
			window.__fsc = { v: "0" };
		});

		const state = await collect(page);
		await page.goto("/basic.html");
		await settledCount(state, "pageview", 1);

		expect(scripts).not.toContain("/js/vitals.js");
	});
});
