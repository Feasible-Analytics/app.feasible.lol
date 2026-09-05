//
// SitePicker.tsx
// Switching sites: a searchable menu that also holds the two site destinations.
//
// Created: 2026-09-04
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useMemo, useRef, useState } from "react";

import type { Navigation } from "../api/types";
import { useDismiss } from "../lib/dom";
import { t } from "../lib/i18n";
import { Chevron } from "./atoms";

/** SEARCH_THRESHOLD is how many sites earn a search box. Below it the whole
 *  list is on screen already, and a field to filter four rows is a field that
 *  only ever costs a keystroke. */
export const SEARCH_THRESHOLD = 8;

/**
 * matchSites narrows the list to what somebody typed.
 *
 * The match is a plain case-insensitive substring rather than anything fuzzier:
 * these are domains, the reader knows how theirs is spelled, and a clever
 * matcher that ranks "acme.com" below "my-acme-staging.dev" for the query
 * "acme" is worse than no ranking at all. Order is preserved so the list does
 * not rearrange itself under the cursor as characters arrive.
 */
export function matchSites(sites: string[], search: string): string[] {
	const needle = search.trim().toLowerCase();
	if (needle === "") return sites;

	return sites.filter((site) => site.toLowerCase().includes(needle));
}

/**
 * SitePicker switches sites, and is the way to the two places that belong to
 * the site rather than to the dashboard.
 *
 * It was a native select, which handed us type-ahead and a mobile picker for
 * free. It is a menu now because the two destinations have to live with the
 * name they belong to: a gear floating beside the control was a second, unnamed
 * button whose target changed depending on a dropdown next to it.
 */
export function SitePicker({
	current,
	sites,
	navigation,
	onPick,
}: {
	current: string;
	sites: string[];
	navigation?: Navigation;
	onPick: (domain: string) => void;
}) {
	const [open, setOpen] = useState(false);
	const [search, setSearch] = useState("");
	const [active, setActive] = useState(0);

	const wrap = useRef<HTMLDivElement>(null);
	const field = useRef<HTMLInputElement>(null);
	const list = useRef<HTMLDivElement>(null);

	useDismiss(wrap, open, () => setOpen(false));

	const matches = useMemo(() => matchSites(sites, search), [sites, search]);
	const searchable = sites.length >= SEARCH_THRESHOLD;

	// Opening starts on the site already showing, so Enter without touching
	// anything is a no-op rather than a jump to whatever sorted first.
	useEffect(() => {
		if (!open) return;

		setSearch("");
		setActive(Math.max(0, sites.indexOf(current)));

		if (searchable) field.current?.focus();
	}, [open, sites, current, searchable]);

	// Typing moves the highlight back to the top, because the row it was on may
	// no longer be in the list at all.
	useEffect(() => setActive(0), [search]);

	// The highlighted row is scrolled into view rather than left below the fold.
	// An account with fifty sites is exactly who arrows through this list.
	useEffect(() => {
		if (!open) return;

		list.current?.querySelector<HTMLElement>('[data-active="true"]')
			?.scrollIntoView({ block: "nearest" });
	}, [open, active]);

	if (sites.length === 0) return null;

	// Arrow keys move the highlight and Enter takes it. They are handled on the
	// wrapper rather than on the field, so they work the same whether or not the
	// list is long enough to have a search box to type into.
	const onKeyDown = (event: React.KeyboardEvent) => {
		if (event.key === "ArrowDown" || event.key === "ArrowUp") {
			event.preventDefault();

			if (matches.length === 0) return;

			const step = event.key === "ArrowDown" ? 1 : -1;
			setActive((was) => (was + step + matches.length) % matches.length);

			return;
		}

		if (event.key === "Enter") {
			const picked = matches[active];
			if (!picked) return;

			event.preventDefault();
			setOpen(false);
			onPick(picked);
		}
	};

	return (
		<div ref={wrap} className="relative" onKeyDown={onKeyDown}>
			<button
				type="button"
				// The id is what the `0` shortcut reaches for. A ref threaded down
				// from App would be the same lookup with more moving parts, on a
				// control there is exactly one of.
				id="site-picker"
				aria-expanded={open}
				aria-haspopup="menu"
				aria-label={t("dashboard.topbar.site")}
				onClick={() => setOpen((was) => !was)}
				className="flex h-control max-w-52 items-center gap-1.5 border-2 border-line bg-card px-2.5 text-sm font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
			>
				<span className="truncate">{current}</span>
				<Chevron className="shrink-0" />
			</button>

			{open && (
				<div role="menu" className="absolute left-0 z-40 mt-1 w-72 max-w-[calc(100vw-1rem)] border-2 border-line bg-card p-1 pop">
					{/* The two destinations that belong to the site, not to the
					    dashboard. They sit under the name they act on, which is
					    the whole reason they moved out of the bar. A shared or
					    public dashboard has neither. */}
					{navigation && (
						<div className="flex gap-1 border-b border-line pb-1">
							<a
								role="menuitem"
								href={navigation.sites_url}
								className="flex flex-1 items-center justify-center gap-1.5 border-2 border-line px-2 py-1.5 text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
							>
								<BackIcon />
								{t("dashboard.navigation.sites")}
							</a>

							{navigation.site_settings_url && (
								<a
									role="menuitem"
									href={navigation.site_settings_url}
									className="flex flex-1 items-center justify-center gap-1.5 border-2 border-line px-2 py-1.5 text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
								>
									<SettingsIcon />
									{t("dashboard.navigation.site_settings")}
								</a>
							)}
						</div>
					)}

					{searchable && (
						<div className="border-b border-line p-1">
							<input
								ref={field}
								type="search"
								value={search}
								onChange={(event) => setSearch(event.target.value)}
								placeholder={t("dashboard.topbar.site_search")}
								aria-label={t("dashboard.topbar.site_search")}
								className="h-8 w-full border-2 border-line bg-card px-2 text-sm text-body placeholder:text-faint focus:border-accent focus:outline-none"
							/>
						</div>
					)}

					<div ref={list} className="scroll-thin max-h-72 overflow-y-auto pt-1">
						{matches.map((site, index) => (
							<button
								key={site}
								type="button"
								role="menuitemradio"
								aria-checked={site === current}
								data-active={index === active}
								onMouseMove={() => setActive(index)}
								onClick={() => {
									setOpen(false);
									onPick(site);
								}}
								className={`flex w-full items-center gap-2 px-2.5 py-1.5 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] ${
 index === active ? "bg-hover" : ""
								} ${site === current ? "font-medium text-accent-ink" : "text-body"}`}
							>
								<span className="flex-1 truncate">{site}</span>
								{site === current && <span aria-hidden="true">✓</span>}
							</button>
						))}

						{matches.length === 0 && (
							<p className="px-2.5 py-3 text-sm text-muted">{t("dashboard.topbar.site_none")}</p>
						)}
					</div>
				</div>
			)}
		</div>
	);
}

/** BackIcon is the arrow on the way out of one site and back to the list. */
function BackIcon() {
	return (
		<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="size-4 shrink-0" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
			<path d="M19 12H5" />
			<path d="m12 19-7-7 7-7" />
		</svg>
	);
}

/** SettingsIcon is an SVG rather than a text glyph so its weight, alignment,
 * and appearance stay consistent across browsers and operating systems. */
function SettingsIcon() {
	return (
		<svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="size-4 shrink-0" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
			<circle cx="12" cy="12" r="3" />
			<path d="M19.4 15a1.7 1.7 0 0 0 .34 1.88l.06.06-2.86 2.86-.06-.06A1.7 1.7 0 0 0 15 19.4a1.7 1.7 0 0 0-1 .6 1.7 1.7 0 0 0-.4 1.1V21H9.6v-.1A1.7 1.7 0 0 0 8.5 19.4a1.7 1.7 0 0 0-1.88.34l-.06.06-2.86-2.86.06-.06A1.7 1.7 0 0 0 4.1 15a1.7 1.7 0 0 0-1.5-1H2v-4h.6a1.7 1.7 0 0 0 1.5-1 1.7 1.7 0 0 0-.34-1.88l-.06-.06L6.56 4.2l.06.06A1.7 1.7 0 0 0 8.5 4.6a1.7 1.7 0 0 0 1-1.5V3h4v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.88-.34l.06-.06 2.86 2.86-.06.06A1.7 1.7 0 0 0 18.9 9a1.7 1.7 0 0 0 1.5 1h.6v4h-.6a1.7 1.7 0 0 0-1 .99Z" />
		</svg>
	);
}
