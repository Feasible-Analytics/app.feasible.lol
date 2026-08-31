//
// i18n.ts
// The dashboard's reader for the one message catalogue.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { bootstrap } from "../api/client";

/**
 * There is one catalogue, and this file does not hold a copy of it.
 *
 * The server negotiates the locale, merges that locale's strings over English
 * string by string, and writes the resulting flat map into the same bootstrap
 * blob the site list already travels in. Every id this dashboard can ask for is
 * therefore already present in what it receives, which is why there is no
 * fallback chain and no bundled catalogue here at all.
 *
 * That is the whole point of doing the merge on the server: the server-rendered
 * screens and this one read the same resolved strings, rather than two
 * implementations of the same fallback rule that drift the first time somebody
 * fixes one of them.
 *
 * A string that is missing anyway renders as its own id — visible on the screen
 * and greppable in the source — because a blank label is a bug nobody reports.
 */

/** The source language, and the plural rules a locale with none of its own is
 *  read under. It matches the Go catalogue's DefaultLocale. */
const DEFAULT_LOCALE = "en";

interface Catalogue {
	locale: string;
	messages: Record<string, string>;
}

/** The parsed blob. It is read once because the bootstrap is written into the
 *  page by the server and cannot change while the page is open, and because
 *  every label on every render would otherwise re-parse the same JSON. */
let loaded: Catalogue | null = null;

/** catalogue reads the blob on first use. It never throws: a page served
 *  without messages renders every string as its id, which is ugly and obvious,
 *  and an obvious failure is the correct one for a missing catalogue. */
function catalogue(): Catalogue {
	if (loaded) return loaded;

	const boot = bootstrap();

	loaded = { locale: boot.locale || DEFAULT_LOCALE, messages: boot.messages };

	return loaded;
}

/** locale is the tag the server chose, for anything that has to branch on the
 *  language itself rather than on a string. */
export function locale(): string {
	return catalogue().locale;
}

/** The tag Intl has already accepted, so the check runs once rather than on
 *  every formatted number. */
let formatting: string | null = null;

/**
 * formatterLocale is the tag to hand Intl.NumberFormat and Intl.DateTimeFormat.
 *
 * Numbers and dates follow the words: a dashboard that says "Besucher" over
 * "1,234" is half-translated in the way people notice. The tag is tried once
 * because a malformed one throws, and it would otherwise throw on every figure
 * on the page rather than in one place with one fallback.
 */
export function formatterLocale(): string {
	if (formatting !== null) return formatting;

	const tag = locale();

	try {
		new Intl.NumberFormat(tag).format(0);
		formatting = tag;
	} catch {
		formatting = DEFAULT_LOCALE;
	}

	return formatting;
}

/**
 * t renders one string.
 *
 * An id with no string comes back as the id, matching the Go side exactly. The
 * alternative — an empty string — is a button with no label and a heading that
 * is simply not there, and neither looks like a missing translation to whoever
 * is looking at it.
 */
export function t(id: string, args?: Record<string, string | number>): string {
	const text = catalogue().messages[id];

	if (!text) return id;

	return interpolate(text, args);
}

/**
 * n renders a string that changes with a count.
 *
 * The id names the base and the plural category is a suffix on it, the same
 * "_one" / "_other" convention the Go catalogue uses, so one set of strings
 * serves both front ends. The count is always available to the string as
 * {count}, so no call site passes it twice.
 */
export function n(id: string, count: number, args?: Record<string, string | number>): string {
	const messages = catalogue().messages;

	// A locale whose categories differ from the source language's falls back to
	// the source language's own category for this count rather than to the id,
	// so an untranslated plural still reads as a sentence.
	const text =
		messages[`${id}_${pluralCategory(locale(), count)}`] ||
		messages[`${id}_${pluralCategory(DEFAULT_LOCALE, count)}`];

	if (!text) return id;

	// The count wins over anything the caller passed under the same name, which
	// is what stops a formatted figure and the plural form disagreeing.
	return interpolate(text, { ...args, count });
}

/**
 * interpolate substitutes {name} placeholders.
 *
 * It is a hand-written scan rather than a replace with a regular expression
 * because the failure mode has to be gentle in the same way the Go side's is:
 * an unknown placeholder is left exactly as written, braces and all, so the
 * broken string is visible and searchable rather than silently blank. A
 * substituted value is never rescanned either, so a search term containing a
 * brace cannot turn into a placeholder.
 */
function interpolate(text: string, args?: Record<string, string | number>): string {
	if (!args || !text.includes("{")) return text;

	let rest = text;
	let out = "";

	for (;;) {
		const open = rest.indexOf("{");
		if (open < 0) break;

		const close = rest.indexOf("}", open);
		if (close < 0) break;

		const value = args[rest.slice(open + 1, close)];

		if (value === undefined) {
			out += rest.slice(0, close + 1);
			rest = rest.slice(close + 1);

			continue;
		}

		out += rest.slice(0, open) + String(value);
		rest = rest.slice(close + 1);
	}

	return out + rest;
}

/**
 * pluralCategory picks the plural form a count takes in a language.
 *
 * It is the same small subset of the CLDR rules the Go side implements, written
 * out rather than pulled in as a dependency: a full rule engine is a large table
 * to answer a question that has two answers in every language the catalogue
 * currently holds. A language with more categories needs its rule here and the
 * extra strings in its files.
 */
function pluralCategory(tag: string, count: number): string {
	const size = Math.abs(count);

	switch (baseLanguage(tag)) {
		case "fr":
			// French treats zero as singular: "0 visiteur", not "0 visiteurs".
			return size <= 1 ? "one" : "other";
		default:
			return size === 1 ? "one" : "other";
	}
}

/** baseLanguage strips a region or script subtag. "de-AT" and "de" share plural
 *  rules, and a table keyed on full tags would need a row per country for no
 *  difference in behaviour. */
function baseLanguage(tag: string): string {
	const index = tag.search(/[-_]/);

	return index > 0 ? tag.slice(0, index) : tag;
}
