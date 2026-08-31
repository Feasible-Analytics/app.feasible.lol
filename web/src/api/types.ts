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
	| "conversion_rate";

/** The date-range presets the engine resolves server-side. They are resolved
 *  there rather than here so the graph, the tables and an export taken a second
 *  apart all agree on which days were in the window. */
export type Preset =
	| "realtime"
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

/** A filter is the positional array form the query API already speaks. Nothing
 *  in this milestone builds one, but the request type carries it so a filter UI
 *  is a new component rather than a new request shape. */
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
	comparison?: ComparisonRow;
}

export interface Warning {
	code: string;
	warning: string;
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

/** What the server writes into the page before the bundle runs. */
export interface Bootstrap {
	sites: string[];
}
