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
 * "Show me the same thing, one week earlier" is the second most common thing
 * anybody does on an analytics dashboard, and it was an arrow key nobody had
 * been told about. The arrows are the same action that key already performs,
 * not a second copy of it, and every row in the menu prints its key so the
 * keyboard layer teaches itself.
 */
export function PeriodPicker({
	state,
	label,
	onNavigate,
	onStep,
	resolved,
	pickCustom,
}: {
	state: UrlState;
	label: string;
	onNavigate: (next: UrlState) => void;
	onStep: (direction: -1 | 1) => void;
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

	// A hotkey pressed while the menu is open applies the period, and the menu
	// has to go with it: a list still offering choices over a dashboard that
	// already changed is a list describing the wrong thing.
	const selection = `${state.preset}|${state.from}|${state.to}`;

	useEffect(() => {
		setOpen(false);
		setCustom(false);
	}, [selection]);

	// Changing the period closes the drawer. A details view is about a slice of
	// a specific window, and leaving it open over a different one would show
	// numbers that no longer answer the question that was asked.
	const pick = (period: Period) => {
		if (period.id === "custom") {
			setCustom(true);
			return;
		}

		setOpen(false);
		setCustom(false);

		if (period.id === "yesterday") {
			const day = yesterday(today());

			onNavigate({ ...state, from: day.from, to: day.to, drawer: null });

			return;
		}

		onNavigate({ ...state, preset: period.preset ?? "28d", from: "", to: "", drawer: null });
	};

	return (
		<div ref={wrap} className="relative">
			{/* One bordered shell, so three hit areas read as one control
			    rather than as three loose buttons that happen to be adjacent. */}
			<div className="flex h-control items-stretch overflow-hidden rounded-md border border-line bg-card">
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
				<div role="menu" className="absolute right-0 z-40 mt-1 w-64 rounded-md border border-line bg-card p-1 shadow-lg">
					{PERIODS.map((period, index) => (
						<div key={period.id}>
							{index > 0 && PERIODS[index - 1]?.group !== period.group && <div className="my-1 border-t border-line" />}

							<button
								type="button"
								role="menuitem"
								onClick={() => pick(period)}
								className={`flex w-full items-center gap-2 rounded-sm px-2.5 py-1.5 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
									isCurrent(period, state) ? "font-medium text-accent" : "text-body"
								}`}
							>
								<span className="flex-1">{t(period.labelId)}</span>

								{/* The key is decoration for a reader who can see it,
								    and part of the row's name for one who cannot. */}
								<span aria-hidden="true" className="rounded border border-line px-1.5 text-[11px] tracking-wide text-muted uppercase">
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
 * It calls the same action the arrow keys do rather than stepping the window
 * itself, and it disables on the same predicate the keyboard ignores, so the
 * two routes to "one period earlier" cannot land in different places.
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
			aria-disabled={!allowed}
			onClick={() => onStep(direction)}
			aria-label={t(direction === -1 ? "dashboard.topbar.previous_period" : "dashboard.topbar.next_period")}
			// Hovering answers "earlier than what?" with the window itself
			// rather than with a direction the reader has to work out.
			title={next ? rangeLabel([next.from, next.to]) : undefined}
			className="flex w-7 items-center justify-center text-sm text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover hover:text-body disabled:pointer-events-none disabled:text-faint"
		>
			<span aria-hidden="true" className="rtl:-scale-x-100">{direction === -1 ? "‹" : "›"}</span>
		</button>
	);
}

/** isCurrent marks the row the dashboard is actually showing. A custom window
 *  is Yesterday only when it is exactly yesterday; any other pair of dates is
 *  a range somebody typed, and no preset row describes it. */
function isCurrent(period: Period, state: UrlState): boolean {
	if (state.from || state.to) {
		const day = yesterday(today());

		return period.id === "yesterday" && state.from === day.from && state.to === day.to;
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
