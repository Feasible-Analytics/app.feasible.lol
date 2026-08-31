//
// send.js
// The transport: one POST, and a small localStorage outbox for the ones that fail.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, VERSION } from "./state.js";

// The outbox key. A request whose connection died was never seen by any server,
// so no amount of durability behind the endpoint can recover it — the only
// place that event still exists is this browser.
const OUTBOX_KEY = "feasible_outbox";

// How many failed events we are willing to hold. Ten is enough to survive a
// tunnel or a dropped Wi-Fi handover and small enough that an ad blocker, which
// makes every single request fail, cannot grow the entry without bound.
const OUTBOX_MAX = 10;

// KEEPALIVE is whether `fetch` will finish a request after the page has gone.
// It is the reason this tracker never has to delay a navigation: the event is
// already the browser's problem, not the page's, so there is nothing to wait
// for and no click to intercept.
//
// The `nk` escape hatch forces the answer to false. The end-to-end suite uses
// it to drive the fallback path that browsers without keepalive take, which is
// otherwise unreachable in a modern browser and would go untested.
export const KEEPALIVE = (() => {
	try {
		return !(win.__feasible || "").nk && "keepalive" in new Request("");
	} catch {
		return false;
	}
})();

// endpoint is set once at bootstrap. It is module state rather than a parameter
// because every send site would otherwise have to carry the configuration
// around purely to hand it back.
let endpoint = "";

// setEndpoint records where events go.
export function setEndpoint(url) {
	endpoint = url;
}

// readOutbox returns the stashed events. Every localStorage access in this file
// is wrapped, because the API throws rather than returning null when storage is
// unavailable and an unguarded read here would take the whole script down.
function readOutbox() {
	try {
		return JSON.parse(localStorage.getItem(OUTBOX_KEY)) || [];
	} catch {
		return [];
	}
}

// writeOutbox replaces the stash, keeping only the newest entries. Storage
// being unavailable costs us the retry, not the page.
function writeOutbox(items) {
	try {
		localStorage.setItem(OUTBOX_KEY, JSON.stringify(items.slice(-OUTBOX_MAX)));
	} catch {}
}

// drain replays whatever failed last time. It runs on a pageview rather than on
// a timer because a pageview is the one moment we know the network is being
// used anyway and the visitor is present.
//
// A replayed event that fails again is dropped rather than stashed a second
// time. Re-stashing would turn an ad blocker — which fails every request
// forever — into a permanent write to localStorage on every page load, for
// events that are never going to arrive.
export function drain() {
	const items = readOutbox();
	if (!items.length) return;

	writeOutbox([]);

	for (const body of items) post(body, null, true);
}

// post sends one already-serialised event.
//
// Three details here are requirements, not preferences:
//
//   - `keepalive` is what lets the request outlive the page. Without it the
//     last event of every visit is lost on unload, and the workaround for that
//     is delaying the navigation, which is how a tracker starts breaking the
//     host page.
//
//   - `text/plain` avoids a CORS preflight. `application/json` is not a simple
//     content type, so every single event would cost an OPTIONS round trip
//     before the POST — double the requests, double the latency, and a second
//     thing for a corporate proxy to break.
//
//   - the callback is best effort and is documented as such. An ad blocker, a
//     network failure or an excluded page means it is never called, so any
//     caller that gates a form submission on it must race it against its own
//     timeout.
export function post(body, callback, isRetry) {
	// A failed event is kept for the next pageview. A replayed one that fails
	// again is not kept a second time.
	const fail = () => {
		if (!isRetry) writeOutbox(readOutbox().concat(body));
	};

	try {
		fetch(endpoint, {
			method: "POST",
			headers: { "Content-Type": "text/plain" },
			keepalive: KEEPALIVE,
			body,
		}).then((res) => {
			// The server answers 202 for everything it understood, including
			// events it decided to drop, and puts the reason in a header. A 4xx
			// is a real failure and worth retrying; a drop is not.
			if (res.status >= 400) {
				fail();
				return;
			}

			if (callback) callback({ status: res.status });
		}, fail);
	} catch {
		fail();
	}
}

// send serialises one event and posts it. The key names are the wire contract
// and are not ours to rename: `n` `u` `d` `r` `p` `i` `sd` `e` `v` `t` and `$`.
// Absent keys are left out entirely rather than sent as null, which keeps a
// pageview under two hundred bytes.
export function send(event, callback) {
	event.v = VERSION;

	post(JSON.stringify(event), callback);
}
