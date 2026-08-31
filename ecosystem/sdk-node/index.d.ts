//
// index.d.ts
// Hand-written types. There is no TypeScript build step, so the package publishes exactly what is written here.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

/** The hosted analytics host. Self-hosters pass their own to `host`. */
export declare const DEFAULT_HOST: string;

/** The environment variable that turns the client into a recorder. */
export declare const DISABLED_ENV: string;

/** The response header carrying the reason an event was classified away. */
export declare const HEADER_DROPPED: string;

/** The request header that asks for the derived event instead of a write. */
export declare const HEADER_DEBUG: string;

/**
 * The visitor's address and User-Agent — the two values the server cannot
 * guess. Both are required on every event: a call carrying neither arrives from
 * a datacentre address with no visitor in it and the event is dropped as a bot.
 */
export interface Visitor {
	clientIp: string;
	userAgent: string;
}

/** Money attached to an event. The amount is in major units, the currency is ISO 4217. */
export interface Revenue {
	amount: number;
	currency: string;
}

/**
 * Server-side attribution overrides. A delayed or offline conversion has no
 * referrer of its own, so without these it is Direct forever.
 */
export interface Attribution {
	referrer?: string;
	utmSource?: string;
	utmMedium?: string;
	utmCampaign?: string;
	utmContent?: string;
	utmTerm?: string;
}

/** One event. `clientIp` and `userAgent` are required and validated at call time. */
export interface FeasibleEvent extends Visitor {
	name?: string;
	url: string;
	domain?: string;
	referrer?: string;
	title?: string;
	props?: Record<string, string | number | boolean>;
	revenue?: Revenue;
	interactive?: boolean;
	scrollDepth?: number;
	engagementTime?: number;
	viewportWidth?: number;
	attribution?: Attribution;
}

/** What a delivered event tells you. A drop reason is not a failure. */
export interface FeasibleResult {
	statusCode: number;
	dropReason: string;
	attempts: number;
	skipped: boolean;
}

export interface FeasibleOptions {
	/** The site identifier, exactly as the site is registered. Required. */
	domain: string;
	/** The analytics host, without a path. Defaults to DEFAULT_HOST. */
	host?: string;
	/** Milliseconds for one attempt. Defaults to 5000. */
	timeout?: number;
	/** Total attempts including the first. Defaults to 3; 1 disables retrying. */
	attempts?: number;
	baseBackoff?: number;
	maxBackoff?: number;
	/** Send nothing and record events in memory. FEASIBLE_DISABLED=1 does the same. */
	disabled?: boolean;
	/** A replacement for the built-in fetch, for tests. */
	fetch?: typeof globalThis.fetch;
}

export interface CallOptions {
	signal?: AbortSignal;
}

/** Something required was missing. `code` is one of the missing_* strings. */
export declare class FeasibleValidationError extends Error {
	readonly name: "FeasibleValidationError";
	readonly field: string;
	readonly code:
		| "missing_domain"
		| "missing_name"
		| "missing_url"
		| "missing_client_ip"
		| "missing_user_agent";
	readonly reason: string;
}

/** The server understood the request and refused it. The body is verbatim. */
export declare class FeasibleApiError extends Error {
	readonly name: "FeasibleApiError";
	readonly statusCode: number;
	readonly body: string;
	readonly attempts: number;
}

/** The request never reached a server, so nothing was counted. */
export declare class FeasibleTransportError extends Error {
	readonly name: "FeasibleTransportError";
	readonly attempt: number;
	readonly cause?: unknown;
}

export declare class FeasibleClient {
	constructor(options: FeasibleOptions);

	readonly domain: string;
	readonly endpoint: string;
	readonly disabled: boolean;

	pageview(event: FeasibleEvent, options?: CallOptions): Promise<FeasibleResult>;
	track(name: string, event: FeasibleEvent, options?: CallOptions): Promise<FeasibleResult>;
	send(event: FeasibleEvent, options?: CallOptions): Promise<FeasibleResult>;
	debug(event: FeasibleEvent, options?: CallOptions): Promise<unknown>;

	/** The events a no-op client accepted, oldest first. */
	recorded(): FeasibleEvent[];
	reset(): void;
}

export declare function createClient(options: FeasibleOptions): FeasibleClient;

/** Takes the visitor out of a Node IncomingMessage (Express, Fastify, Koa, http). */
export declare function visitorFromNodeRequest(request: unknown): Visitor;

/** Takes the visitor out of a WHATWG Request (Next.js, Remix, workers). */
export declare function visitorFromWebRequest(request: unknown): Visitor;

declare const core: {
	DEFAULT_HOST: string;
	DISABLED_ENV: string;
	HEADER_DROPPED: string;
	HEADER_DEBUG: string;
	FeasibleClient: typeof FeasibleClient;
	FeasibleValidationError: typeof FeasibleValidationError;
	FeasibleApiError: typeof FeasibleApiError;
	FeasibleTransportError: typeof FeasibleTransportError;
	createClient: typeof createClient;
	visitorFromNodeRequest: typeof visitorFromNodeRequest;
	visitorFromWebRequest: typeof visitorFromWebRequest;
};

export default core;
