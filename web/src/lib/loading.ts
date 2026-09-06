//
// loading.ts
// One page-level count of the requests in flight behind the dashboard.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useSyncExternalStore } from "react";

/**
 * How long a request may run before the page admits to it.
 *
 * The dashboard keeps the previous answer on screen while the next one loads,
 * so a reader who changes the period sees confident, fully-rendered numbers
 * that are already stale. Something has to say a newer answer is coming — but
 * a cached report comes back in well under a tenth of a second, and a bar that
 * flashes for 80ms reads as the page glitching rather than as work happening.
 *
 * 300ms is past almost every cached response and still well inside the window
 * where somebody starts wondering whether the click registered.
 */
const GRACE_MS = 300;

/**
 * How long the count must stay at zero before the bar goes away.
 *
 * Changing the period aborts every in-flight request and immediately starts a
 * replacement for each. React runs all of the effect cleanups before any of the
 * new effects, so the count genuinely touches zero in between — and hiding on
 * that instant would blink the bar off and back on in the middle of one load.
 */
const SETTLE_MS = 150;

/** How many requests are running right now. Polls are excluded, so this is the
 *  count of work a reader is actually waiting on. */
let inFlight = 0;

/** Whether the bar is on screen. It lags `inFlight` by the two delays above,
 *  which is the entire reason the two values are separate. */
let visible = false;

let showTimer: ReturnType<typeof setTimeout> | undefined;
let hideTimer: ReturnType<typeof setTimeout> | undefined;

const listeners = new Set<() => void>();

/** clearTimers drops both pending transitions, which is what every change to
 *  the count has to do before deciding on a new one. */
function clearTimers(): void {
	if (showTimer !== undefined) {
		clearTimeout(showTimer);
		showTimer = undefined;
	}

	if (hideTimer !== undefined) {
		clearTimeout(hideTimer);
		hideTimer = undefined;
	}
}

/** setVisible publishes a change to every subscriber, and only when the value
 *  actually moved — useSyncExternalStore re-reads on every notification. */
function setVisible(next: boolean): void {
	if (visible === next) return;

	visible = next;

	for (const listener of listeners) listener();
}

/**
 * beginRequest counts one request in and hands back its release.
 *
 * The release is idempotent on purpose. `useRemote` calls it both when the
 * promise settles and when the effect is torn down, because an aborted request
 * takes the second path and only the second path — a count that misses an abort
 * never returns to zero and leaves the bar on for ever.
 *
 * A quiet request is counted nowhere at all: the current-visitors pill and the
 * live view refresh every thirty seconds, and a bar that flashes on that
 * cadence for as long as the tab is open is worse than no bar.
 */
export function beginRequest(quiet = false): () => void {
	if (quiet) return () => {};

	inFlight += 1;

	if (inFlight === 1) {
		clearTimers();

		// Already on screen from the load this one is continuing: stay on, and
		// the settle timer that was about to hide it is now wrong.
		if (!visible) {
			showTimer = setTimeout(() => {
				showTimer = undefined;
				setVisible(true);
			}, GRACE_MS);
		}
	}

	let released = false;

	return () => {
		if (released) return;

		released = true;
		inFlight -= 1;

		if (inFlight > 0) return;

		clearTimers();

		// Nothing was ever shown, so there is nothing to fade out: this is the
		// cached answer that beat the grace window.
		if (!visible) return;

		hideTimer = setTimeout(() => {
			hideTimer = undefined;
			setVisible(false);
		}, SETTLE_MS);
	};
}

/** subscribe registers a listener and hands back its own removal, which is the
 *  shape useSyncExternalStore wants. */
export function subscribe(listener: () => void): () => void {
	listeners.add(listener);

	return () => {
		listeners.delete(listener);
	};
}

/** isLoading reports whether the bar should be on screen right now. */
export function isLoading(): boolean {
	return visible;
}

/** requestsInFlight is the raw count, exposed so the tests can assert that it
 *  returns to zero on success, on failure and on abort. */
export function requestsInFlight(): number {
	return inFlight;
}

/** resetLoading drops all state. Tests only — nothing in the app has a reason
 *  to forget requests that are still running. */
export function resetLoading(): void {
	clearTimers();
	inFlight = 0;
	visible = false;
	listeners.clear();
}

/**
 * useLoading re-renders its caller whenever the bar should appear or disappear.
 *
 * The server-side value is false because the bar is a statement about work in
 * this browser, and a bar baked into the first paint of a page that has not
 * asked for anything yet would be a lie that hydration then has to undo.
 */
export function useLoading(): boolean {
	return useSyncExternalStore(subscribe, isLoading, () => false);
}
