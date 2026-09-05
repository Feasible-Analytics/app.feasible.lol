//
// TopBar.test.ts
// Regression tests for the live number's query contract.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import type { Filter, Navigation } from "../api/types";
import type { UrlState } from "../lib/url";
import { CHART_TYPES } from "./MainGraph";
import { accountMenuGroups, currentVisitorsRequest, periodLabel, siteSwitchURL } from "./TopBar";

test("the current visitors number always requests an exact answer", () => {
	const filter: Filter = ["is", "visit:country", ["US"]];
	const request = currentVisitorsRequest([filter]);

	assert.equal(request.exact, true);
	assert.deepEqual(request.filters?.[0], filter);
	assert.deepEqual(request.filters?.at(-1), [
		"is_not",
		"event:name",
		["engagement"],
		{ case_sensitive: true },
	]);
});

test("the live count is planned at event grain", () => {
	// The extra metric is the whole fix for a live number that read zero. Only
	// metrics that count on either table leave the planner free to choose
	// sessions, where excluding engagement means "this visit never pinged" —
	// and almost every real visit pings. Asking for one event-scoped metric
	// forces event grain, where the filter excludes pings rather than people.
	const request = currentVisitorsRequest([]);

	assert.equal(request.metrics[0], "visitors", "the pill reads metrics[0]");
	assert.ok(
		request.metrics.includes("pageviews"),
		"an event-scoped metric must be present or the count collapses to zero",
	);
});

test("custom dates are friendly on the period button", () => {
	globalThis.document = {
		getElementById: () => ({
			textContent: JSON.stringify({
				locale: "en",
				messages: {
					"dashboard.format.date_long": "{month} {day}, {year}",
					"dashboard.format.range": "{from} – {to}",
				},
			}),
		}),
	} as unknown as Document;

	const state: UrlState = {
		domain: "example.com",
		preset: "28d",
		from: "2026-09-01",
		to: "2026-09-01",
		compare: "off",
		filters: [],
		labels: {},
		drawer: null,
		behavior: {
			tab: "goals",
			property: "",
			funnel: 0,
			exploreAnchor: null,
			exploreDirection: "forward",
			exploreGrouping: "exact",
			exploreTrail: [],
		},
	};

	assert.equal(periodLabel(state), "Sep 1, 2026");
});

test("switching sites keeps the period, the comparison and the filters", () => {
	// The picker reloads the page so the server-rendered navigation links match
	// the new site. That reload used to drop the search string, which reset
	// every selector the moment somebody changed site.
	const url = siteSwitchURL("other.example", "?period=day&compare=off&f=is,visit:country,US");

	assert.equal(url, "/dashboard/other.example?period=day&compare=off&f=is,visit:country,US");
});

test("a site with no query string switches cleanly", () => {
	assert.equal(siteSwitchURL("other.example", ""), "/dashboard/other.example");
});

/** account builds a Navigation with every optional destination present, so each
 * test below removes only the one it is about. */
function account(overrides: Partial<Navigation> = {}): Navigation {
	return {
		name: "E2E Test Owner",
		email: "e2e@example.com",
		sites_url: "/sites",
		site_settings_url: "/sites/domain/a.example/settings",
		account_url: "/settings",
		billing_url: "/billing",
		logout_url: "/logout",
		csrf: "token",
		...overrides,
	};
}

/** rowIDs is every row in the menu, flattened, for the presence assertions. */
function rowIDs(groups: ReturnType<typeof accountMenuGroups>): string[] {
	return groups.flatMap((group) => group.rows.map((row) => row.id));
}

test("the account menu is grouped, and ends with a separated sign out", () => {
	const groups = accountMenuGroups(account(), "system", "line", true);

	assert.deepEqual(groups.map((group) => group.id), ["destinations", "help", "graph", "theme", "session"]);

	// Only the two groups of choices carry a heading: the others are
	// self-evident, and a heading over one row reads as a label for that row.
	assert.deepEqual(
		groups.filter((group) => group.label).map((group) => group.id),
		["graph", "theme"],
	);

	assert.deepEqual(groups.at(-1)?.rows.map((row) => row.kind), ["signout"]);
});

test("a destination nobody may reach is not in the menu", () => {
	// Billing is absent for a member who cannot manage it, and site settings is
	// absent when no site is in scope. The server decides both by omitting the
	// URL, so the menu must key off the URL rather than re-deriving the rule.
	assert.ok(rowIDs(accountMenuGroups(account(), "system", "line", true)).includes("billing"));
	assert.ok(!rowIDs(accountMenuGroups(account({ billing_url: undefined }), "system", "line", true)).includes("billing"));

	assert.ok(rowIDs(accountMenuGroups(account(), "system", "line", true)).includes("site_settings"));
	assert.ok(!rowIDs(accountMenuGroups(account({ site_settings_url: undefined }), "system", "line", true)).includes("site_settings"));

	// The two that are always there stay there.
	for (const id of ["sites", "account", "shortcuts", "signout"]) {
		assert.ok(
			rowIDs(accountMenuGroups(account({ billing_url: undefined, site_settings_url: undefined }), "system", "line", true)).includes(id),
			`${id} must be in every menu`,
		);
	}
});

test("exactly one theme is marked current, and it is the one in force", () => {
	for (const theme of ["light", "dark", "system"] as const) {
		const rows = accountMenuGroups(account(), theme, "line", true)
			.flatMap((group) => group.rows)
			.filter((row) => row.kind === "theme");

		assert.equal(rows.length, 3, "all three choices are always offered");

		const current = rows.filter((row) => row.current);

		assert.equal(current.length, 1, `${theme} marked ${current.length} rows current`);
		assert.equal(current[0]?.id, `theme:${theme}`);
	}
});

test("the shortcut row advertises the key that opens the overlay", () => {
	// The whole reason the button left the bar is that the key is discoverable
	// from the menu instead. A row with no key printed loses that.
	const shortcuts = accountMenuGroups(account(), "system", "line", true)
		.flatMap((group) => group.rows)
		.find((row) => row.id === "shortcuts");

	assert.equal(shortcuts?.kind, "action");
	assert.equal(shortcuts?.kind === "action" ? shortcuts.hint : "", "?");
});

test("a locked account is offered no shortcut row it cannot use", () => {
	// A locked dashboard binds no keys at all, so the row would close the menu
	// and do nothing — the silent no-op the house rules forbid. It draws no
	// graph either, so the shape rows go with them for the same reason.
	const groups = accountMenuGroups(account(), "system", null, false);

	assert.deepEqual(groups.map((group) => group.id), ["destinations", "theme", "session"]);
	assert.ok(!rowIDs(groups).includes("shortcuts"));
	assert.ok(!rowIDs(groups).includes("chart:line"));

	// Everything else still stands: a locked account still signs out.
	assert.ok(rowIDs(groups).includes("signout"));
	assert.ok(rowIDs(groups).includes("sites"));
});

test("exactly one graph shape is marked current, and it is the one drawn", () => {
	for (const shape of CHART_TYPES) {
		const rows = accountMenuGroups(account(), "system", shape, true)
			.flatMap((group) => group.rows)
			.filter((row) => row.kind === "chart");

		assert.deepEqual(rows.map((row) => row.id), ["chart:line", "chart:bar"]);

		const current = rows.filter((row) => row.kind === "chart" && row.current);
		assert.equal(current.length, 1, shape);
		assert.equal(current[0]?.id, `chart:${shape}`);
	}
});
