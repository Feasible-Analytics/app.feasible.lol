//
// public-layout.spec.js
// Browser layout checks for the billing screens at narrow viewports.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { expect, test } from "@playwright/test";

// The application's compiled stylesheet, the same file the binary serves at
// /app/assets/app.css. It is the CSS that decides whether these screens fit.
const css = readFileSync(new URL("../../internal/auth/assets/app.css", import.meta.url), "utf8");

const repoRoot = fileURLToPath(new URL("../..", import.meta.url));
const fixtureDir = mkdtempSync(join(tmpdir(), "feasible-billing-pages-"));

// The Go test renders through the real route table into a temporary directory.
// Chromium receives complete documents with inline production CSS, so no HTTP
// listener or application server is involved.
execFileSync("go", ["test", "./internal/billingui", "-run", "^TestWritePublicBrowserFixtures$", "-count=1"], {
	cwd: repoRoot,
	env: { ...process.env, FEASIBLE_PUBLIC_FIXTURE_DIR: fixtureDir },
	stdio: "inherit",
});

const fixtures = JSON.parse(readFileSync(join(fixtureDir, "manifest.json"), "utf8"));

test.afterAll(() => rmSync(fixtureDir, { recursive: true, force: true }));

for (const width of [320, 381, 390]) {
	// Each width gets its own browser test so a regression names the affected
	// viewport directly rather than hiding it in one aggregate assertion.
	for (const fixture of fixtures) test(`${fixture.path} fits a ${width}px viewport`, async ({ page }) => {
		const body = readFileSync(join(fixtureDir, fixture.file), "utf8");
		await page.setViewportSize({ width, height: 640 });
		await page.setContent(body.replace("</head>", `<style>${css}</style></head>`));

		const layout = await page.evaluate(() => {
			// Every element that draws something, measured against the viewport.
			// The document's scroll width alone misses a box overflowing to the
			// left, which is the one a right-to-left reader sees first.
			const overflowing = [...document.querySelectorAll("body *")]
				.filter((element) => {
					const box = element.getBoundingClientRect();

					return box.width > 0 && (box.left < -0.5 || box.right > window.innerWidth + 0.5);
				})
				.map((element) => `${element.tagName.toLowerCase()}.${element.className}`.slice(0, 120));

			return {
				documentWidth: document.documentElement.scrollWidth,
				viewportWidth: window.innerWidth,
				overflowing: overflowing.slice(0, 5),
				header: document.querySelector("header").getBoundingClientRect().toJSON(),
				// A buy button off the side of a phone is a sale that does not
				// happen.
				buttons: [...document.querySelectorAll("button[type=submit], a.btn")]
					.map((element) => element.getBoundingClientRect().toJSON()),
			};
		});

		expect(layout.overflowing).toEqual([]);
		expect(layout.documentWidth).toBeLessThanOrEqual(layout.viewportWidth);

		expect(layout.header.x).toBeGreaterThanOrEqual(0);
		expect(layout.header.right).toBeLessThanOrEqual(width);

		for (const button of layout.buttons) {
			expect(button.x).toBeGreaterThanOrEqual(0);
			expect(button.right).toBeLessThanOrEqual(width);
		}
	});
}
