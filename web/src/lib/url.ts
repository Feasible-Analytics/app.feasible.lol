//
// url.ts
// The URL: the shareable half of the dashboard's state.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useCallback, useEffect, useState } from "react";

import { shared } from "../api/client";
import type { DateRange, JourneyAnchor, JourneyAnchorType, Preset } from "../api/types";
import type { CompareMode } from "./compare";
import type { FilterLabels, FilterState } from "./filters";
import { readFilters, readLabels, writeFilters } from "./filters";

/** DEFAULT_BASE is where the authenticated dashboard is mounted. */
export const DEFAULT_BASE = "/dashboard";

/**
 * base is the path prefix every URL this app builds must keep.
 *
 * On the authenticated dashboard it is /dashboard. Behind a shared link it is
 * /share/<token>, and behind a public dashboard it is /public/<domain>, both
 * handed to us by the server.
 *
 * This function is the whole fix for a real, reproducible bug. The incumbent's
 * shared dashboard built its URLs against its own dashboard path, so the moment
 * a reader applied a filter the /share/<token> segment was dropped — the new
 * URL pointed at a dashboard the reader had no account for, which redirected to
 * a login, which redirected back. Copying the URL after filtering produced a
 * link that was simply broken.
 *
 * Nothing in this file may hard-code the prefix. Every href goes through here.
 */
export function base(): string {
	return shared()?.base ?? DEFAULT_BASE;
}

/** BASE is the authenticated prefix, kept for the parse fallback below. Prefer
 *  base() everywhere a URL is constructed. */
export const BASE = DEFAULT_BASE;

const PRESETS: Preset[] = [
	"realtime",
	"5m",
	"day",
	"24h",
	"7d",
	"28d",
	"91d",
	"month",
	"last_month",
	"year",
	"12mo",
	"all",
];

/** DEFAULT_PRESET matches the engine's own default, so a bare /dashboard/site
 *  and an explicit ?period=28d are the same page rather than two. */
export const DEFAULT_PRESET: Preset = "28d";

/**
 * DrawerState is which details view is open.
 *
 * It lives in the URL rather than in component state because a details view is
 * the thing people actually send each other — "look at the referrer breakdown
 * for last week" — and a drawer that vanishes on Back is a drawer people learn
 * not to open.
 */
export interface DrawerState {
	card: string;
	tab: string;
	page: number;
	search: string;
	sort: string;
	descending: boolean;
	/** The secondary dimension the list is broken down by, empty for none. */
	breakdown: string;
}

export type BehaviorTab = "goals" | "properties" | "funnels" | "explore";

/** BehaviorState is the shareable selection inside the dashboard's lower
 * analysis workspace. Keeping it beside filters and the period means Back,
 * Forward, refresh, and a pasted link all restore the same report. */
export interface BehaviorState {
	tab: BehaviorTab;
	property: string;
	funnel: number;
	exploreAnchor: JourneyAnchor | null;
	exploreDirection: "forward" | "backward";
	exploreGrouping: "exact" | "prefix";
	exploreTrail: JourneyAnchor[];
}

export interface UrlState {
	domain: string;
	preset: Preset;
	/** Set only when preset is a custom range; the two bounds as YYYY-MM-DD. */
	from: string;
	to: string;
	compare: CompareMode;
	/** Every filter in force, ANDed together. In the URL rather than in state
	 *  because a filtered dashboard is the thing people send each other, and a
	 *  filter that vanishes on paste makes the link a lie. */
	filters: FilterState[];
	/** Display labels for values whose stored form says nothing on its own, so a
	 *  recipient sees the same pill the sender did. */
	labels: FilterLabels;
	drawer: DrawerState | null;
	behavior: BehaviorState;
}

/** dateRange turns the URL's period into the wire form the engine reads. */
export function dateRange(state: UrlState): DateRange {
	if (state.from && state.to) return [state.from, state.to];

	return state.preset;
}

/** parse reads the whole dashboard state out of one URL. Everything unknown
 *  falls back to a default rather than erroring: a hand-edited or truncated link
 *  should still open a working dashboard. */
export function parse(url: URL): UrlState {
	const prefix = base();
	const path = url.pathname.startsWith(prefix) ? url.pathname.slice(prefix.length) : url.pathname;

	// Behind a share link the prefix already names the site, so the path after
	// it is empty and the domain comes from the bootstrap instead.
	const fromPath = decodeURIComponent(path.replace(/^\/+|\/+$/g, "").split("/")[0] ?? "");
	const domain = fromPath || (shared()?.domain ?? "");

	const params = url.searchParams;
	const raw = params.get("period") ?? "";
	const from = params.get("from") ?? "";
	const to = params.get("to") ?? "";

	const preset: Preset = PRESETS.includes(raw as Preset) ? (raw as Preset) : DEFAULT_PRESET;
	const compare = params.get("compare");

	return {
		domain,
		preset,
		from: isDate(from) && isDate(to) ? from : "",
		to: isDate(from) && isDate(to) ? to : "",
		compare: compare === "year_over_year" || compare === "off" ? compare : "previous_period",
		filters: readFilters(params),
		labels: readLabels(params),
		drawer: parseDrawer(params),
		behavior: parseBehavior(params),
	};
}

/** parseBehavior accepts only the small typed vocabulary the Explore endpoint
 * supports; a damaged trail loses the trail rather than the whole dashboard. */
function parseBehavior(params: URLSearchParams): BehaviorState {
	const rawTab = params.get("analysis");
	const tab: BehaviorTab = rawTab === "properties" || rawTab === "funnels" || rawTab === "explore" ? rawTab : "goals";
	const rawType = params.get("explore_type");
	const type = isJourneyType(rawType) ? rawType : null;
	const value = params.get("explore_value") ?? "";
	const label = params.get("explore_label") ?? "";
	const rawFunnel = Number(params.get("funnel") ?? "0");

	return {
		tab,
		property: params.get("property") ?? "",
		funnel: Number.isSafeInteger(rawFunnel) && rawFunnel > 0 ? rawFunnel : 0,
		exploreAnchor: type && value ? { type, value, label: label || undefined } : null,
		exploreDirection: params.get("explore_direction") === "backward" ? "backward" : "forward",
		exploreGrouping: params.get("explore_grouping") === "prefix" ? "prefix" : "exact",
		exploreTrail: parseJourneyTrail(params.get("explore_trail")),
	};
}

/** parseJourneyTrail validates every node before allowing it into continuation
 * requests, and caps hand-edited URLs so one link cannot grow without bound. */
function parseJourneyTrail(raw: string | null): JourneyAnchor[] {
	if (!raw) return [];

	try {
		const parsed = JSON.parse(raw) as unknown;
		if (!Array.isArray(parsed)) return [];

		return parsed.slice(0, 20).flatMap((item): JourneyAnchor[] => {
			if (!item || typeof item !== "object") return [];
			const node = item as Partial<JourneyAnchor>;
			if (!isJourneyType(node.type) || typeof node.value !== "string" || !node.value) return [];

			return [{ type: node.type, value: node.value, label: typeof node.label === "string" ? node.label : undefined }];
		});
	} catch {
		return [];
	}
}

/** isJourneyType narrows untrusted URL text to the API's anchor vocabulary. */
function isJourneyType(value: unknown): value is JourneyAnchorType {
	return value === "page" || value === "event" || value === "goal";
}

/** isDate accepts only the bare-date form the engine documents, so a bad bound
 *  is dropped here rather than returned as a 400 the user cannot act on. */
function isDate(value: string): boolean {
	return /^\d{4}-\d{2}-\d{2}$/.test(value);
}

/** parseDrawer reads the open details view. The card and tab travel as one
 *  `card:tab` pair so a shared link carries both halves or neither — a card
 *  without its tab would open the drawer on whatever tab the recipient happened
 *  to have last used, which is not the view that was shared. */
function parseDrawer(params: URLSearchParams): DrawerState | null {
	const details = params.get("details");
	if (!details) return null;

	const [card = "", tab = ""] = details.split(":");
	if (!card || !tab) return null;

	const sort = params.get("dsort") ?? "visitors:desc";
	const [key = "visitors", direction = "desc"] = sort.split(":");

	return {
		card,
		tab,
		page: Math.max(1, Number(params.get("dpage") ?? "1") || 1),
		search: params.get("dq") ?? "",
		sort: key,
		descending: direction !== "asc",
		breakdown: params.get("dbreak") ?? "",
	};
}

/** href renders a state back into a URL. Every default is omitted, which keeps
 *  the common link short enough to read and means "the same view" always
 *  produces the same string. */
export function href(state: UrlState): string {
	const params = new URLSearchParams();

	if (state.from && state.to) {
		params.set("from", state.from);
		params.set("to", state.to);
	} else if (state.preset !== DEFAULT_PRESET) {
		params.set("period", state.preset);
	}

	if (state.compare !== "previous_period") params.set("compare", state.compare);

	writeFilters(params, state.filters, state.labels);

	const behavior = state.behavior;
	if (behavior.tab !== "goals") params.set("analysis", behavior.tab);
	if (behavior.property) params.set("property", behavior.property);
	if (behavior.funnel > 0) params.set("funnel", String(behavior.funnel));
	if (behavior.exploreAnchor) {
		params.set("explore_type", behavior.exploreAnchor.type);
		params.set("explore_value", behavior.exploreAnchor.value);
		if (behavior.exploreAnchor.label && behavior.exploreAnchor.label !== behavior.exploreAnchor.value) {
			params.set("explore_label", behavior.exploreAnchor.label);
		}
	}
	if (behavior.exploreDirection === "backward") params.set("explore_direction", "backward");
	if (behavior.exploreGrouping === "prefix") params.set("explore_grouping", "prefix");
	if (behavior.exploreTrail.length > 0) params.set("explore_trail", JSON.stringify(behavior.exploreTrail));

	const drawer = state.drawer;
	if (drawer) {
		params.set("details", `${drawer.card}:${drawer.tab}`);
		if (drawer.page > 1) params.set("dpage", String(drawer.page));
		if (drawer.search) params.set("dq", drawer.search);
		if (drawer.sort !== "visitors" || !drawer.descending) {
			params.set("dsort", `${drawer.sort}:${drawer.descending ? "desc" : "asc"}`);
		}
		if (drawer.breakdown) params.set("dbreak", drawer.breakdown);
	}

	const search = params.toString();

	// Behind a share link the prefix already identifies the site, so appending
	// the domain would produce /share/<token>/example.com — a path the server
	// serves but nobody can read, and one that would break the moment the link
	// were revoked and reissued.
	const prefix = base();
	const path = shared() ? prefix : `${prefix}/${encodeURIComponent(state.domain)}`;

	return `${path}${search ? `?${search}` : ""}`;
}

/**
 * useUrlState is the router.
 *
 * It is thirty lines rather than a routing library because the dashboard has
 * exactly one route with a query string on it. A library would add a dependency
 * and a mental model to hold something this hook holds in one useState and one
 * popstate listener.
 */
export function useUrlState(): [UrlState, (next: UrlState, mode?: "push" | "replace") => void] {
	const [state, setState] = useState<UrlState>(() => parse(new URL(location.href)));

	useEffect(() => {
		// Back and Forward have to move the dashboard, not just the address
		// bar. This is the half of "shareable and Back-able" that is easy to
		// forget, and its absence is only noticed by somebody who has already
		// lost their place.
		const onPop = () => setState(parse(new URL(location.href)));

		addEventListener("popstate", onPop);

		return () => removeEventListener("popstate", onPop);
	}, []);

	const navigate = useCallback((next: UrlState, mode: "push" | "replace" = "push") => {
		const url = href(next);

		// Replacing rather than pushing when nothing moved keeps the history
		// clean: without it, changing a sort three times means three Backs
		// before the drawer closes.
		if (url === location.pathname + location.search) {
			setState(next);
			return;
		}

		if (mode === "push") history.pushState(null, "", url);
		else history.replaceState(null, "", url);

		setState(next);
	}, []);

	return [state, navigate];
}
