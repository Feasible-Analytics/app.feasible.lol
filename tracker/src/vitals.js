//
// vitals.js
// The optional Web Vitals mode of the main tracker bundle.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { onCLS } from "web-vitals/onCLS.js";
import { onINP } from "web-vitals/onINP.js";
import { onLCP } from "web-vitals/onLCP.js";
import { onTTFB } from "web-vitals/onTTFB.js";
import { win, doc, loc } from "./state.js";

// pending groups metric updates by the navigation identity supplied by the
// maintained Web Vitals implementation. A document may contain several SPA
// routes, and using the current location when the tab is hidden would assign
// the preceding route's final measurements to the route that replaced it.
const pending = new Map();

// reported remembers the metric ids already sent. Repeated tab hiding must not
// duplicate one document observation, while bfcache restoration receives new
// metric ids and is therefore allowed to report again.
const reported = new Set();

// documentURL is the stable fallback for browser versions without soft
// navigation attribution. It deliberately never follows history changes.
const documentURL = loc.href;

// metricKey returns the navigation-level identity shared by the four metrics.
function metricKey(metric) {
	return [metric.navigationType, metric.navigationId, metric.navigationURL || documentURL].join(":");
}

// rounded keeps durations in whole milliseconds and CLS at useful precision.
function rounded(metric) {
	return metric.name === "CLS" ? Math.round(metric.value * 10000) / 10000 : Math.round(metric.value);
}

// flush reports the latest complete values for one navigation exactly once.
function flush(key) {
	const group = pending.get(key);
	if (!group) return;

	const props = {};
	for (const metric of group.metrics.values()) {
		if (reported.has(metric.id)) continue;
		props[metric.name.toLowerCase()] = rounded(metric);
	}

	if (!Object.keys(props).length) return;

	for (const metric of group.metrics.values()) reported.add(metric.id);
	pending.delete(key);

	// The captured navigation URL is handed back explicitly. The base reporter
	// applies privacy rules to this URL, not to whichever SPA route is current
	// by the time a delayed metric reaches this flush.
	win.feasible("Web Vitals", { u: group.url, props, interactive: false });
}

// flushAll finalizes every navigation still waiting when the document hides.
function flushAll() {
	for (const key of [...pending.keys()]) flush(key);
}

// collect records the latest official value for one metric and navigation.
function collect(metric) {
	const key = metricKey(metric);
	const url = metric.navigationURL || documentURL;
	let group = pending.get(key);

	if (!group) {
		group = { url, metrics: new Map() };
		pending.set(key, group);
	}

	group.metrics.set(metric.name, metric);

	// A soft navigation callback arrives after the previous route has reached
	// its final value. Flush it promptly instead of retaining that route until
	// the whole document closes.
	if (url !== loc.href) setTimeout(() => flush(key));
}

// __collectForTest injects one browser-shaped metric into the same pending map
// official callbacks use. It exists so route-transition privacy can be tested
// deterministically even on browsers that do not expose soft-navigation timing
// entries; real supported-browser navigation is exercised separately.
export function __collectForTest(metric) {
	collect(metric);
}

// __flushForTest finalizes deterministic pending metrics through the real
// reporter and exclusion path.
export function __flushForTest() {
	flushAll();
}

// sampled decides once whether this document participates in Vitals capture.
function sampled(rate) {
	const parsed = Number.parseFloat(rate);
	return parsed >= 1 || (parsed > 0 && Math.random() < parsed);
}

// start enables official Web Vitals collection for this document.
export function start(rate) {
	if (!sampled(rate)) return;

	const options = { reportSoftNavs: true };
	onCLS(collect, options);
	onINP(collect, options);
	onLCP(collect, options);
	onTTFB(collect, options);

	// The package delivers pending final values before these listeners because
	// its observers were registered first. This follows its batching guidance:
	// collect updates continuously, then send the final batch when hidden.
	addEventListener("pagehide", flushAll);
	doc.addEventListener("visibilitychange", () => {
		if (doc.visibilityState === "hidden") flushAll();
	});
}
