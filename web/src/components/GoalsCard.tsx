//
// GoalsCard.tsx
// Goals, custom properties, funnels, and exploratory journeys in one section.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useMemo, useState } from "react";

import {
	bootstrap,
	funnelReport,
	funnels,
	goalsReport,
	journeyReport,
	properties,
	propertyReport,
	query,
} from "../api/client";
import type {
	DateRange,
	Filter,
	Funnel,
	FunnelReport,
	Goal,
	GoalReport,
	JourneyAnchor,
	JourneyReport,
	Property,
	PropertyReport,
	StatsResponse,
} from "../api/types";
import type { FilterState } from "../lib/filters";
import { compact, exact, metricAxisValue, metricTitle } from "../lib/format";
import { t } from "../lib/i18n";
import type { RemoteState } from "../lib/useStats";
import { useNearViewport, useRemote } from "../lib/useStats";
import type { BehaviorState, BehaviorTab } from "../lib/url";
import { Bar, Failure, InfoDot, Spinner } from "./atoms";

type JourneyDirection = "forward" | "backward";

interface Props {
	domain: string;
	range: DateRange;
	filters: Filter[];
	exact: boolean;
	onFilter: (filter: FilterState, label: string) => void;
	behavior: BehaviorState;
	onBehaviorChange: (behavior: BehaviorState) => void;
}

interface AnchorOptions {
	pages: JourneyAnchor[];
	events: JourneyAnchor[];
}

const TABS: BehaviorTab[] = ["goals", "properties", "funnels", "explore"];

/** behaviorEnabled keeps the default Goals report lazy while ensuring a
 * shared URL for another tab starts its request even before the card enters
 * the viewport. Browser scroll restoration can otherwise leave a deep-linked
 * tab displaying its loading state without ever scheduling the request. */
export function behaviorEnabled(tab: BehaviorTab, near: boolean): boolean {
	return near || tab !== "goals";
}

/** GoalsCard keeps every behavior-analysis mode visible in one full-width
 * section. Empty states belong inside their tabs so an unconfigured site still
 * teaches the reader which analyses are available and where to set them up. */
export function GoalsCard({ domain, range, filters, exact: exactAnswer, onFilter, behavior, onBehaviorChange }: Props) {
	const [ref, near] = useNearViewport<HTMLElement>();
	const [expanded, setExpanded] = useState(false);
	const request = useMemo(() => ({ dateRange: range, filters, exact: exactAnswer }), [range, filters, exactAnswer]);
	const settingsURL = bootstrap().navigation?.conversions_url;
	const tab = behavior.tab;
	const enabled = behaviorEnabled(tab, near);

	// The partial-reporting date belongs in the header's help bubble, but only
	// the panel that fetched the report knows it, so the panel reports it up.
	const [partialFrom, setPartialFrom] = useState<string>();
	const [shown, setShown] = useState(tab);

	// A tab change forgets the date in the same render that changes the tab,
	// before the incoming panel mounts. Waiting for that panel to answer would
	// leave the previous tab's caveat standing over numbers it does not
	// describe.
	if (shown !== tab) {
		setShown(tab);
		setPartialFrom(undefined);
	}

	return (
		<section
			ref={ref}
			className={`tint-languages flex min-h-card flex-col rounded-md border border-line bg-card shadow-sm lg:col-span-2 ${
				expanded ? "lg:min-h-[640px]" : ""
			}`}
		>
			<header className="flex min-h-12 shrink-0 items-center gap-2 border-b border-line px-3 sm:px-5">
				<nav aria-label={t("dashboard.behavior.tabs_label")} className="scroll-thin flex min-w-0 items-center gap-0.5 overflow-x-auto">
					{TABS.map((candidate) => (
						<button
							key={candidate}
							type="button"
							aria-pressed={tab === candidate}
							onClick={() => onBehaviorChange({ ...behavior, tab: candidate })}
							className={`shrink-0 rounded-sm px-2.5 py-1.5 text-xs transition-colors duration-150 ease-[var(--ease-ui)] ${
								tab === candidate ? "bg-accent/10 font-semibold text-accent" : "font-medium text-muted hover:text-body"
							}`}
						>
							{behaviorTabLabel(candidate)}
						</button>
					))}
				</nav>

				<InfoDot text={behaviorCaveat(tab, partialFrom)} />

				<button
					type="button"
					aria-label={t(expanded ? "dashboard.behavior.collapse" : "dashboard.behavior.expand")}
					aria-pressed={expanded}
					onClick={() => setExpanded((value) => !value)}
					className="ml-auto flex size-7 shrink-0 items-center justify-center rounded-sm text-muted transition-colors duration-150 hover:bg-hover hover:text-body"
				>
					<span aria-hidden="true" className="text-base leading-none">{expanded ? "↙" : "↗"}</span>
				</button>
			</header>

			<div className="min-h-[350px] flex-1">
				{tab === "goals" && (
					<GoalsPanel domain={domain} request={request} enabled={enabled} onFilter={onFilter} settingsURL={settingsURL} onPartial={setPartialFrom} />
				)}
				{tab === "properties" && (
					<PropertiesPanel domain={domain} request={request} enabled={enabled} onFilter={onFilter} settingsURL={settingsURL} selected={behavior.property} onSelected={(property) => onBehaviorChange({ ...behavior, property })} />
				)}
				{tab === "funnels" && (
					<FunnelsPanel domain={domain} request={request} enabled={enabled} settingsURL={settingsURL} selected={behavior.funnel} onSelected={(funnel) => onBehaviorChange({ ...behavior, funnel })} onPartial={setPartialFrom} />
				)}
				{tab === "explore" && <ExplorePanel domain={domain} request={request} enabled={enabled} behavior={behavior} onBehaviorChange={onBehaviorChange} />}
			</div>
		</section>
	);
}

/** GoalsPanel renders every configured conversion, including zero-result goals,
 * and turns matchable rows into the same dashboard filters used elsewhere. */
function GoalsPanel({
	domain,
	request,
	enabled,
	onFilter,
	settingsURL,
	onPartial,
}: {
	domain: string;
	request: { dateRange: DateRange; filters: Filter[]; exact: boolean };
	enabled: boolean;
	onFilter: Props["onFilter"];
	settingsURL?: string;
	onPartial: (from?: string) => void;
}) {
	const report = useRemote<GoalReport>(
		JSON.stringify({ domain, request }),
		enabled,
		(signal) => goalsReport(domain, request, signal),
	);
	const rows = report.data?.rows ?? [];
	const peak = Math.max(1, ...rows.map((row) => row.unique_conversions));
	const onlyEmptyAutomatic = rows.length > 0 && rows.every((row) => row.goal.is_automatic && row.total_conversions === 0);

	// A failed request keeps the rows it last succeeded with, so the caveat is
	// withheld whenever the panel is showing a failure rather than a table. A
	// note explaining numbers nobody can see explains nothing.
	const partialFrom = report.error ? undefined : rows.find((row) => row.partial)?.from;

	useEffect(() => onPartial(partialFrom), [onPartial, partialFrom]);

	if (report.error) return <PanelFailure state={report} />;
	if (!report.data) return <PanelLoading label={t("dashboard.goals.loading")} />;

	return (
		<PanelFrame
			footer={settingsURL ? <a href={settingsURL} className="text-xs font-medium text-muted transition-colors hover:text-accent">{t("dashboard.behavior.goals.manage")} →</a> : undefined}
		>
			{onlyEmptyAutomatic && (
				<div className="flex flex-col gap-2 border-b border-line bg-accent/5 px-4 py-3 sm:flex-row sm:items-center sm:px-5">
					<div className="min-w-0 flex-1"><p className="text-sm font-medium text-body">{t("dashboard.behavior.goals.automatic_ready")}</p><p className="text-xs leading-relaxed text-muted">{t("dashboard.behavior.goals.automatic_ready_hint")}</p></div>
					{settingsURL && <a href={settingsURL} className="shrink-0 rounded-md border border-line bg-card px-3 py-1.5 text-xs font-medium text-body transition-colors hover:bg-hover">{t("dashboard.behavior.goals.add_business_goal")}</a>}
				</div>
			)}
			{rows.length === 0 ? (
				<BehaviorEmpty title={t("dashboard.goals.empty")} body={t("dashboard.goals.empty_hint")} href={settingsURL} />
			) : (
				<div className="px-4 sm:px-5">
					<div className="grid h-8 grid-cols-[minmax(0,1fr)_60px_60px_60px] items-center gap-2 text-[11px] font-medium tracking-wide text-muted uppercase sm:grid-cols-[minmax(0,1fr)_90px_90px_80px]">
						<span>{t("dashboard.column.goal")}</span><span className="text-right">{t("dashboard.column.uniques")}</span><span className="text-right">{t("dashboard.column.total")}</span><span className="text-right">{t("dashboard.column.conversion_rate")}</span>
					</div>

					<ul className="pb-2">
						{rows.map((row) => {
							const filter = goalFilter(row.goal);
							const revenue = row.goal.is_revenue ? formatMoney(row.revenue, row.currency ?? row.goal.currency ?? "") : "";

							return (
								<li key={row.goal.id} className="group/row relative grid min-h-11 grid-cols-[minmax(0,1fr)_60px_60px_60px] items-center gap-2 rounded-sm sm:grid-cols-[minmax(0,1fr)_90px_90px_80px]">
									{row.unique_conversions > 0 && <Bar share={row.unique_conversions / peak} />}
									{filter && <button type="button" onClick={() => onFilter(filter, row.label)} title={t("dashboard.row.filter_by", { name: row.label })} className="absolute inset-0 rounded-sm"><span className="sr-only">{t("dashboard.row.filter_by", { name: row.label })}</span></button>}
									<span className="pointer-events-none relative min-w-0 pl-2"><span className="block truncate text-sm font-medium text-body" title={row.label}>{row.label}</span>{revenue && <span className="block truncate text-[11px] text-muted">{t("dashboard.behavior.goals.revenue", { amount: revenue })}</span>}</span>
									<NumberCell value={row.unique_conversions} /><NumberCell value={row.total_conversions} />
									<span className="tnum pointer-events-none relative text-right text-sm text-body" title={metricTitle("conversion_rate", row.conversion_rate)}>{metricAxisValue("conversion_rate", row.conversion_rate)}</span>
								</li>
							);
						})}
					</ul>
				</div>
			)}
		</PanelFrame>
	);
}

/** PropertiesPanel lets a reader choose one enabled property and applies a
 * clicked value to the whole dashboard using the canonical property dimension. */
function PropertiesPanel({ domain, request, enabled, onFilter, settingsURL, selected, onSelected }: { domain: string; request: { dateRange: DateRange; filters: Filter[]; exact: boolean }; enabled: boolean; onFilter: Props["onFilter"]; settingsURL?: string; selected: string; onSelected: (property: string) => void }) {
	const list = useRemote<Property[]>(domain, enabled, (signal) => properties(domain, signal));
	const available = list.data ?? [];
	const current = available.some((property) => property.name === selected) ? selected : (available[0]?.name ?? "");

	const report = useRemote<PropertyReport>(JSON.stringify({ domain, selected: current, request }), enabled && Boolean(current), (signal) => propertyReport(domain, current, request, signal));

	if (list.error) return <PanelFailure state={list} />;
	if (!list.data) return <PanelLoading label={t("dashboard.behavior.properties.loading")} />;
	if (list.data.length === 0) return <BehaviorEmpty title={t("dashboard.behavior.properties.empty")} body={t("dashboard.behavior.properties.empty_hint")} href={settingsURL} />;

	return (
		<PanelFrame footer={settingsURL ? <a href={settingsURL} className="text-xs font-medium text-muted transition-colors hover:text-accent">{t("dashboard.behavior.properties.manage")} →</a> : undefined}>
			<SelectorBar label={t("dashboard.behavior.properties.selector_label")} value={current} onChange={onSelected}>
				{list.data.map((property) => <option key={property.id} value={property.name}>{property.name} · {propertyScopeLabel(property.scope)}</option>)}
			</SelectorBar>
			{report.error ? <PanelFailure state={report} compact /> : !report.data ? <PanelLoading label={t("dashboard.behavior.properties.loading_values")} compact /> : report.data.rows.length === 0 ? <BehaviorEmpty title={t("dashboard.behavior.properties.no_values")} body={t("dashboard.empty.hint")} /> : <PropertyRows report={report.data} name={current} onFilter={onFilter} />}
		</PanelFrame>
	);
}

/** PropertyRows keeps the missing bucket visible but non-clickable: absence is
 * diagnostic data and not a literal value that can be sent to a query filter. */
function PropertyRows({ report, name, onFilter }: { report: PropertyReport; name: string; onFilter: Props["onFilter"] }) {
	const peak = Math.max(1, ...report.rows.map((row) => row.visitors));

	return (
		<div className="px-4 sm:px-5">
			<div className="grid h-8 grid-cols-[minmax(0,1fr)_70px_70px_70px] items-center gap-2 text-[11px] font-medium tracking-wide text-muted uppercase sm:grid-cols-[minmax(0,1fr)_100px_100px_100px]">
				<span>{name}</span><span className="text-right">{t("dashboard.column.visitors")}</span><span className="text-right">{t("dashboard.column.visits")}</span><span className="text-right">{t("dashboard.behavior.properties.events")}</span>
			</div>
			<ul className="pb-2">
				{report.rows.map((row) => (
					<li key={`${row.missing ? "missing" : "value"}:${row.value}`} className="group/row relative grid h-10 grid-cols-[minmax(0,1fr)_70px_70px_70px] items-center gap-2 rounded-sm sm:grid-cols-[minmax(0,1fr)_100px_100px_100px]">
						{row.visitors > 0 && <Bar share={row.visitors / peak} />}
						{!row.missing && <button type="button" onClick={() => onFilter({ operator: "is", dimension: `event:props:${name}`, values: [row.value] }, row.value)} title={t("dashboard.row.filter_by", { name: row.value })} className="absolute inset-0 rounded-sm"><span className="sr-only">{t("dashboard.row.filter_by", { name: row.value })}</span></button>}
						<span className={`pointer-events-none relative truncate pl-2 text-sm ${row.missing ? "italic text-muted" : "text-body"}`} title={row.value}>{row.value}</span>
						<NumberCell value={row.visitors} /><NumberCell value={row.visits} /><NumberCell value={row.events} />
					</li>
				))}
			</ul>
		</div>
	);
}

/** FunnelsPanel measures one saved funnel at a time and visualizes both the
 * surviving audience and the loss between steps without fabricating a chart. */
function FunnelsPanel({ domain, request, enabled, settingsURL, selected, onSelected, onPartial }: { domain: string; request: { dateRange: DateRange; filters: Filter[]; exact: boolean }; enabled: boolean; settingsURL?: string; selected: number; onSelected: (funnel: number) => void; onPartial: (from?: string) => void }) {
	const list = useRemote<Funnel[]>(domain, enabled, (signal) => funnels(domain, signal));
	const available = list.data ?? [];
	const current = available.some((funnel) => funnel.id === selected) ? selected : (available[0]?.id ?? 0);

	const report = useRemote<FunnelReport>(JSON.stringify({ domain, selected: current, request }), enabled && current > 0, (signal) => funnelReport(domain, current, request, signal));

	// Both requests keep whatever they last succeeded with, so the caveat is
	// withheld unless a chart is actually on screen: a failed or absent funnel
	// list means the report below it is not being rendered at all.
	const charted = Boolean(list.data?.length) && !list.error && !report.error;
	const partialFrom = charted && report.data?.partial ? report.data.from : undefined;

	useEffect(() => onPartial(partialFrom), [onPartial, partialFrom]);

	if (list.error) return <PanelFailure state={list} />;
	if (!list.data) return <PanelLoading label={t("dashboard.behavior.funnels.loading")} />;
	if (list.data.length === 0) return <BehaviorEmpty title={t("dashboard.behavior.funnels.empty")} body={t("dashboard.behavior.funnels.empty_hint")} href={settingsURL} />;

	return (
		<PanelFrame footer={settingsURL ? <a href={settingsURL} className="text-xs font-medium text-muted transition-colors hover:text-accent">{t("dashboard.behavior.funnels.manage")} →</a> : undefined}>
			<SelectorBar label={t("dashboard.behavior.funnels.selector_label")} value={String(current)} onChange={(value) => onSelected(Number(value))}>
				{list.data.map((funnel) => <option key={funnel.id} value={funnel.id}>{funnel.name}</option>)}
			</SelectorBar>
			{report.error ? <PanelFailure state={report} compact /> : !report.data ? <PanelLoading label={t("dashboard.behavior.funnels.loading_report")} compact /> : <FunnelChart report={report.data} />}
		</PanelFrame>
	);
}

/** FunnelChart uses horizontal bars because step labels and exact losses remain
 * readable on a phone; a connected chart would force them into tooltips. */
function FunnelChart({ report }: { report: FunnelReport }) {
	const first = Math.max(1, report.steps[0]?.visitors ?? 0);

	return (
		<div className="px-4 pb-3 sm:px-5">
			<div className="mb-2 flex items-center justify-between text-xs text-muted"><span>{report.funnel.strict_order ? t("dashboard.behavior.funnels.strict") : t("dashboard.behavior.funnels.sequential")}</span><span>{t("dashboard.behavior.funnels.overall", { rate: metricAxisValue("conversion_rate", report.steps.at(-1)?.conversion_rate ?? 0) })}</span></div>
			{report.steps.length === 0 ? <BehaviorEmpty title={t("dashboard.behavior.funnels.no_data")} body={t("dashboard.empty.hint")} /> : (
				<ol className="space-y-2">
					{report.steps.map((step, index) => (
						<li key={`${step.position}:${step.goal.id}`} className="relative overflow-hidden rounded-md border border-line bg-page/40 px-3 py-2.5">
							<span aria-hidden="true" className="absolute inset-y-0 left-0 bg-accent/10 transition-[width] duration-200" style={{ width: `${Math.max(0.7, (step.visitors / first) * 100)}%` }} />
							<div className="relative flex items-center gap-3"><span className="tnum flex size-6 shrink-0 items-center justify-center rounded-full bg-card text-xs font-semibold text-muted shadow-sm">{index + 1}</span><span className="min-w-0 flex-1"><span className="block truncate text-sm font-medium text-body">{step.label}</span>{index > 0 && <span className="block text-[11px] text-down">{t("dashboard.behavior.funnels.dropoff", { count: compact(step.drop_off), rate: metricAxisValue("conversion_rate", step.drop_off_rate) })}</span>}</span><span className="text-right"><span className="tnum block text-sm font-semibold text-body" title={exact(step.visitors)}><span className="sr-only">{exact(step.visitors)}</span><span aria-hidden="true">{compact(step.visitors)}</span></span><span className="tnum block text-[11px] text-muted">{metricAxisValue("conversion_rate", step.conversion_rate)}</span></span></div>
						</li>
					))}
				</ol>
			)}
		</div>
	);
}

/** ExplorePanel discovers useful page and event anchors from actual data, adds
 * configured goals, and lets each clicked result become the next step. */
function ExplorePanel({ domain, request, enabled, behavior, onBehaviorChange }: { domain: string; request: { dateRange: DateRange; filters: Filter[]; exact: boolean }; enabled: boolean; behavior: BehaviorState; onBehaviorChange: (behavior: BehaviorState) => void }) {
	const options = useRemote<AnchorOptions>(JSON.stringify({ domain, request }), enabled, (signal) => anchorOptions(domain, request, signal));
	const goals = useRemote<GoalReport>(JSON.stringify({ domain, request, goals: true }), enabled, (signal) => goalsReport(domain, request, signal));
	const allOptions = useMemo(() => [...(options.data?.pages ?? []), ...(options.data?.events ?? []), ...(goals.data?.rows ?? []).map((row) => ({ type: "goal" as const, value: String(row.goal.id), label: row.label, goal_id: row.goal.id }))], [options.data, goals.data]);
	const [search, setSearch] = useState("");
	const visibleOptions = useMemo(() => filterAnchors(allOptions, search), [allOptions, search]);
	const anchor = behavior.exploreAnchor ?? allOptions[0] ?? null;
	const direction = behavior.exploreDirection;
	const trail = behavior.exploreTrail;
	const report = useRemote<JourneyReport>(JSON.stringify({ domain, anchor, direction, grouping: behavior.exploreGrouping, trail, request }), enabled && Boolean(anchor), (signal) => journeyReport(domain, anchor as JourneyAnchor, direction, behavior.exploreGrouping, trail, request, signal));

	if (options.error) return <PanelFailure state={options} />;
	if (!options.data || !goals.data) return <PanelLoading label={t("dashboard.behavior.explore.loading")} />;
	if (allOptions.length === 0) return <BehaviorEmpty title={t("dashboard.behavior.explore.empty")} body={t("dashboard.behavior.explore.empty_hint")} />;

	/** chooseAnchor resets the path when the reader starts from the selector. */
	const chooseAnchor = (key: string) => {
		onBehaviorChange({ ...behavior, exploreAnchor: allOptions.find((item) => anchorKey(item) === key) ?? allOptions[0] ?? null, exploreTrail: [] });
	};

	/** continueTo appends the current node before following the selected result. */
	const continueTo = (next: JourneyAnchor) => {
		onBehaviorChange({
			...behavior,
			exploreAnchor: next,
			exploreTrail: anchor ? extendJourneyTrail(trail, anchor) : trail,
		});
	};

	/** rewind returns to a prior breadcrumb and discards everything after it. */
	const rewind = (index: number) => {
		const next = trail[index];
		if (!next) return;
		onBehaviorChange({ ...behavior, exploreAnchor: next, exploreTrail: trail.slice(0, index) });
	};

	return (
		<PanelFrame>
			<div className="flex flex-col gap-2 border-b border-line px-4 py-3 sm:px-5">
				<div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(160px,0.45fr)]">
					<label className="min-w-0 text-[11px] font-medium tracking-wide text-muted uppercase">{t("dashboard.behavior.explore.start")}<select value={anchor ? anchorKey(anchor) : ""} onChange={(event) => chooseAnchor(event.target.value)} className="mt-1 block h-control w-full rounded-md border border-line bg-card px-2.5 text-sm text-body">{anchor && !visibleOptions.some((item) => anchorKey(item) === anchorKey(anchor)) && <option value={anchorKey(anchor)}>{anchorLabel(anchor)}</option>}<AnchorGroup label={t("dashboard.behavior.explore.pages")} anchors={visibleOptions.filter((item) => item.type === "page")} /><AnchorGroup label={t("dashboard.behavior.explore.events")} anchors={visibleOptions.filter((item) => item.type === "event")} /><AnchorGroup label={t("dashboard.behavior.explore.goals")} anchors={visibleOptions.filter((item) => item.type === "goal")} /></select></label>
					<label className="text-[11px] font-medium tracking-wide text-muted uppercase">{t("dashboard.behavior.explore.search_label")}<input type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("dashboard.behavior.explore.search_placeholder")} className="mt-1 block h-control w-full rounded-md border border-line bg-page px-2.5 text-sm font-normal tracking-normal text-body normal-case placeholder:text-muted" /></label>
				</div>
				<div className="flex flex-wrap items-center gap-2">
					<div className="flex h-control rounded-md border border-line bg-page p-0.5" aria-label={t("dashboard.behavior.explore.direction")}>{(["backward", "forward"] as JourneyDirection[]).map((candidate) => <button key={candidate} type="button" aria-pressed={direction === candidate} onClick={() => onBehaviorChange({ ...behavior, exploreDirection: candidate, exploreTrail: [] })} className={`rounded-sm px-3 text-xs transition-colors ${direction === candidate ? "bg-card font-medium text-body shadow-sm" : "text-muted hover:text-body"}`}>{journeyDirectionLabel(candidate)}</button>)}</div>
					<div className="flex h-control rounded-md border border-line bg-page p-0.5" aria-label={t("dashboard.behavior.explore.grouping")}>{(["exact", "prefix"] as const).map((candidate) => <button key={candidate} type="button" aria-pressed={behavior.exploreGrouping === candidate} onClick={() => onBehaviorChange({ ...behavior, exploreGrouping: candidate, exploreTrail: [] })} className={`rounded-sm px-3 text-xs transition-colors ${behavior.exploreGrouping === candidate ? "bg-card font-medium text-body shadow-sm" : "text-muted hover:text-body"}`}>{journeyGroupingLabel(candidate)}</button>)}</div>
				</div>
			</div>

			{trail.length > 0 && <nav aria-label={t("dashboard.behavior.explore.trail")} className="scroll-thin flex items-center gap-1 overflow-x-auto border-b border-line px-4 py-2 text-xs sm:px-5">{trail.map((item, index) => <span key={`${anchorKey(item)}:${index}`} className="flex shrink-0 items-center gap-1"><button type="button" onClick={() => rewind(index)} className="max-w-40 truncate rounded-sm px-1.5 py-1 text-muted hover:bg-hover hover:text-body">{anchorLabel(item)}</button><span aria-hidden="true" className="text-muted">›</span></span>)}<span className="shrink-0 font-medium text-body">{anchor && anchorLabel(anchor)}</span></nav>}

			{report.error ? <PanelFailure state={report} compact /> : !report.data ? <PanelLoading label={t("dashboard.behavior.explore.loading_steps")} compact /> : report.data.steps.length === 0 ? <BehaviorEmpty title={t("dashboard.behavior.explore.no_steps")} body={t("dashboard.empty.hint")} /> : <JourneyRows report={report.data} onContinue={continueTo} />}
		</PanelFrame>
	);
}

/** AnchorGroup labels selector families without flattening their meaning. */
function AnchorGroup({ label, anchors }: { label: string; anchors: JourneyAnchor[] }) {
	if (anchors.length === 0) return null;
	return <optgroup label={label}>{anchors.map((anchor) => <option key={anchorKey(anchor)} value={anchorKey(anchor)}>{anchorLabel(anchor)}</option>)}</optgroup>;
}

/** JourneyRows exposes every non-terminal result as a keyboard next step. */
function JourneyRows({ report, onContinue }: { report: JourneyReport; onContinue: (anchor: JourneyAnchor) => void }) {
	const peak = Math.max(1, ...report.steps.map((step) => step.visitors));

	return (
		<div className="px-4 py-2 sm:px-5">
			<div className="mb-1 flex items-center justify-between text-xs text-muted"><span>{journeyHeading(report.direction, anchorLabel(report.anchor))}</span><span>{t("dashboard.behavior.explore.anchor_visitors", { count: compact(report.visitors) })}</span></div>
			<ul>{report.steps.map((step, index) => <li key={`${anchorKey(step.anchor)}:${index}`} className="group/row relative flex min-h-10 items-center rounded-sm">{step.visitors > 0 && <Bar share={step.visitors / peak} />}{!step.terminal && <button type="button" onClick={() => onContinue(step.anchor)} className="absolute inset-0 rounded-sm" title={t("dashboard.behavior.explore.continue", { name: anchorLabel(step.anchor) })}><span className="sr-only">{t("dashboard.behavior.explore.continue", { name: anchorLabel(step.anchor) })}</span></button>}<span className={`pointer-events-none relative min-w-0 flex-1 truncate pl-2 text-sm ${step.terminal ? "italic text-muted" : "font-medium text-body"}`}>{anchorLabel(step.anchor)}</span><span className="tnum pointer-events-none relative w-20 text-right text-sm text-body" title={exact(step.visitors)}><span className="sr-only">{exact(step.visitors)}</span><span aria-hidden="true">{compact(step.visitors)}</span></span><span className="pointer-events-none relative w-6 text-right text-muted" aria-hidden="true">{step.terminal ? "" : "→"}</span></li>)}</ul>
		</div>
	);
}

/** SelectorBar gives Properties and Funnels identical control treatment. */
function SelectorBar({ label, value, onChange, children }: { label: string; value: string; onChange: (value: string) => void; children: React.ReactNode }) {
	return <div className="border-b border-line px-4 py-3 sm:px-5"><label className="block max-w-sm text-[11px] font-medium tracking-wide text-muted uppercase">{label}<select value={value} onChange={(event) => onChange(event.target.value)} className="mt-1 block h-control w-full rounded-md border border-line bg-card px-2.5 text-sm text-body">{children}</select></label></div>;
}

/** PanelFrame pins optional management navigation to the bottom. */
function PanelFrame({ children, footer }: { children: React.ReactNode; footer?: React.ReactNode }) {
	return <div className="flex h-full min-h-[350px] flex-col"><div className="min-h-0 flex-1">{children}</div>{footer && <footer className="flex min-h-[42px] shrink-0 items-center border-t border-line px-4 sm:px-5">{footer}</footer>}</div>;
}

/** BehaviorEmpty serves unconfigured and zero-result states without hiding tabs. */
function BehaviorEmpty({ title, body, href }: { title: string; body: string; href?: string }) {
	return <div className="flex min-h-[300px] flex-col items-center justify-center gap-1.5 px-6 text-center"><p className="text-sm font-medium text-body">{title}</p><p className="max-w-md text-xs leading-relaxed text-muted">{body}</p>{href && <a href={href} className="mt-2 rounded-md border border-line px-3 py-1.5 text-xs font-medium text-body transition-colors hover:bg-hover">{t("dashboard.behavior.configure")}</a>}</div>;
}

/** PanelLoading preserves the tabs while one selected report is loading. */
function PanelLoading({ label, compact: compactPanel = false }: { label: string; compact?: boolean }) {
	return <div className={compactPanel ? "h-56" : "h-[350px]"}><Spinner label={label} /></div>;
}

/** PanelFailure keeps the retry local to the tab whose request failed. */
function PanelFailure<T>({ state, compact: compactPanel = false }: { state: RemoteState<T>; compact?: boolean }) {
	return <div className={compactPanel ? "h-56" : "h-[350px]"}><Failure message={state.error ?? t("dashboard.error.query_failed")} onRetry={state.reload} /></div>;
}

/** NumberCell renders an abbreviated count with its exact value on hover. */
function NumberCell({ value }: { value: number }) {
	return <span className="tnum pointer-events-none relative text-right text-sm text-body" title={exact(value)}><span className="sr-only">{exact(value)}</span><span aria-hidden="true">{compact(value)}</span></span>;
}

/** goalFilter uses the goal definition itself as the filter dimension. Rebuilding
 * it as a page or event predicate here would lose scroll thresholds, property
 * constraints, legacy event compatibility, and pageview-only semantics. */
export function goalFilter(goal: Goal): FilterState | null {
	if (!goal.id) return null;
	return { operator: "is", dimension: "event:goal", values: [String(goal.id)] };
}

/** anchorOptions reads actual pages and custom events instead of examples. */
async function anchorOptions(domain: string, request: { dateRange: DateRange; filters: Filter[]; exact: boolean }, signal: AbortSignal): Promise<AnchorOptions> {
	const base = { date_range: request.dateRange, filters: request.filters.length ? request.filters : undefined, exact: request.exact || undefined, pagination: { limit: 50 } };
	const [pages, events] = await Promise.all([
		query(domain, { ...base, metrics: ["visitors"], dimensions: ["event:page"] }, signal),
		query(domain, { ...base, metrics: ["events"], dimensions: ["event:name"] }, signal),
	]);

	return { pages: anchorsFromRows(pages, "page"), events: anchorsFromRows(events, "event").filter((anchor) => anchor.value !== "pageview" && anchor.value !== "engagement") };
}

/** anchorsFromRows preserves the query engine's server ordering. */
function anchorsFromRows(response: StatsResponse, type: "page" | "event"): JourneyAnchor[] {
	return response.results.map((row) => row.dimensions[0] ?? "").filter(Boolean).map((value) => ({ type, value, label: value }));
}

/** anchorKey is collision-free across page, event, and goal groups. */
export function anchorKey(anchor: JourneyAnchor): string {
	return `${anchor.type}:${anchor.value}`;
}

/** filterAnchors gives the compact native selector a keyboard-friendly search
 * without changing the stable server order of its results. */
export function filterAnchors(anchors: JourneyAnchor[], search: string): JourneyAnchor[] {
	const needle = search.trim().toLocaleLowerCase();
	if (!needle) return anchors;

	return anchors.filter((anchor) => anchorLabel(anchor).toLocaleLowerCase().includes(needle));
}

/** extendJourneyTrail records the node being left exactly once before the next
 * request, leaving the supplied trail immutable for React state comparisons. */
export function extendJourneyTrail(trail: JourneyAnchor[], current: JourneyAnchor): JourneyAnchor[] {
	return [...trail, current];
}

/** anchorLabel prefers the server's readable label. */
function anchorLabel(anchor: JourneyAnchor): string {
	return anchor.label || anchor.value;
}

/** behaviorTabLabel keeps catalogue references static for translation audits. */
function behaviorTabLabel(tab: BehaviorTab): string {
	switch (tab) {
		case "properties": return t("dashboard.behavior.tab.properties");
		case "funnels": return t("dashboard.behavior.tab.funnels");
		case "explore": return t("dashboard.behavior.tab.explore");
		default: return t("dashboard.behavior.tab.goals");
	}
}

/** behaviorCaveat gives each analysis mode its own concise explanation, and
 * appends the reporting start date when the window reaches back past the point
 * the configuration became measurable. A shortened chart reads as a collapse
 * without it. */
export function behaviorCaveat(tab: BehaviorTab, partialFrom?: string): string[] {
	const caveat = (() => {
		switch (tab) {
			case "properties": return t("dashboard.behavior.properties.caveat");
			case "funnels": return t("dashboard.behavior.funnels.caveat");
			case "explore": return t("dashboard.behavior.explore.caveat");
			default: return t("dashboard.behavior.goals.caveat");
		}
	})();

	if (!partialFrom) return [caveat];

	return [caveat, t("dashboard.behavior.partial", { from: readableDate(partialFrom) })];
}

/** propertyScopeLabel explains whether a value describes one event or visit. */
function propertyScopeLabel(scope: Property["scope"]): string {
	return scope === "session" ? t("dashboard.behavior.properties.scope.session") : t("dashboard.behavior.properties.scope.event");
}

/** journeyDirectionLabel names the two exploration directions. */
function journeyDirectionLabel(direction: JourneyDirection): string {
	return direction === "backward" ? t("dashboard.behavior.explore.backward") : t("dashboard.behavior.explore.forward");
}

/** journeyGroupingLabel names exact paths and automatic directory grouping. */
function journeyGroupingLabel(grouping: "exact" | "prefix"): string {
	return grouping === "prefix" ? t("dashboard.behavior.explore.grouping_prefix") : t("dashboard.behavior.explore.grouping_exact");
}

/** journeyHeading builds the contextual heading without dynamic message ids. */
function journeyHeading(direction: JourneyDirection, anchor: string): string {
	return direction === "backward"
		? t("dashboard.behavior.explore.backward_from", { anchor })
		: t("dashboard.behavior.explore.forward_from", { anchor });
}

/** formatMoney renders stored minor units in the requested currency. */
function formatMoney(value: number, currency: string): string {
	if (!currency) return exact(value);
	try {
		return new Intl.NumberFormat(undefined, { style: "currency", currency, maximumFractionDigits: 2 }).format(value / 100);
	} catch {
		return `${currency} ${exact(value)}`;
	}
}

/** readableDate keeps a server timestamp short enough to read inside a
 * sentence. A timestamp it cannot parse is shown as it arrived, because a value
 * the reader can quote back to us is worth more than a tidy dash. */
function readableDate(value: string): string {
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? value : date.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}
