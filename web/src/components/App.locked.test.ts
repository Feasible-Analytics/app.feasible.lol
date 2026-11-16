//
// App.locked.test.ts
// Tests that the dashboard's locked state ends in the page footer.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { App } from "./App";

// The bootstrap is read once per bundle, so this file pins the locked state and
// App.test.ts pins the empty one.
globalThis.document = {
	getElementById: () => ({
		textContent: JSON.stringify({
			sites: ["example.com"],
			locale: "en",
			version: "dev",
			navigation: {
				name: "Ada",
				email: "ada@example.com",
				sites_url: "/sites",
				account_url: "/settings",
				logout_url: "/logout",
				csrf: "token",
			},
			lock: { reason: "lifecycle", error: "Your trial has ended." },
			messages: {
				"dashboard.footer.credit": "Made with love by {company}",
				"dashboard.locked.title": "Your dashboard is locked",
			},
		}),
	}),
} as unknown as Document;

// The locked screen reads its site from the address bar.
globalThis.location = { href: "https://app.example/dashboard/example.com", search: "" } as unknown as Location;

test("a locked account sees the locked screen and then the footer", () => {
	const markup = renderToStaticMarkup(createElement(App));

	assert.match(markup, /Your dashboard is locked[\s\S]*Your trial has ended\.[\s\S]*<\/main><footer/);
	assert.match(markup, /Made with love by <a href="https:\/\/cloudmanic.com"/);
	assert.match(markup, /<span class="tnum">dev<\/span><\/div><\/footer><\/div>$/);
});
