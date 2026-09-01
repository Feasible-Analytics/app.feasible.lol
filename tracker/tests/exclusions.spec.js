//
// exclusions.spec.js
// Who we refuse to count, and the edge cases that used to take the script down with them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { test, expect } from "@playwright/test";
import { collect, named, settledCount, stubOutbound } from "./helpers.js";

// warnings collects what the script says about itself. A tracker that fails
// quietly is indistinguishable from one that was never installed, which is the
// single most expensive support conversation in this product category — so the
// warning is part of the contract, not a debugging aid.
function warnings(page) {
	const messages = [];

	page.on("console", (message) => {
		if (message.type() === "warning") messages.push(message.text());
	});

	return messages;
}

// droppedPageview calls the public pageview API and fails with a visible
// sentinel if a known client-side drop does not answer promptly.
async function droppedPageview(page) {
	return page.evaluate(
		() =>
			new Promise((resolve) => {
				window.feasible("pageview", { callback: resolve });
				setTimeout(() => resolve("timed out"), 2000);
			}),
	);
}

test("an excluded path sends no pageview", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/exclude.html");
	await page.waitForTimeout(600);

	expect(state.events).toHaveLength(0);
});

// The privacy hole. A customer excludes /order/* to keep order ids out of the
// dashboard, and every one of those ids still arrives attached to a custom
// event's URL.
test("an excluded path suppresses custom events too", async ({ page }) => {
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/exclude.html");

	await page.click("#custom");
	await page.waitForTimeout(500);

	expect(state.events).toHaveLength(0);

	await page.click("#outbound");
	await page.waitForURL("https://example.com/from-excluded");
	await page.waitForTimeout(300);

	expect(state.events).toHaveLength(0);
});

// The callback is answered immediately when we are the reason nothing was sent.
// Leaving a signup form waiting for a timeout we could have ended is a page we
// made worse.
test("an excluded page answers the callback rather than leaving it hanging", async ({ page }) => {
	await collect(page);

	await page.goto("/exclude.html");

	const result = await page.evaluate(
		() =>
			new Promise((resolve) => {
				window.feasible("Order Placed", { callback: (r) => resolve(r) });
				setTimeout(() => resolve("timed out"), 2000);
			}),
	);

	expect(result).toEqual({ status: null });
	expect(await droppedPageview(page)).toEqual({ status: null });
});

// hash x exclusions. Matching the pathname alone is why `/#/patients/**` never
// matched anything: customers believed their sensitive hash routes were
// excluded, and every one of those URLs was still being sent.
test("hash x exclusions: the pattern is matched against the path and the hash", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/hash.html");
	await settledCount(state, "pageview", 1);

	await page.click("#private");
	await page.waitForTimeout(600);

	// The excluded hash route produced nothing.
	expect(named(state, "pageview")).toHaveLength(1);

	// And a custom event fired from it produces nothing either, which is the
	// whole point of excluding it.
	await page.click("#custom");
	await page.waitForTimeout(400);

	expect(named(state, "Hash Custom")).toHaveLength(0);
});

test("hash x exclusions: a hash route that is not excluded still counts", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/hash.html");
	await settledCount(state, "pageview", 1);

	await page.click("#assign");
	await settledCount(state, "pageview", 2);

	await page.click("#custom");
	await settledCount(state, "Hash Custom", 1);
});

// The default that keeps a developer's own reloads out of production numbers.
// The documented consequence is that a Capacitor, Cordova or Electron shell
// serves its pages from localhost and records nothing until the flag is set.
test("localhost is not counted without the opt-in", async ({ page }) => {
	const state = await collect(page);
	const messages = warnings(page);

	await page.addInitScript(() => {
		window.__feasible = { track: 1 };
	});

	await page.goto("/plain.html");
	await page.waitForTimeout(600);

	expect(state.events).toHaveLength(0);
	expect(messages.join(" ")).toContain("localhost");
});

test("an automated browser is not counted, and says so", async ({ page }) => {
	const state = await collect(page);
	const messages = warnings(page);

	await page.addInitScript(() => {
		window.__feasible = { track: 0 };
	});

	await page.goto("/basic.html");
	await page.waitForTimeout(600);

	expect(state.events).toHaveLength(0);
	expect(messages.join(" ")).toContain("automated");
	expect(await droppedPageview(page)).toEqual({ status: null });
});

test("denied consent drops pageviews and answers their callbacks", async ({ page }) => {
	const state = await collect(page);
	const messages = warnings(page);

	await page.addInitScript(() => {
		window.__feasible = { consent: false };
	});
	await page.goto("/basic.html");

	expect(await droppedPageview(page)).toEqual({ status: null });
	expect(state.events).toHaveLength(0);
	expect(messages.join(" ")).toContain("excluded");
});

test("Do Not Track drops pageviews and answers their callbacks", async ({ page }) => {
	const state = await collect(page);
	const messages = warnings(page);

	await page.addInitScript(() => {
		Object.defineProperty(navigator, "doNotTrack", { configurable: true, value: "1" });
	});
	await page.goto("/basic.html");

	expect(await droppedPageview(page)).toEqual({ status: null });
	expect(state.events).toHaveLength(0);
	expect(messages.join(" ")).toContain("excluded");
});

test("a missing domain drops pageviews and answers their callbacks", async ({ page }) => {
	const state = await collect(page);
	const messages = warnings(page);

	await page.goto("/missing-domain.html");

	expect(await droppedPageview(page)).toEqual({ status: null });
	expect(state.events).toHaveLength(0);
	expect(messages.join(" ")).toContain("no data-domain");
});

// A widely installed crypto wallet extension injects `window.phantom` into
// every page. Checking it discards every visit from everyone who has the
// extension, which is what happened to the incumbent for as long as it took
// somebody to notice.
test("a wallet extension's window.phantom is not a headless browser", async ({ page }) => {
	const state = await collect(page);

	await page.addInitScript(() => {
		Object.defineProperty(navigator, "webdriver", { configurable: true, get: () => false });

		window.phantom = { solana: { isPhantom: true } };
		window.__feasible = { track: 0 };
	});

	await page.goto("/basic.html");

	await settledCount(state, "pageview", 1);
});

test("the checks that are real headless markers still fire", async ({ page }) => {
	const state = await collect(page);

	await page.addInitScript(() => {
		Object.defineProperty(navigator, "webdriver", { configurable: true, get: () => false });

		window._phantom = {};
		window.__feasible = { track: 0 };
	});

	await page.goto("/basic.html");
	await page.waitForTimeout(600);

	expect(state.events).toHaveLength(0);
});

test("self-exclusion keeps a person's own visits out of the numbers", async ({ page }) => {
	const state = await collect(page);
	const messages = warnings(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => localStorage.setItem("feasible_ignore", "true"));
	await page.reload();
	await page.waitForTimeout(600);

	expect(named(state, "pageview")).toHaveLength(1);
	expect(messages.join(" ")).toContain("excluded");
});

// window.localStorage does not return null when storage is unavailable, it
// throws — in Chrome Incognito inside an iframe, under a block-third-party-
// cookies setting, and in a sandboxed frame. An unguarded read does not lose
// the self-exclusion feature, it kills the script on line one and the site
// sends nothing at all.
test("storage that throws costs the outbox, not the page", async ({ page }) => {
	const state = await collect(page);

	await page.addInitScript(() => {
		Object.defineProperty(window, "localStorage", {
			configurable: true,
			get() {
				throw new DOMException("access is denied for this document", "SecurityError");
			},
		});
	});

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	// And everything downstream of it still works.
	await page.click("#custom");
	await settledCount(state, "Custom Event", 1);
});
