//
// App.tsx
// The dashboard: what loads when, and where each piece of state lives.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useCallback, useEffect, useRef, useState } from "react";

import { bootstrap } from "../api/client";
import type { Metric, StatsRequest } from "../api/types";
import { usePref, useTheme } from "../lib/prefs";
import type { CardDef, Tab } from "../lib/reports";
import { CARDS, findCard, findTab } from "../lib/reports";
import type { DrawerState } from "../lib/url";
import { dateRange, useUrlState } from "../lib/url";
import { useStats } from "../lib/useStats";
import { Drawer } from "./Drawer";
import { MainGraph } from "./MainGraph";
import { ReportCard } from "./ReportCard";
import { TopBar } from "./TopBar";
import { TILE_METRICS, TopStats } from "./TopStats";

/** The metrics the graph can draw. The three session ratios on the tile row are
 *  read as a single figure over the period and have no honest per-bucket value,
 *  so they are not in this list. */
const GRAPH_METRICS: Metric[] = ["visitors", "visits", "pageviews"];

/**
 * App is the whole dashboard.
 *
 * The initial paint is four requests, not eight: the totals, the graph, the
 * live-visitor pill, and whichever report cards are near the viewport. An
 * unfiltered 28-day query is seconds of work today, so the cheapest way to make
 * this page survivable is to not ask for what nobody is looking at yet.
 */
export function App() {
	const [state, navigate] = useUrlState();
	const [theme, setTheme] = useTheme();
	const [sites] = useState(() => bootstrap().sites.slice().sort());

	// Which metric the graph draws is a personal preference and belongs in
	// localStorage — putting it in the URL would mean every shared link
	// silently changed the recipient's graph.
	const [metric, setMetric] = usePref<Metric>("metric", "visitors", GRAPH_METRICS);

	// The element the drawer was opened from, so focus can be handed back to
	// it. A ref rather than state: changing it must not re-render the dashboard
	// sitting behind an open drawer.
	const opener = useRef<HTMLElement | null>(null);

	// Whether this session is the one that pushed the drawer's history entry.
	// Somebody who arrived on a shared drawer link has no entry to go back to,
	// and calling history.back() for them would take them off the dashboard.
	const pushed = useRef(false);

	// A bare /dashboard has no site in it. Replacing rather than pushing means
	// Back leaves the app instead of bouncing between the bare URL and the
	// resolved one forever.
	useEffect(() => {
		if (state.domain || sites.length === 0) return;

		navigate({ ...state, domain: sites[0] as string }, "replace");
	}, [state, sites, navigate]);

	const range = dateRange(state);
	const comparing = state.compare !== "off";

	const totalsBody: StatsRequest = {
		metrics: TILE_METRICS,
		date_range: range,
		include: comparing ? { comparisons: { mode: state.compare === "off" ? "previous_period" : state.compare } } : undefined,
	};

	const totals = useStats(state.domain, state.domain ? totalsBody : null);

	const graph = useStats(
		state.domain,
		state.domain ? { metrics: [metric], date_range: range, dimensions: ["time"] } : null,
	);

	const uniqueVisitors = totals.data?.results[0]?.metrics[0] ?? 0;

	/** openDetails pushes the drawer into the URL. Pushing rather than replacing
	 *  is what makes Back close it, which is the gesture everybody tries first,
	 *  and what makes the open drawer a link worth sending. */
	const openDetails = useCallback(
		(card: CardDef, tab: Tab, from: HTMLElement | null) => {
			opener.current = from;
			pushed.current = true;

			navigate({
				...state,
				drawer: { card: card.id, tab: tab.id, page: 1, search: "", sort: "visitors", descending: true, breakdown: "" },
			});
		},
		[state, navigate],
	);

	/** updateDrawer replaces rather than pushes. Paging and sorting are
	 *  adjustments to one view, and pushing each one would mean closing the
	 *  drawer took as many Backs as the reader made changes. */
	const updateDrawer = useCallback(
		(next: DrawerState) => navigate({ ...state, drawer: next }, "replace"),
		[state, navigate],
	);

	const closeDetails = useCallback(() => {
		if (pushed.current) {
			pushed.current = false;
			history.back();
			return;
		}

		navigate({ ...state, drawer: null }, "replace");
	}, [state, navigate]);

	// Backing out of the drawer by keyboard or gesture leaves no entry for the
	// close button to consume.
	useEffect(() => {
		if (!state.drawer) pushed.current = false;
	}, [state.drawer]);

	if (sites.length === 0) return <NoSites />;

	const drawerCard = state.drawer ? findCard(state.drawer.card) : undefined;
	const drawerTabId = drawerCard && state.drawer ? findTab(drawerCard, state.drawer.tab).id : undefined;

	return (
		<>
			<TopBar
				state={state}
				sites={sites}
				onNavigate={(next) => navigate(next)}
				theme={theme}
				onTheme={setTheme}
				resolved={totals.data?.query.date_range}
			/>

			<main className="mx-auto max-w-shell px-4 py-5 sm:px-5">
				<section className="overflow-hidden rounded-md border border-line bg-card shadow-sm">
					<TopStats stats={totals} selected={metric} onSelect={setMetric} comparing={comparing} />
					<div className="p-4 sm:p-5">
						<MainGraph stats={graph} metric={metric} />
					</div>
				</section>

				<div className="mt-5 grid grid-cols-1 gap-5 lg:grid-cols-2">
					{CARDS.map((card) => (
						<ReportCard
							key={card.id}
							domain={state.domain}
							card={card}
							range={range}
							total={uniqueVisitors}
							onOpenDetails={openDetails}
							drawerTab={drawerCard?.id === card.id ? drawerTabId : undefined}
						/>
					))}
				</div>
			</main>

			{drawerCard && state.drawer && (
				<Drawer
					domain={state.domain}
					card={drawerCard}
					state={state.drawer}
					range={range}
					onChange={updateDrawer}
					onClose={closeDetails}
					opener={opener.current}
				/>
			)}
		</>
	);
}

/** NoSites is the state a brand-new account lands in. It says what to do next
 *  rather than showing an empty dashboard, because an empty dashboard looks
 *  broken and a broken dashboard gets a support ticket. */
function NoSites() {
	return (
		<div className="flex min-h-screen items-center justify-center px-6">
			<div className="max-w-md text-center">
				<h1 className="text-lg font-semibold text-body">No sites yet</h1>
				<p className="mt-2 text-sm text-muted">
					Add a site and install the tracking snippet, and its traffic will appear here within a few seconds.
				</p>
			</div>
		</div>
	);
}
