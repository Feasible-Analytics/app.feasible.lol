//
// Footer.test.ts
// Tests for the dashboard footer: the version link rule and where it renders.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import type { Bootstrap, Shared } from "../api/types";
import { Footer, WithFooter, releaseURL } from "./Footer";

globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			locale: "en",
			messages: { "dashboard.footer.credit": "Made with love by {company}" },
		}),
	}),
} as unknown as Document;

/** boot is a bootstrap with only the fields the footer reads set. */
function boot(version: string, shared?: Shared): Bootstrap {
	return { sites: ["example.com"], locale: "en", hour_cycle: "24", messages: {}, version, shared };
}

/** share is a shared link's mode, embedded or not. */
function share(embed: boolean): Shared {
	return { mode: "share", base: "/share/abc", domain: "example.com", capability: "abc", embed, storage: !embed };
}

/** frame renders a stand-in screen inside the footer layout. */
function frame(bootstrap: Bootstrap): string {
	return renderToStaticMarkup(createElement(WithFooter, { boot: bootstrap, children: createElement("main", null, "screen") }));
}

test("a clean release tag links to its release page", () => {
	assert.equal(
		releaseURL("v0.0.59"),
		"https://github.com/Feasible-Analytics/app.feasible.lol/releases/tag/v0.0.59",
	);
	assert.equal(
		releaseURL("v12.3.140"),
		"https://github.com/Feasible-Analytics/app.feasible.lol/releases/tag/v12.3.140",
	);
});

test("a version with no release page does not link", () => {
	for (const version of ["dev", "v0.0.59-3-g7b0e58a-dirty", "v0.0.59-dirty", "7b0e58a", "0.0.59", "v0.0", "", "v0.0.59\n"]) {
		assert.equal(releaseURL(version), undefined, version);
	}
});

test("the footer prints a release version as a link that opens a new tab", () => {
	const markup = renderToStaticMarkup(createElement(Footer, { version: "v0.0.59" }));

	assert.match(
		markup,
		/<a href="https:\/\/github.com\/Feasible-Analytics\/app.feasible.lol\/releases\/tag\/v0.0.59" target="_blank" rel="noopener"[^>]*>v0.0.59<\/a>/,
	);
});

test("the footer prints any other version verbatim, without a link", () => {
	for (const version of ["dev", "v0.0.59-3-g7b0e58a-dirty"]) {
		const markup = renderToStaticMarkup(createElement(Footer, { version }));

		assert.match(markup, new RegExp(`<span[^>]*>${version}</span>`));
		assert.doesNotMatch(markup, /github\.com/);
	}
});

test("the footer leaves the version out when the page carried none", () => {
	const markup = renderToStaticMarkup(createElement(Footer, { version: "" }));

	assert.doesNotMatch(markup, /<span/);
	assert.doesNotMatch(markup, /github\.com/);
});

test("the credit is one sentence with the company linked inside it", () => {
	const markup = renderToStaticMarkup(createElement(Footer, { version: "dev" }));

	assert.match(
		markup,
		/<p>Made with love by <a href="https:\/\/cloudmanic.com" target="_blank" rel="noopener"[^>]*>Cloudmanic Labs, LLC<\/a><\/p>/,
	);
});

test("the footer renders under the authenticated dashboard and a shared link", () => {
	for (const bootstrap of [boot("v0.0.59"), boot("v0.0.59", share(false))]) {
		const markup = frame(bootstrap);

		assert.match(markup, /<main>screen<\/main><footer/);
		assert.match(markup, /Made with love by/);
	}
});

test("an embed renders the screen alone, with no footer and no layout around it", () => {
	assert.equal(frame(boot("v0.0.59", share(true))), "<main>screen</main>");
});
