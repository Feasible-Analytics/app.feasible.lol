//
// specialgoals.ts
// The four goals the tracker fires on its own, and what each one breaks down by.
//
// Created: 2026-09-11
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Filter, Goal } from "../api/types";

/**
 * SpecialGoal is an automatic goal whose detail is already on the event.
 *
 * The tracker detects these four without anybody writing code, and each one
 * arrives carrying the single value a reader wants next: which link, which
 * file, which page. That makes the goal and its breakdown one report rather
 * than two, so filtering by the goal replaces the goals table with the
 * breakdown instead of leaving a table holding one row.
 */
export interface SpecialGoal {
	/** The event name the tracker sends. Byte-identical to the automatic goal's
	 *  stored definition, which is what ties a filtered goal id to this entry. */
	event: string;
	/** The tab's label while this goal is the filter. */
	labelId: string;
	/** The dimension the breakdown groups by. */
	dimension: string;
	/** The heading over that dimension's column. */
	headingId: string;
	/** The caveat bubble's sentence for this breakdown. */
	caveatId: string;
}

/**
 * SPECIAL_GOALS is keyed by the wire event name.
 *
 * `url` is the property the tracker attaches to a click, and it is read here
 * without consulting the site's property allow-list: the customer never sent it
 * deliberately and has nothing to enable, so requiring an allow-list entry
 * would leave this tab permanently empty on every site.
 *
 * Form submissions and 404s carry no property of their own, so both break down
 * by the page the event fired on — which is the page holding the form, and the
 * address that was not found.
 */
export const SPECIAL_GOALS: SpecialGoal[] = [
	{
		event: "Outbound Link: Click",
		labelId: "dashboard.behavior.special.outbound_links",
		dimension: "event:props:url",
		headingId: "dashboard.column.url",
		caveatId: "dashboard.behavior.special.outbound_links_caveat",
	},
	{
		event: "File Download",
		labelId: "dashboard.behavior.special.file_downloads",
		dimension: "event:props:url",
		headingId: "dashboard.column.url",
		caveatId: "dashboard.behavior.special.file_downloads_caveat",
	},
	{
		event: "Form: Submission",
		labelId: "dashboard.behavior.special.form_actions",
		dimension: "event:page",
		headingId: "dashboard.column.path",
		caveatId: "dashboard.behavior.special.form_actions_caveat",
	},
	{
		event: "404",
		labelId: "dashboard.behavior.special.not_found",
		dimension: "event:page",
		headingId: "dashboard.column.path",
		caveatId: "dashboard.behavior.special.not_found_caveat",
	},
];

/**
 * specialGoal matches a goal definition to its breakdown, or returns undefined.
 *
 * The match is on the stored event name and on the goal having been created by
 * us. Somebody who writes their own goal named "File Download" gets the
 * ordinary goals table, because their event carries whatever properties they
 * chose to send rather than the one the tracker attaches.
 */
export function specialGoal(goal: Goal | undefined): SpecialGoal | undefined {
	if (!goal || !goal.is_automatic || goal.kind !== "event") return undefined;

	return SPECIAL_GOALS.find((candidate) => candidate.event === goal.event_name);
}

/**
 * filteredGoalID reads the one goal id a dashboard is filtered to, or "".
 *
 * Exactly one `is` filter naming exactly one goal counts. Two goals at once is
 * a question about neither of them in particular, and there is no single
 * breakdown that answers it.
 */
export function filteredGoalID(filters: Filter[]): string {
	const chosen = filters.filter((filter) => filter[0] === "is" && filter[1] === "event:goal");
	if (chosen.length !== 1) return "";

	const values = chosen[0]?.[2] ?? [];
	if (values.length !== 1) return "";

	return values[0] ?? "";
}
