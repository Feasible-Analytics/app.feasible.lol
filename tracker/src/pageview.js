//
// pageview.js
// Counting a pageview exactly when a human sees a page: prerender, bfcache, SPA, hash.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { doc, loc, page } from "./state.js";
import { send, drain } from "./send.js";
import { excluded, warn } from "./exclude.js";
import * as engagement from "./engagement.js";

let cfg = null;

// deferred holds a pageview that arrived while nobody was looking at the page.
// It is a single slot rather than a queue: a page that is prerendered and then
// navigated three times before being shown was still only ever seen once.
let deferred = null;
let waiting = false;

// url is the address to report for the current location.
//
// The hash is included only in hash-routing mode. Outside it, `#section` is a
// jump within one page rather than a page of its own, and reporting it would
// split a single article into a row per heading anchor.
function url() {
	return cfg.h ? loc.href : loc.href.split("#")[0];
}

// hidden reports whether the page is being prepared rather than looked at.
//
// Chrome's Speculation Rules API is on by default in Cloudflare and in
// WordPress, so without this check a site records pageviews for pages that were
// speculatively loaded and never shown to anyone. The `hidden` case matters
// just as much: a link opened in a background tab is a page nobody has seen
// yet, and it should count when — if — it is looked at.
function hidden() {
	return (
		doc.visibilityState === "prerender" ||
		doc.visibilityState === "hidden" ||
		doc.prerendering
	);
}

// onVisible fires the pageview that was held back, once.
function onVisible() {
	if (hidden()) return;

	doc.removeEventListener("visibilitychange", onVisible);
	waiting = false;

	const held = deferred;
	deferred = null;

	pageview(held);
}

// pageview sends one pageview, or holds it until somebody looks at the page.
//
// `opts` carries the overrides an SPA or a restore needs: `u` for a custom
// location, `r` for a corrected referrer, `p` for props.
export function pageview(opts) {
	const options = opts || {};

	if (hidden()) {
		deferred = options;

		if (!waiting) {
			waiting = true;
			doc.addEventListener("visibilitychange", onVisible);
		}

		return;
	}

	// Exclusions are checked at send time rather than at load time, because in
	// an SPA the page that is excluded is often not the one the document
	// started on.
	//
	// It says so in the console for the same reason every other refusal does: a
	// glob that matches more than its author meant it to looks exactly like a
	// tracker that is broken, and the only difference between a five-second
	// diagnosis and a support conversation is whether the page said which rule
	// fired.
	if (excluded(cfg)) {
		warn("not tracking — excluded path");
		options.callback?.({ status: null });
		return;
	}

	const target = options.u ? new URL(options.u, loc.href).href : url();

	page.u = target;
	page.k = cfg.h ? target : target.split("#")[0];
	page.t = true;

	engagement.reset();

	// The outbox is replayed on a pageview because that is the moment we know
	// the visitor is present and the network is already being used.
	drain();

	const event = {
		n: "pageview",
		u: target,
		d: page.d,
		t: doc.title || undefined,
	};

	// The referrer is the entry referrer only for the first pageview of the
	// document. Every later one — an SPA route change, a bfcache restore — is
	// handed the page it came from, which is this same site and therefore
	// reads as an internal navigation. Re-sending the original external
	// referrer on every route change is what invents a second visit from
	// Google for a visitor who only ever arrived once.
	const referrer = "r" in options ? options.r : doc.referrer;
	if (referrer) event.r = referrer;

	if (options.p) event.p = options.p;

	send(event, options.callback);
}

// navigated handles a change of address inside one document.
//
// It is deferred to the next task on purpose. Routers routinely call pushState
// and then replaceState in the same tick, and set the document title in the
// render that follows; firing synchronously reports the intermediate URL and
// the previous page's title. Deduplicating on the resulting URL collapses that
// pair into the one pageview the visitor actually experienced.
function navigated() {
	setTimeout(() => {
		if (url() === page.k) return;

		// The page being left is measured before the new one is announced, or
		// the reading time lands on the wrong URL.
		engagement.flush();

		pageview({ r: page.u });
	}, 0);
}

// patch wraps one History method so that a route change announces itself. The
// original is called first and its return value preserved, because a router
// that gets a different answer from pushState than the platform gives is a
// router that breaks.
//
// `replaceState` is patched as well as `pushState`. Several routers implement
// redirects and canonicalisation entirely through replaceState, and a tracker
// that only watches pushState records the URL the visitor was redirected away
// from.
function patch(name) {
	const original = history[name];
	if (typeof original !== "function") return;

	history[name] = function () {
		const result = original.apply(this, arguments);
		navigated();
		return result;
	};
}

// start wires up every way a page can change.
//
// Hash routing is not a separate build and not an either/or branch. A hash
// router navigates through pushState, which does not fire `hashchange`, so a
// tracker that listens only for `hashchange` records exactly one pageview for
// the entire life of the application — and one that listens only to the History
// API misses a plain `location.hash = "#/next"`. Both listeners, one code path,
// deduplicated on the full URL.
export function start(config) {
	cfg = config;
	page.d = cfg.d;

	// A bfcache restore is a pageview with no navigation, no load event and no
	// script execution: without this listener, every visitor who presses Back
	// disappears from the numbers for the rest of their visit.
	//
	// The referrer has to be corrected too. `document.referrer` on a restored
	// page is still whatever it was on the original load, and re-sending it
	// would credit the original source with a second arrival. What the visitor
	// actually came from is another page of this site, which is what is sent.
	addEventListener("pageshow", (event) => {
		if (!event.persisted || !page.t) return;

		pageview({ r: page.u });
	});

	// Manual mode still gets everything above. The incumbent's manual variant
	// silently dropped the restore listener and engagement along with the
	// automatic pageview, so a site that took control of its own pageviews
	// quietly lost three unrelated features.
	if (cfg.m) return;

	patch("pushState");
	patch("replaceState");
	addEventListener("popstate", navigated);
	addEventListener("hashchange", navigated);

	pageview();
}
