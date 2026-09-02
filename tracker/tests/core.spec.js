//
// core.spec.js
// The wire contract: what one event looks like, and how it gets there.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { test, expect } from "@playwright/test";
import { collect, named, settledCount, waitFor } from "./helpers.js";

// The payload keys are the wire contract and are not ours to rename. They match
// an established endpoint byte for byte, which is what lets somebody migrate by
// changing one hostname.
test("a pageview carries exactly the documented keys", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	const [pageview] = named(state, "pageview");

	expect(pageview.d).toBe("fixture.test");
	expect(pageview.u).toContain("/basic.html");
	expect(pageview.t).toBe("Basic — tracker fixture");
	expect(pageview.v).toBe(1);

	// Absent keys are left out rather than sent as null, which is what keeps a
	// pageview under two hundred bytes.
	expect(pageview.k).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
	expect(Object.keys(pageview).sort()).toEqual(["d", "k", "n", "t", "u", "v"]);
});

// Some supported Web Crypto implementations have secure randomness but not
// randomUUID. The compatibility path must still emit the exact UUID contract
// topology persists as its permanent dedupe key.
test("event identity falls back to getRandomValues", async ({ page }) => {
	await page.addInitScript(() => {
		Object.defineProperty(Crypto.prototype, "randomUUID", { configurable: true, value: undefined });
	});
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	expect(named(state, "pageview")[0].k).toMatch(
		/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
	);
});

// text/plain is not a nicety: application/json is not a simple content type, so
// every event would cost an OPTIONS round trip before the POST.
test("events are posted as text/plain, which needs no CORS preflight", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	expect(state.requests[0].method).toBe("POST");
	expect(state.requests[0].contentType).toBe("text/plain");
});

test("a custom event carries its properties", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#custom");
	await settledCount(state, "Custom Event", 1);

	const [event] = named(state, "Custom Event");

	expect(event.p).toEqual({ where: "basic" });
	expect(event.u).toContain("/basic.html");
	expect(event.d).toBe("fixture.test");
});

// Revenue goes out under `$`, which is the key the server reads.
test("revenue is sent under the dollar key", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	await page.click("#revenue");
	await settledCount(state, "Purchase", 1);

	expect(named(state, "Purchase")[0].$).toEqual({ amount: 42.5, currency: "USD" });
});

// The queue exists so that a call made before the bundle arrives is neither a
// ReferenceError nor silently lost. Both the built-in migration alias and a
// site-configured alias keep existing calls working.
test("events queued before the bundle loads are replayed under every API name", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/queue.html");

	await settledCount(state, "Queued Early", 1);
	await settledCount(state, "Legacy Queued", 1);
	await settledCount(state, "Plausible Queued", 1);

	expect(named(state, "Queued Early")[0].p).toEqual({ when: "before" });
	expect(named(state, "Plausible Queued")[0].p).toEqual({ when: "migration" });

	await page.click("#after");
	await settledCount(state, "Legacy Later", 1);

	await page.click("#plausible-after");
	await settledCount(state, "Plausible Later", 1);
});

// The pageview is drained after the queue, so a queued event never arrives
// before the pageview it belongs to.
test("the pageview arrives before the queued events", async ({ page }) => {
	const state = await collect(page);

	await page.goto("/queue.html");
	await settledCount(state, "Legacy Later", 0);
	await waitFor(state, (events) => events.length >= 3, "three events");

	expect(state.events[0].n).toBe("pageview");
});

// The callback is best effort and documented as such, but when we do get an
// answer we have to pass both status and an observable inline drop reason on.
// A shard-side drop happens after this response and therefore remains null.
test("the callback exposes an inline drop reason", async ({ page }) => {
	await collect(page, { dropped: "shield_ip" });

	await page.goto("/basic.html");

	const result = await page.evaluate(
		() =>
			new Promise((resolve) => {
				window.feasible("Gated", { callback: resolve });
				setTimeout(() => resolve("timed out"), 3000);
			}),
	);

	expect(result).toEqual({ status: 202, dropped: "shield_ip" });
});

// No queue behind the endpoint can save an event whose HTTP request never
// completed. The only place that event still exists is the browser it was sent
// from, which is what the outbox is for.
test("a failed event is kept and retried on the next pageview", async ({ page }) => {
	const state = await collect(page);

	// The first request — the first page's pageview — is refused outright.
	state.fail = 1;

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);

	expect(await page.evaluate(() => localStorage.getItem("feasible_outbox"))).toContain("basic.html");

	await page.goto("/spa.html");

	// Two pageviews for basic.html: the one that failed, replayed, and then the
	// new page's own.
	await waitFor(
		state,
		(events) => events.filter((e) => e.n === "pageview" && e.u.includes("basic.html")).length >= 2,
		"the failed pageview was replayed",
	);

	expect(await page.evaluate(() => localStorage.getItem("feasible_outbox"))).toBe("[]");

	const attempts = state.events.filter((event) => event.n === "pageview" && event.u.includes("basic.html"));
	expect(attempts[0].k).toBe(attempts[1].k);
});

// The hardest retry boundary is a request the server durably accepted whose
// 202 never reached the browser. Both attempts must carry one client identity.
test("a lost 202 retries with the same client idempotency key", async ({ page }) => {
	const state = await collect(page);
	state.disconnect = 1;

	await page.goto("/basic.html");
	await expect
		.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("feasible_outbox") || "[]").length))
		.toBe(1);

	await page.goto("/spa.html");
	await waitFor(
		state,
		(events) =>
			events.filter((event) => event.n === "pageview" && event.u && event.u.includes("basic.html")).length >= 2,
		"lost-202 replay",
	);

	const attempts = state.events.filter(
		(event) => event.n === "pageview" && event.u && event.u.includes("basic.html"),
	);
	expect(attempts[0].k).toBe(attempts[1].k);
});

// A browser can cancel a request as the document goes away before fetch has a
// chance to reject. The body must already be durable for the next page.
test("a cancelled request was queued before fetch and retries with the same key", async ({ page }) => {
	const state = await collect(page);
	state.cancel = 1;

	await page.goto("/basic.html");
	await expect
		.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("feasible_outbox") || "[]").length))
		.toBe(1);

	await page.goto("/spa.html");
	await waitFor(
		state,
		(events) =>
			events.filter((event) => event.n === "pageview" && event.u && event.u.includes("basic.html")).length >= 2,
		"cancelled request replay",
	);

	const attempts = state.events.filter(
		(event) => event.n === "pageview" && event.u && event.u.includes("basic.html"),
	);
	expect(attempts[0].k).toBe(attempts[1].k);
});

// A fixed event-count cap silently deleted the oldest body on failure 101.
// Browser quota is now the explicit storage boundary.
test("the retry queue does not evict event 101", async ({ page }) => {
	const state = await collect(page, { status: 503 });
	await page.goto("/basic.html");

	await page.evaluate(() => {
		for (let i = 0; i < 101; i++) window.feasible(`Queued ${i}`);
	});

	await expect
		.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("feasible_outbox") || "[]").length))
		.toBe(102);
});

// localStorage can throw in private modes and at quota. The tracker keeps an
// in-page queue and explicitly reports that it is memory-only.
test("storage failure is explicit and keeps the tracker running", async ({ page }) => {
	await page.addInitScript(() => {
		Object.defineProperty(window, "localStorage", {
			configurable: true,
			get() {
				throw new DOMException("quota", "QuotaExceededError");
			},
		});
	});
	const warnings = [];
	page.on("console", (message) => warnings.push(message.text()));
	const state = await collect(page, { status: 503 });

	await page.goto("/basic.html");
	await settledCount(state, "pageview", 1);
	await expect.poll(() => warnings.some((message) => message.includes("memory-only"))).toBeTruthy();

	await page.click("#custom");
	await settledCount(state, "Custom Event", 1);
});
