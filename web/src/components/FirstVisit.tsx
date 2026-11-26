//
// FirstVisit.tsx
// The dashboard for a site that has not recorded its first visit yet.
//
// Created: 2026-11-26
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useState } from "react";

import type { StatsRequest, Waiting } from "../api/types";
import { t } from "../lib/i18n";
import { useInterval, useStats } from "../lib/useStats";

/** How often the page asks whether the first visit has arrived. Somebody who
 *  has just pasted the snippet is watching this page, so it has to feel live;
 *  it only runs for a site with no visits, so the cost is a handful of small
 *  queries while they set up. */
export const FIRST_VISIT_POLL_MS = 5_000;

/** Every visit the site has ever had. It is a cheap question for a site that
 *  has none, which is the only site it is ever asked about. */
export const FIRST_VISIT_REQUEST: StatsRequest = { metrics: ["visitors"], date_range: "all" };

/**
 * FirstVisit takes the place of the reports for a site that has never recorded
 * a visit. A page of zeros that says "try a wider date range" reads as broken
 * to somebody who has not installed the snippet yet, so this says what is
 * happening and what to do: paste the snippet, or open the install screen for
 * the other ways in. A reader who cannot install the site gets the message
 * without either.
 *
 * It asks every few seconds whether the first visit has arrived and calls
 * onArrived when it has, so the reports appear without a reload.
 */
export function FirstVisit({ waiting, onArrived }: { waiting: Waiting; onArrived: () => void }) {
	// Quiet: this asks on a timer rather than because anybody did anything, so
	// it must never move the page-level loading bar.
	const stats = useStats(waiting.domain, FIRST_VISIT_REQUEST, true, { quiet: true });

	useInterval(stats.reload, FIRST_VISIT_POLL_MS);

	const visitors = stats.data?.results[0]?.metrics[0] ?? 0;

	useEffect(() => {
		if (visitors > 0) onArrived();
	}, [visitors, onArrived]);

	const canInstall = Boolean(waiting.snippet);

	return (
		<section className="border-2 border-line bg-card p-5 sm:p-8">
			<h1 className="text-xl font-semibold text-body">{t("dashboard.waiting.title")}</h1>
			<p className="mt-2 max-w-2xl text-sm leading-6 text-muted">
				{t(canInstall ? "dashboard.waiting.body" : "dashboard.waiting.body_viewer", { site: waiting.domain })}
			</p>

			{waiting.snippet && <Snippet code={waiting.snippet} installURL={waiting.install_url} />}

			<p role="status" className="mt-6 flex items-start gap-2 text-xs leading-5 text-muted">
				<span className="relative mt-1.5 flex size-2 shrink-0" aria-hidden="true">
					<span className="absolute inline-flex size-full animate-ping bg-accent opacity-60" />
					<span className="relative inline-flex size-2 bg-accent" />
				</span>
				<span>{t("dashboard.waiting.live")}</span>
			</p>
		</section>
	);
}

/**
 * Snippet shows the tracking code with a copy button, and a link to the install
 * screen for WordPress, tag managers and the rest. The code wraps rather than
 * scrolling sideways, so a phone shows all of it.
 */
function Snippet({ code, installURL }: { code: string; installURL?: string }) {
	const [copied, setCopied] = useState(false);

	// The button says "Copied" for a moment and then goes back, so a second
	// copy after an edit still gets an answer.
	useEffect(() => {
		if (!copied) return;

		const id = setTimeout(() => setCopied(false), 2_000);

		return () => clearTimeout(id);
	}, [copied]);

	// The clipboard can be refused (an insecure origin, a denied permission).
	// The code is on screen and selectable either way, so a refusal just
	// leaves the button as it was.
	const copy = () => {
		navigator.clipboard?.writeText(code).then(
			() => setCopied(true),
			() => {},
		);
	};

	return (
		<>
			<pre className="mt-5 overflow-hidden border-2 border-line bg-subtle p-3 font-mono text-xs leading-5 break-all whitespace-pre-wrap text-body">
				<code>{code}</code>
			</pre>
			<div className="mt-4 flex flex-wrap gap-3">
				<button
					type="button"
					onClick={copy}
					className="bg-accent px-4 py-2 text-sm font-semibold text-fill-fg hover:bg-accent-hover"
				>
					{t(copied ? "dashboard.waiting.copied" : "dashboard.waiting.copy")}
				</button>
				{installURL && (
					<a href={installURL} className="border-2 border-line px-4 py-2 text-sm font-medium text-body hover:bg-hover">
						{t("dashboard.waiting.install")}
					</a>
				)}
			</div>
		</>
	);
}
