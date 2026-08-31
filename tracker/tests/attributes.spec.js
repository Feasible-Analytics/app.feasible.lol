//
// attributes.spec.js
// Every `data-*` option, in its hyphenated spelling, proved to take effect.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// An option nobody spells correctly is an option that does not exist. The
// hyphenated form is HTML's own convention for `data-*` and the only one a
// customer types from memory, so every option gets a test that fails the moment
// that spelling stops working — and the squashed form every existing
// installation already has keeps its own test beside it.

import { test, expect } from "@playwright/test";
import { collect, named, settledCount, waitFor } from "./helpers.js";

// The path prefix these generated pages live under. It is served by the test
// rather than by the fixture server so that the attributes under test sit in
// the spec that asserts them, instead of in twenty near-identical HTML files
// nobody would keep in step.
const GEN = "/gen/";

// serve answers one generated page whose script tag carries exactly `attrs`.
//
// The escape hatch is set the same way the committed fixtures set it: a real
// browser under automation sets navigator.webdriver, so without saying "count
// this on purpose" the headless rule would refuse every visit and every
// assertion below would pass for the wrong reason.
async function serve(page, attrs, body = "") {
	await page.route(`**${GEN}**`, (route) =>
		route.fulfill({
			status: 200,
			contentType: "text/html; charset=utf-8",
			body: `<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8" />
		<title>Attributes — tracker fixture</title>
		<script>
			window.__feasible = Object.assign({ track: 1 }, window.__feasible);
		</script>
		<script defer ${attrs} src="/js/script.js"></script>
	</head>
	<body>${body}</body>
</html>`,
		}),
	);
}

// warnings records what the script said it was refusing to do. A silent refusal
// is the failure mode this whole suite exists to prevent, so the message is
// asserted rather than merely tolerated.
function warnings(page) {
	const lines = [];

	page.on("console", (message) => {
		if (message.type() === "warning") lines.push(message.text());
	});

	return lines;
}

// The one that started this. On the fixture origin — 127.0.0.1 — the tracker
// refuses to count anything unless this option is set, so it is the option
// whose spelling is most expensive to get wrong: the script simply does not
// track, and until the console warning existed there was nothing to go on.
test("data-capture-on-localhost is honoured in its hyphenated spelling", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="attrs.test" data-capture-on-localhost="1"`);
	await page.goto(`${GEN}basic.html`);

	await settledCount(state, "pageview", 1);
	expect(named(state, "pageview")[0].d).toBe("attrs.test");
});

// The spelling every installation that predates the rename already has. HTML
// lowercases attribute names, so `data-captureOnLocalhost` reaches the script
// as `data-captureonlocalhost`; both have to keep working or an upgrade
// silently stops a customer's numbers.
test("data-captureOnLocalhost, the pre-rename spelling, still works", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="attrs.test" data-captureOnLocalhost="1"`);
	await page.goto(`${GEN}basic.html`);

	await settledCount(state, "pageview", 1);
});

// The refusal has to say why. This is the message that turns "the tracker is
// broken" into a five-second diagnosis.
test("without the option, localhost is refused out loud", async ({ page }) => {
	const state = await collect(page);
	const lines = warnings(page);

	await serve(page, `data-domain="attrs.test"`);
	await page.goto(`${GEN}basic.html`);

	await expect.poll(() => lines.join("\n")).toContain("feasible: not tracking — localhost");
	expect(state.events).toHaveLength(0);
});

test("data-domain names the site every event is attributed to", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="named.example" data-capture-on-localhost="1"`);
	await page.goto(`${GEN}basic.html`);

	await settledCount(state, "pageview", 1);
	expect(named(state, "pageview")[0].d).toBe("named.example");
});

// A missing domain is the commonest install mistake there is, and it is named
// as plainly as the rules that suppress a visit on purpose.
test("a missing data-domain is refused out loud", async ({ page }) => {
	const state = await collect(page);
	const lines = warnings(page);

	await serve(page, `data-capture-on-localhost="1"`);
	await page.goto(`${GEN}basic.html`);

	await expect.poll(() => lines.join("\n")).toContain("feasible: not tracking — no data-domain");
	expect(state.events).toHaveLength(0);
});

test("data-api redirects events to a custom endpoint", async ({ page }) => {
	const state = await collect(page);

	await serve(
		page,
		`data-domain="attrs.test" data-capture-on-localhost="1" data-api="https://collector.example/api/event"`,
	);
	await page.goto(`${GEN}basic.html`);

	await settledCount(state, "pageview", 1);
	expect(state.requests[0].url).toBe("https://collector.example/api/event");
});

test("data-exclude suppresses a matching path, and says so", async ({ page }) => {
	const state = await collect(page);
	const lines = warnings(page);

	await serve(page, `data-domain="attrs.test" data-capture-on-localhost="1" data-exclude="/gen/private/*"`);
	await page.goto(`${GEN}private/order.html`);

	await expect.poll(() => lines.join("\n")).toContain("feasible: not tracking — excluded path");
	expect(state.events).toHaveLength(0);
});

test("data-file-types makes an unusual extension count as a download", async ({ page }) => {
	const state = await collect(page);

	await serve(
		page,
		`data-domain="attrs.test" data-capture-on-localhost="1" data-file-types="stl,3mf"`,
		`<a id="model" href="/files/widget.stl">Download the model</a>`,
	);
	await page.goto(`${GEN}basic.html`);
	await settledCount(state, "pageview", 1);

	await page.click("#model");
	await settledCount(state, "File Download", 1);

	expect(named(state, "File Download")[0].p.url).toContain("/files/widget.stl");
});

// The squashed spelling of a two-word option, which is what an installation
// written against the old reader has on the page today.
test("data-filetypes, the squashed spelling, still works", async ({ page }) => {
	const state = await collect(page);

	await serve(
		page,
		`data-domain="attrs.test" data-capture-on-localhost="1" data-filetypes="stl"`,
		`<a id="model" href="/files/widget.stl">Download the model</a>`,
	);
	await page.goto(`${GEN}basic.html`);
	await settledCount(state, "pageview", 1);

	await page.click("#model");
	await settledCount(state, "File Download", 1);
});

// Replacing the list is the point: an extension the customer did not name is
// no longer a download, which is how they narrow it rather than only widen it.
test("data-file-types replaces the default list rather than adding to it", async ({ page }) => {
	const state = await collect(page);

	await serve(
		page,
		`data-domain="attrs.test" data-capture-on-localhost="1" data-file-types="stl"`,
		`<a id="report" href="/files/report.pdf">Download the report</a>`,
	);
	await page.goto(`${GEN}basic.html`);
	await settledCount(state, "pageview", 1);

	await page.click("#report");

	await page.waitForTimeout(500);
	expect(named(state, "File Download")).toHaveLength(0);
});

test("data-alias exposes the API under a second global name", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="attrs.test" data-capture-on-localhost="1" data-alias="analytics"`);
	await page.goto(`${GEN}basic.html`);
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => window.analytics("Signup", { props: { plan: "pro" } }));
	await settledCount(state, "Signup", 1);

	expect(named(state, "Signup")[0].p).toEqual({ plan: "pro" });
});

test("data-manual withholds the automatic pageview until the site asks", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="attrs.test" data-capture-on-localhost="1" data-manual="true"`);
	await page.goto(`${GEN}basic.html`);

	await page.waitForTimeout(500);
	expect(named(state, "pageview")).toHaveLength(0);

	await page.evaluate(() => window.feasible("pageview"));
	await settledCount(state, "pageview", 1);
});

test("data-hash makes a hash route a page of its own", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="attrs.test" data-capture-on-localhost="1" data-hash="true"`);
	await page.goto(`${GEN}basic.html#/first`);
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => {
		location.hash = "#/second";
	});

	const views = await settledCount(state, "pageview", 2);

	expect(views[0].u).toContain("#/first");
	expect(views[1].u).toContain("#/second");
});

// Without the option a fragment is a jump inside one page, not a page of its
// own — which is what keeps one article from becoming a row per heading anchor.
test("without data-hash a fragment change is not a new pageview", async ({ page }) => {
	const state = await collect(page);

	await serve(page, `data-domain="attrs.test" data-capture-on-localhost="1"`);
	await page.goto(`${GEN}basic.html`);
	await settledCount(state, "pageview", 1);

	await page.evaluate(() => {
		location.hash = "#/second";
	});

	await page.waitForTimeout(500);
	expect(named(state, "pageview")).toHaveLength(1);
});

// Every option at once, in the spelling the documentation shows. Options that
// work alone and break in combination is exactly the failure this project's
// single-bundle build exists to rule out, so the combination is asserted.
test("every option together, all hyphenated", async ({ page }) => {
	const state = await collect(page);

	await serve(
		page,
		`data-domain="attrs.test" data-capture-on-localhost="1" data-hash="true" ` +
			`data-alias="analytics" data-file-types="stl" data-exclude="/gen/private/*" ` +
			`data-api="https://collector.example/api/event"`,
		`<a id="model" href="/files/widget.stl">Download the model</a>`,
	);
	await page.goto(`${GEN}basic.html#/start`);

	await settledCount(state, "pageview", 1);
	await page.evaluate(() => window.analytics("Signup"));
	await waitFor(state, (events) => events.some((e) => e.n === "Signup"), "the aliased call arrived");

	expect(state.requests.every((r) => r.url === "https://collector.example/api/event")).toBe(true);
	expect(named(state, "pageview")[0].u).toContain("#/start");
	expect(named(state, "pageview")[0].d).toBe("attrs.test");
});
