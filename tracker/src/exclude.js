//
// exclude.js
// Who we refuse to count: excluded paths, localhost, headless browsers, self-exclusion.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, loc } from "./state.js";

// The localStorage key a person sets to keep their own visits out of the
// numbers.
const IGNORE_KEY = "feasible_ignore";

// Hostnames that are somebody's own machine rather than a website. `file:` is
// handled separately because it has no hostname at all.
//
// Documented consequence: a Capacitor, Cordova or Electron shell serves its
// pages from one of these, so a hybrid app records nothing until
// `data-capture-on-localhost` is set. That is a deliberate default — the
// alternative is every developer's own reloads landing in production numbers —
// but it has to be said out loud rather than discovered.
const LOCAL_HOST = /^(localhost|127\.0\.0\.1|0\.0\.0\.0|\[?::1\]?)?$/;

let patterns = null;

// compile turns one glob into a regular expression, once. `*` stops at a path
// separator and `**` crosses them, which is the distinction that makes
// `/blog/*` and `/blog/**` two different rules rather than a typo.
//
// Splitting on the wildcards first is what leaves them out of the escaping:
// every other regular-expression metacharacter in what is left is a literal the
// customer typed and has to be neutered, and `*` is the one thing they meant.
// The escape is inlined rather than named because the bundle is on a hard byte
// budget and a one-call helper is pure overhead there.
function compile(pattern) {
	const body = pattern
		.split("**")
		.map((segment) =>
			segment
				.split("*")
				.map((literal) => literal.replace(/[.+?^${}()|[\]\\]/g, "\\$&"))
				.join("[^/]*"),
		)
		.join(".*");

	return new RegExp("^" + body + "$");
}

// excluded reports whether the current page is one the site asked us not to
// count.
//
// The match is against `pathname + hash`, not the pathname alone. Matching the
// pathname only is why `/#/patients/**` never matched anything for the
// incumbent: customers believed their sensitive hash routes were excluded, and
// every one of those URLs was still being sent.
//
// The caller applies this to custom events as well as pageviews. Suppressing
// only pageviews is a real privacy hole — a customer who excludes `/order/*`
// to keep order ids out of the dashboard still has every id arrive attached to
// a custom event's URL.
export function excluded(cfg) {
	if (!cfg.x.length) return false;

	if (!patterns) patterns = cfg.x.map(compile);

	const path = loc.pathname + loc.hash;

	return patterns.some((re) => re.test(path));
}

// stored reads the self-exclusion flag.
//
// The try/catch is the whole point of this function. `window.localStorage`
// does not return null when storage is unavailable, it *throws* — in Chrome
// Incognito inside an iframe, under a "block third-party cookies" setting, and
// in a sandboxed frame. An unguarded read there does not lose the self-exclusion
// feature, it kills the script on line one and the site sends nothing at all.
function stored() {
	try {
		return localStorage.getItem(IGNORE_KEY);
	} catch {
		return null;
	}
}

// ignoreReason reports why this visit will not be counted, or the empty string
// when it will be. It returns the reason rather than a boolean so the console
// warning can say which rule fired — a snippet that is installed correctly and
// silently records nothing is the single most expensive support conversation
// in this product category.
export function ignoreReason(cfg) {
	// The escape hatch. An end-to-end suite drives a real browser, and a real
	// browser under automation sets navigator.webdriver, so without a way to say
	// "count this on purpose" the headless rule below would make the tracker
	// untestable in the only environment that tests it honestly.
	// Headless and automated browsers. The four checks below are the ones that
	// are safe. `window.phantom` is deliberately absent because a widely used
	// wallet extension injects that global into real visitors' pages.
	if (
		!win.__feasible?.track &&
		(win._phantom || win.__nightmare || navigator.webdriver || win.Cypress)
	) {
		return "automated";
	}

	if (!cfg.l && (loc.protocol === "file:" || LOCAL_HOST.test(loc.hostname))) {
		return "localhost";
	}

	if (stored() === "true") return "excluded";

	return "";
}

// warn gives every tracker warning one compressed console prefix. Callers name
// whether tracking stopped or browser durability was reduced.
export function warn(message) {
	console.warn("feasible: " + message);
}
