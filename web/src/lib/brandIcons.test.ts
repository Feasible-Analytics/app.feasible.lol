//
// brandIcons.test.ts
// Every browser and operating system the parser names must resolve to a mark or
// a deliberate globe.
//
// Created: 2026-09-13
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import assert from "node:assert/strict";
import { test } from "node:test";

import { iconFor } from "./brandIcons";

/** Every name internal/useragent/useragent.go can produce. The table below is
 *  the whole vocabulary of the two cards, so a browser added to the parser
 *  without a decision here shows a globe by accident rather than on purpose. */
const BROWSERS = [
	"Chrome",
	"Chromium",
	"Firefox",
	"Safari",
	"Microsoft Edge",
	"Opera",
	"Brave",
	"Vivaldi",
	"DuckDuckGo",
	"Samsung Internet",
	"Yandex Browser",
	"Internet Explorer",
	"Mobile App",
];

const OPERATING_SYSTEMS = [
	"Windows",
	"macOS",
	"iOS",
	"iPadOS",
	"Android",
	"Chrome OS",
	"GNU/Linux",
	"Ubuntu",
	"Fedora",
	"FreeBSD",
];

/** The names that show a globe. None has a public-domain mark, and none carries
 *  enough traffic to be worth drawing one by hand the way Edge and Windows were. */
const GLOBE = new Set([
	"Internet Explorer",
	"Samsung Internet",
	"Yandex Browser",
	"Mobile App",
]);

test("every parsed browser and operating system is either drawn or deliberately a globe", () => {
	for (const name of [...BROWSERS, ...OPERATING_SYSTEMS]) {
		const icon = iconFor(name);

		if (GLOBE.has(name)) {
			assert.equal(icon, null, `${name} is on the globe list but has a mark`);
			continue;
		}

		assert.ok(icon, `${name} has no mark and is not on the globe list`);
		assert.match(icon.path, /^M/, `${name} has a path that is not a path`);
	}
});

test("a value we have never seen falls through to the globe rather than throwing", () => {
	assert.equal(iconFor("HeyTapBrowser"), null);
	assert.equal(iconFor(""), null);
});

test("a black mark renders in the row's colour so it survives a dark card", () => {
	const colourOf = (name: string) => {
		const icon = iconFor(name);
		assert.ok(icon, `${name} has no mark`);

		return icon.color;
	};

	// Apple's guidelines make its mark black, which is invisible on a dark card.
	assert.equal(colourOf("macOS"), null);
	assert.equal(colourOf("iOS"), null);

	// Everything else carries its own brand colour.
	assert.equal(colourOf("Chrome"), "#4285F4");
	assert.equal(colourOf("Windows"), "#0078D4");
});
