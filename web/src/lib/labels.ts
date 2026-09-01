//
// labels.ts
// Turning the codes the engine stores into the words a person reads.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { formatterLocale } from "./i18n";

/**
 * Geography, languages and devices are stored as codes because a code is stable
 * and a name is not: "Türkiye" was "Turkey" two years ago, and a report keyed on
 * the name would have split one country into two rows on the day it changed.
 *
 * The name is therefore a rendering concern, resolved at read time from the
 * platform's own locale data. Two hundred country names and several hundred
 * language names in every language we ship would be a catalogue nobody could
 * review; Intl already has them, translated and maintained, for the locale the
 * server negotiated.
 */

/** The first code point of the regional indicator block. Two of them side by
 *  side are how every platform draws a flag. */
const REGIONAL_INDICATOR = 0x1f1e6;

const UPPERCASE_A = "A".charCodeAt(0);

/** Intl.DisplayNames is not free to construct, and a report card renders it once
 *  per row. One instance per kind, built on first use. */
const displayNames = new Map<string, Intl.DisplayNames | null>();

/**
 * namer returns a shared Intl.DisplayNames, or null where the browser has no
 * data for that kind.
 *
 * A null is a real answer rather than a failure: every caller falls back to the
 * raw code, which is worse to read but is never wrong.
 */
function namer(type: "region" | "language"): Intl.DisplayNames | null {
	const existing = displayNames.get(type);
	if (existing !== undefined) return existing;

	let built: Intl.DisplayNames | null = null;

	try {
		// The negotiated locale rather than the browser's own: a dashboard whose
		// headings are in German and whose country names are in English is
		// half-translated in the way people notice.
		built = new Intl.DisplayNames(formatterLocale(), { type, fallback: "none" });
	} catch {
		built = null;
	}

	displayNames.set(type, built);

	return built;
}

/** isCountryCode accepts only the two-letter alpha-2 form the geo database
 *  emits, so a region name that happens to be two characters long is not turned
 *  into somebody else's flag. */
function isCountryCode(value: string): boolean {
	return /^[A-Z]{2}$/.test(value);
}

/**
 * countryFlag renders a flag from the ISO code alone.
 *
 * A flag per row as an image would be one request per country on every paint of
 * every card, on a product whose pitch is that it does not make requests you did
 * not ask for. Two regional indicator code points cost nothing and are drawn by
 * the platform. Where a platform has no flag glyphs it draws the two letters
 * instead, which still says which country the row is.
 */
export function countryFlag(code: string): string {
	if (!isCountryCode(code)) return "";

	return String.fromCodePoint(
		REGIONAL_INDICATOR + (code.charCodeAt(0) - UPPERCASE_A),
		REGIONAL_INDICATOR + (code.charCodeAt(1) - UPPERCASE_A),
	);
}

/** countryName resolves an alpha-2 code to its name in the reader's language,
 *  falling back to the code itself. */
export function countryName(code: string): string {
	if (!isCountryCode(code)) return code;

	return namer("region")?.of(code) ?? code;
}

/**
 * languageName resolves an Accept-Language tag.
 *
 * The tag is whatever the browser sent, so it can be a bare language ("en"), a
 * language and region ("pt-BR"), or nonsense. Nonsense comes back as itself
 * rather than as an empty row.
 */
export function languageName(tag: string): string {
	if (!tag) return tag;

	try {
		const resolved = namer("language")?.of(tag);
		if (resolved && resolved !== tag) return resolved;
	} catch {
		// Intl throws for syntactically invalid tags even when fallback is
		// disabled. Analytics input is visitor-owned, so the raw stored value is
		// the safe label for anything the platform refuses to name.
	}

	// A tag the browser cannot name is still worth showing with its region
	// spelled out: "en-XX" reads better than nothing at all.
	return tag;
}

/**
 * regionName renders a first-level subdivision.
 *
 * The geo database gives an ISO-3166-2 code ("US-CA") where it has one and the
 * English name ("England") where it does not, and the stored value is whichever
 * of the two it gave. No locale database names subdivisions, so the code is
 * shown as stored — it is what a filter on this dimension matches, and inventing
 * a prettier spelling here would make the pill and the row disagree.
 */
export function regionName(code: string): string {
	return code;
}

/** countryOfRegion pulls the country out of an ISO-3166-2 code, so a region row
 *  can carry the same flag its country row does. A named region has no country
 *  prefix and gets no flag. */
export function countryOfRegion(code: string): string {
	const [country = ""] = code.split("-");

	return isCountryCode(country) ? country : "";
}

/**
 * valueLabel renders one dimension value for a row, a pill or a drawer.
 *
 * It is one function rather than a formatter per component because the label on
 * a report row and the label on the filter pill that row creates have to be the
 * same string. Two renderers is how a pill ends up reading "US" over a row
 * reading "United States", and the reader cannot tell whether that is two
 * filters or one.
 */
export function valueLabel(dimension: string, value: string): string {
	switch (dimension) {
		case "visit:country":
			return countryName(value);
		case "visit:region":
			return regionName(value);
		case "visit:language":
			return languageName(value);
		default:
			return value;
	}
}

/** flagFor is the emoji a row leads with, or an empty string for a dimension
 *  that has no country in it. Cities are stored as a bare name with no country
 *  beside them, so a city row cannot carry a flag without guessing. */
export function flagFor(dimension: string, value: string): string {
	switch (dimension) {
		case "visit:country":
			return countryFlag(value);
		case "visit:region":
			return countryFlag(countryOfRegion(value));
		default:
			return "";
	}
}
