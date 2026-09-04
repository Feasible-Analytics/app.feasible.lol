//
// TopBar.tsx
// The sticky bar: site, live visitors, period, account.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useRef, useState } from "react";

import type { Filter, Preset, StatsRequest } from "../api/types";
import type { Navigation } from "../api/types";
import type { CompareMode } from "../lib/compare";
import { COMPARE_LABELS } from "../lib/compare";
import { useDismiss } from "../lib/dom";
import { calendarDate } from "../lib/format";
import { n, t } from "../lib/i18n";
import type { Period } from "../lib/period";
import { PERIODS } from "../lib/period";
import type { Theme } from "../lib/prefs";
import type { UrlState } from "../lib/url";
import { useStats } from "../lib/useStats";
import { useInterval } from "../lib/useStats";
import { Chevron } from "./atoms";
import { PeriodPicker } from "./PeriodPicker";

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

	// onStep is the same action the arrow keys perform. The arrows in the
	// period control call it rather than stepping the window themselves, so the
	// two routes cannot land in different places.
	onStep: (direction: -1 | 1) => void;
	/** Bumped when the keyboard asks for the custom-range form. A counter rather
	 *  than a flag, because pressing the key twice has to open it twice. */
	// onPeriod is the same action the period hotkeys perform, so a row in the
	// menu and its printed key cannot land in different places.
	onPeriod: (period: Period) => void;

	// asked is the last period anything requested, with a counter that ticks on
	// every request. The picker closes on it.
	asked: { id: string; at: number };
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
export function TopBar({ state, sites, onNavigate, theme, onTheme, resolved, filters, onHelp, onStep, onPeriod, asked, navigation, locked = false }: Props) {
	const label = periodLabel(state);
	const live = state.preset === "realtime" && !state.from;

	return (
		<header className="sticky top-0 z-30 border-b-2 border-line bg-card/95 backdrop-blur">
			<div className="mx-auto flex max-w-shell flex-wrap items-center gap-2 px-4 py-2.5 sm:px-5">
				<a
					href={navigation?.sites_url ?? "/"}
					className="mr-1 font-display text-base font-extrabold tracking-tight text-heading"
					title="feasible.lol"
				>
					Feasible<span className="text-accent">.lol</span>
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
						className="flex size-control items-center justify-center border-2 border-line bg-card text-sm text-muted transition-colors hover:bg-hover hover:text-body"
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
						onStep={onStep}
						resolved={resolved}
						onPeriod={onPeriod}
						asked={asked}
					/>}
					{/* A shared or public dashboard has no account, so it has no
					    menu to fold these into. The keys are still bound there,
					    so without the buttons the whole layer is unreachable for
					    the readers least able to ask for it back. */}
					{!navigation && !locked && <HelpButton onHelp={onHelp} />}
					{!navigation && <ThemeToggle theme={theme} onTheme={onTheme} />}
					{navigation && (
						<AccountMenu
							navigation={navigation}
							theme={theme}
							onTheme={onTheme}
							onHelp={onHelp}
							shortcuts={!locked}
						/>
					)}
				</div>
			</div>
		</header>
	);
}

/**
 * AccountFace is the picture, or the first letter of the name when there is
 * none.
 *
 * The letter is not a placeholder to be apologised for: it is the right answer
 * for anybody who has neither a Google picture nor a Gravatar, and it is what a
 * picture that fails to load falls back to. The source is always our own origin
 * — the server fetched it once, precisely so a browser never tells Google or
 * Gravatar who is looking at which page.
 */
function AccountFace({ navigation }: { navigation: Navigation }) {
	const [broken, setBroken] = useState(false);
	const letter = (navigation.name || navigation.email).slice(0, 1).toUpperCase();

	if (!navigation.avatar_url || broken) return <>{letter}</>;

	return (
		<img
			src={navigation.avatar_url}
			alt=""
			onError={() => setBroken(true)}
			className="size-full object-cover"
		/>
	);
}

/** MenuRow is one row in the account menu. The kinds differ because they are
 * different controls, not different labels: a destination is a link, a theme is
 * one of an exclusive set, and signing out is a form that carries a token. */
export type MenuRow =
	| { kind: "link"; id: string; label: string; href: string }
	| { kind: "action"; id: string; label: string; hint: string }
	| { kind: "theme"; id: string; label: string; theme: Theme; glyph: string; current: boolean }
	| { kind: "signout"; id: string; label: string };

/** MenuGroup is one divider-separated run of rows, with a heading when the rows
 * need one to make sense. */
export interface MenuGroup {
	id: string;
	label?: string;
	rows: MenuRow[];
}

/** The glyph and label id for each theme row. The ids are written out rather
 *  than built from the theme name, so the catalogue's unused-string check can
 *  find them. */
const THEME_ROWS: { theme: Theme; glyph: string; labelId: string }[] = [
	{ theme: "light", glyph: "☀", labelId: "dashboard.menu.theme.light" },
	{ theme: "dark", glyph: "☾", labelId: "dashboard.menu.theme.dark" },
	{ theme: "system", glyph: "◐", labelId: "dashboard.menu.theme.system" },
];

/**
 * accountMenuGroups is what the account menu draws, in the order it draws it.
 *
 * It is a function rather than markup so the rules that decide which rows exist
 * are one testable answer rather than conditionals scattered through JSX.
 * `shortcuts` is false on a locked account, which binds no keys: a row that
 * closes the menu and does nothing else is worse than no row.
 */
export function accountMenuGroups(navigation: Navigation, theme: Theme, shortcuts: boolean): MenuGroup[] {
	const destinations: MenuRow[] = [
		{ kind: "link", id: "sites", label: t("dashboard.navigation.sites"), href: navigation.sites_url },
	];

	if (navigation.site_settings_url) {
		destinations.push({
			kind: "link",
			id: "site_settings",
			label: t("dashboard.navigation.site_settings"),
			href: navigation.site_settings_url,
		});
	}

	destinations.push({
		kind: "link",
		id: "account",
		label: t("dashboard.navigation.account_settings"),
		href: navigation.account_url,
	});

	if (navigation.billing_url) {
		destinations.push({
			kind: "link",
			id: "billing",
			label: t("dashboard.navigation.billing"),
			href: navigation.billing_url,
		});
	}

	const groups: MenuGroup[] = [{ id: "destinations", rows: destinations }];

	if (shortcuts) {
		groups.push({
			id: "help",
			rows: [{ kind: "action", id: "shortcuts", label: t("dashboard.menu.shortcuts"), hint: "?" }],
		});
	}

	groups.push(
		{
			id: "theme",
			label: t("dashboard.menu.theme"),
			rows: THEME_ROWS.map((row) => ({
				kind: "theme" as const,
				id: `theme:${row.theme}`,
				label: t(row.labelId),
				theme: row.theme,
				glyph: row.glyph,
				current: row.theme === theme,
			})),
		},
		{
			id: "session",
			rows: [{ kind: "signout", id: "signout", label: t("dashboard.navigation.sign_out") }],
		},
	);

	return groups;
}

/** AccountMenu holds product navigation, the two controls that are pressed
 * rarely enough not to earn space beside the date range, and the
 * CSRF-protected sign-out. */
function AccountMenu({
	navigation,
	theme,
	onTheme,
	onHelp,
	shortcuts,
}: {
	navigation: Navigation;
	theme: Theme;
	onTheme: (next: Theme) => void;
	onHelp: () => void;
	shortcuts: boolean;
}) {
	const [open, setOpen] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => setOpen(false));

	const groups = accountMenuGroups(navigation, theme, shortcuts);

	return (
		<div ref={wrap} className="relative">
			<button
				type="button"
				aria-expanded={open}
				aria-haspopup="menu"
				aria-label={t("dashboard.navigation.account_menu")}
				onClick={() => setOpen((was) => !was)}
				className="flex size-control items-center justify-center overflow-hidden border-2 border-line bg-subtle text-xs font-semibold text-body transition-colors hover:bg-hover"
			>
				<AccountFace navigation={navigation} />
			</button>

			{open && (
				<div
					role="menu"
					className="scroll-thin absolute right-0 mt-2 max-h-[calc(100vh-5rem)] w-60 max-w-[calc(100vw-1rem)] overflow-y-auto border-2 border-line bg-card p-1.5 pop"
				>
					<div className="px-2.5 py-2">
						<p className="truncate text-sm font-medium text-body">{navigation.name}</p>
						<p className="truncate text-xs text-muted">{navigation.email}</p>
					</div>

					{groups.map((group) => (
						<div key={group.id} role="group" aria-label={group.label} className="border-t border-line pt-1.5">
							{group.label && (
								<p aria-hidden="true" className="px-2.5 pt-1 pb-0.5 text-[10px] font-semibold tracking-wide text-muted uppercase">
									{group.label}
								</p>
							)}
							{group.rows.map((row) => (
								<MenuRowView
									key={row.id}
									row={row}
									navigation={navigation}
									onTheme={onTheme}
									onHelp={() => {
										setOpen(false);
										onHelp();
									}}
								/>
							))}
						</div>
					))}
				</div>
			)}
		</div>
	);
}

/** MenuRowView draws one row. Choosing a theme leaves the menu open, because
 * the page changes underneath it and the next choice is one click away. */
function MenuRowView({
	row,
	navigation,
	onTheme,
	onHelp,
}: {
	row: MenuRow;
	navigation: Navigation;
	onTheme: (next: Theme) => void;
	onHelp: () => void;
}) {
	const base = "flex w-full items-center gap-2 px-2.5 py-2 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover";

	switch (row.kind) {
		case "link":
			return <a role="menuitem" href={row.href} className={`${base} text-body`}>{row.label}</a>;

		case "action":
			return (
				<button
					type="button"
					role="menuitem"
					onClick={onHelp}
					aria-label={t("dashboard.shortcuts.open")}
					className={`${base} text-body`}
				>
					<span className="flex-1">{row.label}</span>
					<span aria-hidden="true" className="tnum border-2 border-line px-1.5 text-[11px] text-muted">{row.hint}</span>
				</button>
			);

		case "theme":
			return (
				<button
					type="button"
					role="menuitemradio"
					aria-checked={row.current}
					onClick={() => onTheme(row.theme)}
					className={`${base} ${row.current ? "font-medium text-body" : "text-body"}`}
				>
					<span aria-hidden="true" className="w-4 text-center text-muted">{row.glyph}</span>
					<span className="flex-1">{row.label}</span>
					{row.current && <span aria-hidden="true" className="text-accent-ink">✓</span>}
				</button>
			);

		case "signout":
			return (
				<form method="post" action={navigation.logout_url}>
					<input type="hidden" name="csrf_token" value={navigation.csrf} />
					<button type="submit" role="menuitem" className={`${base} text-down`}>{row.label}</button>
				</form>
			);

		default:
			return null;
	}
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
				className="h-control cursor-pointer appearance-none border-2 border-line bg-card py-0 pr-7 pl-2.5 text-sm font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
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
			className={`flex h-control items-center gap-2 px-2 text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
 live ? "text-accent-ink" : "text-muted"
			}`}
			title={t("dashboard.topbar.current_visitors.help")}
		>
			<span className="relative flex size-2">
				<span className="absolute inline-flex size-full animate-ping bg-accent opacity-60" />
				<span className="relative inline-flex size-2 bg-accent" />
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
				className={`h-control cursor-pointer appearance-none border-2 border-line bg-card py-0 pr-7 pl-2.5 text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
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

/** HelpButton is the only way into the shortcut layer on a dashboard with no
 * account menu to hold it. A signed-in reader reaches the same thing from that
 * menu, where it does not compete with the period picker. */
function HelpButton({ onHelp }: { onHelp: () => void }) {
	return (
		<button
			type="button"
			onClick={onHelp}
			title={t("dashboard.shortcuts.open")}
			aria-label={t("dashboard.shortcuts.open")}
			className="hidden size-control items-center justify-center border-2 border-line bg-card text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover sm:flex"
		>
			?
		</button>
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
			className="flex size-control items-center justify-center border-2 border-line bg-card text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
		>
			{glyph}
		</button>
	);
}

