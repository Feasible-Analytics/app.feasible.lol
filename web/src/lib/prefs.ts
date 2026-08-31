//
// prefs.ts
// localStorage: the personal half of the dashboard's state.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useCallback, useEffect, useState } from "react";

/**
 * The split between this file and url.ts is deliberate and is the whole state
 * model of the dashboard.
 *
 * The URL stores what a link is about: the site, the date range, the filters,
 * the comparison, the open drawer. Send that link to a colleague and they see
 * what you see.
 *
 * localStorage stores what you personally last had open: which tab of a card,
 * which metric the graph is drawing, whether you are in dark mode. Putting
 * those in the URL would mean every shared link quietly imposed your
 * preferences on whoever opened it.
 */
const PREFIX = "feasible.";

/** read pulls one preference, tolerating a browser with storage switched off.
 *  A private window is a normal way to look at a dashboard and must not be a
 *  crash. */
function read(key: string): string | null {
	try {
		return localStorage.getItem(PREFIX + key);
	} catch {
		return null;
	}
}

/** write stores one preference, ignoring a quota or permission failure. Losing
 *  a remembered tab is not worth an error boundary. */
function write(key: string, value: string): void {
	try {
		localStorage.setItem(PREFIX + key, value);
	} catch {
		/* Storage is unavailable; the preference lasts for this page only. */
	}
}

/**
 * usePref is a piece of state that survives a reload.
 *
 * The allowed list is passed in rather than trusted from storage: a value left
 * behind by an older build — a tab that no longer exists — would otherwise
 * render an empty card with no way for the user to work out why.
 */
export function usePref<T extends string>(key: string, fallback: T, allowed: readonly T[]): [T, (next: T) => void] {
	const [value, setValue] = useState<T>(() => {
		const stored = read(key) as T | null;

		return stored && allowed.includes(stored) ? stored : fallback;
	});

	const set = useCallback(
		(next: T) => {
			setValue(next);
			write(key, next);
		},
		[key],
	);

	return [value, set];
}

export type Theme = "light" | "dark" | "system";

const THEME_KEY = "theme";

/**
 * useTheme owns the `dark` class on <html>.
 *
 * The class is applied by a blocking script in index.html before the first
 * paint; this hook only keeps it in step afterwards. Doing the initial decision
 * in React instead means the page paints light and flips to dark a frame later,
 * which is the most visible defect a dark dashboard can have.
 */
export function useTheme(): [Theme, (next: Theme) => void] {
	const [theme, setTheme] = useState<Theme>(() => {
		const stored = read(THEME_KEY);

		return stored === "dark" || stored === "light" ? stored : "system";
	});

	useEffect(() => {
		const media = matchMedia("(prefers-color-scheme: dark)");

		const apply = () => {
			const dark = theme === "dark" || (theme === "system" && media.matches);
			document.documentElement.classList.toggle("dark", dark);
		};

		apply();

		// Following the system while set to "system" is the behaviour people
		// expect from an OS that switches at sunset; a page that only picks it
		// up on reload looks stuck.
		media.addEventListener("change", apply);

		return () => media.removeEventListener("change", apply);
	}, [theme]);

	const set = useCallback((next: Theme) => {
		setTheme(next);

		try {
			if (next === "system") localStorage.removeItem(PREFIX + THEME_KEY);
			else localStorage.setItem(PREFIX + THEME_KEY, next);
		} catch {
			/* The choice applies to this page only. */
		}
	}, []);

	return [theme, set];
}
