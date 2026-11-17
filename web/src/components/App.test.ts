//
// App.test.ts
// Tests that the dashboard's empty state ends in the page footer.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { App } from "./App";

// The bootstrap is read once per bundle, so this file pins the empty state and
// App.locked.test.ts pins the locked one.
globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			sites: [],
			locale: "en",
			version: "v0.0.59",
			messages: {
				"dashboard.footer.credit": "Made with love by {company}",
				"dashboard.no_sites.title": "No sites yet",
			},
		}),
	}),
} as unknown as Document;

test("an account with no sites sees the empty state and then the footer", () => {
	const markup = renderToStaticMarkup(createElement(App));

	assert.match(markup, /No sites yet[\s\S]*<\/section><\/div><footer/);
	assert.match(markup, /Made with love by <a href="https:\/\/cloudmanic.com"/);
	assert.match(markup, />v0.0.59<\/a><\/div><\/footer><\/div>$/);
});
