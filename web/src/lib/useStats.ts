//
// useStats.ts
// Remote data for the dashboard: one cancellable request per card.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef, useState } from "react";

import { QueryError, query } from "../api/client";
import type { StatsRequest, StatsResponse } from "../api/types";
import { t } from "./i18n";

/** RemoteState is what every hook here hands back: the last answer, whether a
 *  newer one is on its way, and the failure if there was one. */
export interface RemoteState<T> {
	data: T | null;
	loading: boolean;
	error: string | null;
	/** The server's machine-readable reason for a failure, empty when it gave none. */
	errorCode: string;
	/** Bumping this re-runs the request without changing the question, which is
	 *  what the current-visitors poll and the error state's Retry both need. */
	reload: () => void;
}

export interface Stats extends RemoteState<StatsResponse> {
	/** exactFallback is true when the server refused automatic sampling because
	 *  this query needs complete membership and can be retried exactly once. */
	exactFallback: boolean;
}

/**
 * useRemote runs one request and keeps it in step with its key.
 *
 * `key` is the serialised question. Callers build request objects fresh on
 * every render, so the object itself cannot be a dependency — it would re-run
 * the request on every keystroke anywhere on the page — but its serialised form
 * is stable and is the actual question being asked.
 *
 * The previous answer is kept while the next one loads, which is what stops a
 * card collapsing to a spinner every time the date range moves. `enabled` is
 * what makes a below-the-fold card lazy: it stays false until the card is near
 * the viewport, so the initial paint costs four requests rather than eight.
 */
export function useRemote<T>(key: string, enabled: boolean, load: (signal: AbortSignal) => Promise<T>): RemoteState<T> {
	const [data, setData] = useState<T | null>(null);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const [errorCode, setErrorCode] = useState("");
	const [nonce, setNonce] = useState(0);

	// The loader is read through a ref so a fresh closure on every render does
	// not count as a new question.
	const loader = useRef(load);
	loader.current = load;

	useEffect(() => {
		if (!enabled) return;

		const controller = new AbortController();
		let live = true;

		setLoading(true);
		setError(null);
		setErrorCode("");

		loader
			.current(controller.signal)
			.then((response) => {
				if (!live) return;
				setData(response);
				setLoading(false);
			})
			.catch((err: unknown) => {
				// An abort is this component asking a different question, not a
				// failure. Rendering it as one would flash an error on every
				// date-range change.
				if (!live || (err instanceof DOMException && err.name === "AbortError")) return;

				setError(err instanceof Error ? err.message : t("dashboard.error.query_failed"));
				setErrorCode(err instanceof QueryError ? err.code : "");
				setLoading(false);
			});

		return () => {
			live = false;
			controller.abort();
		};
	}, [key, enabled, nonce]);

	return { data, loading, error, errorCode, reload: () => setNonce((n) => n + 1) };
}

/**
 * useStats runs one report and keeps it in step with its inputs.
 *
 * Every card owns its own call rather than sharing one batched request. That is
 * the whole reason a slow query is survivable here: an unfiltered 28-day
 * breakdown takes seconds today, and a single combined request would mean the
 * entire page waited for the slowest card on it.
 */
export function useStats(domain: string, body: StatsRequest | null, enabled = true): Stats {
	const key = body ? JSON.stringify(body) : "";

	const remote = useRemote<StatsResponse>(`${domain}|${key}`, enabled && domain !== "" && key !== "", (signal) =>
		query(domain, JSON.parse(key) as StatsRequest, signal),
	);

	return { ...remote, exactFallback: remote.errorCode === "sampling_requires_exact" };
}

/**
 * useNearViewport reports whether an element has come close to the screen.
 *
 * It latches on: a card that has loaded once must not unload when it scrolls
 * away, or scrolling up and down would re-run a four-second query every time.
 * The margin is generous so the request starts before the card is visible and
 * the data is usually there by the time it is.
 */
export function useNearViewport<T extends Element>(margin = "400px"): [React.RefObject<T | null>, boolean] {
	const ref = useRef<T | null>(null);
	const [near, setNear] = useState(false);

	useEffect(() => {
		const node = ref.current;
		if (!node || near) return;

		// Without IntersectionObserver everything simply loads at once, which
		// is the old behaviour rather than a broken page.
		if (typeof IntersectionObserver === "undefined") {
			setNear(true);
			return;
		}

		const observer = new IntersectionObserver(
			(entries) => {
				if (entries.some((entry) => entry.isIntersecting)) {
					setNear(true);
					observer.disconnect();
				}
			},
			{ rootMargin: margin },
		);

		observer.observe(node);

		return () => observer.disconnect();
	}, [margin, near]);

	return [ref, near];
}

/**
 * useInterval re-runs a callback on a timer, skipping ticks while the tab is
 * hidden. A background tab polling every thirty seconds for hours is a
 * measurable share of somebody's battery for a number nobody is looking at.
 */
export function useInterval(callback: () => void, ms: number): void {
	const saved = useRef(callback);
	saved.current = callback;

	useEffect(() => {
		const tick = () => {
			if (document.visibilityState === "visible") saved.current();
		};

		const id = setInterval(tick, ms);

		// Coming back to the tab should show a current number immediately
		// rather than one up to thirty seconds stale.
		const onVisible = () => {
			if (document.visibilityState === "visible") saved.current();
		};

		document.addEventListener("visibilitychange", onVisible);

		return () => {
			clearInterval(id);
			document.removeEventListener("visibilitychange", onVisible);
		};
	}, [ms]);
}
