//
// MainGraph.tsx
// The main graph: a hand-rolled SVG line chart, 368px tall.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useLayoutEffect, useRef, useState } from "react";

import type { Metric, StatsResponse } from "../api/types";
import { changePercent, comparisonSeries, previousBucketLabel } from "../lib/compare";
import { bucketLong, bucketShort, compact, metricTitle, metricValue, rangeLabel } from "../lib/format";
import { t } from "../lib/i18n";
import { INVERTED } from "../lib/reports";
import { ChangeChip, Failure, Spinner } from "./atoms";
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

/** The plot area's margins. Left is wide enough for a five-character y label,
 *  bottom for one line of dates. */
const PAD = { top: 16, right: 14, bottom: 26, left: 46 };

/** The graph's height, from the design system. */
const HEIGHT = 368;

interface Props {
	stats: { data: StatsResponse | null; loading: boolean; error: string | null; reload: () => void };
	metric: Metric;
	/** Whether to draw the earlier period underneath. The numbers ride on the
	 *  same rows as the current period, so this only decides whether they are
	 *  drawn, never whether they were asked for. */
	comparing: boolean;
}

/**
 * MainGraph plots one metric over the resolved range.
 *
 * It reads `meta.time_labels` rather than the returned rows for its x axis. The
 * engine only returns buckets that had traffic, so a chart built from the rows
 * alone would silently close up an empty Tuesday and draw a week as six days.
 */
export function MainGraph({ stats, metric, comparing }: Props) {
	// Zero until the wrapper has been measured. The chart is not drawn at a
	// guessed width: a default that happens to be wider than the container
	// paints a graph that runs off the side of its own card, and on a phone that
	// is most of the graph.
	const [width, setWidth] = useState(0);
	const [hover, setHover] = useState<number | null>(null);
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
	// that no longer exists after the range changes.
	useEffect(() => setHover(null), [stats.data]);

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

	const runs = contiguous(points);
	const ticks = [0, 0.25, 0.5, 0.75, 1].map((fraction) => Math.round(ceiling * fraction));
	const axisEvery = Math.max(1, Math.ceil(labels.length / Math.max(2, Math.floor(plotWidth / 90))));

	const hovered = hover !== null ? (points[hover] ?? null) : null;
	const hoverLabel = hover !== null ? labels[hover] : undefined;
	const hoveredEarlier = hover !== null ? (previous[hover] ?? null) : null;
	const earlierRuns = contiguous(previous);

	return (
		<div ref={wrap} className="relative px-1" style={{ height: HEIGHT }}>
			{stats.loading && (
				<span aria-hidden="true" className="spinner-grace absolute inset-x-0 top-0 z-10 h-0.5 bg-accent/40" />
			)}

			<svg
				width={width}
				height={HEIGHT}
				role="img"
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
							{compact(tick)}
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
								d={areaPath(points, run.from, run.to, x, y, PAD.top + plotHeight)}
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

				{hover !== null && (
					<g>
						<line
							x1={x(hover)}
							x2={x(hover)}
							y1={PAD.top}
							y2={PAD.top + plotHeight}
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

			{hover !== null && hoverLabel && (
				<div
					// The tooltip is HTML rather than SVG so it can use the same
					// card tokens as everything else and wrap its own text.
					className="pointer-events-none absolute z-20 w-max max-w-56 rounded-md border border-line bg-card px-3 py-2 shadow-lg"
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
