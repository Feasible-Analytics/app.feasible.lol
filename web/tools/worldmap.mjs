//
// worldmap.mjs
// Turns Natural Earth country boundaries into the inline SVG paths the map card draws.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// Run it by hand when the boundaries need refreshing; the output is committed
// like every other compiled asset, so a normal build never fetches anything:
//
//   curl -sSL -o /tmp/ne110m.geojson \
//     https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_110m_admin_0_countries.geojson
//   node web/tools/worldmap.mjs /tmp/ne110m.geojson
//
// The source is Natural Earth, which is public domain and explicitly free of
// any attribution requirement. It is read here and never shipped: what ships is
// the projected, simplified path data this script writes.
//
// There is no mapping library at either end. A runtime one would mean a network
// fetch from a product whose whole promise is a single binary with nothing
// beside it, and the projection this needs — plate carrée, which is two
// divisions — is not worth a dependency.

import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const OUT = join(here, "..", "src", "lib", "worldmap.ts");

/** The drawing width. Every other number here is derived from it, so the whole
 *  map is rescaled by changing this one. */
const WIDTH = 1000;

/**
 * The latitudes the map is cut at.
 *
 * The far north is empty ocean and the far south is Antarctica, which no
 * analytics product has visitors from and which costs more path data than every
 * European country combined. Cutting there is also what gives a world map its
 * familiar proportions rather than the tall, empty box a full -90..90 plate
 * carrée produces.
 */
const LAT_TOP = 84;
const LAT_BOTTOM = -56;

/** Height that keeps a degree of latitude the same size as a degree of
 *  longitude, which is what makes this plate carrée rather than a stretch. */
const HEIGHT = Math.round((WIDTH * (LAT_TOP - LAT_BOTTOM)) / 360);

/**
 * How far a point may sit from the line between its neighbours before it is
 * dropped, in drawing units.
 *
 * This is the one number that decides what the map costs. The card draws it
 * about 500 pixels wide, so one drawing unit is half a pixel and this tolerance
 * is a shade under half a pixel of error — detail no screen can show. Halving
 * it doubles the path data for a difference nobody can see.
 *
 * Rings are simplified one at a time, so a shared border can drift by this much
 * in each direction. The hairline stroke every country carries is what covers
 * the seam; a much coarser tolerance than this would start to show through it.
 */
const TOLERANCE = 0.9;

/** Rings smaller than this in bounding-box area are dropped. A country's
 *  largest ring is always kept, so an island nation never disappears — it is
 *  the scatter of uninhabited rocks around a mainland that goes. */
const MIN_RING_AREA = 1.2;

/** Antarctica has no visitors and the largest outline on the map. */
const SKIPPED = new Set(["AQ"]);

const source = process.argv[2];

if (!source) {
	console.error("usage: node web/tools/worldmap.mjs <ne_110m_admin_0_countries.geojson>");
	process.exit(1);
}

const geojson = JSON.parse(readFileSync(source, "utf8"));

/** project turns a longitude and latitude into drawing coordinates. Plate
 *  carrée: longitude is the x axis and latitude is the y axis, both linear. */
function project([lon, lat]) {
	return [
		((lon + 180) / 360) * WIDTH,
		((LAT_TOP - lat) / (LAT_TOP - LAT_BOTTOM)) * HEIGHT,
	];
}

/**
 * simplify is Douglas–Peucker: keep the point furthest from the line between
 * the ends, recurse on both halves, and drop everything that never gets picked.
 *
 * It is written out rather than pulled in because it is fifteen lines and this
 * script is the only caller, and because a build-time dependency on a geometry
 * package is still a dependency somebody has to audit.
 */
function simplify(points, tolerance) {
	if (points.length < 3) return points;

	const keep = new Uint8Array(points.length);
	keep[0] = 1;
	keep[points.length - 1] = 1;

	const stack = [[0, points.length - 1]];

	while (stack.length > 0) {
		const [start, end] = stack.pop();
		let furthest = -1;
		let distance = tolerance;

		for (let i = start + 1; i < end; i++) {
			const away = perpendicular(points[i], points[start], points[end]);

			if (away > distance) {
				distance = away;
				furthest = i;
			}
		}

		if (furthest < 0) continue;

		keep[furthest] = 1;
		stack.push([start, furthest], [furthest, end]);
	}

	return points.filter((_, index) => keep[index] === 1);
}

/** perpendicular is the distance from a point to the segment between two
 *  others, which is what decides whether a vertex carries any shape. */
function perpendicular(point, start, end) {
	const [px, py] = point;
	const [ax, ay] = start;
	const [bx, by] = end;

	const dx = bx - ax;
	const dy = by - ay;

	if (dx === 0 && dy === 0) return Math.hypot(px - ax, py - ay);

	const along = Math.max(0, Math.min(1, ((px - ax) * dx + (py - ay) * dy) / (dx * dx + dy * dy)));

	return Math.hypot(px - (ax + along * dx), py - (ay + along * dy));
}

/** boxArea is the bounding-box area of a ring, the cheap stand-in for "is this
 *  worth drawing at all". */
function boxArea(points) {
	let minX = Infinity;
	let minY = Infinity;
	let maxX = -Infinity;
	let maxY = -Infinity;

	for (const [x, y] of points) {
		if (x < minX) minX = x;
		if (y < minY) minY = y;
		if (x > maxX) maxX = x;
		if (y > maxY) maxY = y;
	}

	return (maxX - minX) * (maxY - minY);
}

/** ringsOf flattens a feature's geometry into a list of outer rings. Holes are
 *  dropped: at this scale the only ones are enclaves, and an unfilled hole in a
 *  choropleth reads as missing data rather than as a border. */
function ringsOf(geometry) {
	if (geometry.type === "Polygon") return [geometry.coordinates[0]];

	if (geometry.type === "MultiPolygon") return geometry.coordinates.map((polygon) => polygon[0]);

	return [];
}

/** render writes one ring as a path segment. The first pair follows M and the
 *  rest are implicit linetos, which is a valid and shorter spelling than
 *  repeating L for every point on a coastline. */
function render(points) {
	const parts = [];
	let lastX = null;
	let lastY = null;

	for (const [x, y] of points) {
		const rx = Math.round(x * 10) / 10;
		const ry = Math.round(y * 10) / 10;

		// A coastline simplified to a tenth of a unit repeats points; two
		// identical vertices draw nothing and cost eight characters.
		if (rx === lastX && ry === lastY) continue;

		parts.push(`${rx} ${ry}`);
		lastX = rx;
		lastY = ry;
	}

	if (parts.length < 3) return "";

	return `M${parts.join(" ")}Z`;
}

const paths = {};
let dropped = 0;

for (const feature of geojson.features) {
	const properties = feature.properties ?? {};
	const code = properties.ISO_A2_EH ?? properties.ISO_A2 ?? "";

	// Disputed and unassigned territories carry -99 rather than a code. They
	// have no place in a report keyed by ISO codes, because nothing in the
	// pipeline can ever produce a visitor from one.
	if (!/^[A-Z]{2}$/.test(code) || SKIPPED.has(code)) {
		dropped++;
		continue;
	}

	const rings = ringsOf(feature.geometry).map((ring) => simplify(ring.map(project), TOLERANCE));

	// The largest ring is the country; everything else has to earn its bytes.
	const areas = rings.map(boxArea);
	const largest = areas.indexOf(Math.max(...areas));

	const drawn = rings
		.filter((_, index) => index === largest || areas[index] >= MIN_RING_AREA)
		.map(render)
		.filter(Boolean)
		.join("");

	if (!drawn) {
		dropped++;
		continue;
	}

	paths[code] = drawn;
}

const codes = Object.keys(paths).sort();

const body = codes.map((code) => `\t${code}: ${JSON.stringify(paths[code])},`).join("\n");

const file = `//
// worldmap.ts
// The country outlines the Locations map draws, as inline SVG path data.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// GENERATED by web/tools/worldmap.mjs from Natural Earth 1:110m country
// boundaries, which are public domain. Do not edit by hand — re-run the
// generator, which is where the projection and the simplification live.
//
// It is committed rather than fetched because the product is one binary with
// nothing beside it: a map that needed a network round trip would be a map that
// is blank on the machines this is most often run on.

/** The drawing box the paths are projected into: plate carrée, cut at 84°N and
 *  56°S so the empty Arctic and Antarctica cost nothing. */
export const MAP_VIEWBOX = "0 0 ${WIDTH} ${HEIGHT}";

/** One outline per ISO 3166-1 alpha-2 code — the same codes the geo lookup
 *  stores, so a row and its country on the map are keyed by the same string
 *  with nothing to translate between them. */
export const COUNTRY_PATHS: Record<string, string> = {
${body}
};
`;

writeFileSync(OUT, file);

console.log(
	`worldmap: ${codes.length} countries, ${(file.length / 1024).toFixed(1)} KB of source ` +
		`(${dropped} features had no ISO code or were skipped)`,
);
