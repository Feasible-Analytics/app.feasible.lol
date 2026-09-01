//
// playwright.config.js
// The end-to-end suite's configuration.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { defineConfig, devices } from "@playwright/test";

// The fixture server's port. It is well away from the ports the product uses so
// that a running development instance and a test run never collide.
const PORT = 19311;

// Serverless layout checks inject complete rendered documents directly into
// Chromium and must not open the tracker fixture port.
const serverless = process.env.FEASIBLE_PLAYWRIGHT_SERVERLESS === "1";

export default defineConfig({
	testDir: "./tests",
	fullyParallel: true,
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 1 : 0,
	reporter: process.env.CI ? "list" : [["list"]],

	use: {
		baseURL: `http://127.0.0.1:${PORT}`,
		trace: "retain-on-failure",
	},

	projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],

	// Playwright owns the fixture server's lifetime, which is the only way a
	// test run reliably leaves nothing listening behind it.
	webServer: serverless ? undefined : {
		command: "node tests/server.js",
		url: `http://127.0.0.1:${PORT}/basic.html`,
		reuseExistingServer: !process.env.CI,
		stdout: "ignore",
		env: { PORT: String(PORT) },
	},
});
