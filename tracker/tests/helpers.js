//
// helpers.js
// Shared plumbing for the end-to-end suite: capturing events and waiting for them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { expect } from "@playwright/test";

// collect intercepts the event endpoint and records everything the script sends.
//
// Interception rather than a recording server is what keeps the tests
// independent of each other: the captured events live in the test's own scope,
// so two specs running in parallel can never see each other's traffic, and a
// test that navigates away still holds everything the page sent before it went.
//
// `status` drives the failure paths — the outbox retry has no other way to be
// exercised, because a healthy endpoint never produces a failed send.
export async function collect(page, options = {}) {
	const state = {
		events: [],
		requests: [],
		status: options.status || 202,

		// fail makes the next `count` requests fail, which is how a dropped
		// connection is simulated without unplugging anything.
		fail: 0,
	};

	await page.route("**/api/event*", async (route) => {
		const request = route.request();
		const body = request.postData();

		state.requests.push({
			method: request.method(),
			// The endpoint is recorded because `data-api` has no other visible
			// effect: an event sent to the wrong URL looks identical to one sent
			// to the right one from inside the payload.
			url: request.url(),
			contentType: request.headers()["content-type"] || "",
			body,
		});

		try {
			state.events.push(JSON.parse(body));
		} catch {
			state.events.push({ __unparseable: body });
		}

		if (state.fail > 0) {
			state.fail--;
			await route.fulfill({
				status: 500,
				contentType: "text/plain",
				headers: { "access-control-allow-origin": "*" },
				body: "no",
			});
			return;
		}

		await route.fulfill({
			status: state.status,
			contentType: "text/plain",
			headers: { "access-control-allow-origin": "*" },
			body: "ok",
		});
	});

	return state;
}

// stubOutbound answers the external hosts the fixtures link to, so an outbound
// click navigates somewhere real without the test needing the internet.
export async function stubOutbound(page) {
	await page.route("https://example.com/**", (route) =>
		route.fulfill({
			status: 200,
			contentType: "text/html",
			body: "<!doctype html><title>Somewhere else</title><h1>Somewhere else</h1>",
		}),
	);
}

// named returns every captured event with a given name, which is what almost
// every assertion in the suite is actually about.
export function named(state, name) {
	return state.events.filter((event) => event.n === name);
}

// waitFor polls until the captured events satisfy a predicate.
//
// Polling rather than a fixed wait is not a style preference: `keepalive`
// requests are fire-and-forget by design, so there is no promise anywhere in
// the page to await, and a sleep long enough to be reliable would multiply
// across a hundred assertions into a suite nobody runs.
export async function waitFor(state, predicate, message) {
	// The timeout is generous because the suite runs several browsers at once
	// on one machine, and a keepalive request has no promise to await: a tight
	// deadline here buys nothing and produces a flake nobody can reproduce.
	await expect
		.poll(() => predicate(state.events), { message, timeout: 10000 })
		.toBeTruthy();
}

// countOf waits until exactly `n` events of a name have arrived and then holds
// still for a moment to catch a duplicate arriving late. A tracker bug that
// sends two pageviews where it should send one is invisible to an assertion
// that only waits for the first.
export async function settledCount(state, name, n) {
	await waitFor(state, (events) => events.filter((e) => e.n === name).length >= n, `${n}x ${name}`);

	await new Promise((resolve) => setTimeout(resolve, 250));

	return named(state, name);
}
