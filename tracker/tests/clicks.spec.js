//
// clicks.spec.js
// Outbound links, downloads, tagged events and forms — without breaking the page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// The single most important thing the click handler does is nothing. Because
// the request is sent with keepalive the browser finishes it after the page has
// gone, so there is never a reason to hold a navigation up — and holding it up
// is what produced every bug this file is about.

import { test, expect } from "@playwright/test";
import { collect, named, settledCount, stubOutbound, waitFor } from "./helpers.js";

test("an outbound link is recorded and still navigates", async ({ page }) => {
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#outbound");
	await page.waitForURL("https://example.com/pricing");

	await settledCount(state, "Outbound Link: Click", 1);

	expect(named(state, "Outbound Link: Click")[0].p).toEqual({
		url: "https://example.com/pricing",
	});
});

// Ctrl or Cmd clicking an outbound link stopped opening a new tab, because the
// tracker cancelled the click and assigned to window.location instead.
test("a modifier-click opens a new tab and leaves the page alone", async ({ page, context }) => {
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	const opened = context.waitForEvent("page", { timeout: 5000 });
	await page.click("#outbound", { modifiers: ["ControlOrMeta"] });

	const tab = await opened;
	await tab.close();

	expect(page.url()).toContain("/basic.html");
	await settledCount(state, "Outbound Link: Click", 1);
});

// `<a target="_top">` inside an iframe stopped escaping the frame, because the
// interception replaced the browser's own target handling with a bare
// assignment to location. Honouring target means never touching it.
test("a link with a target opens where it says and is still recorded", async ({ page, context }) => {
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	const opened = context.waitForEvent("page", { timeout: 5000 });
	await page.click("#outbound-blank");

	const tab = await opened;
	await tab.close();

	expect(page.url()).toContain("/basic.html");
	await settledCount(state, "Outbound Link: Click", 1);
});

// Something else has already claimed this click — a lightbox, a router, a
// confirm dialog — so it is not the navigation it appears to be, and counting
// it reports click-throughs that never happened.
test("a click another script has claimed is not a click-through", async ({ page }) => {
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#lightbox");
	await page.waitForTimeout(500);

	expect(named(state, "Outbound Link: Click")).toHaveLength(0);
	expect(page.url()).toContain("/basic.html");
});

test("an internal link is not an outbound click", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#internal");
	await page.waitForURL("**/spa.html");
	await page.waitForTimeout(300);

	expect(named(state, "Outbound Link: Click")).toHaveLength(0);
});

// mailto: and tel: are not page navigations and belong to whatever the site
// tags them as.
test("a mailto link is left alone", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#mailto");
	await page.waitForTimeout(500);

	expect(named(state, "Outbound Link: Click")).toHaveLength(0);
});

test("a link to a file is a download", async ({ page }) => {
	const state = await collect(page);

	page.on("download", (download) => download.cancel().catch(() => {}));

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#download");
	await settledCount(state, "File Download", 1);

	expect(named(state, "File Download")[0].p.url).toContain("/files/report.pdf");
});

// A click lands on whatever is under the cursor — the <svg> inside a <span>
// inside the button that carries the class — so the tagged element is routinely
// several levels above the event target.
test("a tagged element is found from the icon inside it", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	// Clicking the SVG itself, which is also the element whose className is not
	// a string and would throw if it were read that way.
	await page.click("#tagged-icon", { force: true });
	await settledCount(state, "Signup", 1);

	expect(named(state, "Signup")[0].p).toEqual({ plan: "pro" });
});

test("hash x outbound links: an outbound click still counts under hash routing", async ({ page }) => {
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/hash.html");
	await settledCount(state, "pageview", 1);

	await page.click("#outbound");
	await page.waitForURL("https://example.com/from-hash");

	await settledCount(state, "Outbound Link: Click", 1);

	expect(named(state, "Outbound Link: Click")[0].p).toEqual({
		url: "https://example.com/from-hash",
	});
});

test("a form submission is recorded", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/forms.html");
	await settledCount(state, "pageview", 1);

	await page.click("#plain button");
	await page.waitForURL("**/basic.html?email=*");

	await settledCount(state, "Form: Submission", 1);
});

test("a tagged form reports the name it was tagged with, once", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/forms.html");
	await settledCount(state, "pageview", 1);

	await page.click("#multi button[value=save]");
	await page.waitForURL("**/basic.html?*");

	const events = await settledCount(state, "Article Saved", 1);

	// One logical event: navigation may replay the durable body before the old
	// page sees its 202, but a click-plus-submit bug would create two UUIDs.
	expect(new Set(events.map((event) => event.k)).size).toBe(1);
});

// form.submit() drops the clicked button's name and value, which silently makes
// "save" and "save and publish" indistinguishable on the server — and it is
// shadowed entirely by the hidden field named "submit" on this fixture, so the
// call throws "not a function". requestSubmit(event.submitter) has neither
// problem. This test forces the path that has to resubmit at all.
test("without keepalive the form is resubmitted with its submitter intact", async ({ page }) => {
	const state = await collect(page);

	await page.addInitScript(() => {
		window.__feasible = { nk: 1 };
	});

	await page.goto("/forms.html");
	await settledCount(state, "pageview", 1);

	await page.click("#multi button[value=publish]");
	await page.waitForURL("**/basic.html?*");

	// The submitter survived, so the server can still tell which button was
	// pressed.
	expect(page.url()).toContain("action=publish");
	expect(page.url()).toContain("title=A+title");

	const events = await settledCount(state, "Article Saved", 1);
	expect(events).toHaveLength(1);
});
