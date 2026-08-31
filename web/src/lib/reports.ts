//
// reports.ts
// What each card and each of its tabs actually asks the query engine for.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Filter, Metric } from "../api/types";

/**
 * A tab is one dimension, plus the wording that makes its numbers readable.
 *
 * The card and the drawer are driven from the same definitions rather than from
 * two parallel lists. A drawer that could show a dimension the card cannot — or
 * label it differently — is the shape of every "these two screens disagree" bug
 * in a reporting product.
 */
export interface Tab {
	id: string;
	/** The label on the tab itself. */
	label: string;
	/** The heading over the label column, in both the card and the drawer. */
	heading: string;
	dimension: string;
	/** Rows whose label is the empty string mean this, rather than nothing. */
	emptyLabel?: string;
	/** Applied on every request for this tab. It is part of what the report
	 *  means, not a user filter: "Campaigns" is traffic that carried a campaign
	 *  tag, and the untagged 93% would otherwise be one row swamping the card. */
	filters?: Filter[];
	/** Sources get an icon; channels and pages do not. */
	favicon?: boolean;
	/** The group the tab sits under, for cards whose tabs are two rows deep. */
	group?: string;
	/** The word an empty state uses: "No sources in this period". */
	noun: string;
}

export interface CardDef {
	id: string;
	title: string;
	/** The CSS class carrying this card's bar tint. */
	tint: string;
	tabs: Tab[];
	/** The footnote on a number that reliably looks like a bug and is not. */
	caveat?: string;
}

/** The metric every report row is ranked and sized by. Visitors is the default
 *  everywhere because it is the number people mean when they say "traffic". */
export const PRIMARY: Metric = "visitors";

/** The columns a details drawer shows. Five fit comfortably in the drawer's
 *  width, which is most of the reason the details view is a drawer at all. */
export const DRAWER_METRICS: Metric[] = ["visitors", "visits", "pageviews", "bounce_rate", "visit_duration"];

export const DRAWER_HEADINGS: Record<string, string> = {
	visitors: "Visitors",
	visits: "Visits",
	pageviews: "Views",
	bounce_rate: "Bounce",
	visit_duration: "Avg. visit",
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
	title: "Top Sources",
	tint: "tint-sources",
	caveat:
		"The visitors in these rows can add up to more than your total unique visitors. " +
		"The same person can arrive from a search engine in the morning and type your address " +
		"in the afternoon — one unique visitor, two source rows. Switch the column to Visits " +
		"in the details view for a figure that does add up.",
	tabs: [
		{
			id: "channels",
			label: "Channels",
			heading: "Channel",
			dimension: "visit:channel",
			emptyLabel: "Direct / None",
			noun: "channels",
		},
		{
			id: "sources",
			label: "Sources",
			heading: "Source",
			dimension: "visit:source",
			emptyLabel: "Direct / None",
			favicon: true,
			noun: "sources",
		},
		{
			id: "utm_source",
			label: "Source",
			group: "Campaigns",
			heading: "UTM source",
			dimension: "visit:utm_source",
			filters: tagged("visit:utm_source"),
			noun: "tagged campaigns",
		},
		{
			id: "utm_medium",
			label: "Medium",
			group: "Campaigns",
			heading: "UTM medium",
			dimension: "visit:utm_medium",
			filters: tagged("visit:utm_medium"),
			noun: "tagged campaigns",
		},
		{
			id: "utm_campaign",
			label: "Campaign",
			group: "Campaigns",
			heading: "UTM campaign",
			dimension: "visit:utm_campaign",
			filters: tagged("visit:utm_campaign"),
			noun: "tagged campaigns",
		},
	],
};

export const PAGES: CardDef = {
	id: "pages",
	title: "Top Pages",
	tint: "tint-pages",
	caveat:
		"Unique visitors can come out higher than pageviews on a site that fires custom events. " +
		"A visitor who only triggers events and never loads a tracked page is counted once as a " +
		"visitor and never as a view.",
	tabs: [
		{ id: "pages", label: "Top Pages", heading: "Page", dimension: "event:page", noun: "pages" },
		{ id: "entry", label: "Entry Pages", heading: "Entry page", dimension: "visit:entry_page", noun: "entry pages" },
		{ id: "exit", label: "Exit Pages", heading: "Exit page", dimension: "visit:exit_page", noun: "exit pages" },
	],
};

export const CARDS: CardDef[] = [SOURCES, PAGES];

/**
 * The secondary dimensions a drawer can break its list down by.
 *
 * Breaking Sources down by Country, or Pages by Device, is a question the query
 * engine already answers in one request — it is the same query with a second
 * dimension. The drawer is the only surface in the product with the width to
 * show the result, which is why it lives here and not on the card.
 */
export const BREAKDOWNS: { id: string; label: string }[] = [
	{ id: "", label: "No breakdown" },
	{ id: "visit:country", label: "Country" },
	{ id: "visit:device", label: "Device" },
	{ id: "visit:browser", label: "Browser" },
	{ id: "visit:os", label: "Operating system" },
	{ id: "visit:channel", label: "Channel" },
	{ id: "visit:source", label: "Source" },
];

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
export function groupsOf(card: CardDef): { key: string; label: string; tab: Tab }[] {
	const seen = new Set<string>();
	const groups: { key: string; label: string; tab: Tab }[] = [];

	for (const tab of card.tabs) {
		const key = tab.group ?? tab.id;
		if (seen.has(key)) continue;

		seen.add(key);
		groups.push({ key, label: tab.group ?? tab.label, tab });
	}

	return groups;
}

/** subTabsOf lists the second tab row for a grouped tab, or nothing when the
 *  active tab stands alone. */
export function subTabsOf(card: CardDef, active: Tab): Tab[] {
	if (!active.group) return [];

	return card.tabs.filter((tab) => tab.group === active.group);
}

/** labelOf renders one dimension value. The empty string is a real answer for
 *  several dimensions — it is what direct traffic looks like — so it gets the
 *  tab's own wording rather than a blank row. */
export function labelOf(tab: Tab, value: string): string {
	if (value) return value;

	return tab.emptyLabel ?? "(none)";
}
