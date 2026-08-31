//
// Realtime.tsx
// The live view: who is here now, and what the last half hour looked like.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Filter, Metric, StatsRequest } from "../api/types";
import { exact } from "../lib/format";
import { t } from "../lib/i18n";
import { useInterval, useStats } from "../lib/useStats";
import { InfoDot } from "./atoms";
import { MainGraph } from "./MainGraph";

/**
 * The realtime view reads raw events, never a summary.
 *
 * That is not a preference — a roll-up bucket only exists once a day is over, so
 * a summary has nothing at all to say about the last thirty minutes. The engine
 * enforces it: a minute-grained range routes straight to the raw tables, and a
 * range wholly inside today has no complete day to read from anyway.
 *
 * The two windows on this screen are both server-resolved presets, cut by one
 * clock, with the shorter wholly inside the longer. Building the shorter one
 * client-side, or by adding up the last five minute buckets of the longer, is
 * how a realtime rate ends up over 100% — and a visitor count does not add
 * across buckets in the first place, because the same person is the same id in
 * every one of them.
 */

/** How often the live numbers are re-asked. Thirty seconds is the shortest
 *  interval that still reads as live rather than as a flicker, and the poll is
 *  skipped entirely while the tab is in the background. */
const REFRESH = 30_000;

/** The metric the live graph plots. Pageviews per minute is the shape of
 *  activity: per-minute visitors on all but the busiest sites is a row of ones
 *  and twos, which shows nothing. */
const GRAPH_METRIC: Metric = "pageviews";

/**
 * Engagement pings are excluded from the current-visitor figure.
 *
 * They fire on tab blur with no navigation behind them, so counting one as
 * activity means somebody who left a tab open in the background is reported as
 * being on the site — and the live number then drifts above everything else on
 * the dashboard for a reason nobody can find.
 */
const NOT_ENGAGEMENT: Filter = ["is_not", "event:name", ["engagement"], { case_sensitive: true }];

interface Props {
	domain: string;
	/** The dashboard's filters. The live view is filtered like every other
	 *  screen, or the pill row above it would be describing a page it does not
	 *  govern. */
	filters: Filter[];
}

/**
 * Realtime replaces the tiles and the main graph while the period is live.
 *
 * The report cards below it are untouched and keep reading the same thirty
 * minutes, which turns them into "what are people looking at right now" for
 * free.
 */
export function Realtime({ domain, filters }: Props) {
	const currentBody: StatsRequest = {
		metrics: ["visitors"],
		date_range: "5m",
		filters: [...filters, NOT_ENGAGEMENT],
	};

	const windowBody: StatsRequest = {
		metrics: ["visitors", "pageviews", "events"],
		date_range: "realtime",
		filters: filters.length ? filters : undefined,
	};

	const graphBody: StatsRequest = {
		metrics: [GRAPH_METRIC],
		date_range: "realtime",
		dimensions: ["time"],
		filters: filters.length ? filters : undefined,
	};

	const current = useStats(domain, domain ? currentBody : null);
	const totals = useStats(domain, domain ? windowBody : null);
	const graph = useStats(domain, domain ? graphBody : null);

	useInterval(() => {
		current.reload();
		totals.reload();
		graph.reload();
	}, REFRESH);

	const now = current.data?.results[0]?.metrics[0] ?? null;
	const row = totals.data?.results[0]?.metrics ?? [];

	return (
		<section className="overflow-hidden rounded-md border border-line bg-card shadow-sm">
			<div className="grid grid-cols-2 border-b border-line sm:grid-cols-4">
				<Cell
					label={t("dashboard.realtime.current_visitors")}
					note={t("dashboard.realtime.last_5m")}
					value={now === null ? "—" : exact(now)}
					accent
					caveat={[t("dashboard.realtime.caveat.window"), t("dashboard.realtime.caveat.engagement")]}
				/>
				<Cell
					label={t("dashboard.column.visitors")}
					note={t("dashboard.realtime.last_30m")}
					value={exact(row[0] ?? 0)}
				/>
				<Cell
					label={t("dashboard.column.pageviews")}
					note={t("dashboard.realtime.last_30m")}
					value={exact(row[1] ?? 0)}
				/>
				<Cell
					label={t("dashboard.realtime.events")}
					note={t("dashboard.realtime.last_30m")}
					value={exact(row[2] ?? 0)}
				/>
			</div>

			<div className="p-4 sm:p-5">
				<div className="mb-1 flex items-baseline gap-2">
					<h2 className="text-sm font-semibold text-body">{t("dashboard.realtime.graph_title")}</h2>
					<span className="text-[11px] text-muted">{t("dashboard.realtime.graph_note")}</span>
				</div>

				<MainGraph stats={graph} metric={GRAPH_METRIC} comparing={false} />
			</div>
		</section>
	);
}

/** Cell is one live figure. The window is printed under every one of them
 *  because two different windows on one strip is exactly the arrangement people
 *  misread, and a label is cheaper than the support ticket. */
function Cell({
	label,
	note,
	value,
	accent = false,
	caveat,
}: {
	label: string;
	note: string;
	value: string;
	accent?: boolean;
	caveat?: string[];
}) {
	return (
		<div className="flex flex-col items-start gap-0.5 border-r border-b border-line px-5 py-4 last:border-r-0 sm:border-b-0 sm:[&:nth-child(4n)]:border-r-0">
			<span className="flex items-center gap-1.5 text-[11px] font-medium tracking-wide text-muted uppercase">
				{label}
				{caveat && <InfoDot text={caveat} />}
			</span>

			<span className={`tnum text-2xl leading-none font-semibold ${accent ? "text-accent" : "text-body"}`}>
				{value}
			</span>

			<span className="text-[11px] text-muted">{note}</span>
		</div>
	);
}
