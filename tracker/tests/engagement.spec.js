//
// engagement.spec.js
// Scroll depth and engaged time, measured without polling and without lying.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { test, expect } from "@playwright/test";
import { collect, named, settledCount, waitFor } from "./helpers.js";

// leave is what actually flushes a measurement: the visitor's attention going
// somewhere else. Nothing is reported mid-read, because a stream of events
// while somebody is still on the page is the thing that makes a tracker
// expensive.
async function leave(page) {
	await page.evaluate(() => window.dispatchEvent(new Event("blur")));
}

// comeBack is the other half of the alt-tab round trip.
async function comeBack(page) {
	await page.evaluate(() => window.dispatchEvent(new Event("focus")));
}

test("scrolling down reports a scroll depth", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
	await leave(page);

	await settledCount(state, "engagement", 1);
	const [engagement] = named(state, "engagement");

	expect(engagement.sd).toBeGreaterThan(90);
	expect(engagement.sd).toBeLessThanOrEqual(100);

	// An engagement is a measurement, not an interaction. Marking it otherwise
	// is what would make every bounce look like an engaged visit.
	expect(engagement.i).toBe(false);
	expect(engagement.u).toContain("/engagement.html");
});

// The deepest point reached, not the deepest point the scroll listener happened
// to hear about. A scroll event is delivered at the next rendering opportunity,
// so scrolling and leaving in one task is the case where a flush can outrun its
// own input — and it under-reported in every engine, not just the slow ones.
test("a flush in the same frame as the scroll still reports the depth", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => {
		window.scrollTo(0, document.documentElement.scrollHeight);
		window.dispatchEvent(new Event("blur"));
	});

	await settledCount(state, "engagement", 1);

	expect(named(state, "engagement")[0].sd).toBeGreaterThan(90);
});

test("time on the page is reported once it is worth reporting", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	await page.waitForTimeout(3500);
	await leave(page);

	await settledCount(state, "engagement", 1);

	const total = named(state, "engagement").reduce((sum, event) => sum + (event.e || 0), 0);

	expect(total).toBeGreaterThanOrEqual(3000);

	// The reported time is the delta since the last report, not a wall-clock
	// timestamp. The incumbent computed `Date.now() - null` here and reported
	// about 1.7 trillion milliseconds as a reading time.
	expect(total).toBeLessThan(60000);
});

// Alt-tabbing to another application leaves the tab visible, so a visibility
// listener alone never fires and the clock keeps running while the person is
// somewhere else entirely. That single missing listener is the difference
// between reporting three seconds and reporting a minute and three seconds.
test("the clock pauses on blur, not only on a visibility change", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	await page.waitForTimeout(300);
	await leave(page);

	// Four seconds in another window. None of it is reading time.
	await page.waitForTimeout(4000);

	await comeBack(page);
	await page.waitForTimeout(200);
	await leave(page);

	await page.waitForTimeout(300);

	const total = named(state, "engagement").reduce((sum, event) => sum + (event.e || 0), 0);

	expect(total).toBeLessThan(2000);
});

// Reading the page height once at load and never again reports a scroll depth
// against a page that no longer exists. A ResizeObserver is how that is kept
// current without forcing a synchronous layout on a timer, which is what shows
// up as an INP failure attributed to us.
test("content that appears after load is measured", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	// The page grows by half again, before anybody has scrolled.
	await page.evaluate(() => window.grow());
	await page.waitForTimeout(200);

	const stopAt = await page.evaluate(() => {
		const height = document.documentElement.scrollHeight;
		const target = height / 2 - window.innerHeight;

		window.scrollTo(0, target);

		return { height, target };
	});

	expect(stopAt.height).toBeGreaterThan(8000);

	await leave(page);
	await settledCount(state, "engagement", 1);

	// Halfway down the grown page. A stale height from before the growth would
	// report roughly three quarters instead.
	expect(named(state, "engagement")[0].sd).toBeLessThan(62);
});

test("nothing new to report means nothing is sent", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => window.scrollTo(0, 4000));
	await leave(page);
	await settledCount(state, "engagement", 1);

	const first = named(state, "engagement").length;

	// Straight back and away again, with no scrolling and no time passing.
	await comeBack(page);
	await leave(page);
	await page.waitForTimeout(400);

	expect(named(state, "engagement")).toHaveLength(first);
});

// manual x engagement. The incumbent's manual build dropped engagement along
// with the automatic pageview, so a site that took control of its own pageviews
// quietly lost its scroll depth and time on page too.
test("manual x engagement: engagement follows a manually tracked pageview", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/manual.html");

	// Nothing has been tracked, so there is no page for a measurement to belong
	// to and none is sent.
	await page.evaluate(() => window.scrollTo(0, 2000));
	await leave(page);
	await page.waitForTimeout(400);

	expect(named(state, "engagement")).toHaveLength(0);

	await comeBack(page);
	await page.click("#pageview");
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
	await leave(page);

	await settledCount(state, "engagement", 1);

	const [engagement] = named(state, "engagement");

	expect(engagement.sd).toBeGreaterThan(50);
	expect(engagement.u).toContain("/manual.html");
});
