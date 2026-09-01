//
// types.ts
// The wire shapes of POST /api/stats/:domain/query, mirrored from the Go structs.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

/** Every metric the query engine knows. Spelling one wrong is a 400 naming the
 *  field, so the union exists to turn that into a compile error instead. */
export type Metric =
	| "visitors"
	| "visits"
	| "pageviews"
	| "events"
	| "bounce_rate"
	| "visit_duration"
	| "views_per_visit"
	| "time_on_page"
	| "scroll_depth"
	| "exit_rate"
	| "conversion_rate"
	/** The numeric property aggregates, which are a family rather than fixed
	 *  names: the property is a parameter, so no union could list them. The
	 *  template literal still catches the mistakes worth catching — an
	 *  aggregate we do not have, or a dimension that is not a property. */
	| `${Aggregate}(event:props:${string})`;

/** The aggregates a numeric property metric may use. */
export type Aggregate = "sum" | "avg" | "min" | "max" | "p50" | "p75" | "p90" | "p95" | "p99";

/** The date-range presets the engine resolves server-side. They are resolved
 *  there rather than here so the graph, the tables and an export taken a second
 *  apart all agree on which days were in the window. */
export type Preset =
	| "realtime"
	/** The last five minutes: who is on the site right now. It is a preset rather
	 *  than a pair of bounds so that it and the thirty-minute graph beside it are
	 *  cut by the same clock. */
	| "5m"
	| "day"
	| "24h"
	| "7d"
	| "28d"
	| "91d"
	| "month"
	| "last_month"
	| "year"
	| "12mo"
	| "all";

/** A custom range travels as its two bounds; anything else travels as its name. */
export type DateRange = Preset | [string, string];

export type Interval = "minute" | "hour" | "day" | "week" | "month";

/** A filter is the positional array form the query API already speaks. The
 *  dashboard's own filter model lives in lib/filters.ts and is converted to this
 *  shape on the way out, so the readable form and the wire form have one
 *  translation between them rather than one per caller. */
export type Filter = [string, string, string[]] | [string, string, string[], { case_sensitive: boolean }];

export type Order = [string, "asc" | "desc"];

export interface Comparison {
	mode: "previous_period" | "year_over_year" | "custom";
	date_range?: DateRange;
}

export interface Include {
	imports?: boolean;
	bots?: boolean;
	time_labels?: boolean;
	total_rows?: boolean;
	page_titles?: boolean;
	comparisons?: Comparison;
}

/** One request. The endpoint refuses unknown fields, so an optional property
 *  here must be omitted rather than sent as undefined — see request() in
 *  client.ts, which strips them. */
export interface StatsRequest {
	metrics: Metric[];
	date_range: DateRange;
	dimensions?: string[];
	filters?: Filter[];
	order_by?: Order[];
	pagination?: { limit?: number; offset?: number };
	include?: Include;
	timezone?: string;
	sample_rate?: number;
	/** Refuses the automatic sampling a very large query would otherwise get,
	 *  and waits for the exact answer instead. */
	exact?: boolean;
}

export interface ComparisonRow {
	metrics: number[];
	/** Null where the earlier value was zero. There is no meaningful percentage
	 *  change from nothing, and rendering one would put a made-up figure on the
	 *  page. */
	change: (number | null)[];
}

export interface Row {
	metrics: number[];
	dimensions: string[];
	enrichments?: { page_title?: string };
	comparison?: ComparisonRow;
}

export interface Warning {
	code: string;
	warning: string;
}

/** Why an answer was read from part of the data rather than all of it. It is
 *  present exactly when the numbers are estimates, so its presence alone is
 *  what the badge branches on. */
export interface Sampling {
	rate: number;
	reason: "requested" | "automatic";
	/** Roughly how many repeated fact-row reads the sampled raw plan represents before applying its rate,
	 *  split by table and period. These estimates and the ceiling are absent for
	 *  a rate the caller asked for. */
	estimated_rows?: number;
	estimated_event_rows?: number;
	estimated_session_rows?: number;
	estimated_primary_rows?: number;
	estimated_comparison_rows?: number;
	expected_sampled_event_rows?: number;
	expected_sampled_session_rows?: number;
	threshold?: number;
	event_metrics: string[];
	session_metrics: string[];
	mixed_metrics: string[];
	sparse: boolean;
	zero_result: boolean;
	uncertainty: string;
	/** Additive totals are inverse-rate expanded. Direct metrics are calculated
	 *  inside selected event/session rows and remain population estimates. */
	scaled_metrics: string[];
	direct_metrics: string[];
	property_coverage?: Record<
		string,
		{
			observed_values: number;
			observed_numeric_values: number;
			estimated_values: number;
			estimated_numeric_values: number;
		}
	>;
}

export interface Meta {
	/** Every bucket in the range, including the empty ones. Without it a graph
	 *  cannot tell a quiet day from a day the tracker was broken. */
	time_labels?: string[];
	/** The bucket still filling up, or null when the range has finished. */
	present_index: number | null;
	metric_warnings?: Record<string, Warning>;
	total_rows?: number;
	interval: Interval;
	sample_rate: number;
	sampling?: Sampling;
	sources: string[];
	comparison_date_range?: string[];
}

export interface ResolvedQuery {
	site_ids: number[];
	metrics: string[];
	dimensions: string[];
	date_range: string[];
	date_range_preset: string;
	timezone: string;
}

export interface StatsResponse {
	results: Row[];
	meta: Meta;
	query: ResolvedQuery;
}

/** One configured conversion goal. The matching details remain present so a
 *  later details surface can explain what the readable label represents. */
export interface Goal {
	id: number;
	site_id: number;
	kind: "page" | "event" | "scroll";
	display_name: string;
	page_pattern?: string;
	event_name?: string;
	scroll_depth?: number;
	is_revenue: boolean;
	currency?: string;
	is_automatic: boolean;
	created_at: number;
}

/** One row in the full-width goals report. */
export interface GoalReportRow {
	goal: Goal;
	label: string;
	unique_conversions: number;
	total_conversions: number;
	converted_visitors: number;
	conversion_rate: number;
	revenue: number;
	average_revenue: number;
	revenue_per_visitor: number;
	currency?: string;
	from: string;
	partial: boolean;
}

/** The goals report plus the period totals used as its denominators. */
export interface GoalReport {
	rows: GoalReportRow[];
	visitors: number;
	visits: number;
	from: string;
	to: string;
}

/** A custom property that has been enabled for analysis. Raw event data may
 * contain other names; this list is the deliberate, scoped reporting surface. */
export interface Property {
	id: number;
	site_id: number;
	name: string;
	scope: "event" | "session";
	created_at: number;
}

/** One custom-property value and its three useful counting grains. */
export interface PropertyReportRow {
	value: string;
	missing?: boolean;
	visitors: number;
	visits: number;
	events: number;
}

/** A property breakdown over the dashboard's active population. */
export interface PropertyReport {
	property: Property;
	rows: PropertyReportRow[];
	from: string;
	to: string;
}

/** One configured step in a funnel. */
export interface FunnelDefinitionStep {
	position: number;
	goal_id: number;
	goal: Goal;
}

/** A reusable ordered conversion funnel. */
export interface Funnel {
	id: number;
	site_id: number;
	name: string;
	strict_order: boolean;
	created_at: number;
	steps: FunnelDefinitionStep[];
}

/** One measured funnel step, including losses from the preceding step. */
export interface FunnelReportStep {
	position: number;
	label: string;
	goal: Goal;
	visitors: number;
	visits: number;
	drop_off: number;
	drop_off_rate: number;
	conversion_rate: number;
}

/** Funnel progress over the dashboard's active date range and filters. */
export interface FunnelReport {
	funnel: Funnel;
	steps: FunnelReportStep[];
	from: string;
	to: string;
	partial: boolean;
}

/** One page, custom event, entrance, or exit adjacent to an explored page. */
export type JourneyAnchorType = "page" | "event" | "goal";

/** A typed node in an exploratory journey. Goal ids travel as strings because
 * selectors and URL parameters share one value representation. */
export interface JourneyAnchor {
	type: JourneyAnchorType;
	value: string;
	label?: string;
	goal_id?: number;
}

export interface JourneyStep {
	anchor: JourneyAnchor;
	terminal?: boolean;
	visitors: number;
	visits: number;
	events: number;
}

/** The immediate paths and actions surrounding one exact page. */
export interface JourneyReport {
	anchor: JourneyAnchor;
	direction: "forward" | "backward";
	trail: JourneyAnchor[];
	steps: JourneyStep[];
	/** Legacy page-neighbor fields remain during the API transition. */
	page?: string;
	next_pages: JourneyStep[];
	previous_pages: JourneyStep[];
	next_events: JourneyStep[];
	previous_events: JourneyStep[];
	views: number;
	visitors: number;
	from: string;
	to: string;
}

/** The read-only mode the dashboard runs in behind a share or public URL. */
export interface Shared {
	mode: "share" | "public";
	/** The path prefix every URL this app builds must keep. It is handed to us
	 *  rather than assumed, because a shared dashboard that drops its own
	 *  /share/<token> segment when a filter is applied produces a link that
	 *  redirects to a login and back forever — which is what the incumbent's
	 *  did. */
	base: string;
	domain: string;
	capability: string;
	embed: boolean;
	theme?: "light" | "dark" | "system";
	background?: string;
	/** Whether the front end may touch localStorage at all. It is false in an
	 *  embed, and not as a preference: in a third-party frame with storage
	 *  blocked, a storage accessor *throws* rather than returning null. */
	storage: boolean;
	segment_id?: number;
}

/** What the server writes into the page before the bundle runs. */
export interface Bootstrap {
	sites: string[];
	navigation?: Navigation;
	lock?: AccountLock;
	/** The locale the server negotiated, for Intl and for the plural rules. */
	locale: string;
	/** Every string the dashboard can ask for, already merged over English by
	 *  the server. It arrives resolved rather than as a locale to look up so the
	 *  browser needs no catalogue and no fallback rule of its own. */
	messages: Record<string, string>;
	shared?: Shared;
}

/** Server-authored authenticated destinations. They are absent from every
 * shared/public view so account identity and CSRF never cross that boundary. */
export interface Navigation {
	name: string;
	email: string;
	sites_url: string;
	site_settings_url?: string;
	conversions_url?: string;
	account_url: string;
	billing_url?: string;
	export_url?: string;
	logout_url: string;
	csrf: string;
}

/** The one account-level refusal rendered in place of every report request. */
export interface AccountLock {
	reason: "lifecycle" | "dormant" | "volume";
	error: string;
}

/** One dated note rendered as a marker on the main graph. */
export interface Annotation {
	id: number;
	site_id: number;
	/** The local date the marker sits on, as YYYY-MM-DD in the site's timezone. */
	shown_on: string;
	body: string;
	author_user_id: number;
	author_name: string;
	created_at: number;
	updated_at: number;
}
