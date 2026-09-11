//
// TopBar.tsx
// The sticky bar: site, live visitors, period, account.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { KeyboardEvent, ReactNode } from "react";
import { useRef, useState } from "react";

import type { Filter, Preset, StatsRequest } from "../api/types";
import type { Navigation } from "../api/types";
import type { CompareMode } from "../lib/compare";
import { COMPARE_LABELS } from "../lib/compare";
import { useDismiss } from "../lib/dom";
import { calendarDate } from "../lib/format";
import { n, t } from "../lib/i18n";
import type { IntervalPref } from "../lib/interval";
import { INTERVAL_LABELS } from "../lib/interval";
import type { Period } from "../lib/period";
import { PERIODS } from "../lib/period";
import type { Theme } from "../lib/prefs";
import type { UrlState } from "../lib/url";
import { useStats } from "../lib/useStats";
import { useInterval } from "../lib/useStats";
import { Chevron } from "./atoms";
import type { ChartType } from "./MainGraph";
import { CHART_LABELS, CHART_TYPES, ShapeIcon } from "./MainGraph";
import { PeriodPicker } from "./PeriodPicker";
import { SitePicker } from "./SitePicker";

interface Props {
	state: UrlState;
	sites: string[];
	onNavigate: (next: UrlState) => void;
	theme: Theme;
	onTheme: (next: Theme) => void;
	/** The shape the graph is drawn as, or null on a screen that has no graph.
	 *  Null removes the rows rather than disabling them: a choice that changes
	 *  nothing visible is a choice that reads as broken. */
	chart: ChartType | null;
	onChart: (next: ChartType) => void;
	/** The bucket width the reader has chosen for the graph. */
	interval: IntervalPref;
	/** The widths worth offering for the range on screen. */
	intervals: IntervalPref[];
	onInterval: (next: IntervalPref) => void;
	/** The window the server actually used, shown under a preset name. */
	resolved: string[] | undefined;
	/** The filters in force. The live pill carries them too, so the number in the
	 *  bar is about the same population as the page under it. */
	filters: Filter[];
	/** The filter pills and their editor. They live in the bar rather than in the
	 *  page because the bar is what follows the reader down a long dashboard, and
	 *  a filter you have to scroll back up to read is one you stop trusting. */
	filterBar?: ReactNode;
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
 * showsLiveCount decides whether the bar has room for the live number.
 *
 * The count steps aside for the filter pills rather than sharing the row with
 * them. Both answer "what am I looking at"; the pills answer it about the
 * choice the reader just made, and on a laptop the two together wrap the bar
 * onto a second line that then follows them down every screen of the page.
 *
 * A locked account has no number at all: nothing behind this bar is fetching.
 */
export function showsLiveCount(locked: boolean, filters: Filter[]): boolean {
	return !locked && filters.length === 0;
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
export function TopBar({ state, sites, onNavigate, theme, onTheme, chart, onChart, interval, intervals, onInterval, resolved, filters, filterBar, onHelp, onStep, onPeriod, asked, navigation, locked = false }: Props) {
	const label = periodLabel(state);
	const view: ViewPrefs = { theme, chart, interval, intervals };
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

				{/* Site settings lives inside the picker rather than beside it.
				    A gear in the bar was a second button with no name on it,
				    whose target changed with whatever the dropdown next to it
				    was showing. */}
				<SitePicker
					current={state.domain}
					sites={sites}
					navigation={navigation}
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

				{showsLiveCount(locked, filters) && <CurrentVisitors
					domain={state.domain}
					filters={filters}
					live={live}
					onOpen={() => onNavigate({ ...state, preset: "realtime", from: "", to: "", drawer: null })}
				/>}

				{/* Its own full-width line on a phone, where there is no room to
				    share one, and the elastic middle of the bar everywhere else:
				    `flex-1` gives it a base width of nothing, so however many
				    pills are on it the bar stays one line and the pills scroll. */}
				{filterBar && <div className="order-last flex w-full min-w-0 items-center sm:order-none sm:w-auto sm:flex-1">{filterBar}</div>}

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
					{/* A shared or public dashboard has no account menu to fold
					    these into, so it gets its own pair. The keys are still
					    bound there, and without a button the whole layer is
					    unreachable for the readers least able to ask for it back. */}
					{!navigation && !locked && <HelpButton onHelp={onHelp} />}
					{!navigation && <SettingsMenu view={view} onTheme={onTheme} onChart={onChart} onInterval={onInterval} onHelp={onHelp} />}
					{navigation && (
						<AccountMenu
							navigation={navigation}
							view={view}
							onTheme={onTheme}
							onChart={onChart}
							onInterval={onInterval}
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

/** MenuRow is one row in a menu. The kinds differ because they are different
 * controls, not different labels: a destination is a link, a theme is one of an
 * exclusive set, and signing out is a form that carries a token.
 *
 * A row carries everything it needs to be drawn, including the sign-out form's
 * target and token, so a menu can render a group of rows without also knowing
 * which account produced them. */
export type MenuRow =
	| { kind: "link"; id: string; label: string; href: string }
	| { kind: "action"; id: string; label: string; hint: string }
	| { kind: "theme"; id: string; label: string; theme: Theme; glyph: string; current: boolean }
	| { kind: "chart"; id: string; label: string; chart: ChartType; current: boolean }
	| { kind: "interval"; id: string; label: string; interval: IntervalPref; current: boolean }
	| { kind: "signout"; id: string; label: string; action: string; csrf: string };

/** MenuActions are the handlers a row can invoke. Every menu supplies all of
 * them, so a row added to the shared builders works wherever it lands rather
 * than being live in one menu and inert in the other. */
export interface MenuActions {
	onTheme: (next: Theme) => void;
	onChart: (next: ChartType) => void;
	onInterval: (next: IntervalPref) => void;
	onHelp: () => void;
}

/** ViewPrefs is everything the shared view menu draws.
 *
 * One argument rather than four positional ones, because the next preference
 * that earns a row would otherwise be the fifth thing a caller has to get in
 * the right order. */
export interface ViewPrefs {
	theme: Theme;
	/** The shape the graph is drawn as, or null on a screen with no graph. */
	chart: ChartType | null;
	/** The bucket width the reader has chosen, which may be one this range no
	 *  longer offers. The row it names simply goes unmarked. */
	interval: IntervalPref;
	/** The widths worth offering for the range on screen. Empty means the range
	 *  has only one sensible width, so the group is left out entirely. */
	intervals: IntervalPref[];
}

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
export function accountMenuGroups(
	navigation: Navigation,
	view: ViewPrefs,
	shortcuts: boolean,
): MenuGroup[] {
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

	groups.push(...viewGroups(view));

	groups.push({
		id: "session",
		rows: [{
			kind: "signout",
			id: "signout",
			label: t("dashboard.navigation.sign_out"),
			action: navigation.logout_url,
			csrf: navigation.csrf,
		}],
	});

	return groups;
}

/**
 * viewGroups is how the graph is drawn and what colour the page is, in the
 * order both menus show them, and it is the whole of the settings menu.
 *
 * One function rather than a list per menu. The shape control was missing from
 * a public dashboard because its rows lived in the account menu's own markup,
 * so there was nowhere else for them to come from; a builder both callers share
 * cannot drift like that again.
 */
export function viewGroups(view: ViewPrefs): MenuGroup[] {
	const groups: MenuGroup[] = [];

	if (view.chart) {
		groups.push({
			id: "graph",
			label: t("dashboard.menu.graph"),
			rows: CHART_TYPES.map((type) => ({
				kind: "chart" as const,
				id: `chart:${type}`,
				label: t(CHART_LABELS[type]),
				chart: type,
				current: type === view.chart,
			})),
		});
	}

	// A screen with no graph has no buckets to widen, and a range with one
	// sensible width has nothing to choose between. Both leave the group out
	// rather than showing rows that change nothing.
	if (view.chart && view.intervals.length > 1) {
		groups.push({
			id: "interval",
			label: t("dashboard.menu.interval"),
			rows: view.intervals.map((width) => ({
				kind: "interval" as const,
				id: `interval:${width}`,
				label: t(INTERVAL_LABELS[width]),
				interval: width,
				current: width === view.interval,
			})),
		});
	}

	groups.push({
		id: "theme",
		label: t("dashboard.menu.theme"),
		rows: THEME_ROWS.map((row) => ({
			kind: "theme" as const,
			id: `theme:${row.theme}`,
			label: t(row.labelId),
			theme: row.theme,
			glyph: row.glyph,
			current: row.theme === view.theme,
		})),
	});

	return groups;
}

/** AccountMenu holds product navigation, the two controls that are pressed
 * rarely enough not to earn space beside the date range, and the
 * CSRF-protected sign-out. */
function AccountMenu({
	navigation,
	view,
	onTheme,
	onChart,
	onInterval,
	onHelp,
	shortcuts,
}: {
	navigation: Navigation;
	view: ViewPrefs;
	onTheme: (next: Theme) => void;
	onChart: (next: ChartType) => void;
	onInterval: (next: IntervalPref) => void;
	onHelp: () => void;
	shortcuts: boolean;
}) {
	const [open, setOpen] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => setOpen(false));

	const groups = accountMenuGroups(navigation, view, shortcuts);

	return (
		<div ref={wrap} className="relative" onKeyDown={(event) => stepFocus(wrap.current, event)}>
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
				<MenuPanel
					groups={groups}
					header={
						<div className="px-2.5 py-2">
							<p className="truncate text-sm font-medium text-body">{navigation.name}</p>
							<p className="truncate text-xs text-muted">{navigation.email}</p>
						</div>
					}
					actions={{
						onTheme,
						onChart,
						onInterval,
						onHelp: () => {
							setOpen(false);
							onHelp();
						},
					}}
				/>
			)}
		</div>
	);
}

/**
 * SettingsMenu is the same view preferences for a reader with no account.
 *
 * A shared or public dashboard is the copy strangers see, and it is the one
 * where nobody can ask us for a missing control. It gets the graph shape and
 * the theme from the same builder the account menu uses.
 */
function SettingsMenu({
	view,
	onTheme,
	onChart,
	onInterval,
	onHelp,
}: {
	view: ViewPrefs;
	onTheme: (next: Theme) => void;
	onChart: (next: ChartType) => void;
	onInterval: (next: IntervalPref) => void;
	onHelp: () => void;
}) {
	const [open, setOpen] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => setOpen(false));

	return (
		<div ref={wrap} className="relative" onKeyDown={(event) => stepFocus(wrap.current, event)}>
			<button
				type="button"
				aria-expanded={open}
				aria-haspopup="menu"
				aria-label={t("dashboard.topbar.settings")}
				title={t("dashboard.topbar.settings")}
				onClick={() => setOpen((was) => !was)}
				className="flex size-control items-center justify-center border-2 border-line bg-card text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
			>
				<GearIcon />
			</button>

			{open && (
				<MenuPanel
					groups={viewGroups(view)}
					actions={{
						onTheme,
						onChart,
						onInterval,
						onHelp: () => {
							setOpen(false);
							onHelp();
						},
					}}
				/>
			)}
		</div>
	);
}

/**
 * MenuPanel is the popover both menus draw into.
 *
 * It is the rows and their grouping; the arrow keys belong to the wrapper
 * outside it, because a menu that has only just been opened still has the focus
 * on its button.
 */
function MenuPanel({
	groups,
	header,
	actions,
}: {
	groups: MenuGroup[];
	header?: ReactNode;
	actions: MenuActions;
}) {
	return (
		<div
			role="menu"
			className="scroll-thin absolute right-0 mt-2 max-h-[calc(100vh-5rem)] w-60 max-w-[calc(100vw-1rem)] overflow-y-auto border-2 border-line bg-card p-1.5 pop"
		>
			{header}

			{groups.map((group, index) => (
				<div
					key={group.id}
					role="group"
					aria-label={group.label}
					className={`pt-1.5 ${header || index > 0 ? "border-t border-line" : ""}`}
				>
					{group.label && (
						<p aria-hidden="true" className="px-2.5 pt-1 pb-0.5 text-[10px] font-semibold tracking-wide text-muted uppercase">
							{group.label}
						</p>
					)}
					{group.rows.map((row) => (
						<MenuRowView key={row.id} row={row} actions={actions} />
					))}
				</div>
			))}
		</div>
	);
}

/**
 * stepFocus moves the focus between rows on the arrow keys, wrapping at both
 * ends so a reader cannot get stuck against the last row.
 *
 * `role=menu` promises a screen reader that the rows are arrow-navigable, so
 * both menus call this rather than one of them keeping the promise.
 */
function stepFocus(wrap: HTMLElement | null, event: KeyboardEvent<HTMLDivElement>) {
	const step = event.key === "ArrowDown" ? 1 : event.key === "ArrowUp" ? -1 : 0;

	if (step === 0 || !wrap) return;

	const rows = Array.from(wrap.querySelectorAll<HTMLElement>('[role^="menuitem"]'));

	if (rows.length === 0) return;

	// The browser would scroll the panel on an arrow key otherwise, moving the
	// rows out from under the focus they are meant to be following.
	event.preventDefault();

	const at = rows.indexOf(document.activeElement as HTMLElement);
	const next = at === -1 ? (step === 1 ? 0 : rows.length - 1) : (at + step + rows.length) % rows.length;

	rows[next]?.focus();
}

/** GearIcon is the settings button's face. Drawn rather than typed, because the
 *  ⚙ character renders at a different weight and baseline in every font the
 *  dashboard can fall back to. */
function GearIcon() {
	return (
		<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" className="fill-current">
			<path
				fillRule="evenodd"
				d="M12.92,6.49 L14.87,6.66 L14.87,9.34 L12.92,9.51 A5.15,5.15 0 0 1 12.55,10.42 L13.80,11.91 L11.91,13.80 L10.42,12.55 A5.15,5.15 0 0 1 9.51,12.92 L9.34,14.87 L6.66,14.87 L6.49,12.92 A5.15,5.15 0 0 1 5.58,12.55 L4.09,13.80 L2.20,11.91 L3.45,10.42 A5.15,5.15 0 0 1 3.08,9.51 L1.13,9.34 L1.13,6.66 L3.08,6.49 A5.15,5.15 0 0 1 3.45,5.58 L2.20,4.09 L4.09,2.20 L5.58,3.45 A5.15,5.15 0 0 1 6.49,3.08 L6.66,1.13 L9.34,1.13 L9.51,3.08 A5.15,5.15 0 0 1 10.42,3.45 L11.91,2.20 L13.80,4.09 L12.55,5.58 A5.15,5.15 0 0 1 12.92,6.49 Z M8,5.6 A2.4,2.4 0 1 0 8,10.4 A2.4,2.4 0 1 0 8,5.6 Z"
			/>
		</svg>
	);
}

/** How many bars each width's icon draws. The count is the point: the narrower
 *  the bucket, the more of them a graph is cut into, which is the difference
 *  between the choices rather than anything a label can say faster. "auto" gets
 *  the middle count, since it is a width and not a fourth kind of thing. */
const BUCKET_BARS: Record<IntervalPref, number> = { auto: 3, hour: 5, day: 4, week: 3, month: 2 };

/** BucketIcon draws a row of bars standing for one bucket width. */
function BucketIcon({ interval }: { interval: IntervalPref }) {
	const bars = BUCKET_BARS[interval];
	const pitch = 12 / bars;

	return (
		<svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true" className="shrink-0">
			<g fill="currentColor">
				{Array.from({ length: bars }, (_, index) => (
					<rect key={index} x={index * pitch + 0.5} y="2" width={pitch - 1} height="8" />
				))}
			</g>
		</svg>
	);
}

/** MenuRowView draws one row. Choosing a theme or a graph shape leaves the menu
 * open, because the page changes underneath it and the next choice is one click
 * away. */
function MenuRowView({ row, actions }: { row: MenuRow; actions: MenuActions }) {
	const base = "flex w-full items-center gap-2 px-2.5 py-2 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover";

	switch (row.kind) {
		case "link":
			return <a role="menuitem" href={row.href} className={`${base} text-body`}>{row.label}</a>;

		case "action":
			return (
				<button
					type="button"
					role="menuitem"
					onClick={actions.onHelp}
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
					onClick={() => actions.onTheme(row.theme)}
					className={`${base} ${row.current ? "font-medium text-body" : "text-body"}`}
				>
					<span aria-hidden="true" className="w-4 text-center text-muted">{row.glyph}</span>
					<span className="flex-1">{row.label}</span>
					{row.current && <span aria-hidden="true" className="text-accent-ink">✓</span>}
				</button>
			);

		case "chart":
			return (
				<button
					type="button"
					role="menuitemradio"
					aria-checked={row.current}
					onClick={() => actions.onChart(row.chart)}
					className={`${base} ${row.current ? "font-medium text-body" : "text-body"}`}
				>
					<span aria-hidden="true" className="flex w-4 justify-center text-muted">
						<ShapeIcon type={row.chart} />
					</span>
					<span className="flex-1">{row.label}</span>
					{row.current && <span aria-hidden="true" className="text-accent-ink">✓</span>}
				</button>
			);

		case "interval":
			return (
				<button
					type="button"
					role="menuitemradio"
					aria-checked={row.current}
					onClick={() => actions.onInterval(row.interval)}
					className={`${base} ${row.current ? "font-medium text-body" : "text-body"}`}
				>
					<span aria-hidden="true" className="flex w-4 justify-center text-muted">
						<BucketIcon interval={row.interval} />
					</span>
					<span className="flex-1">{row.label}</span>
					{row.current && <span aria-hidden="true" className="text-accent-ink">✓</span>}
				</button>
			);

		case "signout":
			return (
				<form method="post" action={row.action}>
					<input type="hidden" name="csrf_token" value={row.csrf} />
					<button type="submit" role="menuitem" className={`${base} text-down`}>{row.label}</button>
				</form>
			);

		default:
			return null;
	}
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
	// Quiet: this reads on a timer rather than because anybody asked, so it must
	// never move the page-level loading bar.
	const stats = useStats(domain, currentVisitorsRequest(filters), true, { quiet: true });

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
	const [open, setOpen] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => setOpen(false));

	const modes: CompareMode[] = ["off", "previous_period", "year_over_year"];

	return (
		<div ref={wrap} className="relative">
			<button
				type="button"
				aria-expanded={open}
				aria-haspopup="menu"
				aria-label={t("dashboard.compare.label")}
				onClick={() => setOpen((was) => !was)}
				className={`flex h-control items-center gap-1.5 border-2 border-line bg-card px-2.5 text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
 state.compare === "off" ? "text-muted" : "font-medium text-body"
				}`}
			>
				{t(COMPARE_LABELS[state.compare])}
				<Chevron />
			</button>

			{open && (
				<div role="menu" className="absolute right-0 z-40 mt-1 w-48 border-2 border-line bg-card p-1 pop">
					{modes.map((mode) => (
						<button
							key={mode}
							type="button"
							role="menuitemradio"
							aria-checked={mode === state.compare}
							onClick={() => {
								setOpen(false);
								onNavigate({ ...state, compare: mode });
							}}
							className={`flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
 mode === state.compare ? "font-medium text-accent-ink" : "text-body"
							}`}
						>
							<span className="flex-1">{t(COMPARE_LABELS[mode])}</span>
							{mode === state.compare && <span aria-hidden="true">✓</span>}
						</button>
					))}
				</div>
			)}
		</div>
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
