//
// atoms.test.ts
// What an empty answer says, and whether the engine's caveats survive to a screen.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { before, test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { Caveats, Empty } from "./atoms";

const HINT = "Try a wider date range.";

before(() => {
	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				locale: "en",
				messages: {
					"dashboard.empty.no_data": "No {what} in this period",
					"dashboard.empty.hint": HINT,
				},
			}),
		}),
	} as unknown as Document;
});

test("an empty answer with no explanation still suggests a wider range", () => {
	const markup = renderToStaticMarkup(createElement(Empty, { what: "sources" }));

	assert.match(markup, /No sources in this period/);
	assert.match(markup, /Try a wider date range/);
});

test("an empty answer the engine explained does not blame the date range", () => {
	const because = "Your page filter was applied to the page each visit started on.";
	const markup = renderToStaticMarkup(createElement(Empty, { what: "sources", because: [because] }));

	assert.match(markup, /No sources in this period/);
	assert.match(markup, new RegExp(because.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));

	// The hint is good advice when there really was nothing in the period, and
	// actively wrong when something else already explains the emptiness.
	assert.doesNotMatch(markup, /Try a wider date range/);
});

test("caveats render one paragraph each and nothing at all when there are none", () => {
	assert.equal(renderToStaticMarkup(createElement(Caveats, { notes: [] })), "");

	const markup = renderToStaticMarkup(createElement(Caveats, { notes: ["First thing.", "Second thing."] }));

	assert.match(markup, /First thing\./);
	assert.match(markup, /Second thing\./);
	assert.match(markup, /role="note"/);
});
