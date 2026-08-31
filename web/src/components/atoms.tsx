//
// atoms.tsx
// The small pieces every card is assembled from.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useId, useState } from "react";

import { t } from "../lib/i18n";

/**
 * Spinner is the only loading affordance in the product.
 *
 * The 100ms fade-in grace on `.spinner-grace` is the whole point of it. An
 * unfiltered 28-day query can take seconds, but a cached one comes back in
 * under a tenth of a second, and a spinner that flashes for 80ms reads as the
 * page glitching rather than as work happening.
 */
export function Spinner({ label = t("common.state.loading") }: { label?: string }) {
	return (
		<div className="spinner-grace flex h-full w-full items-center justify-center" role="status" aria-live="polite">
			<span className="spinner-ring block size-6 rounded-full border-2 border-line border-t-accent" />
			<span className="sr-only">{label}</span>
		</div>
	);
}

/**
 * ChangeChip renders a percentage change beside a figure.
 *
 * A null change is rendered as an em dash rather than as 0% or ∞. The engine
 * returns null when the earlier period was zero, and there is no meaningful
 * percentage change from nothing — printing one puts a fabricated number on the
 * page next to a real one.
 */
export function ChangeChip({ change, invert = false }: { change: number | null | undefined; invert?: boolean }) {
	if (change === null || change === undefined) {
		return (
			<span className="text-xs text-muted" title={t("dashboard.change.no_baseline")}>
				{t("common.state.dash")}
			</span>
		);
	}

	const rounded = Math.round(change);

	if (rounded === 0) return <span className="tnum text-xs text-muted">0%</span>;

	// Bounce rate going up is bad and pageviews going up is good, so the sign
	// alone cannot decide the colour. Getting this backwards on one metric is
	// worse than having no colour at all.
	const good = invert ? rounded < 0 : rounded > 0;

	return (
		<span className={`tnum text-xs font-medium ${good ? "text-up" : "text-down"}`}>
			{rounded > 0 ? "↑" : "↓"} {Math.abs(rounded)}%
		</span>
	);
}

/**
 * InfoDot is a hover-and-focus explanation for a number that looks like a bug.
 *
 * Two figures in this product generate a steady stream of bug reports wherever
 * they appear — per-source visitors summing past the total, and unique visitors
 * exceeding pageviews — and both are correct. A footnote nobody can find is the
 * same as no footnote, so the explanation sits on the number itself.
 */
export function InfoDot({ text }: { text: string | string[] }) {
	const [open, setOpen] = useState(false);
	const id = useId();

	// A tab can add a caveat of its own on top of the card's — "cities are
	// approximate" is true of one tab and false of the two beside it — so the
	// note is a list of paragraphs rather than one string.
	const paragraphs = (Array.isArray(text) ? text : [text]).filter(Boolean);

	return (
		<span className="relative inline-flex">
			<button
				type="button"
				aria-label={t("dashboard.infodot.label")}
				aria-describedby={open ? id : undefined}
				className="flex size-4 items-center justify-center rounded-full border border-line text-[9px] leading-none font-semibold text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:border-accent hover:text-accent"
				onMouseEnter={() => setOpen(true)}
				onMouseLeave={() => setOpen(false)}
				onFocus={() => setOpen(true)}
				onBlur={() => setOpen(false)}
				onClick={(event) => {
					event.stopPropagation();
					setOpen((was) => !was);
				}}
			>
				?
			</button>

			{open && (
				<span
					id={id}
					role="tooltip"
					className="absolute top-6 left-1/2 z-30 flex w-64 -translate-x-1/2 flex-col gap-2 rounded-md border border-line bg-card p-3 text-xs leading-relaxed font-normal text-body shadow-lg"
				>
					{paragraphs.map((paragraph) => (
						<span key={paragraph}>{paragraph}</span>
					))}
				</span>
			)}
		</span>
	);
}

/**
 * Empty is what a card shows when the query succeeded and there was nothing in
 * it. It is deliberately not the same as an error: "no data" is a fact about
 * the site, and dressing it up as a failure sends people looking for a broken
 * tracker that is working fine.
 */
export function Empty({ what }: { what: string }) {
	return (
		<div className="flex h-full flex-col items-center justify-center gap-1 px-6 text-center">
			<p className="text-sm text-body">{t("dashboard.empty.no_data", { what })}</p>
			<p className="text-xs text-muted">{t("dashboard.empty.hint")}</p>
		</div>
	);
}

/**
 * Failure shows the server's own sentence and a way to try again.
 *
 * The endpoint answers a bad request with a message written for the person
 * holding it. Replacing that with "something went wrong" is how a typo in a
 * date becomes a support ticket.
 */
export function Failure({ message, onRetry }: { message: string; onRetry: () => void }) {
	return (
		<div className="flex h-full flex-col items-center justify-center gap-3 px-6 text-center">
			<p className="text-sm text-down">{message}</p>
			<button
				type="button"
				onClick={onRetry}
				className="h-control rounded-md border border-line px-3 text-xs font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
			>
				{t("common.action.retry")}
			</button>
		</div>
	);
}

/**
 * Favicon renders the icon beside a source row.
 *
 * The image is served by our own proxy rather than pulled from the source's
 * origin, so that opening a dashboard does not fan out a request to every site
 * that ever linked to yours — which would leak your traffic sources to them,
 * one referrer header at a time, on a privacy-first product.
 */
export function Favicon({ name }: { name: string }) {
	return (
		<img
			src={`/favicon/sources/${encodeURIComponent(name || "Direct")}`}
			alt=""
			aria-hidden="true"
			width={16}
			height={16}
			loading="lazy"
			className="size-4 shrink-0 rounded-[3px]"
		/>
	);
}

/**
 * Flag is the country marker on a location row.
 *
 * It is two regional indicator code points rendered by the platform, not an
 * image: a flag per row as a request would mean a fan-out to some icon host on
 * every paint of the locations card, which is exactly the behaviour this product
 * exists to not have. It is decorative — the row's own text already names the
 * country — so screen readers skip it.
 */
export function Flag({ glyph }: { glyph: string }) {
	if (!glyph) return null;

	return (
		<span aria-hidden="true" className="w-4 shrink-0 text-center text-sm leading-none">
			{glyph}
		</span>
	);
}

/**
 * Bar is the tinted background a report row's value is drawn against.
 *
 * It is a positioned block behind the text rather than a border or a gradient,
 * because the label has to stay readable at every width — including at 100%,
 * where a foreground bar would sit on top of the first character of every row.
 */
export function Bar({ share }: { share: number }) {
	return (
		<span
			aria-hidden="true"
			className="absolute inset-y-px left-0 rounded-sm bg-bar transition-[width,background-color] duration-150 ease-[var(--ease-ui)] group-hover/row:bg-bar-strong"
			style={{ width: `${Math.max(0.6, Math.min(100, share * 100))}%` }}
		/>
	);
}
