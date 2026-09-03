//
// TopBar.tsx
// The sticky bar: site, live visitors, period, theme.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef, useState } from "react";

import type { Filter, Preset, StatsRequest } from "../api/types";
import type { Navigation } from "../api/types";
import type { CompareMode } from "../lib/compare";
import { COMPARE_LABELS } from "../lib/compare";
import { useDismiss } from "../lib/dom";
import { calendarDate, rangeLabel } from "../lib/format";
import { n, t } from "../lib/i18n";
import { addDays, step, today } from "../lib/period";
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
	/** The filters in force. The live pill carries them too, so the number in the
	 *  bar is about the same population as the page under it. */
	filters: Filter[];
	onHelp: () => void;
	/** Bumped when the keyboard asks for the custom-range form. A counter rather
	 *  than a flag, because pressing the key twice has to open it twice. */
	pickCustom: number;
	navigation?: Navigation;
	locked?: boolean;
}

/**
 * The current-visitor window.
 *
 * Five minutes, and the same five minutes the realtime screen uses. Two windows
 * both labelled "current visitors" is how somebody ends up reading a 30-minute
 * count against a 5-minute one and concluding the dashboard cannot add up.
 */
const CURRENT_RANGE: Preset = "5m";

/** Engagement pings fire on tab blur with no navigation behind them, so a
 *  visitor whose only trace is a ping has left rather than arrived. Counting one
 *  is what makes a live figure drift above the rest of the dashboard. */
const NOT_ENGAGEMENT: Filter = ["is_not", "event:name", ["engagement"], { case_sensitive: true }];

/**
 * siteSwitchURL is where the picker sends the browser for another site.
 *
 * The query string comes along. The full reload is about the server-rendered
 * navigation links, which only ship in the bootstrap, and not about starting
 * over — dropping the search here reset the period, the comparison and every
 * filter every time somebody changed site.
 */
export function siteSwitchURL(domain: string, search: string): string {
	return `/dashboard/${encodeURIComponent(domain)}${search}`;
}

/**
 * currentVisitorsRequest builds the live-pill query, shared with the realtime
 * screen so the two can never disagree about what "current" means.
 *
 * `pageviews` is requested and thrown away, and it is load-bearing. A query made
 * only of metrics that count on either table is planned against `sessions`,
 * where `is_not event:name engagement` means "this visit never sent a ping" —
 * and since almost every real visit sends one, that answers zero. Asking for one
 * event-scoped metric plans the query at event grain, where the filter means
 * "events that are not pings" and the count is the visitors behind them, which
 * is the question being asked. `internal/query/table.go` owns that decision and
 * a test there pins both readings.
 *
 * The number has no room for a sampling caveat of its own, so it explicitly
 * refuses sampling rather than inheriting the query engine's automatic decision.
 */
export function currentVisitorsRequest(filters: Filter[]): StatsRequest {
	return {
		metrics: ["visitors", "pageviews"],
		date_range: CURRENT_RANGE,
		filters: [...filters, NOT_ENGAGEMENT],
		exact: true,
	};
}

/**
 * TopBar is the one control surface on the page.
 *
 * Everything in it writes to the URL rather than to component state, so the
 * address bar is always a description of what is on screen — which is what
 * makes a dashboard link worth sending to somebody.
 */
export function TopBar({ state, sites, onNavigate, theme, onTheme, resolved, filters, onHelp, pickCustom, navigation, locked = false }: Props) {
	const label = periodLabel(state);
	const live = state.preset === "realtime" && !state.from;

	return (
		<header className="sticky top-0 z-30 border-b border-line bg-card/95 backdrop-blur">
			<div className="mx-auto flex max-w-shell flex-wrap items-center gap-2 px-4 py-2.5 sm:px-5">
				<a
					href={navigation?.sites_url ?? "/"}
					className="mr-1 flex items-center gap-2 text-sm font-semibold tracking-tight text-body"
					title="feasible.lol"
				>
					<span className="flex size-6 items-center justify-center rounded-md bg-accent text-[13px] font-bold text-white dark:text-slate-950">
						f
					</span>
					<span className="hidden sm:inline">feasible</span>
				</a>

				<SitePicker
					current={state.domain}
					sites={sites}
					onPick={(domain) => {
						// The authenticated dashboard reloads rather than switching
						// in place: the per-site navigation links are computed on the
						// server and only ship in the bootstrap, so a SPA switch would
						// leave the settings link pointing at the previous site.
						if (navigation) {
							window.location.assign(siteSwitchURL(domain, location.search));
							return;
						}
						onNavigate({ ...state, domain });
					}}
				/>
				{navigation?.site_settings_url && (
					<a
						href={navigation.site_settings_url}
						title={t("dashboard.navigation.site_settings")}
						aria-label={t("dashboard.navigation.site_settings")}
						className="flex size-control items-center justify-center rounded-md border border-line bg-card text-sm text-muted transition-colors hover:bg-hover hover:text-body"
					>
						<SettingsIcon />
					</a>
				)}

				{!locked && <CurrentVisitors
					domain={state.domain}
					filters={filters}
					live={live}
					onOpen={() => onNavigate({ ...state, preset: "realtime", from: "", to: "", drawer: null })}
				/>}

				<div className="ml-auto flex items-center gap-2">
					{/* Comparison is hidden on the live view rather than disabled:
					    there is no previous thirty minutes to compare the last
					    thirty against, and a control that does nothing is worse
					    than one that is not there. */}
					{!locked && !live && <ComparePicker state={state} onNavigate={onNavigate} />}

					{!locked && <PeriodPicker
						state={state}
						label={label}
						onNavigate={onNavigate}
						resolved={resolved}
						pickCustom={pickCustom}
					/>}
					{!locked && <HelpButton onHelp={onHelp} />}
					<ThemeToggle theme={theme} onTheme={onTheme} />
					{navigation && <AccountMenu navigation={navigation} />}
				</div>
			</div>
		</header>
	);
}

/** AccountMenu keeps product navigation and the CSRF-protected sign-out in one
 * compact control that remains usable at mobile widths. */
function AccountMenu({ navigation }: { navigation: Navigation }) {
	const [open, setOpen] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => setOpen(false));

	return (
		<div ref={wrap} className="relative">
			<button
				type="button"
				aria-expanded={open}
				aria-haspopup="menu"
				aria-label={t("dashboard.navigation.account_menu")}
				onClick={() => setOpen((was) => !was)}
				className="flex size-control items-center justify-center rounded-full border border-line bg-subtle text-xs font-semibold text-body transition-colors hover:bg-hover"
			>
				{(navigation.name || navigation.email).slice(0, 1).toUpperCase()}
			</button>

			{open && (
				<div role="menu" className="absolute right-0 mt-2 w-56 rounded-md border border-line bg-card p-1.5 shadow-xl">
					<div className="border-b border-line px-2.5 py-2">
						<p className="truncate text-sm font-medium text-body">{navigation.name}</p>
						<p className="truncate text-xs text-muted">{navigation.email}</p>
					</div>
					<NavItem href={navigation.sites_url} label={t("dashboard.navigation.sites")} />
					{navigation.site_settings_url && <NavItem href={navigation.site_settings_url} label={t("dashboard.navigation.site_settings")} />}
					<NavItem href={navigation.account_url} label={t("dashboard.navigation.account_settings")} />
					{navigation.billing_url && <NavItem href={navigation.billing_url} label={t("dashboard.navigation.billing")} />}
					<form method="post" action={navigation.logout_url}>
						<input type="hidden" name="csrf_token" value={navigation.csrf} />
						<button type="submit" role="menuitem" className="w-full rounded-sm px-2.5 py-2 text-left text-sm text-body hover:bg-hover">
							{t("dashboard.navigation.sign_out")}
						</button>
					</form>
				</div>
			)}
		</div>
	);
}

/** NavItem is one consistent destination in the account menu. */
function NavItem({ href, label }: { href: string; label: string }) {
	return <a role="menuitem" href={href} className="block rounded-sm px-2.5 py-2 text-sm text-body hover:bg-hover">{label}</a>;
}

/** SettingsIcon is an SVG rather than a text glyph so its weight, alignment,
 * and appearance stay consistent across browsers and operating systems. */
function SettingsIcon() {
	return (
		<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="size-4" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
			<circle cx="12" cy="12" r="3" />
			<path d="M19.4 15a1.7 1.7 0 0 0 .34 1.88l.06.06-2.86 2.86-.06-.06A1.7 1.7 0 0 0 15 19.4a1.7 1.7 0 0 0-1 .6 1.7 1.7 0 0 0-.4 1.1V21H9.6v-.1A1.7 1.7 0 0 0 8.5 19.4a1.7 1.7 0 0 0-1.88.34l-.06.06-2.86-2.86.06-.06A1.7 1.7 0 0 0 4.1 15a1.7 1.7 0 0 0-1.5-1H2v-4h.6a1.7 1.7 0 0 0 1.5-1 1.7 1.7 0 0 0-.34-1.88l-.06-.06L6.56 4.2l.06.06A1.7 1.7 0 0 0 8.5 4.6a1.7 1.7 0 0 0 1-1.5V3h4v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.88-.34l.06-.06 2.86 2.86-.06.06A1.7 1.7 0 0 0 18.9 9a1.7 1.7 0 0 0 1.5 1h.6v4h-.6a1.7 1.7 0 0 0-1 .99Z" />
		</svg>
	);
}

/** periodLabel names the current range for the button face. ISO values remain
 *  in the URL and native date inputs, while the visible label follows the
 *  dashboard locale and reads like a date rather than a database value. */
export function periodLabel(state: UrlState): string {
	if (state.from && state.to) {
		const from = calendarDate(state.from);
		const to = calendarDate(state.to);

		return state.from === state.to ? from : t("dashboard.format.range", { from, to });
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
				// The id is what the `0` shortcut reaches for. A ref threaded down
				// from App would be the same lookup with more moving parts, on a
				// control there is exactly one of.
				id="site-picker"
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
 * CurrentVisitors is the live pill, and a way into the live view.
 *
 * It counts the same five minutes the realtime screen counts, with the same
 * engagement exclusion, because "current visitors" has to be one number wherever
 * it appears. It refreshes every thirty seconds and skips the poll entirely
 * while the tab is in the background.
 */
function CurrentVisitors({
	domain,
	filters,
	live,
	onOpen,
}: {
	domain: string;
	filters: Filter[];
	live: boolean;
	onOpen: () => void;
}) {
	const stats = useStats(domain, currentVisitorsRequest(filters));

	useInterval(stats.reload, 30_000);

	const count = stats.data?.results[0]?.metrics[0] ?? null;

	if (count === null) return null;

	return (
		<button
			type="button"
			onClick={onOpen}
			aria-pressed={live}
			className={`flex h-control items-center gap-2 rounded-md px-2 text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
				live ? "text-accent" : "text-muted"
			}`}
			title={t("dashboard.topbar.current_visitors.help")}
		>
			<span className="relative flex size-2">
				<span className="absolute inline-flex size-full animate-ping rounded-full bg-accent opacity-60" />
				<span className="relative inline-flex size-2 rounded-full bg-accent" />
			</span>
			<span className="tnum font-medium text-body">{count}</span>
			<span className="hidden sm:inline">{n("dashboard.topbar.current_visitors", count)}</span>
		</button>
	);
}

/**
 * ComparePicker turns the comparison on and off and chooses what against.
 *
 * The mode is in the URL rather than in a preference, because a link to a
 * dashboard showing "+40% year on year" is a link about the comparison as much
 * as about the period, and a recipient who saw no comparison would not see the
 * same page at all.
 */
function ComparePicker({ state, onNavigate }: { state: UrlState; onNavigate: (next: UrlState) => void }) {
	const modes: CompareMode[] = ["off", "previous_period", "year_over_year"];

	return (
		<label className="relative flex items-center">
			<span className="sr-only">{t("dashboard.compare.label")}</span>
			<select
				value={state.compare}
				onChange={(event) => onNavigate({ ...state, compare: event.target.value as CompareMode })}
				className={`h-control cursor-pointer appearance-none rounded-md border border-line bg-card py-0 pr-7 pl-2.5 text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
					state.compare === "off" ? "text-muted" : "font-medium text-body"
				}`}
			>
				{modes.map((mode) => (
					<option key={mode} value={mode}>
						{t(COMPARE_LABELS[mode])}
					</option>
				))}
			</select>
			<Chevron className="pointer-events-none absolute right-2" />
		</label>
	);
}

/** HelpButton is how somebody who never presses `?` finds out that `?` does
 *  something. A keyboard layer with no visible way in is a keyboard layer only
 *  its author uses. */
function HelpButton({ onHelp }: { onHelp: () => void }) {
	return (
		<button
			type="button"
			onClick={onHelp}
			title={t("dashboard.shortcuts.open")}
			aria-label={t("dashboard.shortcuts.title")}
			className="hidden size-control items-center justify-center rounded-md border border-line bg-card text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover sm:flex"
		>
			?
		</button>
	);
}

/** PeriodPicker is the date-range menu, plus the custom-range form. */
function PeriodPicker({
	state,
	label,
	onNavigate,
	resolved,
	pickCustom,
}: {
	state: UrlState;
	label: string;
	onNavigate: (next: UrlState) => void;
	resolved: string[] | undefined;
	pickCustom: number;
}) {
	const [open, setOpen] = useState(false);
	const [custom, setCustom] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => {
		setOpen(false);
		setCustom(false);
	});

	// The keyboard's route into the two-date form. It skips the menu entirely:
	// somebody who pressed the shortcut has already chosen, and making them
	// click "Custom range…" afterwards would be the shortcut doing half a job.
	useEffect(() => {
		if (pickCustom === 0) return;

		setOpen(true);
		setCustom(true);
	}, [pickCustom]);

	// Changing the period closes the drawer. A details view is about a slice of
	// a specific window, and leaving it open over a different one would show
	// numbers that no longer answer the question that was asked.
	const pick = (id: string, preset?: Preset) => {
		setOpen(false);
		setCustom(false);

		// Yesterday is computed the same way the keyboard shortcut computes it,
		// so the two routes to the same day cannot disagree.
		if (id === "yesterday") {
			const day = step("day", "", "", today(), -1);
			if (day) onNavigate({ ...state, from: day.from, to: day.to, drawer: null });
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
	const [start, setStart] = useState(from || addDays(today(), -6));
	const [end, setEnd] = useState(to || today());

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

/** Chevron is the drop-down arrow drawn over the menus and the native selects.
 *  A select's own arrow cannot be styled the same way in every browser, so it
 *  is hidden and this one is drawn in its place. */
function Chevron({ className = "" }: { className?: string }) {
	return (
		<svg viewBox="0 0 12 12" width="10" height="10" aria-hidden="true" className={`fill-none stroke-current ${className}`}>
			<path d="M3 4.5 6 7.5 9 4.5" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
		</svg>
	);
}
