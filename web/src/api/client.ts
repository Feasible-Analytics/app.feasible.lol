//
// client.ts
// The one call every card in the dashboard makes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { t } from "../lib/i18n";
import type { Annotation, Bootstrap, Shared, StatsRequest, StatsResponse } from "./types";

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

/** bootstrap reads the site list and the message catalogue the server wrote
 *  into the page. A missing or unparseable blob is treated as an empty install
 *  rather than a crash: an account with no sites yet is a real state, and it
 *  should reach the empty screen rather than a white one. An absent catalogue
 *  is the same bargain — every label renders as its own id, which is visible
 *  rather than blank. */
export function bootstrap(): Bootstrap {
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
		};
	} catch {
		return { sites: [], locale: "", messages: {} };
	}
}

/** The share mode this page was served in, read once at module load.
 *
 *  It is read once rather than per call because it cannot change without a
 *  navigation, and because every consumer of it — the router's base path, the
 *  preference store's kill switch, the chrome — has to agree. Two reads that
 *  disagreed would mean an embed that hides its top bar and then tries to write
 *  to localStorage anyway. */
let sharedMode: Shared | undefined;

/** shared returns the share mode, or undefined on the authenticated dashboard. */
export function shared(): Shared | undefined {
	if (sharedMode === undefined) sharedMode = bootstrap().shared;

	return sharedMode;
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
	const response = await fetch(`/api/stats/${encodeURIComponent(domain)}/query`, {
		method: "POST",
		headers: { "Content-Type": "application/json", ...capabilityHeaders() },
		body: JSON.stringify(strip(body)),
		signal,
	});

	if (!response.ok) {
		// The failure body is the same one-field shape whatever the status, so
		// the message is read the same way for a 400 and a 500. A body that is
		// not JSON at all means something in front of us answered — a proxy, a
		// login redirect — and the status is then the only honest thing to say.
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

	return (await response.json()) as StatsResponse;
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
