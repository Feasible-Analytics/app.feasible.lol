//
// vitals.js
// Core Web Vitals, measured with the Performance API and reported once per page.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// This is a second bundle rather than part of the tracker, and the reason is
// arithmetic: the core script is 3,059 of its 3,072 gzipped bytes, so there is
// no room for another metric in it and the budget is not negotiable. Every site
// pays for the core script; only a site that asked for vitals pays for this.
//
// It sends through the public `feasible()` function rather than its own
// transport. That is what makes it small, and it is also what makes it correct:
// the queue stub means load order does not matter, an excluded page silently
// does nothing because the core script already replaced the function with a
// no-op, and there is exactly one place in the product that knows how an event
// is serialised and posted.
//
// Nothing here reads layout. Every number comes from a PerformanceObserver, so
// the browser hands us measurements it has already taken instead of being asked
// to compute one — which is the difference between a script that measures a
// page's responsiveness and a script that damages it.

const win = window;
const doc = document;

// The event name. Title case, matching the other events the scripts generate
// themselves, so it reads properly in the custom events report.
const EVENT = "Web Vitals";

// CLS is scored over session windows rather than as one running total: a
// five-second window that a new shift extends, up to a second's gap, and the
// worst window is the score. A plain sum would make a long-lived single-page
// application score worse the longer somebody keeps it open, which measures
// session length rather than layout stability.
const WINDOW_MS = 5000;
const GAP_MS = 1000;

// The interaction latencies kept for INP. Ten is what the metric is defined
// against: the reported latency is the one at index count/50, capped at the
// tenth worst, so nothing past the tenth can ever be the answer.
const INP_KEPT = 10;

// The shortest interaction worth an entry, in milliseconds.
//
// The platform default is 104ms, which is well above the point where an
// interaction still feels instant — and INP is a percentile over interactions,
// so dropping every fast one leaves only the slow ones and reports a page as
// worse than it is. Forty is low enough to see the ordinary ones and high
// enough not to make an entry out of every keystroke.
//
// It is meaningful only to the event type; the other observers ignore it, which
// is cheaper than a second parameter nothing else would ever set.
const EVENT_MS = 40;

// measured holds what the observers have seen so far.
const measured = { lcp: 0, fcp: 0, ttfb: 0 };

// The CLS accumulator: the current session window and the worst one so far.
let clsWindow = 0;
let clsWindowStart = 0;
let clsWindowLast = 0;
let cls = 0;

// interactions maps an interaction id to that interaction's longest event
// duration. It is a map rather than a running maximum because INP is a
// percentile over interactions, and one interaction fires several events.
const interactions = new Map();

// sent guards the single report. A page can be hidden, restored and hidden
// again; the numbers are cumulative for the life of the document, so reporting
// twice would be the same measurement counted as two.
let sent = false;

// observe subscribes to one entry type, tolerating a browser that does not have
// it. Every one of these is unsupported somewhere, and a script that throws on
// the first missing type would report nothing at all rather than the four
// metrics that browser can measure.
//
// `buffered` asks for the entries that happened before this script loaded,
// which is most of them: paint and the largest contentful paint are usually
// already over by the time a deferred script runs.
function observe(type, handler) {
	try {
		new PerformanceObserver((list) => handler(list.getEntries())).observe({
			type,
			buffered: true,
			durationThreshold: EVENT_MS,
		});
	} catch {}
}

// start wires up the observers.
function start() {
	// The largest contentful paint is reported repeatedly as bigger elements
	// render, and the last one before the page is hidden is the answer. Taking
	// the largest rather than the last guards the case where an entry arrives
	// with a smaller render time than one already seen.
	observe("largest-contentful-paint", (entries) => {
		for (const entry of entries) {
			const at = entry.renderTime || entry.loadTime || entry.startTime;
			if (at > measured.lcp) measured.lcp = at;
		}
	});

	observe("paint", (entries) => {
		for (const entry of entries) {
			if (entry.name === "first-contentful-paint") measured.fcp = entry.startTime;
		}
	});

	// Only shifts the visitor did not cause count. A layout change half a
	// second after a click is the page responding to them, not moving under
	// them, and counting it would penalise every accordion on the internet.
	observe("layout-shift", (entries) => {
		for (const entry of entries) {
			if (entry.hadRecentInput) continue;

			if (
				clsWindow &&
				entry.startTime - clsWindowLast < GAP_MS &&
				entry.startTime - clsWindowStart < WINDOW_MS
			) {
				clsWindow += entry.value;
			} else {
				clsWindow = entry.value;
				clsWindowStart = entry.startTime;
			}

			clsWindowLast = entry.startTime;
			if (clsWindow > cls) cls = clsWindow;
		}
	});

	// One interaction — a tap, a key press — produces several event entries,
	// and the interaction's latency is the longest of them.
	observe("event", (entries) => {
		for (const entry of entries) {
			const id = entry.interactionId;
			if (!id) continue;

			if (!(interactions.get(id) >= entry.duration)) interactions.set(id, entry.duration);
		}
	});

	// Time to first byte comes off the navigation entry rather than an
	// observer, because it is a single value the browser recorded before any
	// script ran.
	try {
		const nav = performance.getEntriesByType("navigation")[0];
		if (nav) measured.ttfb = nav.responseStart;
	} catch {}
}

// inp is the interaction to next paint: the latency at index count/50, capped
// at the tenth worst.
//
// It is the definition rather than the worst interaction, and the difference
// matters on a page people actually use. The worst single interaction on a form
// with two hundred keystrokes is an outlier — a garbage collection, a
// backgrounded tab — and reporting it as INP would fail a page that is fine.
function inp() {
	if (!interactions.size) return 0;

	const sorted = [...interactions.values()].sort((a, b) => b - a).slice(0, INP_KEPT);
	const index = Math.min(Math.floor(interactions.size / 50), sorted.length - 1);

	return sorted[index];
}

// report sends the measurements, once.
//
// It runs when the page is hidden rather than on load, and that is not a
// preference: cumulative layout shift and interaction to next paint are not
// final until the visitor has stopped looking at the page, so a report sent any
// earlier is a smaller number than the truth. Arriving late is fine — the event
// carries the URL it was measured on, and the server's session rules do not
// depend on the order events arrive in.
function report() {
	if (sent) return;

	const props = {};

	// Whole milliseconds for the durations. A largest contentful paint of
	// 2413.7999999523 is not more accurate than 2414 and is three more bytes on
	// every request.
	if (measured.lcp) props.lcp = Math.round(measured.lcp);
	if (measured.fcp) props.fcp = Math.round(measured.fcp);
	if (measured.ttfb) props.ttfb = Math.round(measured.ttfb);

	const latency = inp();
	if (latency) props.inp = Math.round(latency);

	// Layout shift is a unitless score under one, so it keeps four decimals.
	// The other four are milliseconds and round to whole numbers.
	if (cls) props.cls = Math.round(cls * 10000) / 10000;

	// A page nobody interacted with that painted before this script loaded has
	// nothing to say, and an event with no properties is a row that costs
	// storage and answers nothing.
	if (!Object.keys(props).length) return;

	sent = true;

	// Not interactive: a measurement is not something the visitor did, so it
	// must not end a bounce. A page somebody read and left is still a bounce
	// however well it performed.
	if (typeof win.feasible === "function") {
		win.feasible(EVENT, { props, interactive: false });
	}
}

// sample decides whether this page load reports at all.
//
// Vitals are a distribution, not a total: a tenth of the page loads describes
// the same curve as all of them, and on a busy site the other nine tenths are a
// request each for nothing. The default is every load, because under-reporting
// by default is a decision the site owner should make rather than one we make
// quietly on their behalf — `data-sample="0.1"` is how they make it.
function sample() {
	const el = doc.currentScript || doc.querySelector("script[data-vitals]");
	const rate = parseFloat((el && el.getAttribute("data-sample")) || "1");

	if (!(rate > 0)) return false;

	return rate >= 1 || Math.random() < rate;
}

if (sample()) {
	start();

	// pagehide as well as visibilitychange: a page navigated away from on iOS
	// may never go hidden, and one hidden by a tab switch never fires pagehide.
	// Reporting is guarded, so both firing costs one report.
	addEventListener("pagehide", report);

	doc.addEventListener("visibilitychange", () => {
		if (doc.visibilityState === "hidden") report();
	});
}
