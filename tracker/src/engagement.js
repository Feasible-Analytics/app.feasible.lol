//
// engagement.js
// Scroll depth and engaged time, measured without polling and without lying.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, doc, page } from "./state.js";
import { send } from "./send.js";

// The engaged time that makes a page worth reporting on its own, with no scroll
// to show for it. Below this a "visit" is a bounce or a misclick.
const MIN_ENGAGED_MS = 3000;

// Layout figures, kept up to date by a ResizeObserver rather than read on
// demand.
//
// This is the difference between a tracker that is invisible and one that shows
// up as an INP failure attributed to us in PageSpeed Insights. Polling
// `scrollHeight` on a timer forces a synchronous layout every tick; reading it
// inside a ResizeObserver callback is free, because the browser has just
// finished laying the page out and that is why it called us.
let docHeight = 0;
let viewport = 0;
let maxScrolled = 0;

// The engaged-time accumulator.
//
// `startedAt` is 0 when the timer is paused, never null. That is not
// defensiveness for its own sake: the incumbent stored null there, and a
// visibility change arriving before the next pageview computed `Date.now() -
// null`, which is `Date.now()` — around 1.7 trillion milliseconds — reported as
// a reading time. The server clamps absurd values too, because old scripts stay
// in browser caches for months, but the arithmetic is guarded here so it never
// happens in the first place.
let startedAt = 0;
let accumulated = 0;
let sentEngaged = 0;
let sentDepth = 0;

// measure recomputes the page geometry. It runs from the ResizeObserver and on
// a viewport resize, which together cover every way the numbers can change:
// images loading, fonts swapping, an accordion opening, a phone rotating.
function measure() {
	const root = doc.documentElement;
	const body = doc.body;

	viewport = win.innerHeight || root.clientHeight || 0;
	docHeight = Math.max(
		root.scrollHeight || 0,
		body ? body.scrollHeight || 0 : 0,
		viewport,
	);

	record();
}

// record notes how far down the page the visitor has reached. Only the deepest
// point of the visit is kept, because scrolling back up is not un-reading.
function record() {
	const top = win.scrollY || doc.documentElement.scrollTop || 0;
	const reached = top + viewport;

	if (reached > maxScrolled) maxScrolled = reached;
}

// depth is the deepest point reached as a whole percentage, 1 to 100. The
// server stores 255 for "never reported", so returning 0 here would be a real
// measurement of zero rather than an absence, and would drag every average down.
function depth() {
	if (!docHeight) return 0;

	const pct = Math.round((Math.min(maxScrolled, docHeight) / docHeight) * 100);

	return Math.max(1, Math.min(100, pct));
}

// engagedMs is how long the visitor has actually been looking at this page.
// Both terms are guarded, so a paused timer contributes zero rather than a
// wall-clock timestamp.
function engagedMs() {
	return accumulated + (startedAt ? Date.now() - startedAt : 0);
}

// resume starts the clock, but only when the visitor is genuinely here.
//
// `hasFocus` is the check that makes the number honest. A tab can be visible
// and yet not be what the person is looking at — another window is in front of
// it — and counting that time turns three seconds of reading followed by a
// minute in a chat app into a minute and three seconds on the page.
function resume() {
	if (!startedAt && doc.visibilityState === "visible" && doc.hasFocus()) {
		startedAt = Date.now();
	}
}

// pause stops the clock and banks what was accumulated.
function pause() {
	if (startedAt) {
		accumulated += Date.now() - startedAt;
		startedAt = 0;
	}
}

// reset starts a fresh measurement for a new page. It runs on an SPA navigation
// and on a bfcache restore, both of which are new pages as far as a reader is
// concerned even though the document never reloaded.
export function reset() {
	maxScrolled = 0;
	accumulated = 0;
	startedAt = 0;
	sentEngaged = 0;
	sentDepth = 0;

	measure();
	resume();
}

// flush reports what has been measured since the last report, if anything is
// worth reporting.
//
// Two rules keep the numbers meaningful. Nothing is sent unless the visitor got
// deeper into the page or spent at least three more seconds on it, so idling in
// a background tab does not generate a stream of identical events. And a
// completely blank engagement — no depth, no time — is dropped here rather than
// sent for the server to throw away.
//
// The time reported is the delta since the last flush, not the running total,
// so the server can sum them. `i: false` marks it as a measurement rather than
// an interaction, which is what keeps it out of the bounce rule.
export function flush() {
	if (!page.tracked) return;

	const reached = depth();
	const total = engagedMs();
	const delta = total - sentEngaged;

	if (reached <= sentDepth && delta < MIN_ENGAGED_MS) return;
	if (reached <= 0 && delta <= 0) return;

	sentDepth = Math.max(sentDepth, reached);
	sentEngaged = total;

	send({
		n: "engagement",
		u: page.url,
		d: page.domain,
		sd: reached,
		e: delta,
		i: false,
	});
}

// start wires the listeners up once, for the life of the document.
//
// `blur` is listed alongside `visibilitychange` on purpose. Alt-tabbing to
// another application leaves the tab visible, so a visibility listener alone
// never fires and the clock keeps running while the person is somewhere else
// entirely. That single missing listener is the difference between reporting
// 3 seconds and reporting 1m03s.
export function start() {
	if (win.ResizeObserver) {
		const observer = new ResizeObserver(measure);
		observer.observe(doc.documentElement);
		if (doc.body) observer.observe(doc.body);
	}

	addEventListener("scroll", record, { passive: true });
	addEventListener("resize", measure, { passive: true });

	const stop = () => {
		flush();
		pause();
	};

	addEventListener("blur", stop);
	addEventListener("focus", resume);

	// pagehide rather than unload: unload is not fired reliably on mobile and
	// registering a handler for it disqualifies the page from the back/forward
	// cache, which would break the restore this tracker depends on.
	addEventListener("pagehide", stop);

	doc.addEventListener("visibilitychange", () => {
		if (doc.visibilityState === "visible") resume();
		else stop();
	});

	measure();
	resume();
}
