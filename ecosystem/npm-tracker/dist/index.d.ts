//
// index.d.ts
// Hand-written types. There is no TypeScript build step, so the package publishes exactly what is written here.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

/** Money attached to an event. The amount is in major units, the currency is ISO 4217. */
export interface Revenue {
	amount: number;
	currency: string;
}

export interface TrackOptions {
	/** Custom properties. Thirty at most; values must be a string, number or boolean. */
	props?: Record<string, string | number | boolean>;
	/** Revenue attached to this event. */
	revenue?: Revenue;
	/** Called with the server's answer and any inline drop reason. Best effort; it may never fire. */
	callback?: (response: { status: number | null; dropped?: string | null }) => void;
	/** Pass false for something the visitor did not do, so it cannot end a bounce. */
	interactive?: boolean;
	/** Override the URL this event is recorded against. */
	u?: string;
}

/**
 * What a pageview accepts. It is a narrower set than TrackOptions because the
 * script's pageview has nowhere to put revenue or the interaction flag, and a
 * type that promised them would be promising something that silently vanishes.
 */
export interface PageviewOptions {
	/** Custom properties. Thirty at most; values must be a string, number or boolean. */
	props?: Record<string, string | number | boolean>;
	/** Called with the server's answer and any inline drop reason. Best effort; it may never fire. */
	callback?: (response: { status: number | null; dropped?: string | null }) => void;
	/** Override the URL this pageview is recorded against, for an SPA route change. */
	u?: string;
	/** Override the referrer. Pass "" for a page that has none, so the document's is not used. */
	referrer?: string;
}

/** What a track() call resolves to. `sent: false` means there was no browser to send from. */
export interface TrackResult {
	sent: boolean;
	status: number | null;
	dropped: string | null;
}

export interface InitOptions {
	/** The site as registered. Required — without it nothing is recorded. */
	domain: string;
	/** The analytics host the script is served from. Defaults to the hosted service. */
	host?: string;
	/** The script path, for a site using a per-site randomised script. */
	scriptPath?: string;
	/** Path globs not to record. `*` stops at a separator, `**` crosses them. */
	exclude?: string | string[];
	/** Treat a hash change as a navigation. */
	hashRouting?: boolean;
	/** Do not send a pageview automatically; call pageview() yourself. */
	manual?: boolean;
	/** Count visits from localhost and file: URLs, which are excluded by default. */
	trackLocalhost?: boolean;
	/** A second global name for the tracker, for a site keeping an existing snippet. */
	alias?: string;
}

/** The API init() returns. On a server every method is a no-op. */
export interface Tracker {
	track(name: string, options?: TrackOptions): Promise<TrackResult>;
	pageview(options?: PageviewOptions): Promise<TrackResult>;
	enable(): boolean;
	disable(): boolean;
	isEnabled(): boolean;
}

/**
 * Loads the tracker script and installs the queue stub. Safe to call more than
 * once, and safe to call on a server: with no browser it does nothing and
 * returns a stub whose track() resolves immediately.
 */
export declare function init(options: InitOptions): Tracker;

/** Sends a custom event. Resolves on the server's answer or on a timeout. */
export declare function track(name: string, options?: TrackOptions): Promise<TrackResult>;

/** Sends a pageview. */
export declare function pageview(options?: PageviewOptions): Promise<TrackResult>;

/** Counts this browser again by clearing the opt-out. Returns whether the write happened. */
export declare function enable(): boolean;

/** Stops counting this browser by writing the opt-out the script honours. */
export declare function disable(): boolean;

/** Whether this browser is being counted. False on a server. */
export declare function isEnabled(): boolean;

declare const api: {
	init: typeof init;
	track: typeof track;
	pageview: typeof pageview;
	enable: typeof enable;
	disable: typeof disable;
	isEnabled: typeof isEnabled;
};

export default api;
