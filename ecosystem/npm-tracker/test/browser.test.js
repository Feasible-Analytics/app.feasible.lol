//
// browser.test.js
// What init(), track() and the opt-out actually do to a page, against a hand-rolled DOM.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import test from "node:test";
import assert from "node:assert/strict";

import { init, track, pageview, enable, disable, isEnabled } from "../dist/index.mjs";

// installDom puts the smallest DOM this package touches on the global scope: an
// element that remembers attributes, a head that collects children, one
// querySelector, and a localStorage. It is hand-rolled rather than a DOM
// library because the package has no dependencies and its test suite should not
// need one either.
//
// The globals are installed inside the test rather than at module scope, which
// is itself part of the point: the package reads them at call time, so a DOM
// that appears after the import is still found.
function installDom() {
	const scripts = [];
	const store = new Map();

	const head = {
		appendChild(el) {
			scripts.push(el);
			return el;
		},
	};

	const document = {
		head,
		documentElement: head,
		createElement(tag) {
			const attributes = {};

			return {
				tagName: String(tag).toUpperCase(),
				attributes,
				setAttribute(name, value) {
					attributes[name] = String(value);
				},
				getAttribute(name) {
					return name in attributes ? attributes[name] : null;
				},
			};
		},
		querySelector(selector) {
			const match = /^script\[([a-zA-Z-]+)\]$/.exec(selector);
			if (!match) return null;

			return scripts.find((el) => el.getAttribute(match[1]) !== null) || null;
		},
	};

	globalThis.window = {
		localStorage: {
			getItem: (key) => (store.has(key) ? store.get(key) : null),
			setItem: (key, value) => store.set(key, String(value)),
			removeItem: (key) => store.delete(key),
		},
	};

	globalThis.document = document;

	return { scripts, store };
}

// removeDom puts the process back the way it was, so a leaked global cannot
// make the next test pass for the wrong reason.
function removeDom() {
	delete globalThis.window;
	delete globalThis.document;
}

test("init injects one configured script tag and installs the queue stub", (t) => {
	const dom = installDom();
	t.after(removeDom);

	init({
		domain: "example.com",
		host: "https://analytics.example.com/",
		exclude: ["/admin/**", "/preview/*"],
		hashRouting: true,
		manual: true,
		trackLocalhost: true,
		alias: "siteAnalytics",
	});

	assert.equal(dom.scripts.length, 1);

	const el = dom.scripts[0];

	assert.equal(el.tagName, "SCRIPT");
	assert.equal(el.getAttribute("src"), "https://analytics.example.com/js/script.js");
	assert.equal(el.getAttribute("data-domain"), "example.com");
	assert.equal(el.getAttribute("data-exclude"), "/admin/**,/preview/*");
	assert.equal(el.getAttribute("data-hash"), "true");
	assert.equal(el.getAttribute("data-manual"), "true");
	assert.equal(el.getAttribute("data-captureOnLocalhost"), "true");
	assert.equal(el.getAttribute("data-alias"), "siteAnalytics");
	assert.equal(el.getAttribute("defer"), "");

	assert.equal(typeof globalThis.window.feasible, "function");

	// A second init — a re-render, a hot reload, two components — must not
	// install the script twice and double every pageview.
	init({ domain: "example.com" });
	assert.equal(dom.scripts.length, 1);

	delete globalThis.window.feasible;
});

test("a custom script path and the default host are both honoured", (t) => {
	const dom = installDom();
	t.after(removeDom);

	init({ domain: "example.com", scriptPath: "js/fs-abcdefghijklmnop.js" });

	assert.equal(dom.scripts[0].getAttribute("src"), "https://app.feasible.lol/js/fs-abcdefghijklmnop.js");

	delete globalThis.window.feasible;
});

test("init without a domain warns and injects nothing", (t) => {
	const dom = installDom();
	const warnings = [];
	const original = console.warn;

	console.warn = (message) => warnings.push(message);

	t.after(() => {
		console.warn = original;
		removeDom();
	});

	const tracker = init({});

	assert.equal(dom.scripts.length, 0);
	assert.equal(warnings.length, 1);
	assert.match(warnings[0], /needs a domain/);
	assert.equal(typeof tracker.track, "function");
});

test("an event fired before the script arrives is queued and then replayed", async (t) => {
	installDom();
	t.after(() => {
		delete globalThis.window.feasible;
		removeDom();
	});

	init({ domain: "example.com" });

	const pending = track("Signup", { props: { plan: "annual" }, revenue: { amount: 9.99, currency: "USD" } });

	const queue = globalThis.window.feasible.q;

	assert.equal(queue.length, 1);
	assert.equal(queue[0][0], "Signup");
	assert.deepEqual(queue[0][1].props, { plan: "annual" });
	assert.equal(typeof queue[0][1].callback, "function");

	// This is what the tracker script does when it loads: replace the stub and
	// replay whatever the page queued while it was still downloading.
	globalThis.window.feasible = (name, options) => {
		if (options && options.callback) options.callback({ status: 202, dropped: null });
	};

	for (const args of queue) globalThis.window.feasible.apply(null, args);

	assert.deepEqual(await pending, { sent: true, status: 202, dropped: null });
});

test("track passes the caller's own callback through and reports the status", async (t) => {
	installDom();
	t.after(() => {
		delete globalThis.window.feasible;
		removeDom();
	});

	init({ domain: "example.com" });

	globalThis.window.feasible = (name, options) => options.callback({ status: 202, dropped: "shield_ip" });

	const seen = [];
	const result = await pageview({ callback: (response) => seen.push(response) });

	assert.deepEqual(result, { sent: true, status: 202, dropped: "shield_ip" });
	assert.deepEqual(seen, [{ status: 202, dropped: "shield_ip" }]);
});

test("track before init resolves rather than throwing", async (t) => {
	installDom();
	t.after(removeDom);

	assert.deepEqual(await track("Signup"), { sent: false, status: null, dropped: null });
});

test("disable writes the opt-out the script honours, enable clears it", (t) => {
	const dom = installDom();
	t.after(removeDom);

	assert.equal(isEnabled(), true);

	assert.equal(disable(), true);
	assert.equal(dom.store.get("feasible_ignore"), "true");
	assert.equal(isEnabled(), false);

	assert.equal(enable(), true);
	assert.equal(dom.store.has("feasible_ignore"), false);
	assert.equal(isEnabled(), true);
});

test("storage that throws costs the opt-out, not the page", (t) => {
	installDom();
	t.after(removeDom);

	Object.defineProperty(globalThis.window, "localStorage", {
		get() {
			throw new Error("access to storage is denied for this document");
		},
		configurable: true,
	});

	assert.equal(disable(), false);
	assert.equal(enable(), false);
	assert.equal(isEnabled(), true);
});
