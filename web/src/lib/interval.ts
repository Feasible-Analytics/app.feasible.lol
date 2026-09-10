//
// interval.ts
// Which bucket widths a range can sensibly be drawn at.
//
// Created: 2026-09-10
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

/**
 * The bucket widths the graph offers, widest choice first.
 *
 * "auto" is the engine's own choice from the range, which is right nearly all
 * the time. The rest exist for the times it is not: a 91-day range drawn daily
 * is noise, and the same range drawn weekly is a trend. Minute is absent
 * because it only means anything on the live view, which has its own screen.
 */
export const INTERVALS = ["auto", "hour", "day", "week", "month"] as const;

export type IntervalPref = (typeof INTERVALS)[number];

/** The label id for each width. The ids are written out rather than built from
 *  the width's name, so the catalogue's unused-string check can find them. */
export const INTERVAL_LABELS: Record<IntervalPref, string> = {
	auto: "dashboard.menu.interval.auto",
	hour: "dashboard.menu.interval.hour",
	day: "dashboard.menu.interval.day",
	week: "dashboard.menu.interval.week",
	month: "dashboard.menu.interval.month",
};

/** How long one bucket of each width is. The month is the average rather than a
 *  calendar month because this arithmetic only ever estimates how many points a
 *  graph would have to draw, and February against July never changes an answer. */
const BUCKET_MS: Record<Exclude<IntervalPref, "auto">, number> = {
	hour: 3_600_000,
	day: 86_400_000,
	week: 7 * 86_400_000,
	month: 30 * 86_400_000,
};

/** Fewer than two points is not a graph, and past roughly eight hundred the
 *  points are narrower than the line drawn through them. Offering a width
 *  outside those bounds offers a reader a worse picture and a slower query. */
const MIN_BUCKETS = 2;
const MAX_BUCKETS = 800;

/**
 * intervalChoices is which bucket widths are worth offering for a range.
 *
 * A menu that lists every width lets someone ask for a year of hourly buckets,
 * which is nine thousand points nobody can read and the slowest query the
 * engine can be given. Only widths that draw a readable graph are offered.
 *
 * An empty list means the range has exactly one sensible width, so there is no
 * choice to make and no control to show — "auto" already picks it.
 *
 * The range comes back undefined before the first response lands. Every width
 * is offered then, so a stored preference is not thrown away for the moment
 * between the first paint and the first answer.
 */
export function intervalChoices(range: string[] | undefined): IntervalPref[] {
	if (!range || range.length < 2) return [...INTERVALS];

	const start = Date.parse(range[0] ?? "");
	const end = Date.parse(range[1] ?? "");

	if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return [...INTERVALS];

	const span = end - start;

	const fits = (["hour", "day", "week", "month"] as const).filter((width) => {
		const buckets = span / BUCKET_MS[width];

		return buckets >= MIN_BUCKETS && buckets <= MAX_BUCKETS;
	});

	return fits.length > 1 ? ["auto", ...fits] : [];
}

/**
 * effectiveInterval is the width the graph is actually asked for.
 *
 * A stored preference outlives the range it was chosen for: pick hourly on a
 * week, then move to a year, and the preference now asks for nine thousand
 * buckets. A width the current range does not offer falls back to "auto"
 * without being forgotten, so the original range brings the choice back.
 */
export function effectiveInterval(interval: IntervalPref, choices: IntervalPref[]): IntervalPref {
	return choices.includes(interval) ? interval : "auto";
}

/**
 * nextInterval is what the `i` key moves to.
 *
 * It walks the offered widths rather than every width, so the key cannot land
 * on a choice the menu refuses to show.
 */
export function nextInterval(interval: IntervalPref, choices: IntervalPref[]): IntervalPref {
	if (choices.length === 0) return "auto";

	const at = choices.indexOf(effectiveInterval(interval, choices));

	return choices[(at + 1) % choices.length] ?? "auto";
}
