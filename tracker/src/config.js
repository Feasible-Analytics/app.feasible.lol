//
// config.js
// Where the tracker's settings come from: baked in at serve time, or data-* attributes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { win, doc } from "./state.js";

// The default download extensions, held as a comma-delimited string with a
// comma at each end so that a lookup is one `indexOf(",pdf,")` rather than a
// parsed array. That shape is also what a customer's `data-file-types` is
// turned into, so both go through the same three lines.
//
// Matching on the extension is the only thing a script can do from an href, and
// it is documented as a limitation rather than hidden: blob URLs,
// Content-Disposition endpoints and extensionless download routes are invisible
// to it, which is why `download` and `data-fs-download` exist as explicit
// overrides on the link itself.
const FILE_TYPES =
	"7z,avi,csv,dmg,docx,exe,gz,key,midi,mov,mp3,mp4,mpeg,pdf,pkg,pps,ppt,pptx,rar,rtf,txt,wav,wma,wmv,xlsx,zip";

// read pulls one setting off the script tag that loaded us. `data-` attributes
// are the entire configuration surface of the legacy-compatible variant, so a
// missing or misspelled one has to degrade to the default rather than throw
// during load.
function read(el, name) {
	return (el && el.getAttribute("data-" + name)) || "";
}

// resolve produces the settings this page runs with.
//
// Two delivery modes share this one bundle. The per-site script has its
// configuration written into a global by the server immediately before the
// bundle, so there are no attributes for a customer to get wrong; the
// legacy-compatible script has nothing baked in and reads `data-domain` and
// `data-api` off its own script tag, so an existing installation migrates by
// repointing one hostname.
//
// The baked global is cleared on read. Leaving it in place would let a second
// copy of the script — a duplicated snippet, a tag manager — pick up the first
// site's configuration and report every event again under the wrong domain.
//
// Baked values are layered over the attributes rather than replacing them, so a
// per-site script whose server knows only the domain still honours a `data-hash`
// the customer added by hand. A setting that silently does nothing is the worst
// kind of setting.
//
// There is no build variant and no feature switch. Every feature is in every
// build, which is exactly why the combinations that broke for the incumbent —
// hash routing with exclusions, hash routing with outbound links, manual mode
// with engagement — cannot break here: there is only one code path to break.
export function resolve() {
	const baked = win.__fsc;
	win.__fsc = null;

	// currentScript is our own tag during synchronous execution, including under
	// `defer`. The querySelector is the fallback for a bundle injected without a
	// script element of its own.
	const el = doc.currentScript || doc.querySelector("script[data-domain]");

	const cfg = {
		d: read(el, "domain"),
		a: read(el, "api"),
		x: read(el, "exclude"),
		f: read(el, "file-types"),
		n: read(el, "alias"),
		// The three settings that change what an event *means* rather than
		// turning a feature off: manual pageviews, hash routing, and counting
		// a developer's own machine.
		m: !!read(el, "manual"),
		h: !!read(el, "hash"),
		l: !!read(el, "captureOnLocalhost"),
		...baked,
	};

	// The endpoint defaults to the origin the script itself came from, which is
	// what makes a reverse proxy work with no second setting to keep in sync.
	if (!cfg.a) {
		let origin = "";

		try {
			origin = new URL(el.src).origin;
		} catch {
			origin = win.origin || "";
		}

		cfg.a = origin + "/api/event";
	}

	cfg.x = cfg.x ? (Array.isArray(cfg.x) ? cfg.x : cfg.x.split(/\s*,\s*/)) : [];
	cfg.f = "," + [].concat(cfg.f || FILE_TYPES).join(",").toLowerCase().replace(/\./g, "") + ",";

	return cfg;
}
