//
// screenshots.spec.js
// Pictures of the tracker working, for the pull request and for the eyes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// These assert almost nothing on purpose. Their job is to leave an artefact a
// person can look at and see that events came out with the right shape, which
// no amount of green ticks conveys.

import { test, expect } from "@playwright/test";
import { collect, settledCount, stubOutbound } from "./helpers.js";

// pinUUIDs keeps the diagnostic artefacts reproducible now that the client
// identity is visible in every captured payload. It is intentionally scoped to
// screenshots; retry tests need the browser's real generator across documents.
async function pinUUIDs(page) {
	await page.addInitScript(() => {
		let sequence = 0;
		Object.defineProperty(crypto, "randomUUID", {
			value: () => `00000000-0000-4000-8000-${String(++sequence).padStart(12, "0")}`,
		});
	});
}

// diagnosticEvents presents logical events rather than timing-dependent retry
// attempts. Engagement milliseconds depend on scheduler load, and an in-flight
// successful body may be replayed harmlessly before its promise clears storage;
// neither belongs in a committed diagnostic image.
function diagnosticEvents(events) {
	const seen = new Set();
	const stable = [];

	for (const event of events) {
		if (event.k && seen.has(event.k)) continue;
		if (event.k) seen.add(event.k);

		const copy = { ...event };
		if ("e" in copy) copy.e = "measured at runtime";
		stable.push(copy);
	}

	return stable;
}

// panel draws the captured events onto the page being photographed, so the
// screenshot shows the cause and the effect in one frame.
async function panel(page, title, events) {
	await page.evaluate(
		({ title, events }) => {
			const box = document.createElement("div");

			box.setAttribute(
				"style",
				[
					"position:fixed",
					"top:0",
					"right:0",
					"width:26rem",
					"max-height:100vh",
					"overflow:auto",
					"z-index:2147483647",
					"background:#0f172a",
					"color:#e2e8f0",
					"font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace",
					"padding:1rem",
					"box-shadow:-2px 0 12px rgba(0,0,0,.35)",
				].join(";"),
			);

			const heading = document.createElement("div");
			heading.setAttribute("style", "font-weight:700;color:#7dd3fc;margin-bottom:.75rem");
			heading.textContent = `${title} — ${events.length} event(s) sent`;
			box.appendChild(heading);

			for (const event of events) {
				const row = document.createElement("pre");
				row.setAttribute(
					"style",
					"margin:0 0 .6rem;white-space:pre-wrap;word-break:break-all;border-left:2px solid #38bdf8;padding-left:.6rem",
				);
				// Stable ids and engagement duration are part of the wire shape but
				// deliberately vary per run. Normalize only their displayed values so
				// screenshots remain reviewable, deterministic artifacts.
				const displayed = {
					...event,
					k: event.k ? "<stable UUID>" : undefined,
					e: event.e === undefined ? undefined : "<milliseconds>",
				};
				row.textContent = JSON.stringify(displayed, null, 1);
				box.appendChild(row);
			}

			document.body.appendChild(box);
		},
		{ title, events },
	);
}

// shoot writes one screenshot into the directory the pull request points at.
async function shoot(page, name) {
	await page.screenshot({ path: `tests/screenshots/${name}.png`, fullPage: false });
}

test("a pageview, a custom event and an outbound click", async ({ page }) => {
	await pinUUIDs(page);
	const state = await collect(page);
	await stubOutbound(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#custom");
	await settledCount(state, "Custom Event", 1);

	await page.click("#tagged-label");
	await settledCount(state, "Signup", 1);

	await page.click("#download");
	await settledCount(state, "File Download", 1);

	await panel(page, "basic.html", diagnosticEvents(state.events));
	await shoot(page, "01-pageview-and-events");

	expect(state.events.length).toBeGreaterThanOrEqual(4);
});

test("hash routing through both the hash and the History API", async ({ page }) => {
	await pinUUIDs(page);
	const state = await collect(page);

	await page.goto("/hash.html");
	await settledCount(state, "pageview", 1);

	await page.click("#assign");
	await settledCount(state, "pageview", 2);

	await page.click("#push");
	await settledCount(state, "pageview", 3);

	// The excluded route, which produces nothing at all.
	await page.click("#private");
	await page.waitForTimeout(400);

	await panel(page, "hash.html — the excluded route sent nothing", diagnosticEvents(state.events));
	await shoot(page, "02-hash-routing-and-exclusions");

	// Three routes, three pageviews, and nothing at all for the excluded one.
	// The engagement events between them are the page being left each time.
	expect(state.events.filter((event) => event.n === "pageview")).toHaveLength(3);
});

test("scroll depth and time on page", async ({ page }) => {
	await pinUUIDs(page);
	const state = await collect(page);

	await page.goto("/engagement.html");
	await settledCount(state, "pageview", 1);

	await page.waitForTimeout(3200);
	await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
	await page.evaluate(() => window.dispatchEvent(new Event("blur")));

	await settledCount(state, "engagement", 1);

	await panel(page, "engagement.html", diagnosticEvents(state.events));
	await shoot(page, "03-engagement");

	expect(state.events.length).toBeGreaterThanOrEqual(2);
});

test("a single-page application's route changes", async ({ page }) => {
	await pinUUIDs(page);
	const state = await collect(page);

	await page.goto("/spa.html");
	await settledCount(state, "pageview", 1);

	await page.click("#push");
	await settledCount(state, "pageview", 2);

	await page.click("#query");
	await settledCount(state, "pageview", 3);

	await page.click("#push-then-replace");
	await settledCount(state, "pageview", 4);

	await panel(page, "spa.html", diagnosticEvents(state.events));
	await shoot(page, "04-spa-navigation");

	expect(state.events.length).toBeGreaterThanOrEqual(4);
});
