//
// send.js
// The transport: one POST, and a small localStorage outbox for the ones that fail.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, VERSION } from "./state.js";
import { warn } from "./exclude.js";

// The outbox key. A request whose connection died was never seen by any server,
// so no amount of durability behind the endpoint can recover it — the only
// place that event still exists is this browser.
const OUTBOX_KEY = "feasible_outbox";

// A storage failure cannot be made durable by JavaScript. A defined array is
// both the in-page queue and the sentinel that keeps its warning one-time.
let volatileOutbox;

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
		return !win.__feasible?.nk && "keepalive" in new Request("");
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
		return volatileOutbox || JSON.parse(localStorage[OUTBOX_KEY] || "[]");
	} catch {
		warn("memory-only");
		return (volatileOutbox = []);
	}
}

// writeOutbox replaces the complete stash. There is deliberately no event-count
// eviction: the 101st failure is no less real than the first, and browser quota
// failure is handled explicitly by the memory fallback below.
function writeOutbox(items) {
	try {
		if (!volatileOutbox) localStorage[OUTBOX_KEY] = JSON.stringify(items);
	} catch {
		warn("memory-only");
		volatileOutbox = [];
	}
	if (volatileOutbox) volatileOutbox = items;
}

// drain replays whatever failed last time. It runs on a pageview rather than on
// a timer because a pageview is the one moment we know the network is being
// used anyway and the visitor is present.
//
// A replay remains durable while its request is in flight. Success removes that
// exact body; failure changes nothing, and UUID dedupe makes overlap harmless.
export function drain() {
	const items = readOutbox();
	if (!items.length) return;

	for (const body of items) post(body, 0);
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
export function post(body, callback) {
	// Persistence happens before fetch so cancellation cannot destroy the only
	// copy. A zero callback marks a replay that is already in the durable queue.
	if (callback !== 0) writeOutbox(readOutbox().concat(body));

	try {
		fetch(endpoint, {
			method: "POST",
			headers: { "Content-Type": "text/plain" },
			keepalive: KEEPALIVE,
			body,
		}).then(
			// The server answers 202 for everything it understood, including
			// events it decided to drop. A 4xx is a real retryable failure.
			(res) => {
				if (res.ok) {
					writeOutbox(readOutbox().filter((item) => item !== body));
					if (callback) callback({ status: res.status });
				}
			},
			() => 0,
		);
	} catch {}
}

// send serialises one event and posts it. The key names are the wire contract
// and are not ours to rename: `k` `n` `u` `d` `r` `p` `i` `sd` `e` `v` `t` and `$`.
// Absent keys are left out entirely rather than sent as null, which keeps a
// pageview under two hundred bytes.
export function send(event, callback) {
	// The existing browser floor includes crypto.randomUUID. Creating the key
	// before post means a lost 202, cancellation and later replay stay one event.
	event.k = crypto.randomUUID();
	event.v = VERSION;

	post(JSON.stringify(event), callback);
}
