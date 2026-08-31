//
// App.tsx
// The dashboard: what loads when, and where each piece of state lives.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { bootstrap } from "../api/client";
import type { Metric, StatsRequest } from "../api/types";
import type { FilterLabels, FilterState } from "../lib/filters";
import { toApi, toggle } from "../lib/filters";
import { t } from "../lib/i18n";
import { step, today } from "../lib/period";
import { usePref, useTheme } from "../lib/prefs";
import type { CardDef, Tab } from "../lib/reports";
import { CARDS, findCard, findTab, tableTabs } from "../lib/reports";
import type { DrawerState } from "../lib/url";
import { dateRange, useUrlState } from "../lib/url";
import { useStats } from "../lib/useStats";
import { Drawer } from "./Drawer";
import { FilterBar } from "./FilterBar";
import { MainGraph } from "./MainGraph";
import { Realtime } from "./Realtime";
import { ReportCard } from "./ReportCard";
import type { ShortcutActions } from "./Shortcuts";
import { ShortcutsModal, useShortcuts } from "./Shortcuts";
import { TopBar } from "./TopBar";
import { SampledBadge } from "./SampledBadge";
import { TILE_METRICS, TopStats } from "./TopStats";

/** The metrics the graph can draw. The three session ratios on the tile row are
 *  read as a single figure over the period and have no honest per-bucket value,
 *  so they are not in this list. */
const GRAPH_METRICS: Metric[] = ["visitors", "visits", "pageviews"];

/**
 * The bucket widths the `i` key cycles through.
 *
 * "auto" is the engine's own choice from the range, which is right nearly all
 * the time. The rest exist for the times it is not: a 91-day range drawn daily
 * is noise, and the same range drawn weekly is a trend. Minute is absent because
 * it only means anything on the live view, which has its own screen.
 */
const INTERVALS = ["auto", "hour", "day", "week", "month"] as const;

type IntervalPref = (typeof INTERVALS)[number];

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
	const [help, setHelp] = useState(false);

	// Which metric the graph draws, and how wide its buckets are, are personal
	// preferences and belong in localStorage — putting them in the URL would
	// mean every shared link silently changed the recipient's graph.
	const [metric, setMetric] = usePref<Metric>("metric", "visitors", GRAPH_METRICS);
	const [interval, setIntervalPref] = usePref<IntervalPref>("interval", "auto", INTERVALS);

	// Bumped to ask the top bar to open its custom-range form, which is what the
	// `c` shortcut means. A nonce rather than a boolean: pressing `c` twice has
	// to open it twice, and a boolean that is already true is a no-op.
	const [pickCustom, setPickCustom] = useState(0);

	// Whether the reader has refused sampling for this session. It is not in
	// the URL and not a stored preference: it is a deliberate "wait for the
	// slow answer" that should apply to the reading somebody is doing now and
	// not silently to every dashboard they open afterwards.
	const [exact, setExact] = useState(false);

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

	// The live view is the range that is still happening, and nothing about it
	// composes with a comparison: there is no previous thirty minutes worth
	// putting under it, and the engine would happily resolve one.
	const live = state.preset === "realtime" && !state.from;
	const comparing = state.compare !== "off" && !live;

	// One conversion from the readable filter model to the wire form, done here
	// so that every query on the page is filtered by the same list. A card that
	// built its own would eventually be filtered by a different one, and two
	// populations on one screen is the bug this whole file exists to avoid.
	const filters = useMemo(() => toApi(state.filters), [state.filters]);

	// The values already filtered, per dimension, so a filtered row reads as
	// selected rather than as the only row that exists.
	const selected = useMemo(() => {
		const map = new Map<string, Set<string>>();

		for (const filter of state.filters) {
			if (filter.operator !== "is") continue;

			const existing = map.get(filter.dimension) ?? new Set<string>();
			for (const value of filter.values) existing.add(value);
			map.set(filter.dimension, existing);
		}

		return map;
	}, [state.filters]);

	// One comparison object for the tiles and the graph. Two would be two chances
	// for the tile row to be measured against a different period from the line
	// under it, which is the kind of disagreement nobody notices until somebody
	// has already acted on it.
	const comparison =
		comparing && state.compare !== "off" ? ({ comparisons: { mode: state.compare } } as const) : undefined;

	const totalsBody: StatsRequest = {
		metrics: TILE_METRICS,
		date_range: range,
		filters: filters.length ? filters : undefined,
		include: comparison,
		exact: exact || undefined,
	};

	const totals = useStats(state.domain, state.domain ? totalsBody : null);

	const graphBody: StatsRequest = {
		metrics: [metric],
		date_range: range,
		dimensions: [interval === "auto" ? "time" : `time:${interval}`],
		filters: filters.length ? filters : undefined,
		include: comparison,
		exact: exact || undefined,
	};

	const graph = useStats(state.domain, state.domain && !live ? graphBody : null);

	const uniqueVisitors = totals.data?.results[0]?.metrics[0] ?? 0;

	/** applyFilter is what a report row does. The label travels with it so a
	 *  country code chosen here reaches the recipient of the link with its name
	 *  attached, whatever their browser can spell. */
	const applyFilter = useCallback(
		(filter: FilterState, label: string) => {
			const value = filter.values[0] ?? "";
			const labels: FilterLabels = { ...state.labels };

			if (label && label !== value) labels[value] = label;

			// The drawer closes: it is a list of one dimension's values, and the
			// filter just changed which values exist.
			navigate({ ...state, filters: toggle(state.filters, filter), labels, drawer: null });
		},
		[state, navigate],
	);

	const changeFilters = useCallback(
		(next: FilterState[], labels: FilterLabels) => navigate({ ...state, filters: next, labels, drawer: null }),
		[state, navigate],
	);

	/** openDetails pushes the drawer into the URL. Pushing rather than replacing
	 *  is what makes Back close it, which is the gesture everybody tries first,
	 *  and what makes the open drawer a link worth sending. */
	const openDetails = useCallback(
		(card: CardDef, tab: Tab, from: HTMLElement | null) => {
			opener.current = from;
			pushed.current = true;

			// A details view is a table. Opening it from the map lands on the
			// list of the same dimension rather than on a tab the drawer does
			// not have.
			const target = tab.map ? (tableTabs(card).tabs[0]?.id ?? tab.id) : tab.id;

			navigate({
				...state,
				drawer: { card: card.id, tab: target, page: 1, search: "", sort: "visitors", descending: true, breakdown: "" },
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

	const actions: ShortcutActions = {
		onPeriod: (period) => {
			if (period.custom === "pick") {
				setPickCustom((was) => was + 1);
				return;
			}

			if (period.custom === "yesterday") {
				const day = step("day", "", "", today(), -1);
				if (day) navigate({ ...state, from: day.from, to: day.to, drawer: null });
				return;
			}

			navigate({ ...state, preset: period.preset ?? "28d", from: "", to: "", drawer: null });
		},

		onStep: (direction) => {
			const next = step(state.preset, state.from, state.to, today(), direction);
			if (next) navigate({ ...state, from: next.from, to: next.to, drawer: null });
		},

		onCompare: () => navigate({ ...state, compare: state.compare === "off" ? "previous_period" : "off" }),

		onInterval: () => {
			const at = INTERVALS.indexOf(interval);

			setIntervalPref(INTERVALS[(at + 1) % INTERVALS.length] as IntervalPref);
		},

		onSearch: () => {
			// Whichever search box is on screen: the drawer's while it is open,
			// and the filter editor's otherwise. There is never more than one.
			document.querySelector<HTMLInputElement>('input[type="search"]')?.focus();
		},

		onSites: () => {
			const picker = document.getElementById("site-picker");
			if (!(picker instanceof HTMLSelectElement)) return;

			picker.focus();

			// showPicker drops the list open where the browser supports it;
			// focus alone is the honest fallback, and there is no way to force a
			// native select open without it.
			try {
				picker.showPicker();
			} catch {
				/* Focus is enough. */
			}
		},

		onHelp: () => setHelp((was) => !was),

		onEscape: () => {
			if (help) {
				setHelp(false);
				return;
			}

			// An open drawer owns Escape and closes itself. Clearing the filters
			// here as well would answer one keystroke twice, and the reader who
			// meant "close this" would lose the filters they had built.
			if (state.drawer) return;

			if (state.filters.length > 0) navigate({ ...state, filters: [], labels: {} });
		},
	};

	useShortcuts(actions);

	if (sites.length === 0) return <NoSites />;

	const drawerCard = state.drawer ? findCard(state.drawer.card) : undefined;
	const drawerTab = drawerCard && state.drawer ? findTab(drawerCard, state.drawer.tab) : undefined;

	return (
		<>
			<TopBar
				state={state}
				sites={sites}
				onNavigate={(next) => navigate(next)}
				theme={theme}
				onTheme={setTheme}
				resolved={totals.data?.query.date_range}
				filters={filters}
				onHelp={() => setHelp(true)}
				pickCustom={pickCustom}
			/>

			<main className="mx-auto max-w-shell px-4 py-5 sm:px-5">
				<div className="mb-3">
					<FilterBar
						domain={state.domain}
						range={range}
						filters={state.filters}
						labels={state.labels}
						onChange={changeFilters}
					/>
				</div>

				{live ? (
					<Realtime domain={state.domain} filters={filters} />
				) : (
					<section className="overflow-hidden rounded-md border border-line bg-card shadow-sm">
						{/* Above the tiles rather than beside one of them:
						    sampling applies to every figure in the section, and
						    a caveat attached to a single number reads as being
						    about that number alone. */}
						<SampledBadge sampling={totals.data?.meta.sampling} exact={exact} onExact={setExact} />

						<TopStats stats={totals} selected={metric} onSelect={setMetric} comparing={comparing} />
						<div className="p-4 sm:p-5">
							<MainGraph stats={graph} metric={metric} comparing={comparing} />
						</div>
					</section>
				)}

				<div className="mt-5 grid grid-cols-1 gap-5 lg:grid-cols-2">
					{CARDS.map((card) => (
						<ReportCard
							key={card.id}
							domain={state.domain}
							card={card}
							range={range}
							total={uniqueVisitors}
							onOpenDetails={openDetails}
							drawerTab={drawerCard?.id === card.id ? drawerTab?.id : undefined}
							filters={filters}
							onFilter={applyFilter}
							selected={selected}
						/>
					))}
				</div>
			</main>

			{drawerCard && state.drawer && drawerTab && (
				<Drawer
					domain={state.domain}
					card={drawerCard}
					state={state.drawer}
					range={range}
					onChange={updateDrawer}
					onClose={closeDetails}
					opener={opener.current}
					filters={filters}
					compare={comparing ? state.compare : "off"}
					onFilter={applyFilter}
					selected={selected.get(drawerTab.dimension) ?? EMPTY}
				/>
			)}

			{help && <ShortcutsModal onClose={() => setHelp(false)} />}
		</>
	);
}

/** One shared empty set, so a view with no filter on its dimension does not
 *  allocate a new one on every render and re-render its rows for nothing. */
const EMPTY: ReadonlySet<string> = new Set();

/** NoSites is the state a brand-new account lands in. It says what to do next
 *  rather than showing an empty dashboard, because an empty dashboard looks
 *  broken and a broken dashboard gets a support ticket. */
function NoSites() {
	return (
		<div className="flex min-h-screen items-center justify-center px-6">
			<div className="max-w-md text-center">
				<h1 className="text-lg font-semibold text-body">{t("dashboard.no_sites.title")}</h1>
				<p className="mt-2 text-sm text-muted">{t("dashboard.no_sites.body")}</p>
			</div>
		</div>
	);
}
