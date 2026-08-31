//
// ReportCard.tsx
// The card shell and the report row every breakdown in the product reuses.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect } from "react";

import type { DateRange, StatsRequest } from "../api/types";
import { compact, exact, percent } from "../lib/format";
import { usePref } from "../lib/prefs";
import type { CardDef, Tab } from "../lib/reports";
import { PRIMARY, findTab, groupsOf, labelOf, subTabsOf } from "../lib/reports";
import { useNearViewport, useStats } from "../lib/useStats";
import { Bar, Empty, Failure, Favicon, InfoDot, Spinner } from "./atoms";

/** How many rows the card previews. The rest live in the details drawer: a card
 *  is a shape, not a table, and nine rows is where the shape stops being
 *  readable at 534px wide. */
const ROWS = 9;

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
}

/**
 * ReportCard renders one breakdown card.
 *
 * The card owns its own query. That is what makes the page survivable while the
 * engine is slow: a four-second sources query does not hold up the graph, and
 * the fixed 430px height means nothing below it moves when the answer lands.
 */
export function ReportCard({ domain, card, range, total, onOpenDetails, drawerTab }: Props) {
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

	const body: StatsRequest = {
		metrics: [PRIMARY],
		date_range: range,
		dimensions: [active.dimension],
		pagination: { limit: ROWS },
		filters: active.filters,
	};

	const stats = useStats(domain, body, near);
	const rows = stats.data?.results ?? [];
	const peak = Math.max(1, ...rows.map((row) => row.metrics[0] ?? 0));
	const groups = groupsOf(card);
	const subTabs = subTabsOf(card, active);

	return (
		<section
			ref={ref}
			className={`group/card flex h-card flex-col overflow-hidden rounded-md border border-line bg-card shadow-sm ${card.tint}`}
		>
			<header className="flex h-10 shrink-0 items-center gap-2 px-5">
				<h2 className="flex shrink-0 items-center gap-1.5 text-sm font-semibold text-body">
					{card.title}
					{card.caveat && <InfoDot text={card.caveat} />}
				</h2>

				<div className="scroll-thin ml-auto flex items-center gap-0.5 overflow-x-auto">
					{/* Both rows shrink to the smaller size once a group opens its
					    sub-tabs: six tabs at full size do not fit a 534px card,
					    and a strip that scrolls sideways hides the very tab
					    somebody is looking for. */}
					{groups.map((group) => (
						<TabButton
							key={group.key}
							label={group.label}
							small={subTabs.length > 0}
							active={(active.group ?? active.id) === group.key}
							onClick={() => setTabId(group.tab.id)}
						/>
					))}

					{subTabs.length > 0 && (
						<>
							<span aria-hidden="true" className="mx-0.5 h-4 w-px bg-line" />
							{subTabs.map((tab) => (
								<TabButton
									key={tab.id}
									label={tab.label}
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
					<Spinner label={`Loading ${card.title}`} />
				) : rows.length === 0 ? (
					<Empty what={active.noun} />
				) : (
					<>
						<div className="flex h-6 items-center text-[11px] font-medium tracking-wide text-muted uppercase">
							<span className="flex-1 truncate">{active.heading}</span>
							<span className="w-32 pr-1 text-right">Visitors</span>
						</div>

						<ul>
							{rows.map((row) => {
								const value = row.metrics[0] ?? 0;
								const name = labelOf(active, row.dimensions[0] ?? "");

								return (
									<li key={name} className="group/row relative flex h-row items-center">
										<Bar share={value / peak} />

										<span className="relative flex min-w-0 flex-1 items-center gap-2 pl-2 text-sm text-body">
											{active.favicon && <Favicon name={row.dimensions[0] || "Direct"} />}
											<span className="truncate" title={name}>
												{name}
											</span>
										</span>

										{/* The metric column is 128px split in half. The
										    count sits translated over the empty
										    percentage half and slides left on card
										    hover to reveal it — three classes, and the
										    thing that makes these tables feel
										    expensive. */}
										<span className="relative flex w-32 shrink-0 items-center">
											<span
												className="tnum w-16 translate-x-16 text-right text-sm font-medium text-body transition-transform duration-150 ease-[var(--ease-ui)] group-hover/card:translate-x-0"
												title={exact(value)}
											>
												{compact(value)}
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
					className="text-xs font-medium text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent disabled:cursor-default disabled:opacity-40"
				>
					Details →
				</button>
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
				"shrink-0 rounded-sm py-1 whitespace-nowrap transition-colors duration-150 ease-[var(--ease-ui)]",
				small ? "px-1.5 text-[11px]" : "px-2 text-xs",
				active ? "bg-accent/10 font-medium text-accent" : "text-muted hover:text-body",
			].join(" ")}
		>
			{label}
		</button>
	);
}
