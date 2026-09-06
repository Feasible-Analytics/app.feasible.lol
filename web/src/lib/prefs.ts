//
// prefs.ts
// localStorage: the personal half of the dashboard's state.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useCallback, useEffect, useState } from "react";

import { shared } from "../api/client";

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
 *
 * The period and the comparison sit in both, and the rule that keeps them from
 * contradicting each other is that the URL always wins. A link that names
 * either one pins it for whoever opens it; the stored value answers only what a
 * bare /dashboard leaves open, which is what to show somebody who last chose to
 * look at today. url.ts writes both to every URL it builds so that "no opinion"
 * and "deliberately the default" can never look alike.
 */
const PREFIX = "feasible.";

/**
 * storable reports whether this page may touch localStorage at all.
 *
 * The server says no for an embedded dashboard, and that is not a nicety. In a
 * third-party iframe with storage partitioned or third-party cookies blocked —
 * Brave by default, Safari, and any browser somebody has hardened — reading
 * `localStorage` does not return null. **It throws a SecurityError**, and it
 * throws on the *property access*, before any method is called.
 *
 * The incumbent's embedded dashboard read it unguarded, the exception escaped
 * during the first render, and the entire embed showed a blank frame for every
 * one of those users, with an error only the visitor could see.
 *
 * So there are two defences here and both are deliberate. This check keeps an
 * embed from touching storage in the first place, and every access below is
 * still wrapped, because a page can be framed without the server knowing.
 */
function storable(): boolean {
	return shared()?.storage !== false;
}

/** read pulls one preference, tolerating a browser with storage switched off.
 *  A private window is a normal way to look at a dashboard and must not be a
 *  crash. */
function read(key: string): string | null {
	if (!storable()) return null;

	try {
		return localStorage.getItem(PREFIX + key);
	} catch {
		return null;
	}
}

/** write stores one preference, ignoring a quota or permission failure. Losing
 *  a remembered tab is not worth an error boundary. */
function write(key: string, value: string): void {
	if (!storable()) return;

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
/**
 * readPref is usePref's first half, for state that is resolved before React
 * runs. The URL is parsed on the way into the first render, so the remembered
 * period has to be readable there rather than from a hook.
 *
 * The allowed list is checked here for the same reason usePref checks it: a
 * value left by an older build must degrade to the default rather than travel
 * into a query.
 */
export function readPref<T extends string>(key: string, allowed: readonly T[]): T | null {
	const stored = read(key) as T | null;

	return stored && allowed.includes(stored) ? stored : null;
}

/** writePref is the matching half, for the same callers. */
export function writePref(key: string, value: string): void {
	write(key, value);
}

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

/**
 * CLOCK_COOKIE carries the browser's own hour cycle back to the server.
 *
 * Nothing in an HTTP request says what clock somebody set on their device, and
 * the negotiated language is not a stand-in for it — that is the guess this
 * whole feature exists to avoid. Only the browser can answer, so it writes the
 * answer down once and every server-rendered screen reads it afterwards.
 */
const CLOCK_COOKIE = "feasible_clock";

/** A year, matching the language cookie. A clock preference does not go stale,
 *  and this is rewritten whenever the device's answer changes. */
const CLOCK_MAX_AGE = 365 * 24 * 60 * 60;

/**
 * deviceHourCycle reads the clock this browser is set to, as "12" or "24".
 *
 * The formatter is built **with an hour component**, and that is the whole
 * trick. `Intl.DateTimeFormat()` with no options resolves no time fields, so it
 * omits `hourCycle` from resolvedOptions() entirely — the obvious one-liner
 * returns undefined in every browser and would quietly put every "match my
 * device" reader on a 24-hour clock.
 *
 * The formatted string is the fallback for an engine that still reports no
 * cycle: a locale that prints a day period is a 12-hour locale.
 */
function deviceHourCycle(): string {
	const format = new Intl.DateTimeFormat(undefined, { hour: "numeric" });
	const cycle = format.resolvedOptions().hourCycle;

	if (cycle) return cycle === "h11" || cycle === "h12" ? "12" : "24";

	return /[ap]\.?m/i.test(format.format(new Date(2020, 0, 1, 13))) ? "12" : "24";
}

/**
 * reportHourCycle tells the server which dial this device is set to.
 *
 * It runs on load rather than on a settings save because it is not a setting:
 * it is a fact about the browser, and the person whose profile says "match my
 * device" never visits a screen to confirm it.
 *
 * It is skipped entirely inside an embed. A third-party iframe writing a cookie
 * is a third-party cookie — blocked by default in most browsers and pointless
 * in the rest — and the embed has no account whose preference it could be
 * answering anyway.
 */
export function reportHourCycle(): void {
	if (!storable()) return;

	try {
		const value = deviceHourCycle();

		// Written unconditionally rather than only on a change: reading it back
		// to compare costs a parse of the whole cookie header to save a write
		// that happens once per page load, and re-writing also renews the year.
		document.cookie = `${CLOCK_COOKIE}=${value};path=/;max-age=${CLOCK_MAX_AGE};samesite=lax${
			location.protocol === "https:" ? ";secure" : ""
		}`;
	} catch {
		/* No Intl, or cookies are off. The server keeps its 24-hour default. */
	}
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
		// A share URL's theme parameter is applied by the server and wins over
		// anything stored, because the person who built the embed chose it and
		// the reader is a visitor rather than an account holder.
		const forced = shared()?.theme;
		if (forced) return forced;

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

		if (!storable()) return;

		try {
			if (next === "system") localStorage.removeItem(PREFIX + THEME_KEY);
			else localStorage.setItem(PREFIX + THEME_KEY, next);
		} catch {
			/* The choice applies to this page only. */
		}
	}, []);

	return [theme, set];
}
