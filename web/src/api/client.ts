//
// client.ts
// The one call every card in the dashboard makes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { t } from "../lib/i18n";
import type {
	Annotation,
	Bootstrap,
	DateRange,
	Filter,
	Funnel,
	FunnelReport,
	GoalReport,
	JourneyAnchor,
	JourneyReport,
	Property,
	PropertyReport,
	Shared,
	StatsRequest,
	StatsResponse,
} from "./types";

/** QueryError carries the server's own sentence. The endpoint answers a caller
 *  mistake with a message written for the person holding the failing request,
 *  so replacing it with "something went wrong" throws away the only useful
 *  thing in the response. */
export class QueryError extends Error {
	readonly status: number;
	readonly code: string;

	constructor(status: number, message: string, code = "") {
		super(message);
		this.name = "QueryError";
		this.status = status;
		this.code = code;
	}
}

/** The parsed bootstrap. It is read once because it cannot change without a
 *  navigation, and because every consumer of it — the router's base path, the
 *  preference store's kill switch, the chrome, the goals card — has to agree.
 *  Two reads that disagreed would mean an embed that hides its top bar and then
 *  tries to write to localStorage anyway. */
let booted: Bootstrap | undefined;

/** bootstrap returns the site list, share mode, navigation and message
 *  catalogue the server wrote into the page. App and several cards call it on
 *  every render, so it parses the blob once and hands back the same object. */
export function bootstrap(): Bootstrap {
	if (!booted) booted = readBootstrap();

	return booted;
}

/** readBootstrap parses the page's bootstrap block. A missing or unparseable
 *  blob is treated as an empty install rather than a crash: an account with no
 *  sites yet is a real state, and it should reach the empty screen rather than
 *  a white one. An absent catalogue is the same bargain — every label renders
 *  as its own id, which is visible rather than blank. */
function readBootstrap(): Bootstrap {
	const node = document.getElementById("feasible-bootstrap");

	if (!node?.textContent) return { sites: [], locale: "", messages: {} };

	try {
		const parsed = JSON.parse(node.textContent) as Partial<Bootstrap>;
		const messages = parsed.messages;

		return {
			sites: Array.isArray(parsed.sites) ? parsed.sites : [],
			locale: typeof parsed.locale === "string" ? parsed.locale : "",
			messages: messages && typeof messages === "object" && !Array.isArray(messages) ? messages : {},
			shared: parsed.shared,
			navigation: parsed.navigation,
			lock: parsed.lock,
		};
	} catch {
		return { sites: [], locale: "", messages: {} };
	}
}

/** shared returns the share mode this page was served in, or undefined on the
 *  authenticated dashboard. */
export function shared(): Shared | undefined {
	return bootstrap().shared;
}

/** annotations reads the dated notes for a site over a range.
 *
 *  A failure is an empty list rather than an error. Markers are an annotation
 *  on a graph: losing them costs context, and letting that cost the graph
 *  itself would be a strange trade. */
export async function annotations(
	domain: string,
	from: string,
	to: string,
	signal?: AbortSignal,
): Promise<Annotation[]> {
	try {
		const params = new URLSearchParams({ from, to });
		const response = await fetch(
			`/api/sites/${encodeURIComponent(domain)}/annotations?${params.toString()}`,
			{ signal },
		);

		if (!response.ok) return [];

		const body = (await response.json()) as { annotations?: Annotation[] };

		return Array.isArray(body.annotations) ? body.annotations : [];
	} catch {
		return [];
	}
}

/** strip removes undefined properties. The endpoint refuses unknown fields, and
 *  JSON.stringify drops undefined values anyway — but only at the top level of
 *  an object it can see, so a nested `include: { comparisons: undefined }`
 *  would otherwise be sent as `{}` and quietly ask for a comparison of nothing. */
function strip<T>(value: T): T {
	if (Array.isArray(value)) return value.map(strip) as unknown as T;

	if (value && typeof value === "object") {
		const out: Record<string, unknown> = {};

		for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
			if (item !== undefined) out[key] = strip(item);
		}

		return out as T;
	}

	return value;
}

/** withImportedHistory makes migration data part of every dashboard report
 * unless a caller deliberately opts out. The query API keeps imports opt-in so
 * an integration cannot change totals unexpectedly, but the dashboard is the
 * destination of an import and must show that history without requiring a
 * hidden request flag.
 *
 * Live windows need no exception here. A daily aggregate cannot describe who is
 * on the site right now, and the engine refuses that combination for every
 * caller rather than trusting each one to remember. */
function withImportedHistory(body: StatsRequest): StatsRequest {
	return {
		...body,
		include: {
			...body.include,
			imports: body.include?.imports ?? true,
		},
	};
}

/**
 * query runs one report against one site.
 *
 * Every card on the dashboard comes through here. That is deliberate: the
 * moment there are two ways to ask for a number there are two answers to "how
 * many visitors did I have", and no way to tell which one is wrong.
 */
export async function query(
	domain: string,
	body: StatsRequest,
	signal?: AbortSignal,
): Promise<StatsResponse> {
	const response = await dashboardFetch(`/api/stats/${encodeURIComponent(domain)}/query`, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(strip(withImportedHistory(body))),
		signal,
	});

	return (await response.json()) as StatsResponse;
}

/** goalsReport reads configured conversions over the dashboard's current
 * population. Date ranges and filters retain their existing JSON wire form in
 * query parameters so this read remains a GET without inventing a second
 * parser for either structure. */
export async function goalsReport(
	domain: string,
	request: { dateRange: DateRange; filters?: Filter[]; exact?: boolean },
	signal?: AbortSignal,
): Promise<GoalReport> {
	const params = new URLSearchParams({ date_range: JSON.stringify(request.dateRange) });
	if (request.filters?.length) params.set("filters", JSON.stringify(request.filters));
	if (request.exact) params.set("exact", "true");

	const response = await dashboardFetch(`/api/sites/${encodeURIComponent(domain)}/goals/report?${params.toString()}`, {
		signal,
	});

	const report = (await response.json()) as GoalReport;
	report.rows = Array.isArray(report.rows) ? report.rows : [];

	return report;
}

/** properties lists the site's deliberately enabled custom-property
 * dimensions. It is separate from their value reports so switching dashboard
 * tabs does not scan the cold event-details table merely to populate a menu. */
export async function properties(domain: string, signal?: AbortSignal): Promise<Property[]> {
	const response = await dashboardFetch(`/api/sites/${encodeURIComponent(domain)}/properties`, { signal });
	const body = (await response.json()) as { properties?: Property[] };

	return Array.isArray(body.properties) ? body.properties : [];
}

/** propertyReport reads one selected custom property's values over the same
 * population as the surrounding dashboard. */
export async function propertyReport(
	domain: string,
	name: string,
	request: DashboardReportRequest,
	signal?: AbortSignal,
): Promise<PropertyReport> {
	const params = reportParams(request);
	const response = await dashboardFetch(
		`/api/sites/${encodeURIComponent(domain)}/properties/${encodeURIComponent(name)}/report?${params.toString()}`,
		{ signal },
	);
	const report = (await response.json()) as PropertyReport;
	report.rows = Array.isArray(report.rows) ? report.rows : [];

	return report;
}

/** funnels lists reusable funnel definitions without running any of them. */
export async function funnels(domain: string, signal?: AbortSignal): Promise<Funnel[]> {
	const response = await dashboardFetch(`/api/sites/${encodeURIComponent(domain)}/funnels`, { signal });
	const body = (await response.json()) as { funnels?: Funnel[] };

	return Array.isArray(body.funnels) ? body.funnels : [];
}

/** funnelReport measures one selected funnel over the dashboard population. */
export async function funnelReport(
	domain: string,
	id: number,
	request: DashboardReportRequest,
	signal?: AbortSignal,
): Promise<FunnelReport> {
	const params = reportParams(request);
	const response = await dashboardFetch(`/api/sites/${encodeURIComponent(domain)}/funnels/${id}/report?${params.toString()}`, {
		signal,
	});
	const report = (await response.json()) as FunnelReport;
	report.steps = Array.isArray(report.steps) ? report.steps : [];

	return report;
}

/** journeyReport reads the actions immediately before and after an exact page.
 * The current page is sent separately because clicking a neighboring page is
 * how the Explore view continues through a journey. */
export async function journeyReport(
	domain: string,
	anchor: JourneyAnchor,
	direction: "forward" | "backward",
	grouping: "exact" | "prefix",
	trail: JourneyAnchor[],
	request: DashboardReportRequest,
	signal?: AbortSignal,
): Promise<JourneyReport> {
	const params = reportParams(request);
	params.set("anchor_type", anchor.type);
	params.set("anchor", anchor.value);
	params.set("direction", direction);
	params.set("trail", JSON.stringify(trail));
	params.set("grouping", grouping);
	const response = await dashboardFetch(`/api/sites/${encodeURIComponent(domain)}/journey?${params.toString()}`, { signal });
	const report = (await response.json()) as JourneyReport;
	report.steps = Array.isArray(report.steps) ? report.steps : [];
	report.trail = Array.isArray(report.trail) ? report.trail : [];
	report.anchor = report.anchor ?? anchor;
	report.direction = report.direction === "backward" ? "backward" : "forward";
	report.next_pages = Array.isArray(report.next_pages) ? report.next_pages : [];
	report.previous_pages = Array.isArray(report.previous_pages) ? report.previous_pages : [];
	report.next_events = Array.isArray(report.next_events) ? report.next_events : [];
	report.previous_events = Array.isArray(report.previous_events) ? report.previous_events : [];

	return report;
}

interface DashboardReportRequest {
	dateRange: DateRange;
	filters?: Filter[];
	timezone?: string;
	exact?: boolean;
}

/** reportParams keeps every behavior report on one query-string contract. */
function reportParams(request: DashboardReportRequest): URLSearchParams {
	const params = new URLSearchParams({ date_range: JSON.stringify(request.dateRange) });
	if (request.filters?.length) params.set("filters", JSON.stringify(request.filters));
	if (request.timezone) params.set("timezone", request.timezone);
	if (request.exact) params.set("exact", "true");

	return params;
}

interface DashboardInit {
	method?: string;
	headers?: Record<string, string>;
	body?: string;
	signal?: AbortSignal;
}

/** dashboardFetch is the one request path every dashboard read shares. It
 *  attaches the public/shared capability and turns the server's error envelope
 *  into a QueryError carrying the server's own sentence, so a 400 and a 500 are
 *  read the same way everywhere. A body that is not JSON at all means something
 *  in front of us answered — a proxy, a login redirect — and the status is then
 *  the only honest thing to say. */
async function dashboardFetch(path: string, init: DashboardInit = {}): Promise<Response> {
	const response = await fetch(path, { ...init, headers: { ...init.headers, ...capabilityHeaders() } });

	if (response.ok) return response;

	let message = t("dashboard.error.query_status", { status: response.status });
	let code = "";

	try {
		const failure = (await response.json()) as { error?: string; code?: string };
		if (failure?.error) message = failure.error;
		if (failure?.code) code = failure.code;
	} catch {
		/* Keep the status-based message. */
	}

	throw new QueryError(response.status, message, code);
}

/** capabilityHeaders carries the public/shared capability on every stats
 * request. Navigation only changes the URL beneath the stable share base, so
 * this value remains attached while filters, periods and drawers change. */
function capabilityHeaders(): Record<string, string> {
	const view = shared();
	if (!view) return {};

	if (view.mode === "share") return { "X-Feasible-Share": view.capability };
	if (view.mode === "public") return { "X-Feasible-Public": view.capability };

	return {};
}
