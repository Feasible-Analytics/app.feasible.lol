//
// TopBar.tsx
// The sticky bar: site, live visitors, period, theme.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef, useState } from "react";

import type { Preset } from "../api/types";
import { rangeLabel } from "../lib/format";
import { n, t } from "../lib/i18n";
import type { Theme } from "../lib/prefs";
import type { UrlState } from "../lib/url";
import { useStats } from "../lib/useStats";
import { useInterval } from "../lib/useStats";

/**
 * The period menu.
 *
 * Every entry but Yesterday and Custom is a preset the engine resolves itself,
 * because a range resolved on the client is a range the graph, the tables and
 * an export can disagree about. Yesterday has no preset, so it travels as an
 * explicit pair of dates like any other custom range.
 *
 * The names are message ids, translated where the menu is drawn. The preset
 * beside each one is a wire value the engine reads and is never translated.
 */
const PERIODS: { id: string; labelId: string; preset?: Preset }[] = [
	{ id: "day", labelId: "dashboard.topbar.period.day", preset: "day" },
	{ id: "yesterday", labelId: "dashboard.topbar.period.yesterday" },
	{ id: "realtime", labelId: "dashboard.topbar.period.realtime", preset: "realtime" },
	{ id: "24h", labelId: "dashboard.topbar.period.24h", preset: "24h" },
	{ id: "7d", labelId: "dashboard.topbar.period.7d", preset: "7d" },
	{ id: "28d", labelId: "dashboard.topbar.period.28d", preset: "28d" },
	{ id: "91d", labelId: "dashboard.topbar.period.91d", preset: "91d" },
	{ id: "month", labelId: "dashboard.topbar.period.month", preset: "month" },
	{ id: "last_month", labelId: "dashboard.topbar.period.last_month", preset: "last_month" },
	{ id: "year", labelId: "dashboard.topbar.period.year", preset: "year" },
	{ id: "12mo", labelId: "dashboard.topbar.period.12mo", preset: "12mo" },
	{ id: "all", labelId: "dashboard.topbar.period.all", preset: "all" },
];

interface Props {
	state: UrlState;
	sites: string[];
	onNavigate: (next: UrlState) => void;
	theme: Theme;
	onTheme: (next: Theme) => void;
	/** The window the server actually used, shown under a preset name. */
	resolved: string[] | undefined;
}

/**
 * TopBar is the one control surface on the page.
 *
 * Everything in it writes to the URL rather than to component state, so the
 * address bar is always a description of what is on screen — which is what
 * makes a dashboard link worth sending to somebody.
 */
export function TopBar({ state, sites, onNavigate, theme, onTheme, resolved }: Props) {
	const label = periodLabel(state);

	return (
		<header className="sticky top-0 z-30 border-b border-line bg-card/95 backdrop-blur">
			<div className="mx-auto flex max-w-shell flex-wrap items-center gap-2 px-4 py-2.5 sm:px-5">
				<a
					href="/"
					className="mr-1 flex items-center gap-2 text-sm font-semibold tracking-tight text-body"
					title="feasible.lol"
				>
					<span className="flex size-6 items-center justify-center rounded-md bg-accent text-[13px] font-bold text-white dark:text-slate-950">
						f
					</span>
					<span className="hidden sm:inline">feasible</span>
				</a>

				<SitePicker current={state.domain} sites={sites} onPick={(domain) => onNavigate({ ...state, domain })} />

				<CurrentVisitors domain={state.domain} />

				<div className="ml-auto flex items-center gap-2">
					<PeriodPicker state={state} label={label} onNavigate={onNavigate} resolved={resolved} />
					<ThemeToggle theme={theme} onTheme={onTheme} />
				</div>
			</div>
		</header>
	);
}

/** periodLabel names the current range for the button face. A custom range is
 *  shown as its own two dates, which are already the reader's own input. */
function periodLabel(state: UrlState): string {
	if (state.from && state.to) {
		return state.from === state.to ? state.from : t("dashboard.format.range", { from: state.from, to: state.to });
	}

	const period = PERIODS.find((entry) => entry.preset === state.preset);

	return period ? t(period.labelId) : state.preset;
}

/**
 * SitePicker switches sites.
 *
 * It renders as a plain select rather than a custom menu: it is the one control
 * that may hold hundreds of entries, and a native select gets type-ahead,
 * scrolling and mobile pickers for free that a div would have to re-earn.
 */
function SitePicker({ current, sites, onPick }: { current: string; sites: string[]; onPick: (domain: string) => void }) {
	if (sites.length === 0) return null;

	return (
		<label className="relative flex items-center">
			<span className="sr-only">{t("dashboard.topbar.site")}</span>
			<select
				value={current}
				onChange={(event) => onPick(event.target.value)}
				className="h-control cursor-pointer appearance-none rounded-md border border-line bg-card py-0 pr-7 pl-2.5 text-sm font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
			>
				{sites.map((site) => (
					<option key={site} value={site}>
						{site}
					</option>
				))}
			</select>
			<Chevron className="pointer-events-none absolute right-2" />
		</label>
	);
}

/**
 * CurrentVisitors is the live pill.
 *
 * The window is the engine's realtime preset — thirty minutes, which is the
 * session timeout and therefore exactly the span in which somebody still counts
 * as being on the site. It refreshes every thirty seconds and skips the poll
 * entirely while the tab is in the background.
 */
function CurrentVisitors({ domain }: { domain: string }) {
	const stats = useStats(domain, { metrics: ["visitors"], date_range: "realtime" });

	useInterval(stats.reload, 30_000);

	const count = stats.data?.results[0]?.metrics[0] ?? null;

	if (count === null) return null;

	return (
		<span
			className="flex h-control items-center gap-2 rounded-md px-2 text-sm text-muted"
			title={t("dashboard.topbar.current_visitors.help")}
		>
			<span className="relative flex size-2">
				<span className="absolute inline-flex size-full animate-ping rounded-full bg-accent opacity-60" />
				<span className="relative inline-flex size-2 rounded-full bg-accent" />
			</span>
			<span className="tnum font-medium text-body">{count}</span>
			<span className="hidden sm:inline">{n("dashboard.topbar.current_visitors", count)}</span>
		</span>
	);
}

/** PeriodPicker is the date-range menu, plus the custom-range form. */
function PeriodPicker({
	state,
	label,
	onNavigate,
	resolved,
}: {
	state: UrlState;
	label: string;
	onNavigate: (next: UrlState) => void;
	resolved: string[] | undefined;
}) {
	const [open, setOpen] = useState(false);
	const [custom, setCustom] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => {
		setOpen(false);
		setCustom(false);
	});

	// Changing the period closes the drawer. A details view is about a slice of
	// a specific window, and leaving it open over a different one would show
	// numbers that no longer answer the question that was asked.
	const pick = (id: string, preset?: Preset) => {
		setOpen(false);
		setCustom(false);

		if (id === "yesterday") {
			const day = isoDay(-1);
			onNavigate({ ...state, from: day, to: day, drawer: null });
			return;
		}

		onNavigate({ ...state, preset: preset ?? "28d", from: "", to: "", drawer: null });
	};

	return (
		<div ref={wrap} className="relative">
			<button
				type="button"
				aria-expanded={open}
				aria-haspopup="menu"
				onClick={() => setOpen((was) => !was)}
				className="flex h-control items-center gap-1.5 rounded-md border border-line bg-card px-2.5 text-sm font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
				title={resolved ? rangeLabel(resolved) : undefined}
			>
				{label}
				<Chevron />
			</button>

			{open && (
				<div
					role="menu"
					className="absolute right-0 z-40 mt-1 w-60 rounded-md border border-line bg-card p-1 shadow-lg"
				>
					{PERIODS.map((period) => {
						const active = period.id === "yesterday" ? !!state.from : !state.from && state.preset === period.preset;

						return (
							<button
								key={period.id}
								type="button"
								role="menuitem"
								onClick={() => pick(period.id, period.preset)}
								className={`flex w-full items-center justify-between rounded-sm px-2.5 py-1.5 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
									active ? "font-medium text-accent" : "text-body"
								}`}
							>
								{t(period.labelId)}
							</button>
						);
					})}

					<div className="my-1 border-t border-line" />

					{custom ? (
						<CustomRange
							from={state.from}
							to={state.to}
							onApply={(from, to) => {
								setOpen(false);
								setCustom(false);
								onNavigate({ ...state, from, to, drawer: null });
							}}
						/>
					) : (
						<button
							type="button"
							role="menuitem"
							onClick={() => setCustom(true)}
							className="w-full rounded-sm px-2.5 py-1.5 text-left text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
						>
							{t("dashboard.topbar.custom_range")}
						</button>
					)}

					{/* The resolved window is shown under the menu because date
					    maths is the single biggest source of "your numbers are
					    wrong", and the answer is usually that the period was not
					    the one the reader assumed. */}
					{resolved && <p className="px-2.5 pt-2 pb-1 text-[11px] text-muted">{rangeLabel(resolved)}</p>}
				</div>
			)}
		</div>
	);
}

/** CustomRange is the two-date form. It applies nothing until both bounds are
 *  set, so a half-typed range never fires a query the server will refuse. */
function CustomRange({ from, to, onApply }: { from: string; to: string; onApply: (from: string, to: string) => void }) {
	const [start, setStart] = useState(from || isoDay(-6));
	const [end, setEnd] = useState(to || isoDay(0));

	return (
		<div className="flex flex-col gap-2 px-2.5 py-2">
			<label className="flex items-center justify-between gap-2 text-xs text-muted">
				{t("dashboard.topbar.from")}
				<input
					type="date"
					value={start}
					max={end}
					onChange={(event) => setStart(event.target.value)}
					className="h-control rounded-md border border-line bg-card px-2 text-sm text-body"
				/>
			</label>
			<label className="flex items-center justify-between gap-2 text-xs text-muted">
				{t("dashboard.topbar.to")}
				<input
					type="date"
					value={end}
					min={start}
					onChange={(event) => setEnd(event.target.value)}
					className="h-control rounded-md border border-line bg-card px-2 text-sm text-body"
				/>
			</label>
			<button
				type="button"
				disabled={!start || !end}
				onClick={() => onApply(start, end)}
				className="h-control rounded-md bg-accent text-sm font-medium text-white transition-opacity duration-150 ease-[var(--ease-ui)] hover:opacity-90 disabled:opacity-40 dark:text-slate-950"
			>
				{t("dashboard.topbar.apply")}
			</button>
		</div>
	);
}

/** What each theme is called mid-sentence. The ids are written out rather than
 *  built from the theme name, so every string the dashboard can ask for is
 *  findable by searching the source for its id. */
const THEME_NAMES: Record<Theme, string> = {
	light: "dashboard.theme.light",
	dark: "dashboard.theme.dark",
	system: "dashboard.theme.system",
};

/** ThemeToggle cycles light, dark and system. Three states rather than two
 *  because "follow the OS" is the setting most people actually want, and a
 *  two-way switch has no way to express it. */
function ThemeToggle({ theme, onTheme }: { theme: Theme; onTheme: (next: Theme) => void }) {
	const next: Theme = theme === "system" ? "light" : theme === "light" ? "dark" : "system";
	const glyph = theme === "system" ? "◐" : theme === "light" ? "☀" : "☾";
	const description = t("dashboard.topbar.theme", { current: t(THEME_NAMES[theme]), next: t(THEME_NAMES[next]) });

	return (
		<button
			type="button"
			onClick={() => onTheme(next)}
			title={description}
			aria-label={description}
			className="flex size-control items-center justify-center rounded-md border border-line bg-card text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
		>
			{glyph}
		</button>
	);
}

function Chevron({ className = "" }: { className?: string }) {
	return (
		<svg viewBox="0 0 12 12" width="10" height="10" aria-hidden="true" className={`fill-none stroke-current ${className}`}>
			<path d="M3 4.5 6 7.5 9 4.5" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
		</svg>
	);
}

/** useDismiss closes a popover on an outside click or Escape. Both are needed:
 *  a menu that only closes on Escape traps a mouse user, and one that only
 *  closes on an outside click traps a keyboard user. */
function useDismiss(ref: React.RefObject<HTMLElement | null>, open: boolean, close: () => void): void {
	useEffect(() => {
		if (!open) return;

		const onDown = (event: MouseEvent) => {
			if (ref.current && !ref.current.contains(event.target as Node)) close();
		};

		const onKey = (event: KeyboardEvent) => {
			if (event.key === "Escape") close();
		};

		document.addEventListener("mousedown", onDown);
		document.addEventListener("keydown", onKey);

		return () => {
			document.removeEventListener("mousedown", onDown);
			document.removeEventListener("keydown", onKey);
		};
	}, [open, close, ref]);
}

/** isoDay renders a day relative to today as YYYY-MM-DD in the reader's own
 *  timezone, which is what a date picker means by "yesterday". */
function isoDay(offset: number): string {
	const at = new Date();
	at.setDate(at.getDate() + offset);

	return `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, "0")}-${String(at.getDate()).padStart(2, "0")}`;
}
