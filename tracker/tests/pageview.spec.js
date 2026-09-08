//
// pageview.spec.js
// Counting a pageview exactly when a human sees a page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// Every test in this file stands for a bug that cost somebody real data, and
// the last few are the *combinations* — hash routing with pushState, manual
// mode with the bfcache — which is where the bugs actually lived, because each
// feature worked perfectly on its own.

import { test, expect } from "@playwright/test";
import { collect, named, settledCount, waitFor } from "./helpers.js";

// hideUntilRevealed makes the page report a visibility state before any of its
// own scripts run, which is the only way to reproduce a prerender or a
// background tab deterministically.
async function hideUntilRevealed(page, value) {
	await page.addInitScript((state) => {
		window.__visibility = state;

		Object.defineProperty(document, "visibilityState", {
			configurable: true,
			get: () => window.__visibility,
		});
	}, value);
}

// reveal puts the page in front of the visitor and tells it so.
async function reveal(page) {
	await page.evaluate(() => {
		window.__visibility = "visible";
		document.dispatchEvent(new Event("visibilitychange"));
	});
}

// restore fires the event a back/forward-cache restore fires. It is synthesised
// rather than driven through a real Back navigation because a real restore
// depends on the browser's own cache eviction, and a test that silently stops
// exercising the listener is worse than no test.
async function restore(page) {
	await page.evaluate(() =>
		window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted: true })),
	);
}

// Chrome's Speculation Rules API is on by default at two of the largest hosts
// in the world, so without this the numbers include pages nobody ever saw.
test("a prerendered page is not counted until somebody looks at it", async ({ page }) => {
	const state = await collect(page);
	await hideUntilRevealed(page, "prerender");

	await page.goto("/basic.html");
	await page.waitForTimeout(400);

	expect(state.events).toHaveLength(0);

	await reveal(page);
	await settledCount(state, "pageview", 1);

	expect(named(state, "pageview")).toHaveLength(1);
});

test("a page opened in a background tab is counted when it is looked at", async ({ page }) => {
	const state = await collect(page);
	await hideUntilRevealed(page, "hidden");

	await page.goto("/basic.html");
	await page.waitForTimeout(400);

	expect(state.events).toHaveLength(0);

	await reveal(page);
	await settledCount(state, "pageview", 1);
});

// A page that is prerendered and then navigated several times before being
// shown was still only ever seen once.
test("a deferred pageview is fired once, not once per deferral", async ({ page }) => {
	const state = await collect(page);
	await hideUntilRevealed(page, "prerender");

	await page.goto("/basic.html");
	await page.evaluate(() => {
		window.feasible("pageview");
		window.feasible("pageview");
	});

	await reveal(page);
	const pageviews = await settledCount(state, "pageview", 1);

	expect(pageviews).toHaveLength(1);
});

// Pressing Back restores a page with no navigation, no load and no script
// execution. Without this listener every visitor who goes back disappears from
// the numbers for the rest of their visit.
test("a bfcache restore fires a pageview with a corrected referrer", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await restore(page);
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews).toHaveLength(2);

	// document.referrer on a restored page is still whatever it was on the
	// original load, and re-sending it credits the original source with a
	// second arrival that never happened.
	expect(pageviews[1].r).toContain("/basic.html");
});

test("pushState is counted", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#push");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].u).toContain("/spa.html/one");
	expect(pageviews[1].t).toBe("One");

	// The page that was left is the referrer. Re-sending the original external
	// referrer on every route change invents a second visit for a visitor who
	// only ever arrived once.
	expect(pageviews[1].r).toContain("/spa.html");
});

// Several routers implement redirects and canonicalisation entirely through
// replaceState, and a tracker that only watches pushState records the URL the
// visitor was redirected away from.
test("replaceState is counted", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#replace");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].u).toContain("/spa.html/two");
});

test("popstate is counted", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#push");
	await settledCount(state, "pageview", 2);

	await page.click("#back");
	const pageviews = await settledCount(state, "pageview", 3);

	expect(pageviews[2].u).toContain("/spa.html");
	expect(pageviews[2].u).not.toContain("/spa.html/one");
});

// Deduplicating on the pathname rather than the full URL is what silently
// swallowed query-string-only navigations and bfcache pageviews.
test("a query-string-only navigation is counted", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#query");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].u).toContain("?page=2");
});

// A page that stores a filter, a sort or a selected tab in the query string
// rewrites the address on every change, and reaches replaceState to do it.
test("a replaceState that only changes the query string is not a pageview", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#replace-query");
	await page.waitForTimeout(400);

	expect(named(state, "pageview")).toHaveLength(1);

	// The next real navigation still counts, and reports the page it came
	// from rather than the address the filter left behind.
	await page.click("#push");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].u).toContain("/spa.html/one");
	expect(pageviews[1].r).not.toContain("filter=open");
});

// Routers routinely call pushState and then replaceState in the same tick.
// Firing synchronously reports the intermediate URL and the previous page's
// title; deduplicating on the resulting URL collapses the pair into the one
// pageview the visitor experienced.
test("a pushState corrected by a replaceState is one pageview, at the final URL", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#push-then-replace");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews).toHaveLength(2);
	expect(pageviews[1].u).toContain("/spa.html/settled");
	expect(pageviews[1].u).not.toContain("interim");
});

test("outside hash mode a fragment jump is not a pageview", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => {
		location.hash = "#section-two";
	});
	await page.waitForTimeout(400);

	// A #section is a jump within one page. Counting it splits a single article
	// into a row per heading anchor.
	expect(named(state, "pageview")).toHaveLength(1);
});

test("hash mode counts a plain hash change", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/hash.html");
	await settledCount(state, "pageview", 1);

	await page.click("#assign");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].u).toContain("#/about");
});

// THE combination. A hash router navigates through pushState, which does not
// fire hashchange, so a tracker listening only for hashchange records exactly
// one pageview for the entire life of the application.
test("hash x pushState: a hash route changed through the History API is counted", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/hash.html");
	await settledCount(state, "pageview", 1);

	await page.click("#push");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].u).toContain("#/settings");
});

test("manual mode sends no pageview of its own", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/manual.html");
	await page.waitForTimeout(500);

	expect(named(state, "pageview")).toHaveLength(0);
});

test("manual mode sends the pageview the site asks for, with a custom URL", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/manual.html");

	await page.click("#pageview");
	await settledCount(state, "pageview", 1);

	await page.click("#custom-url");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[0].u).toContain("/manual.html");
	expect(pageviews[1].u).toContain("/manual.html/step-two");
});

// manual x bfcache. The incumbent's manual build dropped the pageshow listener
// along with the automatic pageview, so a site that took control of its own
// pageviews quietly lost every visitor who pressed Back.
test("manual x bfcache: a restore re-fires only once a pageview was tracked", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/manual.html");

	// Nothing has been tracked yet, so a restore has no pageview to repeat.
	await restore(page);
	await page.waitForTimeout(400);
	expect(named(state, "pageview")).toHaveLength(0);

	await page.click("#pageview");
	await settledCount(state, "pageview", 1);

	await restore(page);
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews).toHaveLength(2);
	expect(pageviews[1].r).toContain("/manual.html");
});

// declareProperties sets the global before any of the page's own scripts run,
// which is the only way to reproduce a site that declares them in an inline
// script above the snippet.
async function declareProperties(page, props) {
	await page.addInitScript((values) => {
		window.__fsp = values;
	}, props);
}

// The snippet's queue is drained after the first pageview on purpose, so a site
// that only used feasible('init') would miss it — and the first pageview is the
// one that becomes the visit's entry properties. This is the assertion that
// ordering makes easy to get wrong.
test("a property declared before the snippet is on the first pageview", async ({ page }) => {
	await declareProperties(page, { plan: "pro" });

	const state = await collect(page);

	await page.goto("/basic.html");
	const [first] = await settledCount(state, "pageview", 1);

	expect(first.p).toEqual({ plan: "pro" });
});

// Declaring a property must not turn one visit into two. Double counting is the
// failure the manual-mode workaround has and this feature exists to avoid.
test("declaring properties still sends exactly one pageview", async ({ page }) => {
	await declareProperties(page, { plan: "pro" });

	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);
	await page.waitForTimeout(300);

	expect(named(state, "pageview")).toHaveLength(1);
});

test("declared properties reach an SPA route change", async ({ page }) => {
	await declareProperties(page, { plan: "pro" });

	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#push");
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].p).toEqual({ plan: "pro" });
});

test("declared properties reach a bfcache restore", async ({ page }) => {
	await declareProperties(page, { plan: "pro" });

	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await restore(page);
	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews[1].p).toEqual({ plan: "pro" });
});

// The runtime call is for a value that arrives after load — an SPA that learns
// the plan from a fetch. It stamps what comes next and cannot reach what has
// already gone, which is the whole reason the pre-snippet global also exists.
test("a runtime init stamps the next pageview and does not resend the last", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/spa.html");
	const [first] = await settledCount(state, "pageview", 1);

	expect(first.p).toBeUndefined();

	await page.evaluate(() => window.feasible("init", { p: { plan: "pro" } }));
	await page.click("#push");

	const pageviews = await settledCount(state, "pageview", 2);

	expect(pageviews).toHaveLength(2);
	expect(pageviews[1].p).toEqual({ plan: "pro" });

	// The one already sent stays as it was sent.
	expect(pageviews[0].p).toBeUndefined();
});

// The same rule as on a custom event, asserted on a pageview too: the two merge
// in different files and only one of them was covered.
test("a property on a pageview call overrides a declared one", async ({ page }) => {
	await declareProperties(page, { plan: "declared", tier: "free" });

	const state = await collect(page);

	await page.goto("/manual.html");
	await page.evaluate(() => window.feasible("pageview", { p: { plan: "named" } }));

	const [sent] = await settledCount(state, "pageview", 1);

	expect(sent.p).toEqual({ plan: "named", tier: "free" });
});

// These ride on every event for the life of the install, so a value the browser
// will not send is not a dropped event — it is a site that goes dark and stays
// dark, with the outbox filling up and nothing reaching us to be counted.
test("an oversized declaration is trimmed rather than taking the site dark", async ({ page }) => {
	await declareProperties(page, { plan: "x".repeat(50_000) });

	const state = await collect(page);

	await page.goto("/basic.html");
	const [first] = await settledCount(state, "pageview", 1);

	expect(first.p.plan).toHaveLength(2000);
});

// A property backed by a getter that throws is a mistake in somebody else's
// code. It must not stop us counting anybody.
test("a declaration that throws while being read is ignored, not fatal", async ({ page }) => {
	await page.addInitScript(() => {
		window.__fsp = Object.defineProperty({}, "plan", {
			enumerable: true,
			get() {
				throw new Error("no");
			},
		});
	});

	const state = await collect(page);

	await page.goto("/basic.html");
	const [first] = await settledCount(state, "pageview", 1);

	expect(first.p).toBeUndefined();
	expect(first.n).toBe("pageview");
});

// Anything that is not a set of properties has to be refused outright. A string
// spreads into one property per character, which is a dashboard full of noise.
test("a declaration that is not an object is refused", async ({ page }) => {
	await declareProperties(page, "pro");

	const state = await collect(page);

	await page.goto("/basic.html");
	const [first] = await settledCount(state, "pageview", 1);

	expect(first.p).toBeUndefined();
});
