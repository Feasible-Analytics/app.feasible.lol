//
// MainGraph.tsx
// The main graph: a hand-rolled SVG line chart, 368px tall.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useLayoutEffect, useReducer, useRef, useState } from "react";

import type { Annotation, Metric, StatsResponse } from "../api/types";
import { changePercent, comparisonSeries, previousBucketLabel } from "../lib/compare";
import { bucketLong, bucketShort, metricAxisValue, metricTitle, metricValue, rangeLabel } from "../lib/format";
import { n, t } from "../lib/i18n";
import { INVERTED } from "../lib/reports";
import { ChangeChip, Failure, Spinner } from "./atoms";
import { SampledMark } from "./SampledBadge";
import { tileLabel, tileLabelLower } from "./TopStats";

/**
 * The chart is drawn by hand in SVG rather than by a charting library.
 *
 * Three of the four things this graph has to do are unusual — a dashed
 * in-progress bucket, a break in the line where a bucket has no data, and
 * colours that follow the theme tokens rather than a canvas palette — and every
 * library makes all three a fight with its own abstractions. The whole file is
 * smaller than the adapter would have been, and it renders as real DOM, which
 * is what lets the screenshots and the dark theme be exactly right.
 */

/** The plot area's margins. Left is wide enough for a duration such as 16m 40s,
 *  and bottom has room for one line of dates. */
const PAD = { top: 16, right: 14, bottom: 26, left: 60 };

/** The graph's height, from the design system. */
const HEIGHT = 368;

/** The attribute the keyboard shortcut finds a marker by. A data attribute
 *  rather than a class, because a class is a styling decision somebody will
 *  reasonably rename, and the shortcut would go with it silently. */
export const MARKER_ATTRIBUTE = "data-annotation";

/** AnnotationTooltipState separates transient pointer/focus visibility from a
 *  click or tap that deliberately pins a tooltip open. */
export interface AnnotationTooltipState {
	hovered: number | null;
	focused: number | null;
	pinned: number | null;
}

/** AnnotationTooltipAction is the closed interaction set used by markers and
 *  the document-level Escape/outside-click listeners. */
export type AnnotationTooltipAction =
	| { type: "pointer-enter"; index: number; pointerType: string }
	| { type: "pointer-leave"; index: number }
	| { type: "focus"; index: number }
	| { type: "blur"; index: number }
	| { type: "toggle"; index: number }
	| { type: "escape" }
	| { type: "outside" }
	| { type: "reset" };

/** annotationTooltipReducer keeps touch from masquerading as a permanent hover
 *  and makes click/tap, Escape, and outside dismissal deterministic. */
export function annotationTooltipReducer(
	state: AnnotationTooltipState,
	action: AnnotationTooltipAction,
): AnnotationTooltipState {
	switch (action.type) {
		case "pointer-enter":
			return action.pointerType === "touch" ? state : { ...state, hovered: action.index };
		case "pointer-leave":
			return state.hovered === action.index ? { ...state, hovered: null } : state;
		case "focus":
			return { ...state, focused: action.index };
		case "blur":
			return state.focused === action.index ? { ...state, focused: null } : state;
		case "toggle": {
			if (state.pinned !== action.index) return { ...state, pinned: action.index };

			return {
				hovered: state.hovered === action.index ? null : state.hovered,
				focused: state.focused === action.index ? null : state.focused,
				pinned: null,
			};
		}
		case "escape":
		case "outside":
		case "reset":
			return { hovered: null, focused: null, pinned: null };
	}
}

/** visibleAnnotationTooltip chooses the deliberate pin before keyboard focus
 *  and hover, so moving a pointer cannot replace a tooltip opened by tap. */
export function visibleAnnotationTooltip(state: AnnotationTooltipState): number | null {
	return state.pinned ?? state.focused ?? state.hovered;
}

interface Props {
	stats: { data: StatsResponse | null; loading: boolean; error: string | null; reload: () => void };
	metric: Metric;
	/** Whether to draw the earlier period underneath. The numbers ride on the
	 *  same rows as the current period, so this only decides whether they are
	 *  drawn, never whether they were asked for. */
	comparing: boolean;
	/** The dated notes to render as markers. Empty is the normal case and costs
	 *  one map over an empty array. */
	annotations?: Annotation[];
}

/**
 * MainGraph plots one metric over the resolved range.
 *
 * It reads `meta.time_labels` rather than the returned rows for its x axis. The
 * engine only returns buckets that had traffic, so a chart built from the rows
 * alone would silently close up an empty Tuesday and draw a week as six days.
 */
export function MainGraph({ stats, metric, comparing, annotations = [] }: Props) {
	// Zero until the wrapper has been measured. The chart is not drawn at a
	// guessed width: a default that happens to be wider than the container
	// paints a graph that runs off the side of its own card, and on a phone that
	// is most of the graph.
	const [width, setWidth] = useState(0);
	const [hover, setHover] = useState<number | null>(null);
	const [markerState, dispatchMarker] = useReducer(annotationTooltipReducer, {
		hovered: null,
		focused: null,
		pinned: null,
	});
	const wrap = useRef<HTMLDivElement>(null);

	// The chart is drawn at real pixel width rather than with a viewBox, so the
	// stroke width and the label spacing stay constant instead of scaling with
	// the container.
	useLayoutEffect(() => {
		const node = wrap.current;
		if (!node) return;

		const observer = new ResizeObserver((entries) => {
			const measured = entries[0]?.contentRect.width;
			if (measured) setWidth(measured);
		});

		observer.observe(node);

		return () => observer.disconnect();
	}, []);

	// A pointer left over an old chart would keep a tooltip pinned to a bucket
	// that no longer exists after the range changes. An open marker is cleared
	// with it: it is indexed by those same buckets.
	useEffect(() => {
		setHover(null);
		dispatchMarker({ type: "reset" });
	}, [stats.data]);

	// A pinned tooltip behaves like a small popover: tapping elsewhere or
	// pressing Escape closes it. Marker events remain responsible for hover and
	// focus so keyboard and mouse behavior do not depend on document listeners.
	useEffect(() => {
		if (markerState.pinned === null) return;

		const pointerDown = (event: PointerEvent) => {
			const target = event.target;
			if (target instanceof Element && target.closest(`[${MARKER_ATTRIBUTE}]`)) return;
			dispatchMarker({ type: "outside" });
		};
		const keyDown = (event: KeyboardEvent) => {
			if (event.key === "Escape") dispatchMarker({ type: "escape" });
		};

		document.addEventListener("pointerdown", pointerDown);
		document.addEventListener("keydown", keyDown);
		return () => {
			document.removeEventListener("pointerdown", pointerDown);
			document.removeEventListener("keydown", keyDown);
		};
	}, [markerState.pinned]);

	const data = stats.data;
	const labels = data?.meta.time_labels ?? [];
	const interval = data?.meta.interval ?? "day";
	const present = data?.meta.present_index ?? null;

	// A bucket with no row is null, not zero. The two are different facts — a
	// quiet Sunday and a Sunday the tracker was broken — and drawing a
	// confident zero for the second is how a dashboard lies with a straight
	// line.
	const values = new Map<string, number>();
	for (const row of data?.results ?? []) {
		const key = row.dimensions[0];
		if (key !== undefined) values.set(key, row.metrics[0] ?? 0);
	}

	const points = labels.map((label) => values.get(label) ?? null);

	// The overlay is indexed by the *current* period's buckets, because that is
	// how the engine matched the two: bucket three against bucket three. Its
	// dates are different by definition, which is why the tooltip names them.
	const previous = comparing ? comparisonSeries(data, 0) : [];
	const comparisonBounds = data?.meta.comparison_date_range;

	// The wrapper is rendered on every path, including the failure and the
	// loading ones. Returning early instead would mean the ref was never
	// attached on the first render, the observer never fired, and the chart drew
	// itself at whatever width it started with for the rest of the page's life.
	if (stats.error || !data || width === 0) {
		return (
			<div ref={wrap} className="relative px-1" style={{ height: HEIGHT }}>
				{stats.error ? (
					<Failure message={stats.error} onRetry={stats.reload} />
				) : (
					<Spinner label={t("dashboard.graph.loading")} />
				)}
			</div>
		);
	}

	const plotWidth = Math.max(80, width - PAD.left - PAD.right);
	const plotHeight = HEIGHT - PAD.top - PAD.bottom;

	// The overlay counts towards the axis. Scaling to the current period alone
	// would clip a previous period that was busier, and a comparison whose worse
	// half runs off the top of the chart is a comparison that flatters.
	const plotted = [...points, ...previous].filter((value): value is number => value !== null);

	const peak = Math.max(1, ...plotted);
	const ceiling = niceCeiling(peak);

	const x = (index: number) =>
		PAD.left + (labels.length <= 1 ? plotWidth / 2 : (index * plotWidth) / (labels.length - 1));
	const y = (value: number) => PAD.top + plotHeight - (value / ceiling) * plotHeight;

	// Where the plot stops and the axis begins. The markers hang off it, so it
	// is named once rather than added up at four call sites.
	const axis = PAD.top + plotHeight;

	const runs = contiguous(points);
	const ticks = [0, 0.25, 0.5, 0.75, 1].map((fraction) => Math.round(ceiling * fraction));
	const axisEvery = Math.max(1, Math.ceil(labels.length / Math.max(2, Math.floor(plotWidth / 90))));

	const hovered = hover !== null ? (points[hover] ?? null) : null;
	const hoverLabel = hover !== null ? labels[hover] : undefined;
	const hoveredEarlier = hover !== null ? (previous[hover] ?? null) : null;
	const earlierRuns = contiguous(previous);

	const markers = placeMarkers(annotations, labels, interval);
	const marker = visibleAnnotationTooltip(markerState);
	const openMarker = marker !== null ? markers[marker] : undefined;

	return (
		<div ref={wrap} className="relative px-1" style={{ height: HEIGHT }}>
			{stats.loading && (
				<span aria-hidden="true" className="spinner-grace absolute inset-x-0 top-0 z-10 h-0.5 bg-accent/40" />
			)}

			{/* The graph is a separate response from the totals above it. Its own
			    mark remains visible while stale sampled points are held during an
			    exact reload. */}
			<div className="pointer-events-none absolute top-0 left-12 z-10">
				<SampledMark sampling={data.meta.sampling} />
			</div>

			{/* A group rather than an image: an image role hides everything inside
			    it from assistive technology, and the annotation markers below are
			    buttons a screen reader has to be able to reach. */}
			<svg
				width={width}
				height={HEIGHT}
				role="group"
				aria-label={t("dashboard.graph.aria", { metric: tileLabel(metric) })}
				onPointerMove={(event) => {
					const rect = event.currentTarget.getBoundingClientRect();
					const offset = event.clientX - rect.left;
					const step = labels.length <= 1 ? plotWidth : plotWidth / (labels.length - 1);
					const index = Math.round((offset - PAD.left) / step);

					setHover(index >= 0 && index < labels.length ? index : null);
				}}
				onPointerLeave={() => setHover(null)}
			>
				<defs>
					{/* The fill fades out downwards so the area reads as weight
					    under the line rather than as a second solid shape
					    competing with the grid. */}
					<linearGradient id="fs-area" x1="0" y1="0" x2="0" y2="1">
						<stop offset="0%" stopColor="var(--fs-accent)" stopOpacity="0.22" />
						<stop offset="100%" stopColor="var(--fs-accent)" stopOpacity="0.01" />
					</linearGradient>
				</defs>

				{ticks.map((tick) => (
					<g key={tick}>
						<line
							x1={PAD.left}
							x2={width - PAD.right}
							y1={y(tick)}
							y2={y(tick)}
							stroke="var(--fs-line)"
							strokeWidth={1}
						/>
						<text
							x={PAD.left - 10}
							y={y(tick) + 4}
							textAnchor="end"
							className="tnum fill-[var(--fs-muted)] text-[11px]"
						>
							{metricAxisValue(metric, tick)}
						</text>
					</g>
				))}

				{labels.map((label, index) =>
					index % axisEvery === 0 || index === labels.length - 1 ? (
						<text
							key={label}
							x={x(index)}
							y={HEIGHT - 8}
							textAnchor={index === 0 ? "start" : index === labels.length - 1 ? "end" : "middle"}
							className="fill-[var(--fs-muted)] text-[11px]"
						>
							{bucketShort(label, interval)}
						</text>
					) : null,
				)}

				{/* The earlier period is drawn first, thin and neutral, so the
				    current period reads as the subject and the comparison as the
				    backdrop. Reversing the weight makes the chart look like two
				    series of equal standing, which is not the question anybody
				    opened it to answer. */}
				{earlierRuns.map((run) => (
					<path
						key={`earlier-${run.from}`}
						d={linePath(previous, run.from, run.to, x, y)}
						fill="none"
						stroke="var(--fs-faint)"
						strokeWidth={1.5}
						strokeDasharray="3 3"
						strokeLinecap="round"
						strokeLinejoin="round"
					/>
				))}

				{runs.map((run) => {
					// The in-progress bucket is dashed, and only that one edge
					// is. Without it the last point of every live chart looks
					// like a collapse, and somebody asks what broke this
					// morning.
					const dashFrom = present !== null && present === run.to && run.to > run.from ? run.to : -1;
					const solidTo = dashFrom > 0 ? dashFrom - 1 : run.to;

					return (
						<g key={run.from}>
							<path
								d={areaPath(points, run.from, run.to, x, y, axis)}
								fill="url(#fs-area)"
								stroke="none"
							/>
							{solidTo > run.from && (
								<path
									d={linePath(points, run.from, solidTo, x, y)}
									fill="none"
									stroke="var(--fs-accent)"
									strokeWidth={2}
									strokeLinecap="round"
									strokeLinejoin="round"
								/>
							)}
							{dashFrom > 0 && (
								<path
									d={linePath(points, dashFrom - 1, dashFrom, x, y)}
									fill="none"
									stroke="var(--fs-accent)"
									strokeWidth={2}
									strokeDasharray="4 4"
									strokeLinecap="round"
								/>
							)}
							{run.from === run.to && points[run.from] !== null && (
								<circle cx={x(run.from)} cy={y(points[run.from] as number)} r={3} fill="var(--fs-accent)" />
							)}
						</g>
					);
				})}

				{/* Annotations are drawn after both series, so a marker sits on
				    top of the comparison line rather than under it, and they
				    hang off the axis rather than off the line, so a marker never
				    covers the value it is explaining and a day with three notes
				    gets one flag rather than three overlapping ones.

				    The guide is a solid accent rule where the comparison overlay
				    is a neutral dashed one. Two dashed verticals in the same
				    chart read as the same thing said twice, and the marker is
				    the one a reader is meant to be able to pick out of it. */}
				{markers.map((entry, index) => {
					const open = marker === index;
					const tooltipID = `annotation-tooltip-${entry.index}`;

					return (
						<g
							key={entry.index}
							{...{ [MARKER_ATTRIBUTE]: "" }}
							tabIndex={0}
							role="button"
							aria-expanded={open}
							aria-controls={tooltipID}
							aria-label={n("dashboard.graph.annotations", entry.notes.length, {
								date: entry.notes[0]?.shown_on ?? "",
								body: entry.notes[0]?.body ?? "",
							})}
							className="cursor-pointer focus:outline-none"
							onPointerEnter={(event) =>
								dispatchMarker({ type: "pointer-enter", index, pointerType: event.pointerType })
							}
							onPointerLeave={() => dispatchMarker({ type: "pointer-leave", index })}
							onFocus={() => dispatchMarker({ type: "focus", index })}
							onBlur={() => dispatchMarker({ type: "blur", index })}
							onClick={(event) => {
								const closing = markerState.pinned === index;
								dispatchMarker({ type: "toggle", index });
								if (closing) event.currentTarget.blur();
							}}
							onKeyDown={(event) => {
								if (event.key === "Escape") {
									// Escape hands the marker back rather than
									// leaving a card pinned open with the pointer
									// nowhere near it. Stopping it here keeps the
									// page's own Escape from also clearing the
									// filters underneath.
									event.stopPropagation();
									dispatchMarker({ type: "escape" });
									event.currentTarget.blur();
									return;
								}
								if (event.key !== "Enter" && event.key !== " ") return;

								event.preventDefault();
								const closing = markerState.pinned === index;
								dispatchMarker({ type: "toggle", index });
								if (closing) event.currentTarget.blur();
							}}
						>
							<line
								x1={x(entry.index)}
								x2={x(entry.index)}
								y1={PAD.top}
								y2={axis}
								stroke="var(--fs-accent)"
								strokeWidth={open ? 1.5 : 1}
								opacity={open ? 0.75 : 0.4}
							/>

							{/* The focus ring is drawn rather than left to the
							    browser's outline: an outline on a group is a
							    rectangle around its bounding box, and this
							    group is as tall as the chart. */}
							{open && (
								<circle
									cx={x(entry.index)}
									cy={axis + 7}
									r={8.5}
									fill="none"
									stroke="var(--fs-accent)"
									strokeWidth={2}
									opacity={0.4}
								/>
							)}

							{/* The pin is filled with the card colour rather
							    than left hollow, so it stays a solid shape
							    wherever the comparison line passes behind it. */}
							<circle
								cx={x(entry.index)}
								cy={axis + 7}
								r={5}
								fill="var(--fs-card)"
								stroke="var(--fs-accent)"
								strokeWidth={2}
							/>

							<text
								x={x(entry.index)}
								y={axis + 10.5}
								textAnchor="middle"
								className="fill-[var(--fs-accent)] text-[8px] font-bold"
							>
								{entry.notes.length > 1 ? entry.notes.length : ""}
							</text>
						</g>
					);
				})}

				{hover !== null && (
					<g>
						<line
							x1={x(hover)}
							x2={x(hover)}
							y1={PAD.top}
							y2={axis}
							stroke="var(--fs-line)"
							strokeWidth={1}
						/>
						{hovered !== null && (
							<circle
								cx={x(hover)}
								cy={y(hovered)}
								r={4}
								fill="var(--fs-card)"
								stroke="var(--fs-accent)"
								strokeWidth={2}
							/>
						)}
					</g>
				)}
			</svg>

			{hover !== null && hoverLabel && !openMarker && (
				<div
					// The tooltip is HTML rather than SVG so it can use the same
					// card tokens as everything else and wrap its own text.
					className="pointer-events-none absolute z-20 w-max max-w-56 border-2 border-line bg-card px-3 py-2 pop"
					style={{
						left: Math.min(Math.max(x(hover) - 70, 4), Math.max(4, width - 160)),
						top: hovered !== null ? Math.max(4, y(hovered) - 62) : PAD.top,
					}}
				>
					<p className="text-[11px] text-muted">{bucketLong(hoverLabel, interval)}</p>

					{hovered === null ? (
						<p className="text-sm font-medium text-body">{t("dashboard.graph.no_data")}</p>
					) : (
						<p className="tnum text-sm font-semibold text-body" title={metricTitle(metric, hovered)}>
							{metricValue(metric, hovered)}{" "}
							<span className="text-xs font-normal text-muted">{tileLabelLower(metric)}</span>
						</p>
					)}

					{comparing && (
						<p className="mt-1 flex items-baseline gap-1.5 border-t border-line pt-1 text-[11px] text-muted">
							<span aria-hidden="true">{t("dashboard.graph.vs")}</span>
							{hoveredEarlier === null ? (
								<span>{t("dashboard.graph.no_data")}</span>
							) : (
								<>
									<span className="tnum font-medium text-body">
										{metricValue(metric, hoveredEarlier)}
									</span>
									{hovered !== null && (
										<ChangeChip
											change={changePercent(hovered, hoveredEarlier)}
											invert={INVERTED.has(metric)}
										/>
									)}
								</>
							)}
							{hover !== null && (
								<span className="ml-auto">
									{previousBucketLabel(comparisonBounds, interval, hover) ||
										t("dashboard.graph.earlier_period")}
								</span>
							)}
						</p>
					)}

					{present === hover && <p className="text-[11px] text-muted">{t("dashboard.graph.in_progress")}</p>}
				</div>
			)}

			{/* The note's own card sits above the value tooltip. Somebody who has
			    reached for a marker, by pointer or by Tab, has asked for this one
			    rather than for whatever the pointer happens to be over. */}
			{openMarker && (
				<div
					id={`annotation-tooltip-${openMarker.index}`}
					role="tooltip"
					className="pointer-events-none absolute z-30 w-max max-w-64 border-2 border-line bg-card px-3 py-2 pop"
					style={{
						left: Math.min(Math.max(x(openMarker.index) - 90, 4), Math.max(4, width - 200)),
						// Anchored to its own bottom rather than its top, so a
						// note of any length grows upwards and the card never
						// covers the pin that opened it.
						bottom: HEIGHT - axis + 16,
					}}
				>
					<p className="text-[11px] text-muted">{openMarker.notes[0]?.shown_on}</p>
					{openMarker.notes.map((note) => (
						<p key={note.id} className="mt-1 text-sm text-body">
							{note.body}
							{note.author_name && <span className="text-xs text-muted"> — {note.author_name}</span>}
						</p>
					))}
				</div>
			)}

			{/* The legend names the window the dashes are, because "previous
			    period" is ambiguous the moment the range is a custom one. */}
			{comparing && comparisonBounds && (
				<p className="pointer-events-none absolute top-0 right-1 flex items-center gap-1.5 text-[11px] text-muted">
					<svg width="16" height="2" aria-hidden="true" className="shrink-0">
						<line x1="0" y1="1" x2="16" y2="1" stroke="var(--fs-faint)" strokeWidth="2" strokeDasharray="3 3" />
					</svg>
					{rangeLabel(comparisonBounds)}
				</p>
			)}
		</div>
	);
}

/** Marker is one bucket that carries annotations. */
export interface Marker {
	index: number;
	notes: Annotation[];
}

/**
 * placeMarkers maps dated notes onto graph buckets.
 *
 * A note carries a local date, and a bucket carries whatever label the engine
 * emitted for the interval — a date for a daily chart, a timestamp for an
 * hourly one, a Monday for a weekly one, or a month for a yearly one. Daily
 * and hourly labels match by date prefix; monthly and weekly annotations are
 * first snapped through the same calendar bucket rule as the query engine.
 *
 * The buckets are the ones the graph actually drew, so a filtered graph places
 * its markers against the same axis as its line rather than against an axis of
 * their own.
 *
 * Notes outside the range are dropped rather than clamped to the edge. A marker
 * pinned to the first bucket for something that happened before the range began
 * is a marker that says the wrong thing.
 */
export function placeMarkers(annotations: Annotation[], labels: string[], interval: string): Marker[] {
	if (annotations.length === 0 || labels.length === 0) return [];

	const byIndex = new Map<number, Annotation[]>();

	for (const note of annotations) {
		// A monthly bucket is labelled by its month. A weekly bucket is labelled
		// by the Monday that starts it, so an arbitrary date must be snapped to
		// that Monday before matching. Every other interval starts with the
		// annotation's full date.
		let key = note.shown_on;
		if (interval === "month") key = note.shown_on.slice(0, 7);
		if (interval === "week") key = weekStart(note.shown_on);
		if (!key) continue;

		const index = labels.findIndex((label) => label.startsWith(key));
		if (index < 0) continue;

		const existing = byIndex.get(index);
		if (existing) existing.push(note);
		else byIndex.set(index, [note]);
	}

	return [...byIndex.entries()]
		.map(([index, notes]) => ({ index, notes }))
		.sort((a, b) => a.index - b.index);
}

/** weekStart returns the Monday containing a YYYY-MM-DD local calendar date.
 * UTC is only an arithmetic workspace: annotation and bucket labels are wall
 * clock dates with no timezone, and local Date parsing would move them around
 * daylight-saving changes or when tests run in another timezone. */
function weekStart(label: string): string {
	const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(label);
	if (!match) return "";

	const year = Number(match[1]);
	const month = Number(match[2]);
	const day = Number(match[3]);
	const date = new Date(Date.UTC(year, month - 1, day));

	// Reject calendar overflow such as 2026-02-31 instead of silently placing
	// it in March, which would turn corrupt annotation data into a wrong marker.
	if (date.getUTCFullYear() !== year || date.getUTCMonth() !== month - 1 || date.getUTCDate() !== day) return "";

	const offset = (date.getUTCDay() + 6) % 7;
	date.setUTCDate(date.getUTCDate() - offset);

	return `${String(date.getUTCFullYear()).padStart(4, "0")}-${String(date.getUTCMonth() + 1).padStart(2, "0")}-${String(date.getUTCDate()).padStart(2, "0")}`;
}

/** contiguous groups the buckets that have data into runs, so a gap becomes a
 *  break in the line rather than a dive to the axis. */
function contiguous(points: (number | null)[]): { from: number; to: number }[] {
	const runs: { from: number; to: number }[] = [];
	let start = -1;

	points.forEach((value, index) => {
		if (value === null) {
			if (start >= 0) runs.push({ from: start, to: index - 1 });
			start = -1;
			return;
		}

		if (start < 0) start = index;
	});

	if (start >= 0) runs.push({ from: start, to: points.length - 1 });

	return runs;
}

/** linePath renders one run of points as a polyline. */
function linePath(
	points: (number | null)[],
	from: number,
	to: number,
	x: (index: number) => number,
	y: (value: number) => number,
): string {
	const parts: string[] = [];

	for (let index = from; index <= to; index++) {
		const value = points[index];
		if (value === null || value === undefined) continue;

		parts.push(`${parts.length === 0 ? "M" : "L"}${x(index).toFixed(1)},${y(value).toFixed(1)}`);
	}

	return parts.join(" ");
}

/** areaPath closes a run down to the baseline so the fill has a bottom edge. */
function areaPath(
	points: (number | null)[],
	from: number,
	to: number,
	x: (index: number) => number,
	y: (value: number) => number,
	baseline: number,
): string {
	const line = linePath(points, from, to, x, y);
	if (!line) return "";

	return `${line} L${x(to).toFixed(1)},${baseline.toFixed(1)} L${x(from).toFixed(1)},${baseline.toFixed(1)} Z`;
}

/**
 * niceCeiling rounds the axis maximum up to a round number.
 *
 * An axis topping out at 2,543 makes every gridline an unreadable figure and
 * every comparison between two charts a mental arithmetic problem. Rounding to
 * one significant figure and a bit costs a little headroom and buys labels
 * somebody can actually read.
 */
function niceCeiling(peak: number): number {
	const magnitude = 10 ** Math.floor(Math.log10(peak));
	const scaled = peak / magnitude;
	const step = scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 4 ? 4 : scaled <= 5 ? 5 : 10;

	return Math.max(4, step * magnitude);
}
