//
// TopStats.tsx
// The six tiles across the top, and the metric they hand to the graph.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Metric, StatsResponse } from "../api/types";
import { metricTitle, metricValue } from "../lib/format";
import { INVERTED } from "../lib/reports";
import { ChangeChip, Failure } from "./atoms";

/** The six metrics, in the order they appear. Order is part of the contract
 *  with the response: the engine returns metrics positionally, so this list is
 *  both what is requested and how the numbers are read back. */
export const TILE_METRICS: Metric[] = [
	"visitors",
	"visits",
	"pageviews",
	"views_per_visit",
	"bounce_rate",
	"visit_duration",
];

export const TILE_LABELS: Record<string, string> = {
	visitors: "Unique visitors",
	visits: "Total visits",
	pageviews: "Total pageviews",
	views_per_visit: "Views per visit",
	bounce_rate: "Bounce rate",
	visit_duration: "Visit duration",
};

/** Metrics the graph can draw. A ratio over a whole visit has no honest value
 *  per hour, so the three session ratios are read on a tile and not plotted. */
export const GRAPHABLE: ReadonlySet<string> = new Set(["visitors", "visits", "pageviews"]);

interface Props {
	stats: { data: StatsResponse | null; loading: boolean; error: string | null; reload: () => void };
	selected: Metric;
	onSelect: (metric: Metric) => void;
	comparing: boolean;
}

/**
 * TopStats renders the tiles and drives the graph from whichever one is picked.
 *
 * The tiles and the graph are two requests rather than one, because the tiles
 * need every metric with a comparison and the graph needs one metric bucketed
 * over time. Asking for both in one query would make the six-metric aggregate
 * wait on a time series it does not use.
 */
export function TopStats({ stats, selected, onSelect, comparing }: Props) {
	if (stats.error) {
		return (
			<div className="h-28">
				<Failure message={stats.error} onRetry={stats.reload} />
			</div>
		);
	}

	// The previous answer is held while the next one loads, so changing the date
	// range does not empty the tiles and reflow the page underneath the pointer.
	const row = stats.data?.results[0];
	const first = !row && stats.loading;

	return (
		<div className="relative grid grid-cols-2 border-b border-line sm:grid-cols-3 lg:grid-cols-6">
			{/* The first load renders the same six tiles with the value withheld
			    rather than a spinner in a differently-sized box. The labels are
			    known before the query is asked, and matching the loaded geometry
			    exactly is what stops the whole page below shifting down when the
			    numbers land. */}
			{first &&
				TILE_METRICS.map((metric) => (
					<Tile key={metric} metric={metric} active={metric === selected}>
						<span className="spinner-grace tnum text-2xl leading-none font-semibold text-muted/50" aria-hidden="true">
							—
						</span>
						<span className="sr-only">Loading {TILE_LABELS[metric]}</span>
					</Tile>
				))}

			{!first &&
				TILE_METRICS.map((metric, index) => {
					const value = row?.metrics[index] ?? 0;
					const change = row?.comparison?.change[index];
					const graphable = GRAPHABLE.has(metric);
					const active = metric === selected;

					return (
						<button
							key={metric}
							type="button"
							disabled={!graphable}
							aria-pressed={active}
							onClick={() => graphable && onSelect(metric)}
							title={
								graphable
									? `Draw ${TILE_LABELS[metric]?.toLowerCase()} on the graph`
									: `${TILE_LABELS[metric]} describes a whole visit, so it is not plotted over time`
							}
							className={[
								"group relative flex flex-col items-start gap-1 border-line px-5 py-4 text-left",
								"transition-colors duration-150 ease-[var(--ease-ui)]",
								"border-r border-b last:border-r-0 sm:[&:nth-child(3n)]:border-r-0 lg:[&:nth-child(3n)]:border-r",
								"lg:border-b-0 lg:[&:nth-child(6n)]:border-r-0",
								graphable ? "cursor-pointer hover:bg-hover" : "cursor-default",
								active ? "bg-hover" : "",
							].join(" ")}
						>
							{/* The selected tile owns the graph, and the bar under
							    it is the only thing that says so. Without it the
							    graph appears to change on its own. */}
							{active && <span aria-hidden="true" className="absolute inset-x-0 bottom-0 h-0.5 bg-accent" />}

							<span
								className={`text-[11px] font-medium tracking-wide uppercase ${active ? "text-accent" : "text-muted"}`}
							>
								{TILE_LABELS[metric]}
							</span>

							<span className="flex items-baseline gap-2">
								<span
									className="tnum text-2xl leading-none font-semibold text-body"
									title={metricTitle(metric, value)}
								>
									{metricValue(metric, value)}
								</span>

								{comparing && <ChangeChip change={change} invert={INVERTED.has(metric)} />}
							</span>
						</button>
					);
				})}

			{/* A refresh over data that is already on screen gets a hairline
			    rather than a spinner: the numbers are still true until the new
			    ones land, and blanking them would be a downgrade. */}
			{stats.loading && (
				<span aria-hidden="true" className="spinner-grace absolute inset-x-0 bottom-0 h-0.5 bg-accent/40" />
			)}
		</div>
	);
}

/** Tile is the shell the loading placeholder and the loaded figure share, so
 *  the two are the same height to the pixel and nothing below them moves when
 *  the query answers. */
function Tile({ metric, active, children }: { metric: Metric; active: boolean; children: React.ReactNode }) {
	return (
		<div
			className={[
				"relative flex flex-col items-start gap-1 border-r border-b border-line px-5 py-4 text-left",
				"last:border-r-0 sm:[&:nth-child(3n)]:border-r-0 lg:[&:nth-child(3n)]:border-r",
				"lg:border-b-0 lg:[&:nth-child(6n)]:border-r-0",
			].join(" ")}
		>
			{active && <span aria-hidden="true" className="absolute inset-x-0 bottom-0 h-0.5 bg-accent" />}

			<span className={`text-[11px] font-medium tracking-wide uppercase ${active ? "text-accent" : "text-muted"}`}>
				{TILE_LABELS[metric]}
			</span>

			<span className="flex items-baseline gap-2">{children}</span>
		</div>
	);
}
