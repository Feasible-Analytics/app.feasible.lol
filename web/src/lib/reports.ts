//
// reports.ts
// What each card and each of its tabs actually asks the query engine for.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Filter, Metric } from "../api/types";
import { t } from "./i18n";
import { valueLabel } from "./labels";

/**
 * A tab is one dimension, plus the wording that makes its numbers readable.
 *
 * The card and the drawer are driven from the same definitions rather than from
 * two parallel lists. A drawer that could show a dimension the card cannot — or
 * label it differently — is the shape of every "these two screens disagree" bug
 * in a reporting product.
 *
 * Every human-readable field here is a message id rather than a string, and it
 * is translated where it is rendered. Resolving it in this table instead would
 * bake the language in at module scope, where a change of locale could only be
 * picked up by reloading the page.
 */
export interface Tab {
	id: string;
	/** The label on the tab itself. */
	labelId: string;
	/** The heading over the label column, in both the card and the drawer. */
	headingId: string;
	dimension: string;
	/** A response enrichment shown alongside the primary dimension. It is not a
	 *  grouping key, so changing or missing titles cannot split the path row. */
	companion?: { enrichment: "page_title"; headingId: string };
	/** Rows whose label is the empty string mean this, rather than nothing. */
	emptyLabelId?: string;
	/** Applied on every request for this tab. It is part of what the report
	 *  means, not a user filter: "Campaigns" is traffic that carried a campaign
	 *  tag, and the untagged 93% would otherwise be one row swamping the card. */
	filters?: Filter[];
	/** Sources get an icon; channels and pages do not. */
	favicon?: boolean;
	/** The group the tab sits under, for cards whose tabs are two rows deep. */
	groupId?: string;
	/** The word an empty state uses: "No sources in this period". */
	nounId: string;
	/** A footnote about this tab's numbers specifically. It sits beside the card's
	 *  own caveat rather than replacing it, because "cities are approximate" is
	 *  true of one tab and false of the two next to it. */
	caveatId?: string;
	/** Draw this tab as the choropleth rather than as a list. It is the same
	 *  query and the same dimension as the tab beside it — a second view of one
	 *  report, not a second report — which is why it is a flag here rather than
	 *  a card of its own. */
	map?: boolean;
}

export interface CardDef {
	id: string;
	titleId: string;
	/** The CSS class carrying this card's bar tint. */
	tint: string;
	tabs: Tab[];
	/** The footnote on a number that reliably looks like a bug and is not. */
	caveatId?: string;
}

/** The metric every report row is ranked and sized by. Visitors is the default
 *  everywhere because it is the number people mean when they say "traffic". */
export const PRIMARY: Metric = "visitors";

/** The columns a details drawer shows. Five fit comfortably in the drawer's
 *  width, which is most of the reason the details view is a drawer at all. */
export const DRAWER_METRICS: Metric[] = ["visitors", "visits", "pageviews", "bounce_rate", "visit_duration"];

/** The heading over each drawer column, as message ids. The keys are the wire
 *  metric names, which are never translated — a translated metric name is a
 *  query the engine refuses. */
export const DRAWER_HEADINGS: Record<string, string> = {
	visitors: "dashboard.column.visitors",
	visits: "dashboard.column.visits",
	pageviews: "dashboard.column.pageviews",
	bounce_rate: "dashboard.column.bounce_rate",
	visit_duration: "dashboard.column.visit_duration",
};

/** Metrics where a rise is bad news. Colouring a change chip by sign alone gets
 *  these three exactly backwards. */
export const INVERTED: ReadonlySet<string> = new Set(["bounce_rate", "exit_rate"]);

/** notCampaignTagged excludes the untagged bucket from a UTM report. */
function tagged(dimension: string): Filter[] {
	return [["is_not", dimension, [""], { case_sensitive: true }]];
}

export const SOURCES: CardDef = {
	id: "sources",
	titleId: "dashboard.report.sources.title",
	tint: "tint-sources",
	caveatId: "dashboard.report.sources.caveat",
	tabs: [
		{
			id: "channels",
			labelId: "dashboard.tab.channels",
			headingId: "dashboard.dimension.channel",
			dimension: "visit:channel",
			emptyLabelId: "dashboard.value.direct",
			nounId: "dashboard.noun.channels",
		},
		{
			id: "sources",
			labelId: "dashboard.tab.sources",
			headingId: "dashboard.dimension.source",
			dimension: "visit:source",
			emptyLabelId: "dashboard.value.direct",
			favicon: true,
			nounId: "dashboard.noun.sources",
		},
		{
			id: "utm_source",
			labelId: "dashboard.dimension.source",
			groupId: "dashboard.group.campaigns",
			headingId: "dashboard.dimension.utm_source",
			dimension: "visit:utm_source",
			filters: tagged("visit:utm_source"),
			nounId: "dashboard.noun.tagged_campaigns",
		},
		{
			id: "utm_medium",
			labelId: "dashboard.tab.medium",
			groupId: "dashboard.group.campaigns",
			headingId: "dashboard.dimension.utm_medium",
			dimension: "visit:utm_medium",
			filters: tagged("visit:utm_medium"),
			nounId: "dashboard.noun.tagged_campaigns",
		},
		{
			id: "utm_campaign",
			labelId: "dashboard.tab.campaign",
			groupId: "dashboard.group.campaigns",
			headingId: "dashboard.dimension.utm_campaign",
			dimension: "visit:utm_campaign",
			filters: tagged("visit:utm_campaign"),
			nounId: "dashboard.noun.tagged_campaigns",
		},
	],
};

export const PAGES: CardDef = {
	id: "pages",
	titleId: "dashboard.report.pages.title",
	tint: "tint-pages",
	caveatId: "dashboard.report.pages.caveat",
	tabs: [
		{
			id: "pages",
			labelId: "dashboard.report.pages.title",
			headingId: "dashboard.dimension.page",
			dimension: "event:page",
			companion: { enrichment: "page_title", headingId: "dashboard.dimension.page_title" },
			nounId: "dashboard.noun.pages",
		},
		{
			id: "entry",
			labelId: "dashboard.tab.entry_pages",
			headingId: "dashboard.dimension.entry_page",
			dimension: "visit:entry_page",
			nounId: "dashboard.noun.entry_pages",
		},
		{
			id: "exit",
			labelId: "dashboard.tab.exit_pages",
			headingId: "dashboard.dimension.exit_page",
			dimension: "visit:exit_page",
			nounId: "dashboard.noun.exit_pages",
		},
	],
};

/** The wording every dimension derived from a browser's own self-description
 *  uses for a blank. It is one id because a malformed or synthetic user agent
 *  blanks browser, operating system, device class and screen size all at once,
 *  and four different spellings of that would look like four bugs. */
const NOT_SET = "dashboard.value.not_set";

export const LOCATIONS: CardDef = {
	id: "locations",
	titleId: "dashboard.report.locations.title",
	tint: "tint-locations",
	caveatId: "dashboard.report.locations.caveat",
	tabs: [
		{
			id: "map",
			labelId: "dashboard.tab.map",
			headingId: "dashboard.dimension.country",
			dimension: "visit:country",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.countries",
			map: true,
		},
		{
			id: "countries",
			labelId: "dashboard.tab.countries",
			headingId: "dashboard.dimension.country",
			dimension: "visit:country",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.countries",
		},
		{
			id: "regions",
			labelId: "dashboard.tab.regions",
			headingId: "dashboard.dimension.region",
			dimension: "visit:region",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.regions",
		},
		{
			id: "cities",
			labelId: "dashboard.tab.cities",
			headingId: "dashboard.dimension.city",
			dimension: "visit:city",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.cities",
			caveatId: "dashboard.report.locations.cities_caveat",
		},
	],
};

export const DEVICES: CardDef = {
	id: "devices",
	titleId: "dashboard.report.devices.title",
	tint: "tint-devices",
	caveatId: "dashboard.report.devices.caveat",
	tabs: [
		{
			id: "browser",
			labelId: "dashboard.dimension.browser",
			groupId: "dashboard.group.browsers",
			headingId: "dashboard.dimension.browser",
			dimension: "visit:browser",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.browsers",
		},
		{
			id: "browser_version",
			labelId: "dashboard.tab.version",
			groupId: "dashboard.group.browsers",
			headingId: "dashboard.dimension.browser_version",
			dimension: "visit:browser_version",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.browser_versions",
		},
		{
			id: "os",
			labelId: "dashboard.tab.system",
			groupId: "dashboard.group.systems",
			headingId: "dashboard.dimension.os",
			dimension: "visit:os",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.operating_systems",
		},
		{
			id: "os_version",
			labelId: "dashboard.tab.version",
			groupId: "dashboard.group.systems",
			headingId: "dashboard.dimension.os_version",
			dimension: "visit:os_version",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.system_versions",
		},
		{
			id: "device",
			labelId: "dashboard.tab.type",
			groupId: "dashboard.group.devices",
			headingId: "dashboard.dimension.device",
			dimension: "visit:device",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.device_types",
		},
		{
			id: "screen",
			labelId: "dashboard.tab.screen",
			groupId: "dashboard.group.devices",
			headingId: "dashboard.dimension.screen",
			dimension: "visit:screen",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.screen_sizes",
		},
	],
};

export const LANGUAGES: CardDef = {
	id: "languages",
	titleId: "dashboard.report.languages.title",
	tint: "tint-languages",
	caveatId: "dashboard.report.languages.caveat",
	tabs: [
		{
			id: "languages",
			labelId: "dashboard.report.languages.title",
			headingId: "dashboard.dimension.language",
			dimension: "visit:language",
			emptyLabelId: NOT_SET,
			nounId: "dashboard.noun.languages",
		},
	],
};

export const CARDS: CardDef[] = [SOURCES, PAGES, LOCATIONS, DEVICES, LANGUAGES];

/** dimensionsOf returns the ordered grouping dimensions for a card or drawer
 * request. Response enrichments never appear here. */
export function dimensionsOf(tab: Tab, breakdown = ""): string[] {
	const dimensions = [tab.dimension];
	if (breakdown) dimensions.push(breakdown);

	return dimensions;
}

/** breakdownValueIndex returns where a drawer row stores its optional
 * breakdown after the primary dimension. */
export function breakdownValueIndex(_tab: Tab): number {
	return 1;
}

/**
 * The secondary dimensions a drawer can break its list down by.
 *
 * Breaking Sources down by Country, or Pages by Device, is a question the query
 * engine already answers in one request — it is the same query with a second
 * dimension. The drawer is the only surface in the product with the width to
 * show the result, which is why it lives here and not on the card.
 */
export const BREAKDOWNS: { id: string; labelId: string }[] = [
	{ id: "", labelId: "dashboard.breakdown.none" },
	{ id: "visit:country", labelId: "dashboard.dimension.country" },
	{ id: "visit:city", labelId: "dashboard.dimension.city" },
	{ id: "visit:device", labelId: "dashboard.dimension.device" },
	{ id: "visit:screen", labelId: "dashboard.dimension.screen" },
	{ id: "visit:browser", labelId: "dashboard.dimension.browser" },
	{ id: "visit:os", labelId: "dashboard.dimension.os" },
	{ id: "visit:language", labelId: "dashboard.dimension.language" },
	{ id: "visit:channel", labelId: "dashboard.dimension.channel" },
	{ id: "visit:source", labelId: "dashboard.dimension.source" },
];

/**
 * tableTabs is a card with its map tab removed.
 *
 * The details drawer is a table, and a map has no table form. Leaving the tab
 * in there would open a second copy of the Countries table under a name that
 * promises a picture, which is the kind of small lie that makes people stop
 * trusting the tab strip.
 */
export function tableTabs(card: CardDef): CardDef {
	if (!card.tabs.some((tab) => tab.map)) return card;

	return { ...card, tabs: card.tabs.filter((tab) => !tab.map) };
}

/** findCard resolves a card id from the URL, tolerating a stale link. */
export function findCard(id: string): CardDef | undefined {
	return CARDS.find((card) => card.id === id);
}

/** findTab resolves a tab id within a card, falling back to the first tab so
 *  that a link written against an older build still opens something. */
export function findTab(card: CardDef, id: string): Tab {
	return card.tabs.find((tab) => tab.id === id) ?? (card.tabs[0] as Tab);
}

/** groupsOf lists a card's top tab row: the distinct groups, with ungrouped
 *  tabs standing for themselves. */
export function groupsOf(card: CardDef): { key: string; labelId: string; tab: Tab }[] {
	const seen = new Set<string>();
	const groups: { key: string; labelId: string; tab: Tab }[] = [];

	for (const tab of card.tabs) {
		const key = tab.groupId ?? tab.id;
		if (seen.has(key)) continue;

		seen.add(key);
		groups.push({ key, labelId: tab.groupId ?? tab.labelId, tab });
	}

	return groups;
}

/** subTabsOf lists the second tab row for a grouped tab, or nothing when the
 *  active tab stands alone. */
export function subTabsOf(card: CardDef, active: Tab): Tab[] {
	if (!active.groupId) return [];

	return card.tabs.filter((tab) => tab.groupId === active.groupId);
}

/** labelOf renders one dimension value. The empty string is a real answer for
 *  several dimensions — it is what direct traffic looks like — so it gets the
 *  tab's own wording rather than a blank row. The value itself is the visitor's
 *  own data and is never translated. */
export function labelOf(tab: Tab, value: string): string {
	if (value) return valueLabel(tab.dimension, value);

	return t(tab.emptyLabelId ?? "dashboard.value.none");
}
