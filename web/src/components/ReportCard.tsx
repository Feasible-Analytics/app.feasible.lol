//
// ReportCard.tsx
// The card shell and the report row every breakdown in the product reuses.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect } from "react";

import type { DateRange, Filter, StatsRequest } from "../api/types";
import type { FilterState } from "../lib/filters";
import { compact, exact, percent } from "../lib/format";
import { t } from "../lib/i18n";
import { flagFor } from "../lib/labels";
import { usePref } from "../lib/prefs";
import type { CardDef, Tab } from "../lib/reports";
import { PRIMARY, dimensionsOf, findTab, groupsOf, labelOf, subTabsOf } from "../lib/reports";
import { useNearViewport, useStats } from "../lib/useStats";
import { Bar, Empty, Failure, Favicon, Flag, InfoDot, Spinner } from "./atoms";
import { SampledMark } from "./SampledBadge";
import { WorldMap } from "./WorldMap";

/** How many rows the card previews. The rest live in the details drawer: a card
 *  is a shape, not a table, and nine rows is where the shape stops being
 *  readable at 534px wide. */
const ROWS = 9;

/** How many rows the map asks for. A choropleth needs every country at once —
 *  the ninth-busiest is not where a world map stops being interesting — and
 *  there are fewer than 250 of them, so this is the whole answer rather than a
 *  page of it. */
const MAP_ROWS = 300;

/** One shared empty set for a card whose dimension carries no filter, so the
 *  map does not get a fresh object on every render and repaint every country
 *  for nothing. */
const EMPTY_SELECTION: ReadonlySet<string> = new Set();

interface Props {
	domain: string;
	card: CardDef;
	range: DateRange;
	/** Unique visitors over the whole period, the denominator for the hover
	 *  percentage. It comes from the top-stats query rather than from summing
	 *  the rows, because the rows are a page and a page has no total. */
	total: number;
	onOpenDetails: (card: CardDef, tab: Tab, opener: HTMLElement | null) => void;
	/** The tab the drawer is currently showing, so the card and the drawer stay
	 *  in step while both are open. */
	drawerTab?: string;
	/** Every filter in force. The card sends them with its own query — a card
	 *  that ignored them would sit next to the totals showing a different
	 *  population, which is the shape of every "these two numbers disagree" bug
	 *  in a reporting product. */
	filters: Filter[];
	/** Clicking a row filters the whole dashboard by it, and clicking it again
	 *  takes the filter off. */
	onFilter: (filter: FilterState, label: string) => void;
	/** The filtered values, keyed by dimension. The card looks up its own active
	 *  tab rather than being handed one dimension's worth: which tab is showing
	 *  is remembered per card in localStorage, so the card is the only thing that
	 *  knows it. */
	selected: ReadonlyMap<string, ReadonlySet<string>>;
	/** Whether the reader has refused sampling. The card has to be told, rather
	 *  than deciding for itself, or a dashboard would show exact totals above a
	 *  grid of estimates and the two would disagree with no way to tell why. */
	exact: boolean;
}

/**
 * ReportCard renders one breakdown card.
 *
 * The card owns its own query. That is what makes the page survivable while the
 * engine is slow: a four-second sources query does not hold up the graph, and
 * the fixed 430px height means nothing below it moves when the answer lands.
 */
export function ReportCard({
	domain,
	card,
	range,
	total,
	onOpenDetails,
	drawerTab,
	filters,
	onFilter,
	selected,
	exact: exactAnswer,
}: Props) {
	const [ref, near] = useNearViewport<HTMLElement>();

	// Which tab you last had open is a personal preference, not something a
	// shared link should impose on whoever opens it — so it lives in
	// localStorage while the date range lives in the URL.
	const ids = card.tabs.map((tab) => tab.id);
	const [tabId, setTabId] = usePref(`card.${card.id}`, ids[0] as string, ids);

	// While the drawer is open it is the authority on which tab is showing:
	// the drawer's tab came from the URL, and a card showing a different one
	// behind it would make the two look like different reports.
	const active = findTab(card, drawerTab ?? tabId);

	// Switching tab inside the drawer sticks. Reverting the card to its old tab
	// the moment the drawer closes would undo a choice the reader just made.
	useEffect(() => {
		if (drawerTab && drawerTab !== tabId) setTabId(drawerTab);
	}, [drawerTab, tabId, setTabId]);

	// The tab's own filters come first and the reader's after, because the tab's
	// are part of what the report means — "Campaigns" is traffic that carried a
	// tag — and dropping them would change the report rather than widen it.
	const combined = [...(active.filters ?? []), ...filters];

	const body: StatsRequest = {
		metrics: [PRIMARY],
		date_range: range,
		dimensions: dimensionsOf(active),
		pagination: { limit: active.map ? MAP_ROWS : ROWS },
		filters: combined.length ? combined : undefined,
		include: active.companion ? { page_titles: true } : undefined,
		exact: exactAnswer || undefined,
	};

	const stats = useStats(domain, body, near);
	const rows = stats.data?.results ?? [];
	const on = selected.get(active.dimension);
	const peak = Math.max(1, ...rows.map((row) => row.metrics[0] ?? 0));
	const groups = groupsOf(card);
	const subTabs = subTabsOf(card, active);

	return (
		<section
			ref={ref}
			className="group/card flex h-card flex-col overflow-hidden border-2 border-line bg-card"
		>
			<header className="flex h-10 shrink-0 items-center gap-2 px-5">
				<h2 className="flex shrink-0 items-center gap-1.5 text-sm font-semibold text-body">
					{t(card.titleId)}
					<SampledMark sampling={stats.data?.meta.sampling} />
					{(active.caveatId || card.caveatId) && (
						<InfoDot
							text={[active.caveatId, card.caveatId].filter(Boolean).map((id) => t(id as string))}
						/>
					)}
				</h2>

				<div className="scroll-thin ml-auto flex items-center gap-0.5 overflow-x-auto">
					{/* Both rows shrink to the smaller size once a group opens its
					    sub-tabs: six tabs at full size do not fit a 534px card,
					    and a strip that scrolls sideways hides the very tab
					    somebody is looking for. */}
					{groups.map((group) => (
						<TabButton
							key={group.key}
							label={t(group.labelId)}
							small={subTabs.length > 0}
							active={(active.groupId ?? active.id) === group.key}
							onClick={() => setTabId(group.tab.id)}
						/>
					))}

					{subTabs.length > 0 && (
						<>
							<span aria-hidden="true" className="mx-0.5 h-4 w-px bg-line" />
							{subTabs.map((tab) => (
								<TabButton
									key={tab.id}
									label={t(tab.labelId)}
									small
									active={active.id === tab.id}
									onClick={() => setTabId(tab.id)}
								/>
							))}
						</>
					)}
				</div>
			</header>

			<div className="min-h-cardbody flex-1 px-5">
				{stats.error ? (
					<Failure message={stats.error} onRetry={stats.reload} />
				) : !stats.data ? (
					<Spinner label={t("dashboard.card.loading", { title: t(card.titleId) })} />
				) : rows.length === 0 ? (
					<Empty what={t(active.nounId)} />
				) : active.map ? (
					<WorldMap rows={rows} onFilter={onFilter} selected={on ?? EMPTY_SELECTION} />
				) : (
					<>
						<div className="flex h-6 items-center text-[11px] font-medium tracking-wide text-muted uppercase">
							<span className="flex-1 truncate">{t(active.headingId)}</span>
							<span className="w-32 pr-1 text-right">{t("dashboard.column.visitors")}</span>
						</div>

						<ul>
							{rows.map((row) => {
								const value = row.metrics[0] ?? 0;
								const raw = row.dimensions[0] ?? "";
								const name = labelOf(active, raw);
								const companion = active.companion
									? (row.enrichments?.[active.companion.enrichment] ?? "")
									: "";
								const filtered = on?.has(raw) ?? false;

								return (
									<li
										key={row.dimensions.join("|")}
										className={`group/row relative flex items-center ${companion ? "h-row-stacked" : "h-row"}`}
									>
										<Bar share={value / peak} />

										{/* The whole row is the control. A separate
										    "filter by this" affordance would be a
										    second target on a compact row, and the row
										    itself is what everybody clicks first. */}
										<button
											type="button"
											aria-pressed={filtered}
											onClick={() =>
												onFilter({ operator: "is", dimension: active.dimension, values: [raw] }, name)
											}
											title={t(
												filtered ? "dashboard.row.stop_filtering" : "dashboard.row.filter_by",
												{ name },
											)}
											className="absolute inset-0"
										>
											<span className="sr-only">
												{t(filtered ? "dashboard.row.stop_filtering" : "dashboard.row.filter_by", {
													name,
												})}
											</span>
										</button>

										<span className="pointer-events-none relative flex min-w-0 flex-1 items-center gap-2 pl-2 text-sm text-body">
											{active.favicon && <Favicon name={raw || "Direct"} />}
											<Flag glyph={flagFor(active.dimension, raw)} />
											<span
												className={`flex min-w-0 flex-col justify-center leading-tight ${companion ? "gap-0.5" : ""}`}
											>
												{companion && (
													<span className="truncate text-xs font-medium" title={companion}>
														{companion}
													</span>
												)}
												<span
													className={`truncate ${companion ? "text-[10px] text-muted" : ""} ${
														filtered ? "font-medium text-accent-ink" : ""
													}`}
													title={name}
												>
													{name}
												</span>
											</span>
										</span>

										{/* The metric column is 128px split in half. The
										    count sits translated over the empty
										    percentage half and slides left on card
										    hover to reveal it — three classes, and the
										    thing that makes these tables feel
										    expensive. */}
										<span className="pointer-events-none relative flex w-32 shrink-0 items-center">
											<span
												className="tnum w-16 translate-x-16 text-right text-sm font-medium text-body transition-transform duration-150 ease-[var(--ease-ui)] group-hover/card:translate-x-0"
												title={exact(value)}
											>
												<span className="sr-only">{exact(value)}</span>
												<span aria-hidden="true">{compact(value)}</span>
											</span>
											<span className="tnum w-16 pr-1 text-right text-sm text-muted opacity-0 transition-opacity duration-150 ease-[var(--ease-ui)] group-hover/card:opacity-100">
												{percent(value, total)}
											</span>
										</span>
									</li>
								);
							})}
						</ul>
					</>
				)}
			</div>

			<footer className="flex h-[34px] shrink-0 items-center border-t border-line px-5">
				<button
					type="button"
					disabled={rows.length === 0}
					onClick={(event) => onOpenDetails(card, active, event.currentTarget)}
					className="text-xs font-medium text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent-ink disabled:cursor-default disabled:opacity-40"
				>
					{t("dashboard.card.details")}
				</button>

				{/* The geolocation database is licensed on the condition that its
				    maker is credited wherever its data is shown. */}
				{card.attribution && (
					<a
						href={card.attribution.href}
						target="_blank"
						rel="noopener noreferrer"
						className="ml-auto text-[11px] text-faint transition-colors duration-150 ease-[var(--ease-ui)] hover:text-muted"
					>
						{t(card.attribution.labelId)}
					</a>
				)}
			</footer>
		</section>
	);
}

/** TabButton is one tab. The active one is filled rather than underlined so it
 *  still reads as selected inside a strip that scrolls sideways on a phone. */
function TabButton({
	label,
	active,
	small = false,
	onClick,
}: {
	label: string;
	active: boolean;
	small?: boolean;
	onClick: () => void;
}) {
	return (
		<button
			type="button"
			aria-pressed={active}
			onClick={onClick}
			className={[
				"shrink-0 py-1 whitespace-nowrap transition-colors duration-150 ease-[var(--ease-ui)]",
				small ? "px-1.5 text-[11px]" : "px-2 text-xs",
				active ? "bg-accent/10 font-medium text-accent-ink" : "text-muted hover:text-body",
			].join(" ")}
		>
			{label}
		</button>
	);
}
