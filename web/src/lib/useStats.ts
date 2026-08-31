//
// useStats.ts
// One query per card, cancelled when the question changes.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef, useState } from "react";

import { QueryError, query } from "../api/client";
import type { StatsRequest, StatsResponse } from "../api/types";

export interface Stats {
	data: StatsResponse | null;
	loading: boolean;
	error: string | null;
	/** Bumping this re-runs the query without changing the question, which is
	 *  what the current-visitors poll and the error state's Retry both need. */
	reload: () => void;
}

/**
 * useStats runs one report and keeps it in step with its inputs.
 *
 * Every card owns its own call rather than sharing one batched request. That is
 * the whole reason a slow query is survivable here: an unfiltered 28-day
 * breakdown takes seconds today, and a single combined request would mean the
 * entire page waited for the slowest card on it.
 *
 * `enabled` is what makes a below-the-fold card lazy. It stays false until the
 * card is near the viewport, so the initial paint costs four requests rather
 * than eight.
 */
export function useStats(domain: string, body: StatsRequest | null, enabled = true): Stats {
	const [data, setData] = useState<StatsResponse | null>(null);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const [nonce, setNonce] = useState(0);

	// The body is a fresh object literal on every render, so it cannot be a
	// dependency directly — it would re-run the query on every keystroke
	// anywhere on the page. Its serialised form is stable and is the actual
	// question being asked.
	const key = body ? JSON.stringify(body) : "";

	// Holding the previous answer while the next one loads is what stops a card
	// collapsing to a spinner every time the date range moves. The fixed card
	// height keeps the layout still; this keeps the content still.
	const previous = useRef<StatsResponse | null>(null);

	useEffect(() => {
		if (!enabled || !domain || !key) return;

		const controller = new AbortController();
		let live = true;

		setLoading(true);
		setError(null);

		query(domain, JSON.parse(key) as StatsRequest, controller.signal)
			.then((response) => {
				if (!live) return;
				previous.current = response;
				setData(response);
				setLoading(false);
			})
			.catch((err: unknown) => {
				// An abort is this component asking a different question, not a
				// failure. Rendering it as one would flash an error on every
				// date-range change.
				if (!live || (err instanceof DOMException && err.name === "AbortError")) return;

				setError(err instanceof QueryError || err instanceof Error ? err.message : "the query failed");
				setLoading(false);
			});

		return () => {
			live = false;
			controller.abort();
		};
	}, [domain, key, enabled, nonce]);

	return {
		data,
		loading,
		error,
		reload: () => setNonce((n) => n + 1),
	};
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
