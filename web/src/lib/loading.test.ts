//
// loading.test.ts
// The page-level loading counter: what it counts, and when it shows.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import test from "node:test";

import { beginRequest, isLoading, requestsInFlight, resetLoading, subscribe } from "./loading";

/** The two delays the module is built around, restated here so a test reads as
 *  "just before" and "just after" rather than as bare numbers. */
const GRACE_MS = 300;
const SETTLE_MS = 150;

test("the count rises with each request and returns to zero when all settle", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const first = beginRequest();
	assert.equal(requestsInFlight(), 1);

	const second = beginRequest();
	assert.equal(requestsInFlight(), 2);

	first();
	assert.equal(requestsInFlight(), 1);

	second();
	assert.equal(requestsInFlight(), 0);
});

test("releasing twice cannot drive the count below zero", (t) => {
	// This is the aborted request. A period change tears the effect down and
	// the rejected promise settles as well, so the same release is called from
	// both paths — and a count that went negative would never show the bar
	// again.
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const other = beginRequest();
	const aborted = beginRequest();

	aborted();
	aborted();

	assert.equal(requestsInFlight(), 1);

	other();
	assert.equal(requestsInFlight(), 0);
});

test("an aborted request that never settles still returns the count to zero", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const release = beginRequest();
	t.mock.timers.tick(GRACE_MS);
	assert.equal(isLoading(), true);

	release();
	t.mock.timers.tick(SETTLE_MS);

	assert.equal(requestsInFlight(), 0);
	assert.equal(isLoading(), false);
});

test("a quiet request moves neither the count nor the bar", (t) => {
	// The current-visitors pill and the live view refresh on a thirty-second
	// timer. A bar that appears on that cadence for as long as the tab is open
	// is worse than no bar at all.
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const poll = beginRequest(true);
	assert.equal(requestsInFlight(), 0);

	t.mock.timers.tick(GRACE_MS * 4);
	assert.equal(isLoading(), false);

	poll();
	assert.equal(requestsInFlight(), 0);
	assert.equal(isLoading(), false);
});

test("a request that answers inside the grace window never shows the bar", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const seen: boolean[] = [];
	subscribe(() => seen.push(isLoading()));

	const release = beginRequest();
	t.mock.timers.tick(GRACE_MS - 20);
	release();

	t.mock.timers.tick(GRACE_MS * 4);

	assert.equal(isLoading(), false);
	assert.deepEqual(seen, []);
});

test("two overlapping requests produce one continuous visible period", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const seen: boolean[] = [];
	subscribe(() => seen.push(isLoading()));

	const first = beginRequest();
	t.mock.timers.tick(GRACE_MS);
	assert.equal(isLoading(), true);

	const second = beginRequest();
	first();
	t.mock.timers.tick(SETTLE_MS * 4);
	assert.equal(isLoading(), true);

	second();
	t.mock.timers.tick(SETTLE_MS);
	assert.equal(isLoading(), false);

	// One rise and one fall. Anything more is the flicker this exists to stop.
	assert.deepEqual(seen, [true, false]);
});

test("a replacement request started on the same tick keeps the bar on", (t) => {
	// Changing the period aborts every request and starts a replacement for
	// each. React runs all of the effect cleanups before any of the new
	// effects, so the count really does touch zero in the middle of one load.
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	const seen: boolean[] = [];
	subscribe(() => seen.push(isLoading()));

	const old = beginRequest();
	t.mock.timers.tick(GRACE_MS);
	assert.equal(isLoading(), true);

	old();
	const replacement = beginRequest();

	t.mock.timers.tick(SETTLE_MS * 4);
	assert.equal(isLoading(), true);

	replacement();
	t.mock.timers.tick(SETTLE_MS);

	assert.deepEqual(seen, [true, false]);
});

test("unsubscribing stops the notifications", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	t.after(() => resetLoading());
	resetLoading();

	let calls = 0;
	const stop = subscribe(() => {
		calls += 1;
	});

	stop();

	const release = beginRequest();
	t.mock.timers.tick(GRACE_MS);
	release();
	t.mock.timers.tick(SETTLE_MS);

	assert.equal(calls, 0);
});
