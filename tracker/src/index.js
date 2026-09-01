//
// index.js
// Bootstrap: resolve the configuration, decide whether to run, wire everything up.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win } from "./state.js";
import { resolve } from "./config.js";
import { configure } from "./send.js";
import { ignoreReason, warn } from "./exclude.js";
import * as pageview from "./pageview.js";
import * as engagement from "./engagement.js";
import * as clicks from "./clicks.js";

const cfg = resolve();

// api is the function sites call. `feasible('pageview')` and
// `feasible('Signup', {props: {...}})` are the same entry point, because a
// pageview is an event and giving it a second spelling only creates a second
// thing to get wrong.
function api(name, options) {
	if (reason) {
		options?.callback?.({ status: null });
		return;
	}

	if (name === "pageview") pageview.pageview(options);
	else clicks.custom(name, options);
}

// install replaces the queue stub the snippet defined and replays whatever was
// queued before the bundle arrived.
//
// The stub exists so that `feasible('Signup')` in an inline script, or in a
// framework that mounts before a deferred script runs, is not a ReferenceError
// and is not silently lost. Draining it here is what makes those calls arrive.
//
// `alias` exposes the same function under a second global name. It is how a
// site migrating from another provider keeps its existing snippet and its
// existing calls working while it changes one hostname.
function install(fn) {
	for (const name of ["feasible", cfg.n]) {
		if (!name) continue;

		const existing = win[name];
		const queued = existing && existing.q;

		win[name] = fn;

		if (queued) for (const args of queued) fn(...args);
	}
}

// One reason string, one warning. A missing domain is the commonest install
// mistake there is, so it is named as plainly as the rules that suppress a
// visit on purpose.
const reason = ignoreReason(cfg) || (cfg.d ? "" : "no data-domain");

if (reason) {
	warn("not tracking — " + reason);
} else {
	configure(cfg.a, cfg);

	// Engagement and the click handlers are wired before the first pageview so
	// that an interaction on a page that is still deferred — prerendered, or
	// loaded in a background tab — is not lost between the two.
	engagement.start();
	clicks.start(cfg);
	pageview.start(cfg);

	// One script tag still controls the optional mode, but sites that leave it
	// off no longer download the maintained collector at all. The module reports
	// through the public function installed immediately below, retaining the
	// base tracker's consent, bot and captured-route exclusion checks.
	if (cfg.v > 0) import("./vitals.js").then((vitals) => vitals.start(cfg.v));
}

// The queue is drained last, so an event a site queued before the bundle loaded
// arrives after the pageview it belongs to rather than before it.
install(api);
