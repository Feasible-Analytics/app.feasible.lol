//
// format.ts
// Turning the engine's numbers and bucket labels into something readable.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Interval, Metric } from "../api/types";
import { formatterLocale, n, t } from "./i18n";

/** The month names, built once per locale. They come from Intl rather than from
 *  a table of English abbreviations, because the axis of a translated dashboard
 *  saying "Jan" is the sort of half-finished detail that makes the rest of the
 *  translation look untrustworthy. */
let months: { locale: string; names: string[] } | null = null;

/** monthName is the short form of a one-based month number. The month is
 *  formatted in UTC from a fixed year so nothing here can slide a bucket into
 *  the neighbouring month on a reader whose clock is behind the date. */
function monthName(month: number): string {
	const locale = formatterLocale();

	if (!months || months.locale !== locale) {
		const format = new Intl.DateTimeFormat(locale, { month: "short", timeZone: "UTC" });

		months = {
			locale,
			names: Array.from({ length: 12 }, (_, index) => format.format(new Date(Date.UTC(2000, index, 1)))),
		};
	}

	return months.names[month - 1] ?? t("dashboard.format.unknown_month");
}

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
		return t("dashboard.format.compact.thousand", { value: trim(scaled) });
	}

	if (abs < 1_000_000_000) return t("dashboard.format.compact.million", { value: trim(value / 1_000_000) });

	return t("dashboard.format.compact.billion", { value: trim(value / 1_000_000_000) });
}

/** trim keeps one useful decimal at every compact scale, while dropping a
 *  trailing ".0" so an exact thousand still reads as 1k rather than 1.0k. */
function trim(value: number): string {
	const rounded = value.toFixed(1);

	return rounded.endsWith(".0") ? rounded.slice(0, -2) : rounded;
}

/** exact renders the full number with thousands separators, for the tooltip
 *  behind every abbreviated figure. The separator follows the catalogue's
 *  locale rather than the browser's, so the words and the numbers on one screen
 *  cannot come from two different languages. */
export function exact(value: number): string {
	return Math.round(value).toLocaleString(formatterLocale());
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

	if (hours > 0) return t("dashboard.format.duration.hours", { hours, minutes: pad(minutes) });
	if (minutes > 0) return t("dashboard.format.duration.minutes", { minutes, seconds: pad(secs) });

	return t("dashboard.format.duration.seconds", { seconds: secs });
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

/** metricAxisValue renders graph ticks in the metric's unit while keeping
 *  ratio labels compact enough for the chart's fixed left margin. Tooltips use
 *  metricValue and metricTitle because they have room for more precision. */
export function metricAxisValue(metric: Metric, value: number): string {
	switch (metric) {
		case "bounce_rate":
		case "exit_rate":
		case "conversion_rate":
		case "scroll_depth":
			return `${trimDecimal(value)}%`;

		case "visit_duration":
		case "time_on_page":
			return duration(value);

		case "views_per_visit":
			return trimDecimal(value);

		default:
			return compact(value);
	}
}

/** trimDecimal keeps as much as two decimal places but removes zeroes that
 *  only make an axis noisier, turning 2.00 into 2 and 2.50 into 2.5. */
function trimDecimal(value: number): string {
	return value.toFixed(2).replace(/\.00$/, "").replace(/(\.\d)0$/, "$1");
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
			return n("dashboard.format.seconds", Math.round(value), { value: exact(value) });

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

	if (share > 0 && share < 1) return t("dashboard.format.percent_tiny");

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
			return t("dashboard.format.time", { hour: at.h, minute: pad(at.min) });
		case "hour":
			return t("dashboard.format.hour", { hour: at.h });
		case "month":
			return t("dashboard.format.month_short", { month: monthName(at.m), year: String(at.y).slice(2) });
		default:
			return t("dashboard.format.day_short", { day: at.d, month: monthName(at.m) });
	}
}

/** bucketLong is the tooltip label: unambiguous, because a tooltip is what
 *  somebody reads when the short form left them guessing. */
export function bucketLong(label: string, interval: Interval): string {
	const at = bucketDate(label);
	const day = t("dashboard.format.date_long", { month: monthName(at.m), day: at.d, year: at.y });

	switch (interval) {
		case "minute":
			return t("dashboard.format.date_time", { date: day, hour: at.h, minute: pad(at.min) });
		case "hour":
			return t("dashboard.format.date_hour", { date: day, hour: at.h });
		case "week":
			return t("dashboard.format.week_of", { date: day });
		case "month":
			return t("dashboard.format.month_long", { month: monthName(at.m), year: at.y });
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
	if (start === end) return calendarDate(start);

	return t("dashboard.format.range", { from: calendarDate(start), to: calendarDate(end) });
}

/** calendarDate renders an ISO calendar value without interpreting it as an
 *  instant. It is used for date controls and resolved ranges so a date in the
 *  site's timezone cannot slide backward in the reader's timezone. */
export function calendarDate(iso: string): string {
	const [y = "", m = "", d = ""] = iso.split("-");

	return t("dashboard.format.date_long", { month: monthName(Number(m)), day: Number(d), year: y });
}
