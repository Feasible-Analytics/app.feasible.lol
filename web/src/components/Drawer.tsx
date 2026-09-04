//
// Drawer.tsx
// The details view: a right-side drawer, not a centred modal.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useMemo, useRef, useState } from "react";

import type { DateRange, Filter, StatsRequest } from "../api/types";
import type { CompareMode } from "../lib/compare";
import type { FilterState } from "../lib/filters";
import { exact, metricValue } from "../lib/format";
import { formatterLocale, t } from "../lib/i18n";
import { flagFor } from "../lib/labels";
import type { CardDef } from "../lib/reports";
import {
	BREAKDOWNS,
	DRAWER_HEADINGS,
	DRAWER_METRICS,
	INVERTED,
	dimensionsOf,
	findTab,
	groupsOf,
	labelOf,
	subTabsOf,
	tableTabs,
} from "../lib/reports";
import type { DrawerState } from "../lib/url";
import { useStats } from "../lib/useStats";
import { ChangeChip, Empty, Failure, Favicon, Flag, Spinner } from "./atoms";
import { SampledBadge } from "./SampledBadge";

/**
 * The details view is a drawer rather than a centred modal, and the difference
 * is not decoration.
 *
 * A modal dims the page and takes the context away exactly when somebody is
 * drilling into it. The drawer keeps the dashboard readable behind a light
 * scrim, so you can still see the totals the row you clicked is a slice of, and
 * it has the horizontal room for five metric columns and a secondary breakdown
 * — neither of which fits in a centred box.
 */

/** One page of rows. A hundred is where a details list stops being something
 *  you read and starts being something you search, which is why the header has
 *  a search box and explicit paging rather than an infinite scroll that never
 *  lets you reach the end. */
const PAGE = 100;

interface Props {
	domain: string;
	card: CardDef;
	state: DrawerState;
	range: DateRange;
	onChange: (next: DrawerState) => void;
	onClose: () => void;
	/** The element that opened the drawer; focus goes back to it on close. */
	opener: HTMLElement | null;
	/** The dashboard's filters. The drawer is a bigger view of the card behind
	 *  it, so it has to be looking at the same population — a details view that
	 *  ignored the filters would show rows the card it opened from does not. */
	filters: Filter[];
	/** Off, or the mode the earlier period is chosen by. */
	compare: CompareMode;
	/** Clicking a row filters the whole dashboard and closes the drawer, which is
	 *  what somebody who just found the row they wanted is asking for. */
	onFilter: (filter: FilterState, label: string) => void;
	/** Values already filtered on this tab's dimension. */
	selected: ReadonlySet<string>;
}

/**
 * Drawer renders the full breakdown for one card.
 *
 * Every piece of its state that describes *what is on screen* — the tab, the
 * page, the sort, the search, the breakdown — is in the URL. That is what makes
 * a details view worth linking to, and it is why Back closes the drawer instead
 * of leaving the dashboard.
 */
export function Drawer({
	domain,
	card,
	state,
	range,
	onChange,
	onClose,
	opener,
	filters: applied,
	compare,
	onFilter,
	selected,
}: Props) {
	const panel = useRef<HTMLDivElement>(null);

	// The drawer shows the card without its map: everything in here is a table,
	// and findTab falls back to the first tab, so a link saved on the map opens
	// the list of the same countries rather than nothing.
	const listing = tableTabs(card);
	const tab = findTab(listing, state.tab);

	// Whether the reader has refused sampling for this panel. It is local to
	// the drawer rather than shared with the dashboard behind it, because the
	// two ask different questions and only one of them is likely to be sampled.
	const [exactAnswer, setExactAnswer] = useState(false);

	// The search box types faster than a four-second query can answer, so the
	// input is local and the URL only catches up once typing stops.
	const [typed, setTyped] = useState(state.search);

	useEffect(() => setTyped(state.search), [state.search]);

	useEffect(() => {
		if (typed === state.search) return;

		const id = setTimeout(() => onChange({ ...state, search: typed, page: 1 }), 300);

		return () => clearTimeout(id);
	}, [typed, state, onChange]);

	useFocusTrap(panel, onClose, opener);

	const filters = useMemo<Filter[]>(() => {
		const list: Filter[] = [...(tab.filters ?? []), ...applied];

		if (state.search.trim()) {
			list.push(["contains", tab.dimension, [state.search.trim()], { case_sensitive: false }]);
		}

		return list;
	}, [tab, state.search, applied]);

	const dimensions = dimensionsOf(tab, state.breakdown);

	const body: StatsRequest = {
		metrics: DRAWER_METRICS,
		date_range: range,
		dimensions,
		filters: filters.length ? filters : undefined,
		order_by: [[state.sort, state.descending ? "desc" : "asc"]],
		pagination: { limit: PAGE, offset: (state.page - 1) * PAGE },
		include: {
			total_rows: true,
			page_titles: tab.companion ? true : undefined,
			// The earlier period is looked up by the keys already on this page
			// rather than paginated on its own, so a comparison costs one extra
			// query and never attaches a number to the wrong row.
			comparisons: compare === "off" ? undefined : { mode: compare },
		},
		exact: exactAnswer || undefined,
	};

	const stats = useStats(domain, body);
	const rows = stats.data?.results ?? [];
	const totalRows = stats.data?.meta.total_rows ?? 0;
	const warnings = stats.data?.meta.metric_warnings ?? {};

	const first = totalRows === 0 ? 0 : (state.page - 1) * PAGE + 1;
	const last = Math.min(state.page * PAGE, totalRows);
	const lastPage = Math.max(1, Math.ceil(totalRows / PAGE));

	const groups = groupsOf(listing);
	const subTabs = subTabsOf(listing, tab);
	const breakdown = BREAKDOWNS.find((entry) => entry.id === state.breakdown);

	/** sort flips a column, or switches to it descending first — which is what
	 *  somebody clicking "Bounce" almost always wants to see. */
	const sort = (key: string) => {
		if (state.sort === key) onChange({ ...state, descending: !state.descending, page: 1 });
		else onChange({ ...state, sort: key, descending: true, page: 1 });
	};

	return (
		<div className="fixed inset-0 z-50">
			{/* The scrim is light on purpose. Dimming the page as hard as a modal
			    would throw away the only reason this is a drawer. */}
			<button
				type="button"
				aria-label={t("dashboard.drawer.close")}
				onClick={onClose}
				className="drawer-scrim absolute inset-0 bg-[var(--fs-scrim)]"
			/>

			<div
				ref={panel}
				role="dialog"
				aria-modal="true"
				aria-label={t("dashboard.drawer.title", { card: t(card.titleId), tab: t(tab.labelId) })}
				className="drawer-panel absolute inset-y-0 right-0 flex w-full flex-col border-l-2 border-line bg-card min-[900px]:w-[max(560px,min(50vw,900px))]"
			>
				<div className="flex shrink-0 flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
					<input
						type="search"
						value={typed}
						onChange={(event) => setTyped(event.target.value)}
						placeholder={t("dashboard.drawer.search_placeholder", {
							heading: t(tab.headingId).toLocaleLowerCase(formatterLocale()),
						})}
						aria-label={t("dashboard.drawer.search_label", { heading: t(tab.headingId) })}
						className="h-control min-w-0 flex-1 border-2 border-line bg-page px-2.5 text-sm text-body placeholder:text-muted"
					/>

					<label className="flex items-center">
						<span className="sr-only">{t("dashboard.drawer.breakdown")}</span>
						<select
							value={state.breakdown}
							onChange={(event) => onChange({ ...state, breakdown: event.target.value, page: 1 })}
							className="h-control cursor-pointer border-2 border-line bg-card px-2 text-sm text-body"
						>
							{BREAKDOWNS.map((entry) => (
								<option key={entry.id || "none"} value={entry.id}>
									{t(entry.labelId)}
								</option>
							))}
						</select>
					</label>

					<span className="tnum ml-auto text-xs text-muted">
						{totalRows === 0
							? "0"
							: t("dashboard.drawer.showing", { first, last, total: exact(totalRows) })}
					</span>

					<Pager
						page={state.page}
						lastPage={lastPage}
						onGo={(page) => onChange({ ...state, page })}
					/>

					<button
						type="button"
						onClick={onClose}
						aria-label={t("dashboard.drawer.close")}
						className="flex size-control items-center justify-center border-2 border-line text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
					>
						✕
					</button>
				</div>

				{/* The card's own tabs repeat here so a dimension can be switched
				    without closing and reopening. */}
				<div className="scroll-thin flex shrink-0 items-center gap-0.5 overflow-x-auto border-b border-line px-4 py-1.5">
					{groups.map((group) => (
						<DrawerTab
							key={group.key}
							label={t(group.labelId)}
							active={(tab.groupId ?? tab.id) === group.key}
							onClick={() => onChange({ ...state, tab: group.tab.id, page: 1, search: "" })}
						/>
					))}

					{subTabs.length > 0 && (
						<>
							<span aria-hidden="true" className="mx-1 h-4 w-px bg-line" />
							{subTabs.map((entry) => (
								<DrawerTab
									key={entry.id}
									label={t(entry.labelId)}
									active={tab.id === entry.id}
									onClick={() => onChange({ ...state, tab: entry.id, page: 1, search: "" })}
								/>
							))}
						</>
					)}
				</div>

				{/* Filtering and searching are exactly what takes a report off
				    the pre-aggregated summaries, so this is the panel where an
				    answer is most likely to be an estimate. */}
				<SampledBadge
					sampling={stats.data?.meta.sampling}
					exact={exactAnswer}
					exactFallback={stats.exactFallback}
					onExact={setExactAnswer}
				/>

				<div className="scroll-thin min-h-0 flex-1 overflow-auto">
					{stats.error ? (
						<div className="h-64">
							<Failure message={stats.error} onRetry={stats.reload} />
						</div>
					) : !stats.data ? (
						<div className="h-64">
							<Spinner label={t("dashboard.drawer.loading")} />
						</div>
					) : rows.length === 0 ? (
						<div className="h-64">
							<Empty
								what={
									state.search
										? t("dashboard.drawer.empty_search", { noun: t(tab.nounId), search: state.search })
										: t(tab.nounId)
								}
							/>
						</div>
					) : (
						<table className="w-full border-collapse text-sm">
							<thead className="sticky top-0 z-10 bg-card">
								<tr className="border-b border-line">
									<Th
										label={t(tab.headingId)}
										sorted={state.sort === tab.dimension}
										descending={state.descending}
										onSort={() => sort(tab.dimension)}
										align="left"
										grow
									/>
									{/* The breakdown column sizes to its contents rather
									    than sharing the slack with the label column. Two
									    growing columns split the width evenly, which
									    squeezed "Desktop" down to an ellipsis next to a
									    city name with room to spare. */}
									{breakdown?.id && <Th label={t(breakdown.labelId)} align="left" />}
									{DRAWER_METRICS.map((metric) => (
										<Th
											key={metric}
											label={t(DRAWER_HEADINGS[metric] ?? metric)}
											sorted={state.sort === metric}
											descending={state.descending}
											onSort={() => sort(metric)}
											align="right"
										/>
									))}
								</tr>
							</thead>

							<tbody>
								{rows.map((row, index) => {
									const raw = row.dimensions[0] ?? "";
									const name = labelOf(tab, raw);
									const companion = tab.companion
										? (row.enrichments?.[tab.companion.enrichment] ?? "")
										: "";
									const on = selected.has(raw);

									return (
										<tr
											key={`${row.dimensions.join("|")}-${index}`}
											className={`h-drawerrow ${index % 2 === 1 ? "bg-zebra" : ""}`}
										>
											<td className="w-full max-w-0 px-4">
												{/* The label is the control here rather than the
												    whole row: the drawer's rows carry five
												    numbers a reader wants to select and copy,
												    and a row-wide button eats the selection. */}
												<button
													type="button"
													aria-pressed={on}
													onClick={() =>
														onFilter({ operator: "is", dimension: tab.dimension, values: [raw] }, name)
													}
													title={t(
														on ? "dashboard.row.stop_filtering" : "dashboard.row.filter_by",
														{ name },
													)}
													className="flex w-full items-center gap-2 text-left"
												>
													{tab.favicon && <Favicon name={raw || "Direct"} />}
													<Flag glyph={flagFor(tab.dimension, raw)} />
													<span className="flex min-w-0 flex-col justify-center leading-tight">
														{companion && (
															<span className="truncate text-xs font-medium text-body" title={companion}>
																{companion}
															</span>
														)}
														<span
															className={`truncate transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent-ink ${
 on
 ? "font-medium text-accent-ink"
																	: companion
																		? "text-[10px] text-muted"
																		: "text-body"
															}`}
															title={name}
														>
															{name}
														</span>
													</span>
												</button>
											</td>

											{/* The breakdown is the second grouping dimension, so
											    its value is the second entry on every row. */}
											{breakdown?.id && (
												<td className="px-3">
													<span
														className="block max-w-48 truncate text-muted"
														title={row.dimensions[1] || t("dashboard.value.unknown")}
													>
														{row.dimensions[1] || t("dashboard.value.unknown")}
													</span>
												</td>
											)}

											{DRAWER_METRICS.map((metric, position) => (
												<td key={metric} className="tnum px-3 text-right whitespace-nowrap text-body">
													<span className="inline-flex items-baseline gap-1.5">
														{metricValue(metric, row.metrics[position] ?? 0)}
														{/* The delta rides on the sorted column only.
														    Five change chips on a 36px row is a
														    wall of arrows, and the column somebody
														    ordered by is the one they are reading. */}
														{compare !== "off" && metric === state.sort && (
															<ChangeChip
																change={row.comparison?.change[position]}
																invert={INVERTED.has(metric)}
															/>
														)}
													</span>
												</td>
											))}
										</tr>
									);
								})}
							</tbody>
						</table>
					)}
				</div>

				{Object.keys(warnings).length > 0 && (
					<footer className="shrink-0 border-t border-line px-4 py-2">
						{Object.entries(warnings).map(([metric, warning]) => (
							<p key={metric} className="text-[11px] leading-relaxed text-muted">
								<span className="font-medium text-body">{t(DRAWER_HEADINGS[metric] ?? metric)}:</span>{" "}
								{warning.warning}
							</p>
						))}
					</footer>
				)}
			</div>
		</div>
	);
}

/** Pager is the explicit prev/next. Explicit rather than infinite scroll
 *  because a details list is something people page through looking for a row,
 *  and an infinite list has no "I have seen everything" state. */
function Pager({ page, lastPage, onGo }: { page: number; lastPage: number; onGo: (page: number) => void }) {
	return (
		<span className="flex items-center gap-1">
			<button
				type="button"
				aria-label={t("dashboard.drawer.previous_page")}
				disabled={page <= 1}
				onClick={() => onGo(page - 1)}
				className="flex size-control items-center justify-center border-2 border-line text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover disabled:opacity-30"
			>
				‹
			</button>
			<button
				type="button"
				aria-label={t("dashboard.drawer.next_page")}
				disabled={page >= lastPage}
				onClick={() => onGo(page + 1)}
				className="flex size-control items-center justify-center border-2 border-line text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover disabled:opacity-30"
			>
				›
			</button>
		</span>
	);
}

/** Th is one sortable, sticky column heading. A heading with no onSort is a
 *  column the engine cannot order by — the secondary breakdown — and it renders
 *  as plain text rather than as a button that does nothing. */
function Th({
	label,
	sorted,
	descending,
	onSort,
	align,
	grow = false,
}: {
	label: string;
	sorted?: boolean;
	descending?: boolean;
	onSort?: () => void;
	align: "left" | "right";
	/** The label columns absorb the slack; the metric columns size to their
	 *  contents, so five of them never squeeze the name down to an ellipsis. */
	grow?: boolean;
}) {
	const classes = `px-3 py-2 text-[11px] font-medium tracking-wide uppercase ${
		align === "right" ? "text-right whitespace-nowrap" : "px-4 text-left"
	} ${grow ? "w-full" : "w-px"} ${sorted ? "text-accent-ink" : "text-muted"}`;

	if (!onSort) {
		return (
			<th scope="col" className={classes}>
				{label}
			</th>
		);
	}

	return (
		<th scope="col" className={classes} aria-sort={sorted ? (descending ? "descending" : "ascending") : "none"}>
			<button
				type="button"
				onClick={onSort}
				className="transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent-ink"
			>
				{label}
				{sorted && <span aria-hidden="true"> {descending ? "↓" : "↑"}</span>}
			</button>
		</th>
	);
}

/** DrawerTab is the drawer's copy of a card tab. */
function DrawerTab({ label, active, onClick }: { label: string; active: boolean; onClick: () => void }) {
	return (
		<button
			type="button"
			aria-pressed={active}
			onClick={onClick}
			className={`shrink-0 px-2 py-1 text-xs whitespace-nowrap transition-colors duration-150 ease-[var(--ease-ui)] ${
 active ? "bg-accent/10 font-medium text-accent-ink" : "text-muted hover:text-body"
			}`}
		>
			{label}
		</button>
	);
}

/**
 * useFocusTrap keeps the keyboard inside the drawer and gives it back on close.
 *
 * Returning focus to the row that opened it is the half people notice: without
 * it, closing the drawer drops a keyboard user back at the top of the document
 * and they have to tab all the way down to where they were.
 */
function useFocusTrap(panel: React.RefObject<HTMLElement | null>, onClose: () => void, opener: HTMLElement | null): void {
	// The closer is read through a ref rather than listed as a dependency: the
	// caller rebuilds it on every URL change, and re-running this effect for
	// that would hand focus to the opener and then to the search box every time
	// the reader sorts a column or turns a page.
	const close = useRef(onClose);
	close.current = onClose;

	useEffect(() => {
		const node = panel.current;
		if (!node) return;

		const focusables = () =>
			Array.from(
				node.querySelectorAll<HTMLElement>(
					'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])',
				),
			).filter((element) => element.offsetParent !== null);

		focusables()[0]?.focus();

		const onKey = (event: KeyboardEvent) => {
			if (event.key === "Escape") {
				event.stopPropagation();
				close.current();
				return;
			}

			if (event.key !== "Tab") return;

			const list = focusables();
			const firstElement = list[0];
			const lastElement = list[list.length - 1];
			if (!firstElement || !lastElement) return;

			if (event.shiftKey && document.activeElement === firstElement) {
				event.preventDefault();
				lastElement.focus();
			} else if (!event.shiftKey && document.activeElement === lastElement) {
				event.preventDefault();
				firstElement.focus();
			}
		};

		document.addEventListener("keydown", onKey);

		// The page behind must not scroll while the drawer is open, or a
		// trackpad flick moves the dashboard instead of the list being read.
		const previousOverflow = document.body.style.overflow;
		document.body.style.overflow = "hidden";

		return () => {
			document.removeEventListener("keydown", onKey);
			document.body.style.overflow = previousOverflow;
			opener?.focus();
		};
	}, [panel, opener]);
}
