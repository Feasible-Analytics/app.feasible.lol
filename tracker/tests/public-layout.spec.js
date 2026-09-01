//
// public-layout.spec.js
// Browser layout checks for every server-rendered public page at narrow viewports.
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

const css = readFileSync(new URL("../../internal/pages/assets/pages.css", import.meta.url), "utf8");

const repoRoot = fileURLToPath(new URL("../..", import.meta.url));
const fixtureDir = mkdtempSync(join(tmpdir(), "feasible-public-pages-"));

// The Go test renders through the real route table into a temporary directory.
// Chromium receives complete documents with inline production CSS, so no HTTP
// listener or application server is involved.
execFileSync("go", ["test", "./internal/pages", "-run", "^TestWritePublicBrowserFixtures$", "-count=1"], {
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

		const layout = await page.evaluate(() => ({
			documentWidth: document.documentElement.scrollWidth,
			viewportWidth: window.innerWidth,
			brand: document.querySelector(".brand").getBoundingClientRect().toJSON(),
			nav: document.querySelector("header nav").getBoundingClientRect().toJSON(),
			links: [...document.querySelectorAll("header nav a")].map((link) => link.getBoundingClientRect().toJSON()),
			brandFont: Number.parseFloat(getComputedStyle(document.querySelector(".brand")).fontSize),
			navFont: Number.parseFloat(getComputedStyle(document.querySelector("header nav a")).fontSize),
		}));

		expect(layout.documentWidth).toBeLessThanOrEqual(layout.viewportWidth);
		expect(layout.brand.x).toBeGreaterThanOrEqual(0);
		expect(layout.brand.right).toBeLessThanOrEqual(width);
		expect(layout.nav.x).toBeGreaterThanOrEqual(0);
		expect(layout.nav.right).toBeLessThanOrEqual(width);
		for (const link of layout.links) {
			expect(link.x).toBeGreaterThanOrEqual(0);
			expect(link.right).toBeLessThanOrEqual(width);
		}
		expect(layout.brandFont).toBeLessThanOrEqual(16);
		expect(layout.navFont).toBe(14);
		if (fixture.path === "/pricing" && width === 320) {
			const directions = await page.locator(".timeline li").evaluateAll((elements) => elements.map((element) => getComputedStyle(element).flexDirection));
			expect(directions).toEqual(["column", "column", "column", "column"]);
		}
	});
}
