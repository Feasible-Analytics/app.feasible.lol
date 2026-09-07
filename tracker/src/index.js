//
// index.js
// Bootstrap: resolve the configuration, decide whether to run, wire everything up.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, globals } from "./state.js";
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

	// init sets the properties every later event carries. It cannot reach a
	// pageview already sent, and a site that needs the first one uses __fsp.
	// `props` is taken as well as `p` because that is how every other call
	// here spells it.
	if (name === "init") declare(options?.p || options?.props);
	else if (name === "pageview") pageview.pageview(options);
	else clicks.custom(name, options);
}

// declare stores the properties every event will carry, bounded and copied.
//
// Bounded because these ride on everything: one oversized value is a body the
// browser refuses to send, on every event, for the life of the install — and
// nothing then reaches the server to be counted. Copied so a later edit of the
// site's own object cannot change what its earlier events said. Wrapped because
// a property backed by a getter that throws would otherwise stop the tracker
// dead. Anything rejected says so out loud.
function declare(given) {
	globals.p = undefined;

	try {
		if (given && typeof given === "object") {
			const kept = {};

			// The server's own caps, applied here as well. Past thirty, or past
			// two thousand characters, is dropped rather than sent.
			for (const key of Object.keys(given).slice(0, 30)) kept[key] = ("" + given[key]).slice(0, 2000);

			globals.p = kept;
			return;
		}
	} catch {}

	if (given) warn("properties ignored");
}

// install replaces the queue stub the snippet defined and replays whatever was
// queued before the bundle arrived.
//
// The stub exists so that `feasible('Signup')` in an inline script, or in a
// framework that mounts before a deferred script runs, is not a ReferenceError
// and is not silently lost. Draining it here is what makes those calls arrive.
// The shipped snippet stubs the default name only; a site that adds an alias is
// editing the snippet already and stubs the second name itself.
//
// Only `feasible` and an explicit `data-alias` are claimed. Taking a global we
// were not given would break whatever already owns it — another analytics tool
// running alongside us keeps its configuration on its own global, and losing it
// stops that tool dead with no error anywhere. A site that wants a second name
// asks for it by name. Reassigning a duplicate is harmless because its queue is
// already gone.
function install(fn) {
	for (const name of ["feasible", cfg.n]) {
		if (!name) continue;

		const queued = win[name]?.q;

		win[name] = fn;

		queued?.forEach((args) => fn(...args));
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

	declare(cfg.p);

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
