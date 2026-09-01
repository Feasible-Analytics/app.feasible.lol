//
// filters.ts
// The filter model, and the URL encoding that makes a filtered view a link.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Filter } from "../api/types";
import { valueLabel } from "./labels";

/**
 * The URL encoding is a contract, not an implementation detail.
 *
 * A filtered dashboard is something people paste into an email — "look at what
 * mobile visitors from Germany did last week" — so the encoding has to survive
 * being copied, truncated by a mail client and typed back in by hand. That rules
 * out anything base64 or JSON-shaped, and it is why every piece of it is
 * readable:
 *
 *   ?f=is,country,DE&f=is,device,Mobile&l=DE,Germany
 *
 * Repeated `f=` parameters AND together; the values inside one `f=` OR together.
 * That asymmetry is the whole grammar, and it is the same one the query engine
 * compiles, so what the URL says and what the SQL does cannot drift apart.
 *
 * Nothing in this file renders a string. Everything user-facing is a message id
 * the caller translates, which is what keeps it testable in Node — and what
 * stops a filter pill being assembled out of translated fragments.
 */

/** The operators the engine implements. There are no others: an operator the
 *  compiler does not know comes back as a 400 naming it, and inventing one here
 *  would turn a shareable link into an error message. */
export type Operator = "is" | "is_not" | "contains" | "contains_not" | "matches" | "matches_not";

export const OPERATORS: { id: Operator; labelId: string }[] = [
	{ id: "is", labelId: "dashboard.filter.operator.is" },
	{ id: "is_not", labelId: "dashboard.filter.operator.is_not" },
	{ id: "contains", labelId: "dashboard.filter.operator.contains" },
	{ id: "contains_not", labelId: "dashboard.filter.operator.contains_not" },
	{ id: "matches", labelId: "dashboard.filter.operator.matches" },
	{ id: "matches_not", labelId: "dashboard.filter.operator.matches_not" },
];

const OPERATOR_IDS = new Set<string>(OPERATORS.map((entry) => entry.id));

/** One filter. The values OR together, which is what a multi-select in the
 *  filter editor produces. */
export interface FilterState {
	operator: Operator;
	/** The full API dimension, such as `visit:country`. The URL carries a short
	 *  alias; everything above this file works in the long name so there is one
	 *  spelling to compare against. */
	dimension: string;
	values: string[];
}

/** Labels for values whose stored form says nothing on its own. They travel in
 *  the URL so a recipient sees the same pill the sender did, even for a value
 *  their browser cannot name. */
export type FilterLabels = Record<string, string>;

/**
 * One filterable dimension.
 *
 * The alias is what appears in the URL. It exists because `?f=is,country,DE` is
 * something somebody can read, edit and explain over the phone, and
 * `?f=is,visit%3Acountry,DE` is not. The alias is never translated — it is part
 * of the link, and a link that changed language would stop working when it was
 * forwarded.
 */
export interface DimensionDef {
	alias: string;
	dimension: string;
	labelId: string;
	/** The heading this dimension sits under in the filter menu. */
	groupId: string;
	/** False keeps a server-resolved filter readable and round-trippable while
	 * preventing the generic breakdown editor from offering an invalid query. */
	menu?: boolean;
}

export const FILTERABLE: DimensionDef[] = [
	{
		alias: "page",
		dimension: "event:page",
		labelId: "dashboard.dimension.page",
		groupId: "dashboard.filter.group.page",
	},
	{
		alias: "entry_page",
		dimension: "visit:entry_page",
		labelId: "dashboard.dimension.entry_page",
		groupId: "dashboard.filter.group.page",
	},
	{
		alias: "exit_page",
		dimension: "visit:exit_page",
		labelId: "dashboard.dimension.exit_page",
		groupId: "dashboard.filter.group.page",
	},
	{
		alias: "hostname",
		dimension: "event:hostname",
		labelId: "dashboard.dimension.hostname",
		groupId: "dashboard.filter.group.page",
	},
	{
		alias: "page_title",
		dimension: "event:page_title",
		labelId: "dashboard.dimension.page_title",
		groupId: "dashboard.filter.group.page",
	},

	{
		alias: "channel",
		dimension: "visit:channel",
		labelId: "dashboard.dimension.channel",
		groupId: "dashboard.filter.group.acquisition",
	},
	{
		alias: "source",
		dimension: "visit:source",
		labelId: "dashboard.dimension.source",
		groupId: "dashboard.filter.group.acquisition",
	},
	{
		alias: "referrer",
		dimension: "visit:referrer",
		labelId: "dashboard.dimension.referrer",
		groupId: "dashboard.filter.group.acquisition",
	},
	{
		alias: "utm_source",
		dimension: "visit:utm_source",
		labelId: "dashboard.dimension.utm_source",
		groupId: "dashboard.filter.group.acquisition",
	},
	{
		alias: "utm_medium",
		dimension: "visit:utm_medium",
		labelId: "dashboard.dimension.utm_medium",
		groupId: "dashboard.filter.group.acquisition",
	},
	{
		alias: "utm_campaign",
		dimension: "visit:utm_campaign",
		labelId: "dashboard.dimension.utm_campaign",
		groupId: "dashboard.filter.group.acquisition",
	},

	{
		alias: "country",
		dimension: "visit:country",
		labelId: "dashboard.dimension.country",
		groupId: "dashboard.filter.group.location",
	},
	{
		alias: "region",
		dimension: "visit:region",
		labelId: "dashboard.dimension.region",
		groupId: "dashboard.filter.group.location",
	},
	{
		alias: "city",
		dimension: "visit:city",
		labelId: "dashboard.dimension.city",
		groupId: "dashboard.filter.group.location",
	},

	{
		alias: "browser",
		dimension: "visit:browser",
		labelId: "dashboard.dimension.browser",
		groupId: "dashboard.filter.group.device",
	},
	{
		alias: "browser_version",
		dimension: "visit:browser_version",
		labelId: "dashboard.dimension.browser_version",
		groupId: "dashboard.filter.group.device",
	},
	{
		alias: "os",
		dimension: "visit:os",
		labelId: "dashboard.dimension.os",
		groupId: "dashboard.filter.group.device",
	},
	{
		alias: "os_version",
		dimension: "visit:os_version",
		labelId: "dashboard.dimension.os_version",
		groupId: "dashboard.filter.group.device",
	},
	{
		alias: "device",
		dimension: "visit:device",
		labelId: "dashboard.dimension.device",
		groupId: "dashboard.filter.group.device",
	},
	{
		alias: "screen",
		dimension: "visit:screen",
		labelId: "dashboard.dimension.screen",
		groupId: "dashboard.filter.group.device",
	},
	{
		alias: "language",
		dimension: "visit:language",
		labelId: "dashboard.dimension.language",
		groupId: "dashboard.filter.group.device",
	},

	{
		alias: "event",
		dimension: "event:name",
		labelId: "dashboard.dimension.event_name",
		groupId: "dashboard.filter.group.behaviour",
	},
	{
		alias: "goal",
		dimension: "event:goal",
		labelId: "dashboard.column.goal",
		groupId: "dashboard.filter.group.behaviour",
		menu: false,
	},
];

const BY_ALIAS = new Map(FILTERABLE.map((entry) => [entry.alias, entry]));
const BY_DIMENSION = new Map(FILTERABLE.map((entry) => [entry.dimension, entry]));

/** The engine's own ceiling. Parsing stops here rather than building a request
 *  the server is certain to refuse, so a hand-mangled URL degrades to a working
 *  dashboard instead of an error page. */
const MAX_FILTERS = 32;

/** The prefix a custom property dimension carries on the wire. */
const PROP_PREFIX = "event:props:";

/** aliasOf shortens a dimension for the URL, leaving anything with no alias —
 *  a custom property — spelled out in full. */
export function aliasOf(dimension: string): string {
	return BY_DIMENSION.get(dimension)?.alias ?? dimension;
}

/** dimensionOf expands a URL alias back to the API name. A name that is already
 *  a full dimension passes through, so both spellings work in a hand-typed
 *  link. */
export function dimensionOf(alias: string): string {
	return BY_ALIAS.get(alias)?.dimension ?? alias;
}

/**
 * dimensionLabel names a dimension on a pill, as a message id or as a literal.
 *
 * A custom property has no entry in the registry and no translation: the
 * property name is whatever the site's own code sent, and inventing a message
 * id for it would be inventing an id per customer. The flag says which of the
 * two the caller is holding.
 */
export function dimensionLabel(dimension: string): { id: string } | { text: string } {
	const known = BY_DIMENSION.get(dimension);
	if (known) return { id: known.labelId };

	if (dimension.startsWith(PROP_PREFIX)) return { text: dimension.slice(PROP_PREFIX.length) };

	return { text: dimension };
}

/**
 * escapeSegment protects the separator.
 *
 * A page path can contain a comma and a regex certainly can, so the comma that
 * separates the parts of an `f=` cannot also be a character values are allowed
 * to carry unannounced. A backslash escapes itself and a comma; nothing else is
 * special, which keeps the common URL free of escapes entirely.
 */
function escapeSegment(value: string): string {
	return value.replace(/\\/g, "\\\\").replace(/,/g, "\\,");
}

/** splitSegments is the reader for the same encoding. A trailing lone backslash
 *  is taken literally rather than eating the end of the string, because a
 *  truncated link should still parse into something. */
function splitSegments(raw: string): string[] {
	const parts: string[] = [];
	let current = "";

	for (let i = 0; i < raw.length; i++) {
		const character = raw[i];

		if (character === "\\" && i + 1 < raw.length) {
			current += raw[i + 1];
			i++;
			continue;
		}

		if (character === ",") {
			parts.push(current);
			current = "";
			continue;
		}

		current += character;
	}

	parts.push(current);

	return parts;
}

/** encodeFilter renders one filter as the body of an `f=` parameter. */
export function encodeFilter(filter: FilterState): string {
	return [filter.operator, aliasOf(filter.dimension), ...filter.values].map(escapeSegment).join(",");
}

/**
 * decodeFilter reads one `f=` back.
 *
 * Anything it cannot make sense of returns null and is dropped rather than
 * throwing. A link that has been through a mail client, a chat window and a
 * copy-paste has plenty of ways to arrive damaged, and a damaged filter should
 * cost the reader that filter, not the whole page.
 */
export function decodeFilter(raw: string): FilterState | null {
	const parts = splitSegments(raw);
	if (parts.length < 3) return null;

	const [operator = "", alias = "", ...values] = parts;

	if (!OPERATOR_IDS.has(operator)) return null;
	if (!alias) return null;
	const dimension = dimensionOf(alias);
	if (dimension === "event:goal" && operator !== "is" && operator !== "is_not") return null;

	return { operator: operator as Operator, dimension, values };
}

/** encodeLabel renders one `l=` pair. */
export function encodeLabel(value: string, label: string): string {
	return [value, label].map(escapeSegment).join(",");
}

/** decodeLabel reads one back, returning null for anything malformed. */
export function decodeLabel(raw: string): [string, string] | null {
	const parts = splitSegments(raw);
	if (parts.length !== 2) return null;

	const [value = "", label = ""] = parts;
	if (!label) return null;

	return [value, label];
}

/** readFilters pulls every `f=` out of a query string, in order. */
export function readFilters(params: URLSearchParams): FilterState[] {
	const filters: FilterState[] = [];

	for (const raw of params.getAll("f")) {
		const filter = decodeFilter(raw);
		if (filter) filters.push(filter);

		if (filters.length >= MAX_FILTERS) break;
	}

	return filters;
}

/** readLabels pulls every `l=` out of a query string. */
export function readLabels(params: URLSearchParams): FilterLabels {
	const labels: FilterLabels = {};

	for (const raw of params.getAll("l")) {
		const pair = decodeLabel(raw);
		if (pair) labels[pair[0]] = pair[1];
	}

	return labels;
}

/**
 * writeFilters appends the filter state to a query string.
 *
 * Only the labels a filter actually uses are written. Carrying every label the
 * session ever collected would make the URL grow forever as somebody clicks
 * around, and the ones no filter references say nothing to a recipient.
 */
export function writeFilters(params: URLSearchParams, filters: FilterState[], labels: FilterLabels): void {
	const needed = new Set<string>();

	for (const filter of filters) {
		params.append("f", encodeFilter(filter));

		for (const value of filter.values) {
			if (labels[value] !== undefined) needed.add(value);
		}
	}

	for (const value of needed) {
		params.append("l", encodeLabel(value, labels[value] as string));
	}
}

/**
 * toApi turns the filter list into the positional wire form.
 *
 * Every filter is sent case-sensitive. That is the engine's default and the one
 * people mean by "is": a country code, a device class and a path are all stored
 * exactly as they will be filtered, and folding case would quietly widen a
 * filter somebody built by clicking a row.
 */
export function toApi(filters: FilterState[]): Filter[] {
	return filters.map((filter): Filter => [
		filter.operator,
		filter.dimension,
		filter.values,
		{ case_sensitive: true },
	]);
}

/** same reports whether two filters are the same predicate, so clicking a row
 *  twice can toggle rather than stack two identical filters. */
function same(a: FilterState, b: FilterState): boolean {
	return (
		a.operator === b.operator &&
		a.dimension === b.dimension &&
		a.values.length === b.values.length &&
		a.values.every((value, index) => value === b.values[index])
	);
}

/**
 * toggle is what clicking a report row does.
 *
 * Clicking the same row again removes the filter, and clicking a different row
 * of the same report replaces it. Stacking `country is US` and `country is DE`
 * instead would produce a dashboard with no rows at all, which reads as the
 * filter being broken rather than as two filters that cannot both be true.
 */
export function toggle(filters: FilterState[], next: FilterState): FilterState[] {
	if (filters.some((filter) => same(filter, next))) {
		return filters.filter((filter) => !same(filter, next));
	}

	const others = filters.filter((filter) => filter.dimension !== next.dimension || filter.operator !== next.operator);

	return [...others, next];
}

/** remove drops the filter at one position, which is what a pill's ✕ does. */
export function remove(filters: FilterState[], index: number): FilterState[] {
	return filters.filter((_, position) => position !== index);
}

/** replace swaps one filter in place, so editing a pill does not send it to the
 *  end of the row and make the reader hunt for it. */
export function replace(filters: FilterState[], index: number, next: FilterState): FilterState[] {
	return filters.map((filter, position) => (position === index ? next : filter));
}

/**
 * pillMessage picks the sentence a pill is rendered from.
 *
 * There is one complete sentence per operator rather than a dimension, a verb
 * and a value glued together at render time. Word order is not universal, and a
 * translator handed three fragments cannot put them in the order their own
 * grammar needs — which is exactly how a filter pill ends up reading backwards
 * in the languages nobody on the team can check.
 *
 * Two or more values collapse to a count, because spelling out a five-value OR
 * would push every other pill off the row. The full list is on the pill's title.
 */
export function pillMessage(filter: FilterState): string {
	const pair = PILL_MESSAGES[filter.operator];

	return filter.values.length > 1 ? pair.many : pair.one;
}

/**
 * The twelve sentences, written out.
 *
 * Building the id from the operator would be three lines shorter and invisible
 * to the catalogue's completeness check, which scans the source for literal ids
 * — so half of these would be reported as strings nobody uses and deleted by
 * somebody tidying up. An id that only exists at runtime is an id no tool can
 * see.
 */
const PILL_MESSAGES: Record<Operator, { one: string; many: string }> = {
	is: { one: "dashboard.filter.pill.is", many: "dashboard.filter.pill.is_any" },
	is_not: { one: "dashboard.filter.pill.is_not", many: "dashboard.filter.pill.is_not_any" },
	contains: { one: "dashboard.filter.pill.contains", many: "dashboard.filter.pill.contains_any" },
	contains_not: {
		one: "dashboard.filter.pill.contains_not",
		many: "dashboard.filter.pill.contains_not_any",
	},
	matches: { one: "dashboard.filter.pill.matches", many: "dashboard.filter.pill.matches_any" },
	matches_not: { one: "dashboard.filter.pill.matches_not", many: "dashboard.filter.pill.matches_not_any" },
};

/**
 * valueOf renders one filter value, or the empty string when the value itself is
 * empty.
 *
 * The explicit label from the URL wins over the platform's own rendering: a
 * recipient whose browser has no name for a country code still sees the country
 * the sender saw, which is the entire reason `l=` exists. A blank comes back
 * blank rather than as "(not set)", because that wording is a translated string
 * and this file does not render any.
 */
export function valueOf(dimension: string, value: string, labels: FilterLabels): string {
	if (labels[value] !== undefined) return labels[value] as string;

	return valueLabel(dimension, value);
}
