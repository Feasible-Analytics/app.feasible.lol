//
// reports.ts
// What each card and each of its tabs actually asks the query engine for.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Filter, Metric } from "../api/types";
import { t } from "./i18n";

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

export const CARDS: CardDef[] = [SOURCES, PAGES];

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
	{ id: "visit:device", labelId: "dashboard.dimension.device" },
	{ id: "visit:browser", labelId: "dashboard.dimension.browser" },
	{ id: "visit:os", labelId: "dashboard.dimension.os" },
	{ id: "visit:channel", labelId: "dashboard.dimension.channel" },
	{ id: "visit:source", labelId: "dashboard.dimension.source" },
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
	if (value) return value;

	return t(tab.emptyLabelId ?? "dashboard.value.none");
}
