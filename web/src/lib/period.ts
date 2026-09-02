//
// period.ts
// Stepping the date range backwards and forwards a whole period at a time.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Preset } from "../api/types";

/**
 * The arrow keys move a period by one of itself.
 *
 * The result is always an explicit pair of dates rather than another preset,
 * because "the 28 days before the last 28 days" is not a preset and pretending
 * otherwise would mean the URL said one thing and the screen showed another. An
 * explicit pair is also the only form that survives being sent to somebody
 * tomorrow: a preset re-resolves against their clock, a date does not.
 */

/** A window as two inclusive local dates. */
export interface Window {
	from: string;
	to: string;
}

/** Presets that cover a fixed number of days ending today. */
const DAY_SPANS: Partial<Record<Preset, number>> = {
	day: 1,
	"7d": 7,
	"28d": 28,
	"91d": 91,
};

/** Presets that cover whole calendar months, and how many. A year is twelve of
 *  them rather than a span in days, so stepping it lands on 1 January rather
 *  than on whatever date is 365 days back. */
const MONTH_SPANS: Partial<Record<Preset, number>> = {
	month: 1,
	last_month: 1,
	year: 12,
	"12mo": 12,
};

/**
 * windowOf renders the current period as a pair of dates, or null for a period
 * that cannot be stepped.
 *
 * All time has nothing before it, and the two live windows are about now by
 * definition — "the previous thirty minutes" is a range somebody can ask for
 * with the date picker, not a thing the live screen should silently become.
 */
export function windowOf(preset: Preset, from: string, to: string, today: string): Window | null {
	if (from && to) return { from, to };

	const days = DAY_SPANS[preset];
	if (days) return { from: addDays(today, 1 - days), to: today };

	const months = MONTH_SPANS[preset];
	if (months) {
		const anchor = preset === "last_month" ? addMonths(startOfMonth(today), -1) : startOfPeriod(preset, today);

		return { from: anchor, to: lastDayOfMonth(addMonths(anchor, months - 1)) };
	}

	return null;
}

/** startOfPeriod is where a calendar preset begins in the current period. */
function startOfPeriod(preset: Preset, today: string): string {
	if (preset === "year") return `${today.slice(0, 4)}-01-01`;
	if (preset === "12mo") return startOfMonth(addMonths(today, -11));

	return startOfMonth(today);
}

/**
 * step moves a window by one of its own length.
 *
 * A day-spanned window moves by its day count and a month-spanned one by its
 * month count, so stepping back from August lands on the whole of July rather
 * than on 2 July to 1 August. Getting that wrong is the difference between a
 * comparable number and a number that straddles two months.
 */
export function step(preset: Preset, from: string, to: string, today: string, direction: -1 | 1): Window | null {
	const current = windowOf(preset, from, to, today);
	if (!current) return null;

	const months = from && to ? 0 : (MONTH_SPANS[preset] ?? 0);

	if (months > 0) {
		const start = addMonths(current.from, months * direction);

		return { from: start, to: lastDayOfMonth(addMonths(start, months - 1)) };
	}

	const length = dayCount(current.from, current.to);

	return {
		from: addDays(current.from, length * direction),
		to: addDays(current.to, length * direction),
	};
}

/** dayCount is how many days a window covers, both ends included. */
export function dayCount(from: string, to: string): number {
	return Math.round((parse(to) - parse(from)) / 86_400_000) + 1;
}

/** addDays walks the calendar. It works in UTC because UTC has no daylight
 *  saving, so the arithmetic is pure day counting and cannot slide a date by an
 *  hour twice a year. */
export function addDays(date: string, days: number): string {
	const at = new Date(parse(date));
	at.setUTCDate(at.getUTCDate() + days);

	return render(at);
}

/** addMonths walks whole months, clamping to the end of a shorter one so that
 *  31 January minus a month is 28 February rather than 3 March. */
export function addMonths(date: string, months: number): string {
	const at = new Date(parse(date));
	const day = at.getUTCDate();

	at.setUTCDate(1);
	at.setUTCMonth(at.getUTCMonth() + months);

	const lastDay = new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 0)).getUTCDate();
	at.setUTCDate(Math.min(day, lastDay));

	return render(at);
}

/** startOfMonth is the first of the month a date falls in. */
export function startOfMonth(date: string): string {
	return `${date.slice(0, 7)}-01`;
}

/** lastDayOfMonth is the last date of the month a date falls in. */
export function lastDayOfMonth(date: string): string {
	const at = new Date(parse(date));

	return render(new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 0)));
}

/** today renders the reader's own current date, which is what a date picker and
 *  a keyboard shortcut both mean by "today". */
export function today(): string {
	const at = new Date();

	return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

/** parse reads YYYY-MM-DD as a UTC instant, the one form the arithmetic here
 *  works in. A missing part falls back to the epoch rather than to NaN, so a
 *  damaged bound produces a date the screen can show instead of "Invalid Date". */
function parse(date: string): number {
	const [year = 1970, month = 1, day = 1] = date.split("-").map(Number);

	return Date.UTC(year, month - 1, day);
}

/** render is the inverse of parse: a UTC instant back to YYYY-MM-DD. */
function render(at: Date): string {
	return `${pad(at.getUTCFullYear(), 4)}-${pad(at.getUTCMonth() + 1)}-${pad(at.getUTCDate())}`;
}

/** pad zero-fills a calendar or clock field, so that 2026-9-2 is written
 *  2026-09-02 and 5 seconds past the minute reads 0:05. Two wide is the default
 *  because every field but the year is two wide. */
export function pad(value: number, width = 2): string {
	return String(value).padStart(width, "0");
}
