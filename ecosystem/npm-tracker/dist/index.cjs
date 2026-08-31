//
// index.cjs
// The CommonJS build of the loader. Hand-written and committed: there is no bundler and no build step.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

"use strict";

// The hosted analytics host. Self-hosters pass their own to `host`.
const DEFAULT_HOST = "https://app.feasible.lol";

// The default script path. A site with a per-site randomised script passes its
// own path here and nothing else changes.
const DEFAULT_SCRIPT_PATH = "/js/script.js";

// The localStorage key the tracker script itself honours. It is spelled here so
// that opting a browser out through this package and opting it out by hand are
// the same switch.
const IGNORE_KEY = "feasible_ignore";

// The marker on the script tag this package injects, so a second init() — a
// re-render, a hot reload, two components each calling it — cannot install the
// script twice and double every pageview.
const LOADER_ATTRIBUTE = "data-feasible-loader";

// How long a track() promise waits for the script's callback before resolving
// anyway. The callback is best effort by contract: an ad blocker means it never
// fires, and a signup form left waiting forever on a promise that cannot settle
// is a page this package made worse.
const CALLBACK_TIMEOUT = 3000;

// isBrowser is the guard every single access in this file goes through. Nothing
// here reads window, document, navigator or localStorage at module scope —
// importing this package on a server must be inert, because a loader that
// throws during server-side rendering takes the whole page down with it.
function isBrowser() {
	return typeof window !== "undefined" && typeof document !== "undefined";
}

// storage returns localStorage or null. The try/catch is not defensive
// decoration: reading window.localStorage *throws* in a sandboxed frame, in
// private browsing inside an iframe, and under a blocked-cookies setting.
function storage() {
	if (!isBrowser()) return null;

	try {
		return window.localStorage || null;
	} catch {
		return null;
	}
}

// serverResult is what every call resolves to when there is no browser. It says
// `sent: false` rather than throwing, so the same analytics call can sit in a
// component that renders on both sides.
function serverResult() {
	return Promise.resolve({ sent: false, status: null });
}

// serverStub is what init() returns on a server: the same shape as the browser
// object, and every method a no-op. Returning a stub rather than undefined
// means calling code never needs a null check it will forget on one path.
const serverStub = {
	track: () => serverResult(),
	pageview: () => serverResult(),
	enable: () => false,
	disable: () => false,
	isEnabled: () => false,
};

// installQueue puts the queue stub on the page synchronously, before the script
// has loaded. Without it a track() during hydration is either a ReferenceError
// or an event that vanishes; with it the call is held and replayed when the
// script arrives.
//
// The stub goes on the primary global only. The alias exists so a legacy
// snippet keeps working, and installing the same queue under both names would
// have the script replay every queued event twice.
function installQueue() {
	if (typeof window.feasible === "function") return window.feasible;

	const stub = function () {
		(stub.q = stub.q || []).push(arguments);
	};

	window.feasible = stub;

	return stub;
}

// scriptSource builds the URL the script is loaded from. The script is served
// by the analytics host, never bundled into this package, so a fix to the
// tracker reaches every site without anybody publishing a new npm version.
function scriptSource(options) {
	const host = String(options.host || DEFAULT_HOST).replace(/\/+$/, "");
	const path = String(options.scriptPath || DEFAULT_SCRIPT_PATH);

	return host + (path.startsWith("/") ? path : "/" + path);
}

// injectScript adds the script tag, translating the options into the data-*
// attributes the script reads. It returns the existing tag when one is already
// there, which is what makes init() safe to call more than once.
function injectScript(options) {
	const existing = document.querySelector("script[" + LOADER_ATTRIBUTE + "]");
	if (existing) return existing;

	const el = document.createElement("script");

	el.setAttribute("defer", "");
	el.setAttribute(LOADER_ATTRIBUTE, "true");
	el.setAttribute("data-domain", options.domain);

	const exclude = Array.isArray(options.exclude) ? options.exclude.join(",") : options.exclude;

	if (exclude) el.setAttribute("data-exclude", String(exclude));
	if (options.alias) el.setAttribute("data-alias", String(options.alias));
	if (options.hashRouting) el.setAttribute("data-hash", "true");
	if (options.manual) el.setAttribute("data-manual", "true");
	if (options.trackLocalhost) el.setAttribute("data-captureOnLocalhost", "true");

	el.setAttribute("src", scriptSource(options));

	(document.head || document.documentElement).appendChild(el);

	return el;
}

// init loads the tracker and returns the API. On a server it is a documented
// no-op that returns a stub, so it is safe to call at the top of a component
// that renders in both places.
function init(options = {}) {
	if (!isBrowser()) return serverStub;

	const domain = String(options.domain || "").trim();

	if (!domain) {
		// A missing domain is the commonest install mistake there is, and a
		// script installed with no domain records nothing while looking
		// perfectly installed.
		if (typeof console !== "undefined") console.warn("[feasible] init() needs a domain; nothing will be recorded");

		return serverStub;
	}

	// The queue goes in before the script tag, so an event fired between these
	// two lines is held rather than lost.
	installQueue();
	injectScript({ ...options, domain });

	return { track, pageview, enable, disable, isEnabled };
}

// track sends a custom event and resolves with what the server said. The
// promise settles either way — on the callback, or on the timeout — because the
// common reason it never settles is an ad blocker, and code waiting on it is
// usually a form waiting to navigate.
function track(name, options = {}) {
	if (!isBrowser()) return serverResult();

	const feasible = window.feasible;
	if (typeof feasible !== "function") return serverResult();

	return new Promise((resolve) => {
		let settled = false;
		let timer;

		const finish = (result) => {
			if (settled) return;

			settled = true;
			clearTimeout(timer);
			resolve(result);
		};

		const forwarded = {
			...options,
			callback: (response) => {
				if (typeof options.callback === "function") options.callback(response);

				finish({
					sent: true,
					status: response && typeof response.status === "number" ? response.status : null,
				});
			},
		};

		timer = setTimeout(() => finish({ sent: true, status: null }), CALLBACK_TIMEOUT);

		feasible(name, forwarded);
	});
}

// pageview sends a pageview. It is the same entry point as any other event
// because a pageview is an event, and a second spelling would only be a second
// thing to get wrong.
function pageview(options = {}) {
	return track("pageview", options);
}

// enable counts this browser again by clearing the opt-out. It returns whether
// the write happened, because storage can be unavailable and silently claiming
// success would be a person believing they had opted back in.
function enable() {
	const store = storage();
	if (!store) return false;

	try {
		store.removeItem(IGNORE_KEY);
		return true;
	} catch {
		return false;
	}
}

// disable stops counting this browser by writing the opt-out the tracker script
// honours. It is per-browser and per-device: it is how somebody keeps their own
// visits out of their own numbers.
function disable() {
	const store = storage();
	if (!store) return false;

	try {
		store.setItem(IGNORE_KEY, "true");
		return true;
	} catch {
		return false;
	}
}

// isEnabled reports whether this browser is being counted. On a server the
// answer is false — nothing is being counted there. In a browser whose storage
// cannot be read it is true, because that is what the script decides too, and
// two different answers to the same question is worse than either answer.
function isEnabled() {
	if (!isBrowser()) return false;

	const store = storage();
	if (!store) return true;

	try {
		return store.getItem(IGNORE_KEY) !== "true";
	} catch {
		return true;
	}
}

// The default key mirrors the ESM build's default export, so a bundler that
// interops the two shapes gets the same object either way. It is a separate
// object rather than a reference back to module.exports, because a self-
// referencing export is a cycle that breaks any tool that walks it.
const api = { init, track, pageview, enable, disable, isEnabled };

module.exports = { init, track, pageview, enable, disable, isEnabled, default: api };
