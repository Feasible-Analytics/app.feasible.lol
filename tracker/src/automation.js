//
// automation.js
// The tells a script leaves that a page can see and a server cannot.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

const win = window;

// The signals, as one letter each. They are letters rather than words because
// every byte here rides on every event, and the server is the only reader.
const NO_WINDOW = "o";
const NO_SCREEN = "s";

/**
* signals reports what looks wrong about this browser, or undefined when
* nothing does.
*
* It reports rather than decides. The server classifies, which means the
* threshold can change without reshipping a script that lives on other
* people's pages, and a visitor we get wrong is a stored row with a reason
* on it rather than a person who silently never existed.
*
* What is deliberately not measured is as important as what is. Nothing here
* reads the GPU, the canvas, the audio stack or the font list. Those are the
* strongest automation tells available and they are also exactly the
* fingerprinting surfaces this product exists to not touch — and the browsers
* that randomise them, Brave and Firefox with resistFingerprinting, belong to
* the privacy-minded people most likely to be reading a site that chose us.
* Detecting bots by fingerprinting real visitors is not a trade worth making.
*
* Everything below is instead a claim the browser makes about itself that
* cannot be true.
*/
export function signals() {
	let found = "";

	try {
		// A real browser is drawn inside a window with tabs and an address bar,
		// so it has an outer size. A headless one has no window at all.
		if (!win.outerWidth && !win.outerHeight) found += NO_WINDOW;

		// A browser with no display to be on.
		if (!win.screen.width || !win.screen.height) found += NO_SCREEN;
	} catch {
		// A browser that throws on any of this is strange, but strange is not
		// the same as automated, and guessing would cost a real visitor.
		return undefined;
	}

	return found || undefined;
}

// Comparing navigator.platform against the operating system named in the user
// agent is not here, though it looks like it should be. Device emulation
// changes one and not the other — Playwright's stock desktop profiles and
// Chrome's own responsive mode both do — so it fires on somebody testing their
// own site rather than on a robot.
