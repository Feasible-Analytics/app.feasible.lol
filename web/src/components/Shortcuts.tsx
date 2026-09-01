//
// Shortcuts.tsx
// The keyboard layer, and the one modal in the product.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef } from "react";

import type { Preset } from "../api/types";
import { t } from "../lib/i18n";

/**
 * Shortcuts nobody can discover are shortcuts nobody uses.
 *
 * A dashboard people live in earns its keyboard layer several times over, but
 * only if there is a way to find out it has one. The `?` overlay is that way,
 * and it is generated from the same table the key handler reads, so a shortcut
 * cannot exist without appearing in the list or appear in the list without
 * working.
 *
 * This overlay is the one modal in the product. Everything else that could have
 * been a modal — the details view, the filter editor — is a drawer or a popover,
 * because both are about the numbers on the page behind them. A list of key
 * bindings is not, so covering the page costs nothing.
 */

/** A period the keyboard can jump to. `yesterday` and `custom` have no preset:
 *  one travels as an explicit pair of dates, the other opens the picker. */
export interface PeriodKey {
	key: string;
	labelId: string;
	preset?: Preset;
	custom?: "yesterday" | "pick";
}

/** The period names are the top bar's own, not a second set. A shortcut list
 *  that named a period differently from the menu would be describing a period
 *  the reader cannot then find. */
export const PERIOD_KEYS: PeriodKey[] = [
	{ key: "d", labelId: "dashboard.topbar.period.day", preset: "day" },
	{ key: "e", labelId: "dashboard.topbar.period.yesterday", custom: "yesterday" },
	{ key: "r", labelId: "dashboard.topbar.period.realtime", preset: "realtime" },
	{ key: "h", labelId: "dashboard.topbar.period.24h", preset: "24h" },
	{ key: "w", labelId: "dashboard.topbar.period.7d", preset: "7d" },
	{ key: "f", labelId: "dashboard.topbar.period.28d", preset: "28d" },
	{ key: "n", labelId: "dashboard.topbar.period.91d", preset: "91d" },
	{ key: "m", labelId: "dashboard.topbar.period.month", preset: "month" },
	{ key: "p", labelId: "dashboard.topbar.period.last_month", preset: "last_month" },
	{ key: "y", labelId: "dashboard.topbar.period.year", preset: "year" },
	{ key: "l", labelId: "dashboard.topbar.period.12mo", preset: "12mo" },
	{ key: "a", labelId: "dashboard.topbar.period.all", preset: "all" },
	{ key: "c", labelId: "dashboard.topbar.custom_range", custom: "pick" },
];

/** The bindings that are not periods, listed for the overlay. */
const ACTION_KEYS: { key: string; labelId: string }[] = [
	{ key: "←  →", labelId: "dashboard.shortcuts.step" },
	{ key: "X", labelId: "dashboard.shortcuts.compare" },
	{ key: "I", labelId: "dashboard.shortcuts.interval" },
	{ key: "K", labelId: "dashboard.shortcuts.annotations" },
	{ key: "/", labelId: "dashboard.shortcuts.search" },
	{ key: "0", labelId: "dashboard.shortcuts.sites" },
	{ key: "?", labelId: "dashboard.shortcuts.list" },
	{ key: "Esc", labelId: "dashboard.shortcuts.escape" },
];

/** What the keyboard can ask the dashboard to do. It is an interface so the
 *  handler and the overlay are the only two things that know about keys, and
 *  every action stays a plain function App already has. */
export interface ShortcutActions {
	onPeriod: (period: PeriodKey) => void;
	onStep: (direction: -1 | 1) => void;
	onCompare: () => void;
	onInterval: () => void;
	onAnnotations: () => void;
	onSearch: () => void;
	onSites: () => void;
	onHelp: () => void;
	onEscape: () => void;
}

/**
 * typing reports whether the keystroke belongs to a field rather than to the
 * page. Without it, typing "wednesday" into a filter box jumps the dashboard to
 * last week halfway through the word.
 */
function typing(target: EventTarget | null): boolean {
	if (!(target instanceof HTMLElement)) return false;

	if (target.isContentEditable) return true;

	return ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName);
}

/**
 * useShortcuts wires the keyboard to the dashboard.
 *
 * Modified keystrokes are left alone entirely: `d` is ours, but ⌘D is the
 * browser's bookmark and stealing it is the fastest way to make somebody turn
 * the whole feature off.
 */
export function useShortcuts(actions: ShortcutActions): void {
	// The handler is registered once and reads the current actions through a ref,
	// so a re-render does not tear the listener down and put it back on every
	// keystroke.
	const saved = useRef(actions);
	saved.current = actions;

	useEffect(() => {
		const onKey = (event: KeyboardEvent) => {
			if (event.metaKey || event.ctrlKey || event.altKey) return;

			const current = saved.current;
			const inField = typing(event.target);

			// Escape is the one key a field never keeps. The `0` shortcut focuses
			// the site picker, and a focused picker swallows every other shortcut
			// — so without a keyboard way back out, pressing `0` by accident
			// leaves the keyboard layer dead until somebody reaches for a mouse.
			if (event.key === "Escape") {
				if (inField) {
					(event.target as HTMLElement).blur();
					return;
				}

				current.onEscape();
				return;
			}

			if (inField) return;

			if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
				event.preventDefault();
				current.onStep(event.key === "ArrowLeft" ? -1 : 1);
				return;
			}

			if (event.key === "?") {
				event.preventDefault();
				current.onHelp();
				return;
			}

			if (event.key === "/") {
				event.preventDefault();
				current.onSearch();
				return;
			}

			if (event.key === "0") {
				event.preventDefault();
				current.onSites();
				return;
			}

			const key = event.key.toLowerCase();

			if (key === "x") {
				current.onCompare();
				return;
			}

			if (key === "i") {
				current.onInterval();
				return;
			}

			if (key === "k") {
				current.onAnnotations();
				return;
			}

			const period = PERIOD_KEYS.find((entry) => entry.key === key);
			if (period) current.onPeriod(period);
		};

		document.addEventListener("keydown", onKey);

		return () => document.removeEventListener("keydown", onKey);
	}, []);
}

/**
 * ShortcutsModal lists every binding.
 *
 * It is generated from the same two tables the handler walks, so the list cannot
 * describe a key that does nothing — which is the failure mode of every
 * hand-maintained shortcut sheet.
 */
export function ShortcutsModal({ onClose }: { onClose: () => void }) {
	const closer = useRef<HTMLButtonElement>(null);

	useEffect(() => {
		// Focus lands on the close button rather than on the panel. Focusing the
		// panel would draw the page's focus ring around the whole dialog, and a
		// glowing box is not what a focus ring is for.
		closer.current?.focus();

		const onKey = (event: KeyboardEvent) => {
			if (event.key !== "Escape") return;

			event.stopPropagation();
			onClose();
		};

		document.addEventListener("keydown", onKey, true);

		return () => document.removeEventListener("keydown", onKey, true);
	}, [onClose]);

	return (
		<div className="fixed inset-0 z-[60] flex items-center justify-center p-4">
			<button
				type="button"
				aria-label={t("dashboard.shortcuts.close")}
				onClick={onClose}
				className="modal-in absolute inset-0 bg-[var(--fs-scrim-modal)]"
			/>

			<div
				role="dialog"
				aria-modal="true"
				aria-label={t("dashboard.shortcuts.title")}
				className="modal-in relative max-h-full w-full max-w-2xl overflow-auto rounded-md border border-line bg-card shadow-2xl"
			>
				<div className="flex items-center border-b border-line px-5 py-3">
					<h2 className="text-sm font-semibold text-body">{t("dashboard.shortcuts.title")}</h2>

					<button
						ref={closer}
						type="button"
						onClick={onClose}
						aria-label={t("dashboard.shortcuts.close")}
						className="ml-auto flex size-control items-center justify-center rounded-md border border-line text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
					>
						✕
					</button>
				</div>

				<div className="grid grid-cols-1 gap-x-8 gap-y-6 px-5 py-4 sm:grid-cols-2">
					<Section title={t("dashboard.shortcuts.periods")} rows={PERIOD_KEYS} />
					<Section title={t("dashboard.shortcuts.other")} rows={ACTION_KEYS} />
				</div>

				<p className="border-t border-line px-5 py-3 text-xs text-muted">
					{t("dashboard.shortcuts.footnote")}
				</p>
			</div>
		</div>
	);
}

/** Section is one titled column of the list. */
function Section({ title, rows }: { title: string; rows: { key: string; labelId: string }[] }) {
	return (
		<div>
			<h3 className="mb-2 text-[11px] font-medium tracking-wide text-muted uppercase">{title}</h3>

			<ul className="flex flex-col gap-1.5">
				{rows.map((row) => (
					<li key={row.key} className="flex items-center gap-3 text-sm">
						<kbd className="flex h-6 min-w-6 shrink-0 items-center justify-center rounded border border-line bg-subtle px-1.5 font-mono text-xs font-medium text-body uppercase">
							{row.key}
						</kbd>
						<span className="text-body">{t(row.labelId)}</span>
					</li>
				))}
			</ul>
		</div>
	);
}
