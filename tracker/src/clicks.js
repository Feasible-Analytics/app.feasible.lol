//
// clicks.js
// Outbound links, downloads, tagged events and form submissions — without breaking the page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { doc, loc, page } from "./state.js";
import { send, KEEPALIVE } from "./send.js";

// The class prefix that tags an element for tracking.
const TAG = "feasible-event-";

// How far up the tree to look for a tag. A click lands on whatever is under the
// cursor — the `<svg>` inside a `<span>` inside the `<button>` that carries the
// class — so the element the site tagged is routinely three levels above the
// event target.
const TAG_DEPTH = 3;

// How long the no-keepalive fallback will hold a form submission before giving
// up and letting it through anyway. The event is worth a moment; the customer's
// signup is worth more.
const SUBMIT_TIMEOUT = 500;

let cfg = null;

// classesOf returns an element's class list as a string.
//
// `className` is not used because on an SVG element it is an SVGAnimatedString
// rather than a string, and calling a string method on it throws — which would
// take out the click handler for every icon button on the page.
function classesOf(el) {
	return (el.getAttribute && el.getAttribute("class")) || "";
}

// tagFor walks up from a click target looking for a tagged element.
function tagFor(el) {
	for (let i = 0; i <= TAG_DEPTH && el && el.getAttribute; i++) {
		if (classesOf(el).indexOf(TAG) >= 0) return el;
		el = el.parentElement;
	}

	return null;
}

// parseTag reads the event name and properties out of an element's classes,
// returning them as a `[name, props]` pair or null when the element carries no
// usable tag.
//
// The format is `feasible-event-<key>=<value>`, with `name` naming the event
// and every other key becoming a property. `+` stands for a space, because a
// class attribute cannot contain one. A bare `feasible-event-<Name>` with no
// `=` is the same thing with the `name=` left off, which is why a missing `=`
// resolves to the `name` key rather than to a branch of its own: `slice(0)`
// over the whole segment is exactly the value the shorthand means.
function parseTag(el) {
	let name = "";
	const props = {};

	for (const cls of classesOf(el).split(/\s+/)) {
		if (cls.indexOf(TAG) !== 0) continue;

		const rest = cls.slice(TAG.length);
		const eq = rest.indexOf("=");

		const key = eq < 0 ? "name" : rest.slice(0, eq);
		const value = rest.slice(eq + 1).replace(/\+/g, " ");

		if (key === "name") name = value;
		else props[key] = value;
	}

	return name ? [name, props] : null;
}

// custom sends one named event. It is the single path for everything that is
// not a pageview: `feasible('Signup')`, an outbound click, a download, a tagged
// element and a form submission all arrive here.
//
// Exclusions are applied to all of them, which is not optional politeness. A
// customer who excludes `/order/*` is doing it to keep order ids out of the
// dashboard; if custom events are exempt, every one of those ids still arrives
// attached to the URL of the event. That is the exact hole this closes.
//
// Revenue arrives as `{amount, currency}` and goes out under `$`, which is the
// wire name the server reads. The callback is best effort: an excluded page
// answers it immediately with a null status so a signup form is not left
// waiting, and an ad blocker never answers it at all.
export function custom(name, options) {
	const opts = options || {};
	const callback = opts.callback;
	const captured = opts.u ? new URL(opts.u, loc.href).href : "";

	const event = {
		n: name,
		u: captured || page.u || loc.href,
		d: page.d,
	};

	if (opts.props) event.p = opts.props;
	if (opts.revenue) event.$ = opts.revenue;
	if (opts.interactive === false) event.i = false;

	send(event, callback);
}

// isDownload decides whether a link points at a file. `download` and
// `data-fs-download` are honoured as explicit overrides, because extension
// matching cannot see a blob URL or a Content-Disposition endpoint.
function isDownload(a) {
	if (a.hasAttribute("download") || a.hasAttribute("data-fs-download")) return true;

	const match = /\.([a-z0-9]+)$/i.exec(a.pathname || "");

	return !!match && cfg.f.indexOf("," + match[1].toLowerCase() + ",") >= 0;
}

// onClick handles every click on the page.
//
// The single most important thing this function does is nothing: it never calls
// preventDefault and never assigns to window.location. Because the request is
// sent with `keepalive` the browser finishes it after the page is gone, so
// there is no reason to hold the navigation up — and holding it up is what
// produced three separate breakages for the incumbent. Ctrl-clicking an
// outbound link stopped opening a new tab. Lightboxes broke, because the
// tracker overrode a click another script had already claimed. And
// `<a target="_top">` inside an iframe stopped escaping the frame, because the
// interception replaced the browser's own target handling with a bare
// assignment to location.
function onClick(event) {
	// A middle click opens a new tab and is a real click-through; any other
	// auxiliary button is not.
	if (event.type === "auxclick" && event.button !== 1) return;

	// Something else has already claimed this click — a lightbox, a router, a
	// confirm dialog — so it is not the navigation it appears to be, and
	// counting it would report click-throughs that never happened.
	if (event.defaultPrevented) return;

	const target = event.target;
	if (!target || !target.closest) return;

	const tagged = tagFor(target);

	// A tagged control that submits a form is handled by the submit listener
	// instead, so that one submission produces one event rather than two.
	if (tagged && !submits(tagged)) {
		const parsed = parseTag(tagged);
		if (parsed) custom(parsed[0], { props: parsed[1] });
	}

	const link = target.closest("a[href]");
	if (!link) return;

	const options = { props: { url: link.href } };

	if (isDownload(link)) {
		custom("File Download", options);
		return;
	}

	// `link.host` is the current host for a relative href, so this comparison
	// needs no special case for one. Only http(s) counts: `mailto:` and `tel:`
	// are not page navigations and belong to whatever the site tags them as.
	if (/^https?:$/.test(link.protocol) && link.host !== loc.host) {
		custom("Outbound Link: Click", options);
	}
}

// submits reports whether an element submits a form when clicked. Anything with
// a `form` property is a form control, and a control with no explicit type
// defaults to submit.
function submits(el) {
	return el.tagName === "FORM" || (el.form && (el.type || "submit") === "submit");
}

// onSubmit records a form submission.
//
// The normal path sends and returns, letting the browser submit the form as it
// always would. The fallback below runs only where `fetch` cannot keep a
// request alive past the page, and it is the one place this tracker ever holds
// a user action up.
function onSubmit(event) {
	if (event.defaultPrevented) return;

	const form = event.target;

	// Set when we resubmit a form ourselves. Without it, requestSubmit would
	// re-enter this handler and hold the submission up a second time, forever.
	if (form.__fs) return;

	const parsed = parseTag(tagFor(event.submitter || form) || form);
	const name = parsed ? parsed[0] : "Form: Submission";
	const props = parsed ? parsed[1] : {};

	if (KEEPALIVE) {
		custom(name, { props });
		return;
	}

	// No keepalive: the request would be killed by the navigation, so the
	// submission waits for it — briefly, and never longer than the timeout.
	event.preventDefault();

	let done = false;

	const resubmit = () => {
		if (done) return;
		done = true;
		form.__fs = 1;

		// requestSubmit, never submit.
		//
		// `form.submit()` drops the name and value of the button that was
		// clicked, which silently breaks every multi-button form on the server
		// side — "save" and "save and publish" become indistinguishable. Worse,
		// `submit` is shadowed entirely by any control in the form with
		// `id="submit"` or `name="submit"`, and the call throws "not a
		// function". requestSubmit takes the submitter and has neither problem.
		if (form.requestSubmit) form.requestSubmit(event.submitter);
		else form.submit();
	};

	custom(name, { props, callback: resubmit });
	setTimeout(resubmit, SUBMIT_TIMEOUT);
}

// start attaches the listeners.
//
// They are attached in the bubble phase on the document, which is what makes
// `event.defaultPrevented` meaningful: every handler the page itself installed
// has already run by the time we see the event, so we can tell a click that is
// going somewhere from one that has been claimed.
export function start(config) {
	cfg = config;

	doc.addEventListener("click", onClick);
	doc.addEventListener("auxclick", onClick);
	doc.addEventListener("submit", onSubmit);
}
