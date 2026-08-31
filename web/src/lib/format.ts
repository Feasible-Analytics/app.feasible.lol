//
// format.ts
// Turning the engine's numbers and bucket labels into something readable.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Interval, Metric } from "../api/types";

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * compact renders a count the way a dashboard tile has room for: 1.2k, 4.5M.
 *
 * The threshold is 1000 rather than 10000 because the tile is 128px wide and a
 * six-digit number simply does not fit. The exact figure is still available —
 * every abbreviated number in the UI carries the full one as a title.
 */
export function compact(value: number): string {
	const abs = Math.abs(value);

	if (abs < 1000) return String(Math.round(value));

	if (abs < 1_000_000) {
		const scaled = value / 1000;
		return `${trim(scaled)}k`;
	}

	if (abs < 1_000_000_000) return `${trim(value / 1_000_000)}M`;

	return `${trim(value / 1_000_000_000)}B`;
}

/** trim drops a trailing ".0" so 1.0k reads as 1k. */
function trim(value: number): string {
	const rounded = Math.abs(value) < 10 ? value.toFixed(1) : String(Math.round(value));

	return rounded.endsWith(".0") ? rounded.slice(0, -2) : rounded;
}

/** exact renders the full number with thousands separators, for the tooltip
 *  behind every abbreviated figure. */
export function exact(value: number): string {
	return Math.round(value).toLocaleString();
}

/**
 * duration renders seconds as m:ss, or h:mm:ss once it passes an hour.
 *
 * Visit duration is the one metric people read as a clock rather than a number,
 * and "158" tells nobody anything that "2m 38s" does not tell them instantly.
 */
export function duration(seconds: number): string {
	const total = Math.max(0, Math.round(seconds));
	const hours = Math.floor(total / 3600);
	const minutes = Math.floor((total % 3600) / 60);
	const secs = total % 60;

	if (hours > 0) return `${hours}h ${pad(minutes)}m`;
	if (minutes > 0) return `${minutes}m ${pad(secs)}s`;

	return `${secs}s`;
}

function pad(value: number): string {
	return value < 10 ? `0${value}` : String(value);
}

/** metricValue renders one metric in its own units. Percentages, durations and
 *  ratios all arrive as bare floats, so the metric name is the only thing that
 *  says which of the three a number is. */
export function metricValue(metric: Metric, value: number): string {
	switch (metric) {
		case "bounce_rate":
		case "exit_rate":
		case "conversion_rate":
		case "scroll_depth":
			return `${Math.round(value)}%`;

		case "visit_duration":
		case "time_on_page":
			return duration(value);

		case "views_per_visit":
			return value.toFixed(2);

		default:
			return compact(value);
	}
}

/** metricTitle is the full-precision form for a tooltip. */
export function metricTitle(metric: Metric, value: number): string {
	switch (metric) {
		case "bounce_rate":
		case "exit_rate":
		case "conversion_rate":
		case "scroll_depth":
			return `${value.toFixed(1)}%`;

		case "visit_duration":
		case "time_on_page":
			return `${Math.round(value).toLocaleString()} seconds`;

		case "views_per_visit":
			return value.toFixed(2);

		default:
			return exact(value);
	}
}

/** percent renders a share of a total, used by the metric column's hover half. */
export function percent(part: number, total: number): string {
	if (!total) return "0%";

	const share = (100 * part) / total;

	if (share > 0 && share < 1) return "<1%";

	return `${Math.round(share)}%`;
}

/**
 * bucketDate reads one of the engine's bucket labels as plain calendar parts.
 *
 * The labels are wall-clock text in the site's timezone, not instants. Handing
 * them to `new Date()` would reinterpret them in the reader's timezone and slide
 * every point on the graph by the offset between the two — which is how a
 * dashboard ends up showing yesterday's traffic under today's date.
 */
export function bucketDate(label: string): { y: number; m: number; d: number; h: number; min: number } {
	const [datePart = "", timePart = ""] = label.split(" ");
	const [y = "1970", m = "01", d = "01"] = datePart.split("-");
	const [h = "0", min = "0"] = timePart.split(":");

	return { y: Number(y), m: Number(m), d: Number(d), h: Number(h), min: Number(min) };
}

/** bucketShort is the axis label: as few characters as still identify the
 *  bucket, because a crowded axis is an unread axis. */
export function bucketShort(label: string, interval: Interval): string {
	const at = bucketDate(label);

	switch (interval) {
		case "minute":
			return `${at.h}:${pad(at.min)}`;
		case "hour":
			return `${at.h}:00`;
		case "month":
			return `${MONTHS[at.m - 1] ?? "?"} ${String(at.y).slice(2)}`;
		default:
			return `${at.d} ${MONTHS[at.m - 1] ?? "?"}`;
	}
}

/** bucketLong is the tooltip label: unambiguous, because a tooltip is what
 *  somebody reads when the short form left them guessing. */
export function bucketLong(label: string, interval: Interval): string {
	const at = bucketDate(label);
	const day = `${MONTHS[at.m - 1] ?? "?"} ${at.d}, ${at.y}`;

	switch (interval) {
		case "minute":
			return `${day}, ${at.h}:${pad(at.min)}`;
		case "hour":
			return `${day}, ${at.h}:00`;
		case "week":
			return `Week of ${day}`;
		case "month":
			return `${MONTHS[at.m - 1] ?? "?"} ${at.y}`;
		default:
			return day;
	}
}

/** rangeLabel renders the resolved window the server actually used, which is
 *  what the top bar shows under a preset name. Date maths is the biggest single
 *  source of "your numbers are wrong", and showing the window removes the
 *  question. */
export function rangeLabel(bounds: string[] | undefined): string {
	if (!bounds || bounds.length !== 2) return "";

	const start = bounds[0]?.slice(0, 10) ?? "";
	const end = bounds[1]?.slice(0, 10) ?? "";

	if (!start || !end) return "";
	if (start === end) return prettyDate(start);

	return `${prettyDate(start)} – ${prettyDate(end)}`;
}

function prettyDate(iso: string): string {
	const [y = "", m = "", d = ""] = iso.split("-");

	return `${MONTHS[Number(m) - 1] ?? "?"} ${Number(d)}, ${y}`;
}
