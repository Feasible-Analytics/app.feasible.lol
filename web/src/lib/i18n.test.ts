//
// i18n.test.ts
// Tests for reading strings out of the catalogue and filling their placeholders.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { t, tx } from "./i18n";

// The catalogue is read once per bundle, so every string these tests use is
// pinned here before the first call.
globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			locale: "en",
			messages: {
				"dashboard.footer.credit": "Made with love by {company}",
				"test.tx.around": "Before {company} after",
				"test.tx.twice": "{company} and {company}",
				"test.tx.unknown": "Hello {who}, from {company}",
				"test.tx.inherited": "{constructor} by {company}",
				"test.tx.unclosed": "By {company} and {open",
				"test.t.values": "{a} then {b}",
			},
		}),
	}),
} as unknown as Document;

/** company is the node every tx test puts at {company}. */
const company = createElement("a", { href: "https://cloudmanic.com" }, "Cloudmanic Labs, LLC");

/** html renders whatever tx returned to markup. */
function html(node: ReturnType<typeof tx>): string {
	return renderToStaticMarkup(createElement("p", null, node));
}

test("tx puts the node at its placeholder", () => {
	assert.equal(
		html(tx("dashboard.footer.credit", { company })),
		'<p>Made with love by <a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a></p>',
	);
});

test("tx keeps the text on both sides of the node in order", () => {
	assert.equal(
		html(tx("test.tx.around", { company })),
		'<p>Before <a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a> after</p>',
	);
});

test("tx fills a placeholder every time it appears", () => {
	assert.equal(
		html(tx("test.tx.twice", { company })),
		'<p><a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a> and <a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a></p>',
	);
});

test("tx leaves an unknown placeholder visible with its braces, as t does", () => {
	assert.equal(
		html(tx("test.tx.unknown", { company })),
		'<p>Hello {who}, from <a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a></p>',
	);
	assert.equal(t("test.tx.unknown", { company: "Cloudmanic" }), "Hello {who}, from Cloudmanic");
});

test("tx leaves an unclosed brace as text", () => {
	assert.equal(
		html(tx("test.tx.unclosed", { company })),
		'<p>By <a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a> and {open</p>',
	);
});

test("tx renders a missing id as the id", () => {
	assert.equal(html(tx("test.tx.nothing_here", { company })), "<p>test.tx.nothing_here</p>");
});

test("a placeholder named after an inherited property is unknown, not filled", () => {
	assert.equal(t("test.tx.inherited", { company: "Cloudmanic" }), "{constructor} by Cloudmanic");
	assert.equal(
		html(tx("test.tx.inherited", { company })),
		'<p>{constructor} by <a href="https://cloudmanic.com">Cloudmanic Labs, LLC</a></p>',
	);
});

test("t never rescans a substituted value", () => {
	assert.equal(t("test.t.values", { a: "{b}", b: 2 }), "{b} then 2");
});
