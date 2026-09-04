//
// PeriodPicker.tsx
// The date range: one control, three hit areas, and a menu that teaches its keys.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef, useState } from "react";

import { useDismiss } from "../lib/dom";
import { rangeLabel } from "../lib/format";
import { t } from "../lib/i18n";
import type { Period } from "../lib/period";
import { PERIODS, addDays, canStep, step, today, yesterday } from "../lib/period";
import type { UrlState } from "../lib/url";
import { Chevron } from "./atoms";

/**
 * PeriodPicker is the date range, its two arrows, and the menu behind it.
 *
 * Every route into it — a row, an arrow, a hotkey — runs the same two actions
 * the keyboard already had, so the mouse and the keyboard cannot land in
 * different places. Each row prints its key, which is how the keyboard layer
 * teaches itself to somebody who never presses `?`.
 */
export function PeriodPicker({
	state,
	label,
	onNavigate,
	onPeriod,
	onStep,
	asked,
	resolved,
}: {
	state: UrlState;
	label: string;
	onNavigate: (next: UrlState) => void;
	onPeriod: (period: Period) => void;
	onStep: (direction: -1 | 1) => void;
	asked: { id: string; at: number };
	resolved: string[] | undefined;
}) {
	const [open, setOpen] = useState(false);
	const [custom, setCustom] = useState(false);

	const wrap = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => {
		setOpen(false);
		setCustom(false);
	});

	// Every period request closes the menu, and Custom range opens the form
	// instead. This watches the counter rather than the URL the request
	// produced, because asking for the period already showing changes no URL and
	// would leave the menu open over a dashboard that had already answered. It
	// is also how the hotkey reaches the form without being made to click
	// "Custom range…" afterwards, which would be the shortcut doing half a job.
	useEffect(() => {
		if (asked.at === 0) return;

		setOpen(asked.id === "custom");
		setCustom(asked.id === "custom");
	}, [asked]);

	return (
		<div ref={wrap} className="relative">
			{/* One bordered shell, so three hit areas read as one control
			    rather than as three loose buttons that happen to be adjacent. */}
			<div className="flex h-control items-stretch overflow-hidden border-2 border-line bg-card">
				<StepButton state={state} direction={-1} onStep={onStep} />

				<button
					type="button"
					aria-expanded={open}
					aria-haspopup="menu"
					onClick={() => setOpen((was) => !was)}
					className="flex items-center gap-1.5 border-x border-line px-2.5 text-sm font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
					title={resolved ? rangeLabel(resolved) : undefined}
				>
					{label}
					<Chevron />
				</button>

				<StepButton state={state} direction={1} onStep={onStep} />
			</div>

			{open && (
				<div role="menu" className="absolute right-0 z-40 mt-1 w-64 border-2 border-line bg-card p-1 pop">
					{PERIODS.map((period, index) => (
						// The wrapper exists only to hang the divider on the row.
						// role="none" keeps it out of the menu's own parent-to-
						// menuitem relationship.
						<div key={period.id} role="none">
							{index > 0 && PERIODS[index - 1]?.group !== period.group && (
								<div role="separator" className="my-1 border-t border-line" />
							)}

							<button
								type="button"
								role="menuitem"
								onClick={() => onPeriod(period)}
								className={`flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
 isCurrent(period, state) ? "font-medium text-accent-ink" : "text-body"
								}`}
							>
								<span className="flex-1">{t(period.labelId)}</span>

								{/* The key is decoration for a reader who can see it,
								    and part of the row's name for one who cannot. */}
								<span aria-hidden="true" className="border-2 border-line px-1.5 text-[11px] tracking-wide text-muted uppercase">
									{period.key}
								</span>
								<span className="sr-only">{t("dashboard.topbar.shortcut_key", { key: period.key.toUpperCase() })}</span>
							</button>
						</div>
					))}

					{custom && (
						<CustomRange
							from={state.from}
							to={state.to}
							onApply={(from, to) => {
								setOpen(false);
								setCustom(false);
								onNavigate({ ...state, from, to, drawer: null });
							}}
						/>
					)}

					{/* The resolved window is shown under the menu because date
					    maths is the single biggest source of "your numbers are
					    wrong", and the answer is usually that the period was not
					    the one the reader assumed. */}
					{resolved && (
						<p className="border-t border-line px-2.5 pt-2 pb-1 text-[11px] text-muted">{rangeLabel(resolved)}</p>
					)}
				</div>
			)}
		</div>
	);
}

/**
 * StepButton is one arrow.
 *
 * It calls the action the arrow keys call rather than stepping the window
 * itself, and disables on the predicate those keys obey.
 */
function StepButton({
	state,
	direction,
	onStep,
}: {
	state: UrlState;
	direction: -1 | 1;
	onStep: (direction: -1 | 1) => void;
}) {
	const allowed = canStep(state.preset, state.from, state.to, today(), direction);
	const next = allowed ? step(state.preset, state.from, state.to, today(), direction) : null;

	return (
		<button
			type="button"
			disabled={!allowed}
			onClick={() => onStep(direction)}
			aria-label={t(direction === -1 ? "dashboard.topbar.previous_period" : "dashboard.topbar.next_period")}
			// Hovering answers "earlier than what?" with the window itself
			// rather than with a direction the reader has to work out.
			title={next ? rangeLabel([next.from, next.to]) : undefined}
			className="flex w-7 items-center justify-center text-sm text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover hover:text-body disabled:text-faint"
		>
			<span aria-hidden="true" className="rtl:-scale-x-100">{direction === -1 ? "‹" : "›"}</span>
		</button>
	);
}

/**
 * isCurrent marks the row the dashboard is actually showing.
 *
 * A pair of dates is Yesterday only when it is exactly yesterday; any other pair
 * is a range somebody typed, and the row that describes that is Custom range.
 * Leaving it unmarked makes an open menu look as though nothing is selected.
 */
export function isCurrent(period: Period, state: UrlState): boolean {
	if (state.from || state.to) {
		const day = yesterday(today());
		const exactlyYesterday = state.from === day.from && state.to === day.to;

		return exactlyYesterday ? period.id === "yesterday" : period.id === "custom";
	}

	return period.preset === state.preset;
}

/** CustomRange is the two-date form. It applies nothing until both bounds are
 *  set, so a half-typed range never fires a query the server will refuse. */
function CustomRange({ from, to, onApply }: { from: string; to: string; onApply: (from: string, to: string) => void }) {
	const [start, setStart] = useState(from || addDays(today(), -6));
	const [end, setEnd] = useState(to || today());

	return (
		<div className="flex flex-col gap-2 border-t border-line px-2.5 py-2">
			<label className="flex items-center justify-between gap-2 text-xs text-muted">
				{t("dashboard.topbar.from")}
				<input
					type="date"
					value={start}
					max={end}
					onChange={(event) => setStart(event.target.value)}
					className="h-control border-2 border-line bg-card px-2 text-sm text-body"
				/>
			</label>
			<label className="flex items-center justify-between gap-2 text-xs text-muted">
				{t("dashboard.topbar.to")}
				<input
					type="date"
					value={end}
					min={start}
					onChange={(event) => setEnd(event.target.value)}
					className="h-control border-2 border-line bg-card px-2 text-sm text-body"
				/>
			</label>
			<button
				type="button"
				disabled={!start || !end}
				onClick={() => onApply(start, end)}
				className="h-control bg-accent text-sm font-medium text-fill-fg transition-opacity duration-150 ease-[var(--ease-ui)] hover:opacity-90 disabled:opacity-40"
			>
				{t("dashboard.topbar.apply")}
			</button>
		</div>
	);
}
