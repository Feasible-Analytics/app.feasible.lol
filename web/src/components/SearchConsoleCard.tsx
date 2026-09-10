//
// SearchConsoleCard.tsx
// What people searched to find the site, from Google Search Console.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useMemo, useState } from "react";

import { bootstrap, searchConsoleReport, shared } from "../api/client";
import type { DateRange, SearchDimension, SearchReport, SearchRow } from "../api/types";
import { calendarDate, compact } from "../lib/format";
import { t } from "../lib/i18n";
import { countryFlag, countryName } from "../lib/labels";
import { useNearViewport, useRemote } from "../lib/useStats";
import { Bar, Flag, InfoDot, NumberCell, PanelEmpty, PanelFailure, PanelFrame, PanelLoading } from "./atoms";

interface Props {
	domain: string;
	range: DateRange;
}

/** The four groupings, in the order the tab strip shows them. Search terms
 *  lead because they are the one thing on this card our own tracker can never
 *  know: Google strips the words out of the referrer before the browser sends
 *  it. */
const TABS: SearchDimension[] = ["query", "page", "country", "device"];

/** How long a reader has to stop typing before the filter is sent. Every
 *  keystroke is a scan of the site's whole keyword table, so this is the
 *  difference between a search box and a denial of service on your own box. */
const SEARCH_DEBOUNCE = 300;

/**
 * SearchConsoleCard shows the half of search traffic that happens before the
 * click: what people typed, how often the site appeared, and where it ranked.
 *
 * It takes no dashboard filters. Search Console rows carry four dimensions of
 * Google's choosing and none of ours, so there is nothing here for a filter on
 * browser or campaign to match — and answering the unfiltered question under a
 * filtered heading is the kind of quiet mismatch that makes a whole dashboard
 * untrustworthy. The card says so in its own caveat rather than pretending.
 */
export function SearchConsoleCard({ domain, range }: Props) {
	const [ref, near] = useNearViewport<HTMLElement>();
	const [dimension, setDimension] = useState<SearchDimension>("query");
	const [typed, setTyped] = useState("");
	const [search, setSearch] = useState("");

	// The typed value drives the input and the settled value drives the
	// request, so the field stays responsive while the network does not see a
	// query per keystroke.
	useEffect(() => {
		const timer = setTimeout(() => setSearch(typed.trim()), SEARCH_DEBOUNCE);

		return () => clearTimeout(timer);
	}, [typed]);

	// A shared link or a public dashboard is never offered this card, and the
	// endpoint behind it refuses them, so the request is not made at all. A
	// share can be pinned to a segment and these rows carry none of the
	// dimensions a segment filters on, which leaves no honest way to narrow
	// them to match what the link was scoped to.
	const readable = near && !shared();

	const request = useMemo(() => ({ dateRange: range, dimension, search }), [range, dimension, search]);
	const report = useRemote<SearchReport>(
		JSON.stringify({ domain, request }),
		readable,
		(signal) => searchConsoleReport(domain, request, signal),
	);

	if (shared()) return null;

	// An install with no Google application configured can never answer this,
	// so the card removes itself rather than teaching every self-hoster about a
	// feature their build does not have.
	if (report.data?.status === "unavailable") return null;

	return (
		<section ref={ref} className="flex min-h-card flex-col border-2 border-line bg-card lg:col-span-2">
			<header className="flex min-h-12 shrink-0 items-center gap-2 border-b border-line px-3 sm:px-5">
				<nav aria-label={t("dashboard.search.tabs_label")} className="scroll-thin flex min-w-0 items-center gap-0.5 overflow-x-auto">
					{TABS.map((candidate) => (
						<button
							key={candidate}
							type="button"
							aria-pressed={dimension === candidate}
							onClick={() => setDimension(candidate)}
							className={`shrink-0 px-2.5 py-1.5 text-xs transition-colors duration-150 ease-[var(--ease-ui)] ${
								dimension === candidate ? "bg-accent/10 font-semibold text-accent-ink" : "font-medium text-muted hover:text-body"
							}`}
						>
							{t(tabLabelId(candidate))}
						</button>
					))}
				</nav>

				<InfoDot text={searchCaveat(report.data)} />
			</header>

			<div className="min-h-[350px] flex-1">
				<SearchPanel report={report} dimension={dimension} typed={typed} onTyped={setTyped} />
			</div>
		</section>
	);
}

/** SearchPanel picks between the four things a request can come back as. */
function SearchPanel({
	report,
	dimension,
	typed,
	onTyped,
}: {
	report: ReturnType<typeof useRemote<SearchReport>>;
	dimension: SearchDimension;
	typed: string;
	onTyped: (value: string) => void;
}) {
	const importsURL = bootstrap().navigation?.imports_url;

	if (report.error) return <PanelFailure state={report} />;
	if (!report.data) return <PanelLoading label={t("dashboard.search.loading")} />;

	if (report.data.status === "not_connected") {
		return <PanelEmpty title={t("dashboard.search.not_connected")} body={t("dashboard.search.not_connected_hint")} href={importsURL} action={t("dashboard.search.connect")} />;
	}

	if (report.data.status === "no_property") {
		return <PanelEmpty title={t("dashboard.search.no_property")} body={t("dashboard.search.no_property_hint")} href={importsURL} action={t("dashboard.search.choose_property")} />;
	}

	const searchable = dimension === "query" || dimension === "page";

	return (
		<PanelFrame note={totalsNote(report.data)} footer={importsURL && <a href={importsURL} className="shrink-0 text-xs font-medium text-muted transition-colors hover:text-accent-ink">{t("dashboard.search.manage")} →</a>}>
			{searchable && (
				<div className="border-b border-line px-4 py-3 sm:px-5">
					<label className="block max-w-sm text-[11px] font-medium tracking-wide text-muted uppercase">
						{t(dimension === "query" ? "dashboard.search.filter_queries" : "dashboard.search.filter_pages")}
						<input
							type="search"
							value={typed}
							onChange={(event) => onTyped(event.target.value)}
							placeholder={t("dashboard.search.filter_placeholder")}
							className="mt-1 block h-control w-full border-2 border-line bg-card px-2.5 text-sm text-body normal-case"
						/>
					</label>
				</div>
			)}

			{report.data.rows.length === 0 ? (
				<PanelEmpty title={t(typed ? "dashboard.search.no_matches" : "dashboard.search.empty")} body={t(typed ? "dashboard.search.no_matches_hint" : "dashboard.search.empty_hint")} />
			) : (
				<SearchRows rows={report.data.rows} dimension={dimension} />
			)}
		</PanelFrame>
	);
}

/** SearchRows draws the table. Rows are not clickable: none of these values is
 *  a dimension the rest of the dashboard can be filtered by, and a row that
 *  looks like a filter and does nothing is worse than one that plainly is not. */
function SearchRows({ rows, dimension }: { rows: SearchRow[]; dimension: SearchDimension }) {
	const peak = Math.max(1, ...rows.map((row) => row.clicks));
	const columns = "grid-cols-[minmax(0,1fr)_60px_70px_56px_56px] sm:grid-cols-[minmax(0,1fr)_90px_100px_80px_80px]";

	return (
		<div className="px-4 sm:px-5">
			<div className={`grid h-8 items-center gap-2 text-[11px] font-medium tracking-wide text-muted uppercase ${columns}`}>
				<span>{t(headingId(dimension))}</span>
				<span className="text-right">{t("dashboard.column.clicks")}</span>
				<span className="text-right">{t("dashboard.column.impressions")}</span>
				<span className="text-right">{t("dashboard.column.ctr")}</span>
				<span className="text-right">{t("dashboard.column.position")}</span>
			</div>

			<ul className="pb-2">
				{rows.map((row) => (
					<li key={row.value} className={`group/row relative grid min-h-10 items-center gap-2 ${columns}`}>
						{row.clicks > 0 && <Bar share={row.clicks / peak} />}
						<span className="pointer-events-none relative flex min-w-0 items-center gap-2 pl-2">
							<RowIcon dimension={dimension} value={row.value} />
							<span className="truncate text-sm text-body" title={rowLabel(dimension, row.value)}>{rowLabel(dimension, row.value)}</span>
						</span>
						<NumberCell value={row.clicks} />
						<NumberCell value={row.impressions} />
						<span className="tnum pointer-events-none relative text-right text-sm text-body">{formatCTR(row.ctr)}</span>
						<span className="tnum pointer-events-none relative text-right text-sm text-body">{formatPosition(row.position)}</span>
					</li>
				))}
			</ul>
		</div>
	);
}

/** RowIcon gives a country its flag, so a country row here reads exactly as the
 *  same country does on the Locations card. Nothing else on this card has an
 *  icon: a search term is text, and a landing page is a path. */
function RowIcon({ dimension, value }: { dimension: SearchDimension; value: string }) {
	if (dimension !== "country") return null;

	return <Flag glyph={countryFlag(value)} />;
}

/** rowLabel resolves a stored code to the reader's own language, so a country
 *  row here reads exactly as it does on the Locations card. */
export function rowLabel(dimension: SearchDimension, value: string): string {
	if (dimension === "country") return countryName(value) || value;

	return value;
}

/** headingId names the label column for each grouping. */
export function headingId(dimension: SearchDimension): string {
	switch (dimension) {
		case "page":
			return "dashboard.dimension.landing_page";
		case "country":
			return "dashboard.dimension.country";
		case "device":
			return "dashboard.dimension.device";
		default:
			return "dashboard.dimension.search_term";
	}
}

/** tabLabelId names each tab. */
export function tabLabelId(dimension: SearchDimension): string {
	switch (dimension) {
		case "page":
			return "dashboard.search.tab_pages";
		case "country":
			return "dashboard.search.tab_countries";
		case "device":
			return "dashboard.search.tab_devices";
		default:
			return "dashboard.search.tab_queries";
	}
}

/** formatCTR renders a click-through rate. One decimal place, because a
 *  well-ranked keyword and a badly-ranked one routinely differ by less than a
 *  whole percentage point and rounding flattens the difference away. */
export function formatCTR(ctr: number): string {
	return `${(ctr * 100).toFixed(1)}%`;
}

/** formatPosition renders an average rank, or a dash when there is none. Zero
 *  is not a rank, so printing it would invent a position better than first. */
export function formatPosition(position: number): string {
	if (position <= 0) return "—";

	return position.toFixed(1);
}

/** totalsNote is the whole-period summary under the table.
 *
 * It totals the rows Google gave us rather than quoting a separate published
 * figure. Google withholds queries too few people searched, so a keyword list
 * can never add up to the site's real click count — and a published total
 * sitting above rows that cannot reach it reads as data we lost. */
export function totalsNote(report: SearchReport): string | undefined {
	if (report.totals.impressions <= 0) return undefined;

	return t("dashboard.search.totals", {
		clicks: compact(report.totals.clicks),
		impressions: compact(report.totals.impressions),
		ctr: formatCTR(report.totals.ctr),
		position: formatPosition(report.totals.position),
	});
}

/** searchCaveat is what the header's help bubble says.
 *
 * All three notes are about a number that reliably looks like a bug and is not:
 * a range whose last days are empty, a click count that disagrees with the
 * Visitors figure above it, and a keyword list that does not add up. */
export function searchCaveat(report: SearchReport | null): string[] {
	const notes = [t("dashboard.search.caveat_delay"), t("dashboard.search.caveat_totals"), t("dashboard.search.caveat_unfiltered")];

	if (report?.updated_through) {
		notes.unshift(t("dashboard.search.caveat_through", { date: calendarDate(report.updated_through) }));
	}

	if (report?.property) {
		notes.push(t("dashboard.search.caveat_property", { property: report.property }));
	}

	return notes;
}
