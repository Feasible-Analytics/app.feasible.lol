//
// WorldMap.tsx
// The choropleth on the Locations card: inline SVG, no mapping library.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useState } from "react";

import type { Row } from "../api/types";
import type { FilterState } from "../lib/filters";
import { compact, exact } from "../lib/format";
import { n, t } from "../lib/i18n";
import { countryFlag, countryName } from "../lib/labels";
import { COUNTRY_PATHS, MAP_VIEWBOX } from "../lib/worldmap";

/**
 * The map is inline SVG with the outlines compiled into the bundle.
 *
 * A mapping library would mean either a runtime fetch or a multi-megabyte
 * topology package, and both are the opposite of what this product is: one
 * binary with nothing beside it, on a machine that may have no route to the
 * internet at all. The outlines are generated once by web/tools/worldmap.mjs
 * and committed like every other compiled asset.
 *
 * It is a second view of the Countries tab rather than a different report. The
 * same query feeds both, clicking a country builds the same filter clicking a
 * row does, and the numbers are the same numbers — a map that disagreed with
 * the table under it would be worse than no map.
 */

/** How many shades the scale has. Five is as many as somebody can tell apart in
 *  a 500px-wide map without a legend entry per step. */
const STEPS = 5;

interface Props {
	/** The country breakdown, exactly as the Countries tab receives it. */
	rows: Row[];
	onFilter: (filter: FilterState, label: string) => void;
	/** Countries already filtered on, so a filtered map still says which one. */
	selected: ReadonlySet<string>;
}

/** What the pointer or the keyboard is currently on. */
interface Hovered {
	code: string;
	visitors: number;
	/** Position within the map's own box, so the tooltip does not need to know
	 *  where on the page the card ended up. */
	x: number;
	y: number;
}

export function WorldMap({ rows, onFilter, selected }: Props) {
	const [hovered, setHovered] = useState<Hovered | null>(null);

	const visitors = new Map<string, number>();
	for (const row of rows) {
		const code = row.dimensions[0];
		if (code) visitors.set(code, row.metrics[0] ?? 0);
	}

	const peak = Math.max(1, ...visitors.values());

	// The quiet countries are drawn first and the busy ones after, which settles
	// two things at once. A focus or hover stroke is never clipped by a
	// neighbour painted over it, and Tab reaches the countries in rank order
	// rather than alphabetically — the first stop is the biggest market, not
	// whichever country happens to start with A.
	//
	// Only countries with traffic are reachable at all: filtering by one nobody
	// visited empties the dashboard, which is not somewhere a Tab key should be
	// able to land.
	const quiet = Object.keys(COUNTRY_PATHS).filter((code) => !visitors.has(code));

	const busy = [...visitors.keys()]
		.filter((code) => COUNTRY_PATHS[code])
		.sort((a, b) => (visitors.get(b) ?? 0) - (visitors.get(a) ?? 0));

	// Countries the outlines do not carry — microstates that are smaller than a
	// pixel at this scale, and the blank bucket. They are real traffic and the
	// legend says so rather than letting the map quietly under-report.
	const unmapped = visitors.size - busy.length;

	/** show anchors the tooltip inside the map box, from either a pointer
	 *  position or a focused country's own outline. */
	const show = (code: string, box: DOMRect, host: DOMRect, pointer?: { x: number; y: number }) => {
		setHovered({
			code,
			visitors: visitors.get(code) ?? 0,
			x: (pointer?.x ?? box.left + box.width / 2) - host.left,
			y: (pointer?.y ?? box.top) - host.top,
		});
	};

	return (
		<div className="relative flex h-full flex-col">
			{/* The map takes the middle of the card and the legend sits on the
			    floor, rather than the two travelling together as one block with
			    dead space above and below it.

			    A group rather than an image: an image role hides everything
			    inside it from assistive technology, and the countries are
			    buttons a screen reader has to be able to reach. */}
			<svg
				viewBox={MAP_VIEWBOX}
				className="my-auto w-full"
				role="group"
				aria-label={t("dashboard.map.aria")}
				onPointerLeave={() => setHovered(null)}
			>
				{[...quiet, ...busy].map((code) => {
					const count = visitors.get(code);
					const on = selected.has(code);
					const live = count !== undefined;

					return (
						<path
							key={code}
							d={COUNTRY_PATHS[code]}
							className={`map-country ${on ? "map-country-on" : ""}`}
							// A country with no visitors is painted from the neutral
							// token rather than from the bottom of the teal ramp: an
							// empty map and a quiet map have to be different pictures,
							// or a broken tracker looks like a slow week.
							fill={count === undefined ? "var(--fs-map-empty)" : `var(--fs-map-${bucket(count, peak)})`}
							tabIndex={live ? 0 : undefined}
							role={live ? "button" : undefined}
							aria-label={
								live
									? n("dashboard.map.country", count ?? 0, {
											country: countryName(code),
											visitors: exact(count ?? 0),
										})
									: undefined
							}
							style={live ? { cursor: "pointer" } : undefined}
							onPointerMove={(event) => {
								if (!live) return;

								const host = event.currentTarget.ownerSVGElement?.parentElement?.getBoundingClientRect();
								if (!host) return;

								show(code, event.currentTarget.getBoundingClientRect(), host, {
									x: event.clientX,
									y: event.clientY,
								});
							}}
							onFocus={(event) => {
								const host = event.currentTarget.ownerSVGElement?.parentElement?.getBoundingClientRect();
								if (!host) return;

								show(code, event.currentTarget.getBoundingClientRect(), host);
							}}
							onBlur={() => setHovered(null)}
							onClick={() => {
								if (!live) return;

								onFilter({ operator: "is", dimension: "visit:country", values: [code] }, countryName(code));
							}}
							onKeyDown={(event) => {
								if (!live) return;

								// Escape dismisses the country the keyboard is on, and
								// stops there: the page's own Escape clears every
								// filter, and closing a tooltip must not also throw
								// away the filters underneath it.
								if (event.key === "Escape") {
									event.stopPropagation();
									event.currentTarget.blur();
									return;
								}

								if (event.key !== "Enter" && event.key !== " ") return;

								event.preventDefault();
								onFilter({ operator: "is", dimension: "visit:country", values: [code] }, countryName(code));
							}}
						/>
					);
				})}
			</svg>

			{hovered && (
				<div
					className="pointer-events-none absolute z-20 w-max max-w-56 -translate-x-1/2 border-2 border-line bg-card px-2.5 py-1.5 pop"
					style={{ left: clamp(hovered.x), top: Math.max(0, hovered.y - 46) }}
				>
					<p className="flex items-center gap-1.5 text-xs text-body">
						<span aria-hidden="true">{countryFlag(hovered.code)}</span>
						{countryName(hovered.code)}
					</p>
					{/* The full figure, not the abbreviated one. A tooltip is what
					    somebody opens to find out exactly, and there is room. */}
					<p className="tnum text-sm font-semibold text-body">
						{n("dashboard.map.visitors", hovered.visitors, { visitors: exact(hovered.visitors) })}
					</p>
				</div>
			)}

			<Legend peak={peak} countries={busy.length} unmapped={unmapped} />
		</div>
	);
}

/**
 * bucket picks a shade for a count.
 *
 * The scale is logarithmic because country traffic is not remotely uniform: one
 * market is usually an order of magnitude ahead of the rest, and a linear ramp
 * paints the entire world in the palest shade and says nothing at all. A log
 * scale spreads the countries that actually differ.
 */
function bucket(count: number, peak: number): number {
	if (count <= 0) return 1;

	const share = Math.log(1 + count) / Math.log(1 + peak);

	return Math.min(STEPS, Math.max(1, Math.ceil(share * STEPS)));
}

/** clamp keeps the tooltip inside the map rather than hanging off the side of
 *  the card, where the card's own overflow would cut it in half. */
function clamp(x: number): number {
	return Math.max(60, x);
}

/**
 * Legend is what makes the shades mean something.
 *
 * It leads with the "no data" swatch rather than tucking it at the end, because
 * the first question a sparse map raises is whether the pale countries are quiet
 * or missing — and that is the one thing the colours alone cannot say.
 */
function Legend({ peak, countries, unmapped }: { peak: number; countries: number; unmapped: number }) {
	return (
		<div className="flex shrink-0 items-center gap-2 pb-1 text-[11px] text-muted">
			<span className="flex items-center gap-1">
				<Swatch fill="var(--fs-map-empty)" />
				{t("dashboard.map.no_visitors")}
			</span>

			<span aria-hidden="true" className="h-3 w-px bg-line" />

			<span className="flex items-center gap-1">
				1
				{Array.from({ length: STEPS }, (_, index) => (
					<Swatch key={index} fill={`var(--fs-map-${index + 1})`} />
				))}
				<span className="tnum">{compact(peak)}</span>
			</span>

			<span
				className="tnum ml-auto"
				title={unmapped > 0 ? n("dashboard.map.too_small_help", unmapped) : undefined}
			>
				{t("dashboard.map.on_map", { count: countries })}
				{unmapped > 0 && (
					<span className="text-faint"> · {t("dashboard.map.too_small", { count: unmapped })}</span>
				)}
			</span>
		</div>
	);
}

/** Swatch is one square of the scale. The border is what keeps the palest step
 *  visible against a white card. */
function Swatch({ fill }: { fill: string }) {
	return <span aria-hidden="true" className="size-3 border-2 border-line" style={{ background: fill }} />;
}
