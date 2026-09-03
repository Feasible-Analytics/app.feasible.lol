//
// send.js
// The transport: one POST, and a small localStorage outbox for the ones that fail.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, loc, hatch, VERSION } from "./state.js";
import { signals } from "./automation.js";
import { excluded, ignoreReason, warn } from "./exclude.js";

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
		return !hatch?.nk && "keepalive" in new Request("");
	} catch {
		return false;
	}
})();

// endpoint is set once at bootstrap. It is module state rather than a parameter
// because every send site would otherwise have to carry the configuration
// around purely to hand it back.
let endpoint = "";
let config = null;

// configure records where events go and the policy applied immediately before
// every live or persisted network attempt.
export function configure(url, cfg) {
	endpoint = url;
	config = cfg;
}

// refusal applies current controls to the event's captured route immediately
// before a live send or persisted replay and names the policy that stopped it.
export function refusal(event) {
	return ignoreReason(config) || (excluded(config, event.u || loc.href) ? "excluded path" : "");
}

// eventID creates the stable RFC 4122 version-4 identity that survives an
// outbox replay. randomUUID is the shortest fast path; getRandomValues keeps
// the same wire contract in browsers whose Web Crypto implementation predates
// randomUUID, without weakening uniqueness to a time stamp or row sequence.
function eventID() {
	if (win.crypto.randomUUID) return win.crypto.randomUUID();

	// This compact RFC 4122 formatter fixes the version and variant nibbles and
	// draws every remaining nibble from Web Crypto. It is deliberately not a
	// Math.random fallback: this value becomes a permanent database receipt.
	return ([1e7] + -1e3 + -4e3 + -8e3 + -1e11).replace(/[018]/g, (digit) =>
		(digit ^ (win.crypto.getRandomValues(new Uint8Array(1))[0] & (15 >> (digit / 4)))).toString(16),
	);
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
	if (volatileOutbox) {
		volatileOutbox = items;
		return;
	}
	try {
		localStorage[OUTBOX_KEY] = JSON.stringify(items);
	} catch {
		warn("memory-only");
		volatileOutbox = items;
	}
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

	writeOutbox(
		items.filter((body) => {
			try {
				if (!refusal(JSON.parse(body))) {
					post(body, 0);
					return true;
				}
			} catch {}
			return false;
		}),
	);
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
	if (callback != 0) writeOutbox(readOutbox().concat(body));

	try {
		fetch(endpoint, {
			method: "POST",
			headers: { "content-type": "text/plain" },
			keepalive: KEEPALIVE,
			body,
		}).then(
			// The server answers 202 for everything it understood, including
			// events it decided to drop, and puts the reason in a header. A
			// non-success response remains in the durable outbox for replay.
			(res) => {
				if (res.ok) {
					writeOutbox(readOutbox().filter((item) => item != body));
					callback?.({
						status: res.status,
						dropped: res.headers.get("x-feasible-dropped"),
					});
				}
			},
			() => 0,
		);
	} catch {}
}

// What the browser says about itself does not change while the document is
// open, so it is read once. The viewport is not read here with it: a window
// gets resized and a phone gets rotated, and an SPA sends a pageview per route,
// so a width captured at load would be wrong for every pageview after the
// first — in the report this field exists to fill.
const automated = signals();

// send serialises one event and posts it. The key names are the wire contract
// and are not ours to rename: `k` `n` `u` `d` `r` `p` `i` `sd` `e` `v` `t` `w`
// `a` and `$`. Absent keys are left out entirely rather than sent as null,
// which keeps a pageview under two hundred bytes.
export function send(event, callback) {
	if (refusal(event)) {
		callback?.({ status: null });
		return;
	}
	// Creating the key before post means a lost 202, cancellation and later
	// replay stay one event with one permanent server receipt.
	event.k = eventID();
	event.v = VERSION;
	event.w = innerWidth || undefined;
	event.a = automated;

	post(JSON.stringify(event), callback);
}
